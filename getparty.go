package getparty

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
	flags "github.com/jessevdk/go-flags"
	"github.com/pkg/errors"
	"github.com/vbauerster/backoff"
	"github.com/vbauerster/backoff/exponential"
	"github.com/vbauerster/mpb/v8"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"
)

type ExpectedError string

func (e ExpectedError) Error() string {
	return string(e)
}

type HttpError struct {
	StatusCode int
	Status     string
}

func (e HttpError) Error() string {
	return fmt.Sprintf("HTTP error: %s", e.Status)
}

const (
	ErrCanceledByUser = ExpectedError("Canceled by user")
	ErrMaxRedirect    = ExpectedError("Max redirects reached")
	ErrMaxRetry       = ExpectedError("Max retries reached")
)

const (
	cmdName     = "getparty"
	projectHome = "https://github.com/vbauerster/getparty"

	maxRedirects        = 10
	refreshRate         = 200
	hUserAgentKey       = "User-Agent"
	hContentDisposition = "Content-Disposition"
	hRange              = "Range"
	hCookie             = "Cookie"
)

// https://regex101.com/r/N4AovD/3
var reContentDisposition = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`)

var userAgents = map[string]string{
	"chrome":  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/105.0.0.0 Safari/537.36",
	"firefox": "Mozilla/5.0 (X11; Linux x86_64; rv:104.0) Gecko/20100101 Firefox/104.0",
	"safari":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.1 Safari/605.1.15",
	"edge":    "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.101 Safari/537.36 Edg/91.0.864.37",
}

// Options struct, represents cmd line options
type Options struct {
	Parts              uint              `short:"p" long:"parts" value-name:"n" default:"1" description:"number of parts"`
	MaxRetry           uint              `short:"r" long:"max-retry" value-name:"n" default:"10" description:"max retry per each part, 0 for infinite"`
	Timeout            uint              `short:"t" long:"timeout" value-name:"sec" default:"15" description:"context timeout"`
	OutFileName        string            `short:"o" long:"output" value-name:"filename" description:"user defined output"`
	JSONFileName       string            `short:"s" long:"session" value-name:"session.json" description:"path to saved session file (optional)"`
	UserAgent          string            `short:"a" long:"user-agent" choice:"chrome" choice:"firefox" choice:"safari" choice:"edge" choice:"getparty" default:"chrome" description:"User-Agent header"`
	BestMirror         bool              `short:"b" long:"best-mirror" description:"pickup the fastest mirror"`
	Quiet              bool              `short:"q" long:"quiet" description:"quiet mode, no progress bars"`
	ForceOverwrite     bool              `short:"f" long:"force" description:"overwrite existing file silently"`
	AuthUser           string            `short:"u" long:"username" description:"basic http auth username"`
	AuthPass           string            `long:"password" description:"basic http auth password"`
	HeaderMap          map[string]string `short:"H" long:"header" value-name:"key:value" description:"arbitrary http header"`
	InsecureSkipVerify bool              `long:"no-check-cert" description:"don't validate the server's certificate"`
	CertsFileName      string            `short:"c" long:"certs-file" value-name:"certs.crt" description:"root certificates to use when verifying server certificates"`
	Debug              bool              `long:"debug" description:"enable debug to stderr"`
	Version            bool              `short:"v" long:"version" description:"show version"`
}

type Cmd struct {
	Ctx     context.Context
	Out     io.Writer
	Err     io.Writer
	options *Options
	parser  *flags.Parser
	logger  *log.Logger
	dlogger *log.Logger
}

func (cmd Cmd) Exit(err error) int {
	if cmd.Ctx.Err() == context.Canceled {
		// most probably user hit ^C, so mark as expected
		err = errors.WithMessage(err, ErrCanceledByUser.Error())
	}
	switch e := errors.Cause(err).(type) {
	case nil:
		return 0
	case *flags.Error:
		if e.Type == flags.ErrHelp {
			return 0
		}
		cmd.parser.WriteHelp(cmd.Err)
		return 2
	case *url.Error:
		cmd.debugOrPrintErr(err, true)
		return cmd.Exit(e.Err)
	case ExpectedError:
		cmd.debugOrPrintErr(err, true)
		return 1
	default:
		cmd.debugOrPrintErr(err, false)
		return 3
	}
}

func (cmd Cmd) debugOrPrintErr(err error, expected bool) {
	var unexpected string
	if !expected {
		unexpected = "unexpected: "
	}
	if cmd.options.Debug {
		// if there is stack trace available, +v will include it
		cmd.dlogger.Printf("%s%+v", unexpected, err)
	} else {
		fmt.Fprintf(cmd.Err, "%s%s\n", unexpected, err.Error())
	}
}

func (cmd *Cmd) Run(args []string, version, commit string) (err error) {
	defer func() {
		// just add method name, without stack trace at the point
		err = errors.WithMessage(err, "run")
	}()
	userAgents[cmdName] = fmt.Sprintf("%s/%s", cmdName, version)
	cmd.options = new(Options)
	cmd.parser = flags.NewParser(cmd.options, flags.Default)
	cmd.parser.Name = cmdName
	cmd.parser.Usage = "[OPTIONS] url"

	args, err = cmd.parser.ParseArgs(args)
	if err != nil {
		return err
	}

	if cmd.options.Version {
		fmt.Fprintf(cmd.Out, "%s (%.7s) (%s)\n", userAgents[cmdName], commit, runtime.Version())
		fmt.Fprintf(cmd.Out, "Project home: %s\n", projectHome)
		return nil
	}

	var userInfo *url.Userinfo
	if cmd.options.AuthUser != "" {
		if cmd.options.AuthPass == "" {
			cmd.options.AuthPass, err = cmd.readPassword()
			if err != nil {
				return err
			}
		}
		userInfo = url.UserPassword(cmd.options.AuthUser, cmd.options.AuthPass)
	}

	setupLogger := func(out io.Writer, prefix string, discard bool) *log.Logger {
		if discard {
			out = io.Discard
		}
		return log.New(out, prefix, log.LstdFlags)
	}
	cmd.logger = setupLogger(cmd.Out, "", cmd.options.Quiet)
	cmd.dlogger = setupLogger(cmd.Err, fmt.Sprintf("[%s] ", cmdName), !cmd.options.Debug)

	if _, ok := cmd.options.HeaderMap[hUserAgentKey]; !ok {
		cmd.options.HeaderMap[hUserAgentKey] = userAgents[cmd.options.UserAgent]
	}

	if cmd.options.Timeout == 0 {
		cmd.options.Timeout = 15
	}

	if cmd.options.BestMirror {
		patcher := makeReqPatcher(cmd.options.HeaderMap, userInfo, false)
		url, err := cmd.bestMirror(args, patcher)
		if err != nil {
			return err
		}
		args = append(args[:0], url)
	}

	// All users of cookiejar should import "golang.org/x/net/publicsuffix"
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return err
	}

	var session *Session
	var location string

	for location == "" {
		switch {
		case cmd.options.JSONFileName != "":
			freshSession := session
			session = new(Session)
			err := session.loadState(cmd.options.JSONFileName)
			if err != nil {
				return err
			}
			err = session.checkPartsSize()
			if err != nil {
				return err
			}
			if freshSession != nil {
				err := session.checkSums(freshSession)
				if err != nil {
					return err
				}
				session.location = freshSession.location
			} else {
				setCookies(session.HeaderMap, session.URL, jar)
				if session.Redirected {
					patcher := makeReqPatcher(session.HeaderMap, userInfo, true)
					freshSession, err = cmd.follow(session.URL, jar, patcher)
					if err != nil {
						return err
					}
					session.location = freshSession.location
				} else {
					session.location = session.URL
				}
			}
			location = session.location
		case len(args) != 0:
			setCookies(cmd.options.HeaderMap, args[0], jar)
			patcher := makeReqPatcher(cmd.options.HeaderMap, userInfo, true)
			session, err = cmd.follow(args[0], jar, patcher)
			if err != nil {
				return err
			}
			state := session.SuggestedFileName + ".json"
			if _, err := os.Stat(state); err != nil {
				if cmd.options.Parts == 0 {
					session.writeSummary(cmd.Out, cmd.options.Quiet)
					return nil
				}
				err = session.checkExistingFile(cmd.Out, cmd.options.ForceOverwrite)
				if err != nil {
					return err
				}
				err = session.calcParts(cmd.options.Parts)
				if err != nil {
					return err
				}
				session.HeaderMap = cmd.options.HeaderMap
				location = session.location
			} else {
				cmd.options.JSONFileName = state
			}
		default:
			return new(flags.Error)
		}
	}

	session.writeSummary(cmd.Out, cmd.options.Quiet)

	progress := mpb.NewWithContext(cmd.Ctx,
		mpb.ContainerOptional(mpb.WithOutput(cmd.Out), !cmd.options.Quiet),
		mpb.ContainerOptional(mpb.WithOutput(nil), cmd.options.Quiet),
		mpb.ContainerOptional(mpb.WithDebugOutput(cmd.Err), cmd.options.Debug),
		mpb.WithRefreshRate(refreshRate*time.Millisecond),
		mpb.WithWidth(64),
	)

	transport, err := cmd.getTransport(true)
	if err != nil {
		return err
	}
	var eg errgroup.Group
	var partsDone uint32
	var tb *mpb.Bar
	tw := session.totalWritten()
	if !cmd.options.Quiet {
		tb = session.makeTotalBar(progress, &partsDone, tw)
	}
	patcher := makeReqPatcher(session.HeaderMap, userInfo, true)
	start := time.Now()
	for i, p := range session.Parts {
		if p.isDone() {
			atomic.AddUint32(&partsDone, 1)
			continue
		}
		p.order = i + 1
		p.name = fmt.Sprintf("P%02d", p.order)
		p.quiet = cmd.options.Quiet
		p.maxTry = int(cmd.options.MaxRetry)
		p.jar = jar
		p.transport = transport
		p.totalBar = tb
		p.dlogger = setupLogger(cmd.Err, fmt.Sprintf("[%s] ", p.name), !cmd.options.Debug)
		req, err := http.NewRequest(http.MethodGet, location, nil)
		if err != nil {
			cmd.logger.Fatalf("%s: %s", p.name, err.Error())
		}
		patcher(req)
		p := p // https://golang.org/doc/faq#closures_and_goroutines
		eg.Go(func() error {
			defer func() {
				if p := recover(); p != nil {
					cmd.dumpState(session)
					panic(p)
				}
				atomic.AddUint32(&partsDone, 1)
			}()
			return p.download(cmd.Ctx, progress, req, cmd.options.Timeout)
		})
	}

	err = eg.Wait()

	session.Parts = filter(session.Parts, func(p *Part) bool { return !p.Skip })

	if err != nil {
		if tb != nil {
			tb.Abort(false)
		}
		progress.Wait()
		if tw != session.totalWritten() {
			session.Elapsed += time.Since(start)
			cmd.dumpState(session)
		}
		return err
	}

	size, err := session.concatenateParts(cmd.dlogger, progress)
	progress.Wait()
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.Out)
	cmd.logger.Printf("%q saved [%d/%d]", session.SuggestedFileName, session.ContentLength, size)
	if session.ContentLength > 0 && size != session.ContentLength {
		return fmt.Errorf("Corrupted download: ExpectedSize=%d SavedSize=%d", session.ContentLength, size)
	}
	if cmd.options.JSONFileName != "" {
		return os.Remove(cmd.options.JSONFileName)
	}
	return nil
}

func (cmd Cmd) follow(
	usrURL string,
	jar http.CookieJar,
	reqPatcher func(*http.Request),
) (session *Session, err error) {
	var redirected bool
	var client *http.Client
	defer func() {
		if redirected && client != nil {
			client.CloseIdleConnections()
		}
		err = errors.Wrap(err, "follow")
	}()

	transport, err := cmd.getTransport(false)
	if err != nil {
		return nil, err
	}
	client = &http.Client{
		Transport: transport,
		Jar:       jar,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errors.WithMessagef(ErrMaxRedirect, "stopped after %d redirects", maxRedirects)
			}
			return http.ErrUseLastResponse
		},
	}

	location := usrURL

	err = backoff.Retry(cmd.Ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt int) (retry bool, err error) {
			for {
				cmd.logger.Printf("GET: %s", location)
				req, err := http.NewRequest(http.MethodGet, location, nil)
				if err != nil {
					return false, err
				}

				reqPatcher(req)

				resp, err := client.Do(req.WithContext(cmd.Ctx))
				if err != nil {
					cmd.dlogger.Printf("ERR: %s", err.Error())
					if attempt+1 == int(cmd.options.MaxRetry) {
						return false, errors.WithMessage(ErrMaxRetry, err.Error())
					}
					cmd.logger.Printf("Retrying follow: %d", attempt+1)
					return true, err
				}

				cmd.logger.Printf("HTTP response: %s", resp.Status)
				if cookies := jar.Cookies(req.URL); len(cookies) != 0 {
					cmd.dlogger.Println("CookieJar:")
					for _, cookie := range cookies {
						cmd.dlogger.Printf("  %q", cookie)
					}
				}

				if isRedirect(resp.StatusCode) {
					redirected = true
					loc, err := resp.Location()
					if err != nil {
						return false, err
					}
					location = loc.String()
					// don't bother closing resp.Body here,
					// it will be closed by underlying RoundTripper
					continue
				}

				if resp.StatusCode != http.StatusOK {
					err = &HttpError{resp.StatusCode, resp.Status}
					if isServerError(resp.StatusCode) {
						return attempt+1 != int(cmd.options.MaxRetry), err
					}
					return false, err
				}

				name := cmd.options.OutFileName
				for i := 0; name == ""; i++ {
					switch i {
					case 0:
						name = parseContentDisposition(resp.Header.Get(hContentDisposition))
					case 1:
						if nURL, err := url.Parse(location); err != nil {
							name = location
						} else {
							nURL.RawQuery = ""
							name, err = url.QueryUnescape(nURL.String())
							if err != nil {
								name = nURL.String()
							}
						}
						name = filepath.Base(name)
					default:
						name = "unknown"
					}
				}

				session = &Session{
					location:          location,
					URL:               usrURL,
					SuggestedFileName: name,
					ContentMD5:        resp.Header.Get("Content-MD5"),
					AcceptRanges:      resp.Header.Get("Accept-Ranges"),
					ContentType:       resp.Header.Get("Content-Type"),
					StatusCode:        resp.StatusCode,
					ContentLength:     resp.ContentLength,
					Redirected:        redirected,
				}

				resp.Body.Close()
				return false, nil
			}
		})
	return session, err
}

func (cmd Cmd) getTransport(pooled bool) (transport *http.Transport, err error) {
	if pooled {
		transport = cleanhttp.DefaultPooledTransport()
	} else {
		transport = cleanhttp.DefaultTransport()
	}
	transport.TLSHandshakeTimeout = time.Duration(cmd.options.Timeout) * time.Second
	if cmd.options.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	} else if cmd.options.CertsFileName != "" {
		caCerts, err := os.ReadFile(cmd.options.CertsFileName)
		if err != nil {
			return nil, err
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCerts)
		transport.TLSClientConfig = &tls.Config{RootCAs: caCertPool}
	}
	return transport, nil
}

func (cmd Cmd) bestMirror(args []string, reqPatcher func(*http.Request)) (best string, err error) {
	defer func() {
		// just add method name, without stack trace at the point
		err = errors.WithMessage(err, "bestMirror")
	}()
	input := os.Stdin
	if len(args) != 0 {
		fd, err := os.Open(args[0])
		if err != nil {
			return "", err
		}
		defer fd.Close()
		input = fd
	}
	urls, err := readLines(input)
	if err != nil {
		return "", err
	}

	var readyWg sync.WaitGroup
	start := make(chan struct{})
	first := make(chan string, 1)
	client := cleanhttp.DefaultClient()
	ctx, cancel := context.WithTimeout(cmd.Ctx, 15*time.Second)
	defer cancel()

	subscribe := func(wg *sync.WaitGroup, start <-chan struct{}, fn func()) {
		go func() {
			wg.Done()
			<-start
			fn()
		}()
	}

	for _, u := range urls {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			cmd.dlogger.Printf("skipping %q: %s", u, err.Error())
			continue
		}
		reqPatcher(req)
		readyWg.Add(1)
		u := u // https://golang.org/doc/faq#closures_and_goroutines
		subscribe(&readyWg, start, func() {
			cmd.dlogger.Printf("fetching: %q", u)
			resp, err := client.Do(req.WithContext(ctx))
			if err != nil {
				cmd.dlogger.Printf("fetch error: %s", err.Error())
			}
			if resp == nil || resp.Body == nil {
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				cmd.dlogger.Printf("fetch %q unexpected status: %s", u, resp.Status)
				return
			}
			select {
			case first <- u:
			default:
			}
		})
	}
	readyWg.Wait()
	close(start)
	select {
	case best = <-first:
		cmd.dlogger.Printf("best mirror found: %q", best)
	case <-ctx.Done():
	}
	return best, ctx.Err()
}

func (cmd Cmd) readPassword() (string, error) {
	fmt.Fprint(cmd.Out, "Enter Password: ")
	bytePassword, err := term.ReadPassword(0)
	if err != nil {
		return "", err
	}
	fmt.Fprintln(cmd.Out)
	return string(bytePassword), nil
}

func (cmd Cmd) dumpState(session *Session) {
	if !session.isResumable() {
		return
	}
	var media io.Writer
	name := session.SuggestedFileName + ".json"
	f, err := os.Create(name)
	if err != nil {
		media = cmd.Err
		name = "stderr"
	} else {
		defer func() {
			if err := f.Close(); err != nil {
				cmd.debugOrPrintErr(err, false)
			}
		}()
		media = f
	}
	err = json.NewEncoder(media).Encode(session)
	if err != nil {
		cmd.debugOrPrintErr(err, false)
	} else {
		fmt.Fprintf(cmd.Err, "session state saved to %q\n", name)
	}
}

func setCookies(headers map[string]string, usrURL string, jar http.CookieJar) {
	if hc, ok := headers[hCookie]; ok {
		var cookies []*http.Cookie
		for _, cookie := range strings.Split(hc, "; ") {
			pair := strings.SplitN(cookie, "=", 2)
			if len(pair) != 2 {
				continue
			}
			cookies = append(cookies, &http.Cookie{Name: pair[0], Value: pair[1]})
		}
		if u, err := url.Parse(usrURL); err == nil {
			jar.SetCookies(u, cookies)
		}
	}
}

func makeReqPatcher(
	headers map[string]string,
	userInfo *url.Userinfo,
	skipCookie bool,
) func(*http.Request) {
	return func(req *http.Request) {
		if userInfo != nil {
			req.URL.User = userInfo
		}
		for k, v := range headers {
			if skipCookie && k == hCookie {
				continue
			}
			req.Header.Set(k, v)
		}
	}
}

func parseContentDisposition(input string) string {
	groups := reContentDisposition.FindAllStringSubmatch(input, -1)
	for _, group := range groups {
		if group[2] != "" {
			return group[2]
		}
		split := strings.Split(group[1], "'")
		if len(split) == 3 && strings.ToLower(split[0]) == "utf-8" {
			unescaped, _ := url.QueryUnescape(split[2])
			return unescaped
		}
		if split[0] != `""` {
			return split[0]
		}
	}
	return ""
}

func isRedirect(status int) bool {
	return status > 299 && status < 400
}

func isServerError(status int) bool {
	return status > 499 && status < 600
}

func readLines(r io.Reader) ([]string, error) {
	var lines []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if len(text) == 0 || strings.HasPrefix(text, "#") {
			continue
		}
		lines = append(lines, text)
	}
	return lines, scanner.Err()
}

func filter(parts []*Part, predicate func(*Part) bool) (filtered []*Part) {
	for _, p := range parts {
		if predicate(p) {
			filtered = append(filtered, p)
		}
	}
	return
}
