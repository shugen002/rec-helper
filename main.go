// Command blrechelper converts an HLS stream to FLV over HTTP without invoking
// ffmpeg or any other external program, and transparently reverse proxies
// everything else to a configurable upstream.
//
// Usage:
//
//	blrechelper -addr 127.0.0.1:8080
//
// An FLV stream is requested with the query form:
//
//	http://127.0.0.1:8080/index.flv?url=<urlencoded http://example.com/stream.m3u8>
//
// Any request that is not an FLV request for /index.flv is forwarded unchanged
// to the upstream, which defaults to https://api.live.bilibili.com.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/shugen002/blrechelper/internal/hlsflv"
)

// defaultProxyTarget is the upstream that non FLV requests are forwarded to.
const defaultProxyTarget = "https://api.live.bilibili.com"

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:8080", "address to listen on")
		proxyTarget = flag.String("proxy-target", defaultProxyTarget, "base URL that non FLV requests are reverse proxied to; the request path and query are appended unchanged, use an empty value to disable proxying")
		referer     = flag.String("referer", "", "Referer header sent to the origin; defaults to the origin root")
		ua          = flag.String("user-agent", hlsflv.DefaultUserAgent, "User-Agent header sent to the origin")
		timeout     = flag.Duration("request-timeout", 20*time.Second, "timeout for a single upstream request")
		liveOnly    = flag.Bool("live", false, "skip the buffered backlog of a live playlist and start near the live edge")
		verbose     = flag.Bool("v", false, "log per segment progress to stderr")
	)
	flag.Usage = usage
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)
	verboseLog := func(format string, args ...any) {
		if *verbose {
			logger.Printf(format, args...)
		}
	}

	proxy, err := newReverseProxy(*proxyTarget)
	if err != nil {
		logger.Fatalf("proxy target %q: %v", *proxyTarget, err)
	}
	if proxy != nil {
		// Route proxy errors through the same logger.
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Printf("proxy %s: %v", r.URL.Path, err)
			http.Error(w, "upstream request failed", http.StatusBadGateway)
		}
	}

	srv := &http.Server{
		Addr: *addr,
		Handler: &handler{
			proxy:   proxy,
			logger:  logger,
			verbose: *verbose,
			opts: hlsflv.Options{
				UserAgent:      *ua,
				Referer:        *referer,
				RequestTimeout: *timeout,
				StartFromLive:  *liveOnly,
				Logger:         verboseLog,
			},
		},
		// No write timeout: an FLV response stays open for the whole session.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Fatalf("listen %s: %v", *addr, err)
	}
	logger.Printf("blrechelper listening on http://%s/ (go %s)", ln.Addr(), runtime.Version())
	logger.Printf("FLV stream:  http://%s/index.flv?url=<urlencoded playlist URL>", ln.Addr())
	if proxy != nil {
		logger.Printf("everything else is proxied to %s", strings.TrimSuffix(*proxyTarget, "/"))
	} else {
		logger.Printf("proxying is disabled; unmatched requests return 404")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("serve: %v", err)
		}
	case <-ctx.Done():
		logger.Printf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
}

// newReverseProxy builds the reverse proxy for non FLV requests. An empty
// target disables proxying.
func newReverseProxy(target string) (*httputil.ReverseProxy, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, nil
	}
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("malformed URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q: only http and https are allowed", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("no host")
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			// SetURL rewrites the scheme, host and the base path, and joins
			// the incoming path onto the target path while keeping the raw
			// query string byte for byte.
			r.SetURL(u)
			r.Out.Host = u.Host

			// Pass the caller's address on to the upstream.
			r.SetXForwarded()

			// A request to this gateway is not a request from a browser
			// sitting on the origin, so drop a Referer and Origin that point
			// back at us rather than leaking the gateway's own address.
			if sameOrigin(r.Out.Header.Get("Referer"), r.In.Host) {
				r.Out.Header.Del("Referer")
			}
			if sameOrigin(r.Out.Header.Get("Origin"), r.In.Host) {
				r.Out.Header.Del("Origin")
			}
		},
		Transport: newUpstreamTransport(),
	}
	return proxy, nil
}

// sameOrigin reports whether a Referer or Origin value points at host.
func sameOrigin(value, host string) bool {
	if value == "" {
		return false
	}
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

// newUpstreamTransport builds the transport shared by proxying and by the HLS
// fetches. ResponseHeaderTimeout bounds a stalled upstream without capping the
// lifetime of a long lived response body, which an FLV stream needs.
func newUpstreamTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `blrechelper - HLS to FLV gateway

Usage:
  %s [flags]

FLV stream:
  http://<addr>/index.flv?url=<urlencoded HLS playlist URL>

Everything else is reverse proxied unchanged to -proxy-target
(default %s).

Flags:
`, os.Args[0], defaultProxyTarget)
	flag.PrintDefaults()
}

// handler serves FLV conversions and proxies every other request.
type handler struct {
	opts hlsflv.Options
	// proxy forwards requests that are not FLV requests. Nil disables it.
	proxy *httputil.ReverseProxy
	// logger receives one line per proxied request. Nil disables the log.
	logger *log.Logger
	// verbose adds the query string to proxied request log lines.
	verbose bool
}

// flvPath is the endpoint that converts an HLS playlist to FLV. It is only a
// conversion request when it also carries a target parameter, so a bare visit
// and a proxied request for the same path are not mistaken for one another.
const flvPath = "/index.flv"

// Query parameter names understood on flvPath.
const (
	// urlQueryKey names the upstream playlist URL.
	urlQueryKey = "url"
	// uaQueryKey and refererQueryKey are the common per request header
	// overrides, listed separately because they are what hotlink protection
	// usually keys on.
	uaQueryKey      = "ua"
	refererQueryKey = "referer"
	originQueryKey  = "origin"
	// headerQueryKey carries any further header as "Name: Value" and may be
	// repeated. Only names on the allow list are accepted.
	headerQueryKey = "header"
)

// urlQueryKeys are the parameter names accepted as the upstream playlist URL.
// The first one that is present wins.
var urlQueryKeys = []string{urlQueryKey, "u", "src"}

// flvRequest is a parsed FLV conversion request.
type flvRequest struct {
	target  string
	headers []hlsflv.RequestHeader
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only an FLV request is handled here. Everything else, including a
	// proxied request whose own query string happens to contain "url", is
	// forwarded untouched.
	req, isFLV, err := parseFLVRequest(r)
	if err != nil {
		// The request was aimed at the FLV endpoint but is malformed, so say so
		// rather than silently proxying it to the API upstream.
		http.Error(w, err.Error()+"\n\n"+usageText(), http.StatusBadRequest)
		return
	}
	if isFLV {
		h.serveFLV(w, r, req)
		return
	}
	// A browser opening the endpoint directly gets help rather than a proxied
	// upstream response.
	if r.Method == http.MethodGet && r.URL.Path == flvPath && r.URL.RawQuery == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, usageText())
		return
	}
	h.serveProxy(w, r)
}

// parseFLVRequest reports whether the request asks for an FLV conversion and
// returns the parsed target and headers.
//
// The path must match exactly, so a proxied request for another endpoint cannot
// be captured just because it has a url parameter.
func parseFLVRequest(r *http.Request) (flvRequest, bool, error) {
	if r.URL.Path != flvPath {
		return flvRequest{}, false, nil
	}
	q := r.URL.Query()

	raw := ""
	for _, key := range urlQueryKeys {
		if v := strings.TrimSpace(q.Get(key)); v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return flvRequest{}, false, nil
	}
	target, err := parsePlaylistURL(raw)
	if err != nil {
		return flvRequest{}, false, err
	}

	headers, err := requestHeaders(q)
	if err != nil {
		return flvRequest{}, false, err
	}
	return flvRequest{target: target, headers: headers}, true, nil
}

// parsePlaylistURL validates the upstream playlist URL.
func parsePlaylistURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("malformed playlist URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q: the playlist URL must be http or https", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("the playlist URL has no host")
	}
	return u.String(), nil
}

// requestHeaders collects the header overrides from the query string.
//
// A parameter that is absent leaves the default header alone, while a parameter
// that is present but empty clears it, so a caller can suppress the default
// Referer on purpose.
func requestHeaders(q url.Values) ([]hlsflv.RequestHeader, error) {
	var out []hlsflv.RequestHeader

	// addOverride records a header only when the parameter was actually given.
	addOverride := func(param, name string) {
		values, present := q[param]
		if !present {
			return
		}
		value := ""
		if len(values) > 0 {
			value = strings.TrimSpace(values[0])
		}
		h, err := hlsflv.NewRequestHeader(name, value)
		if err == nil {
			out = append(out, h)
		}
	}
	addOverride(uaQueryKey, "User-Agent")
	addOverride(refererQueryKey, "Referer")
	addOverride(originQueryKey, "Origin")

	for _, spec := range q[headerQueryKey] {
		h, err := hlsflv.ParseRequestHeader(spec)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// serveProxy forwards a request unchanged to the configured upstream.
func (h *handler) serveProxy(w http.ResponseWriter, r *http.Request) {
	if h.proxy == nil {
		http.NotFound(w, r)
		return
	}
	if h.logger == nil {
		h.proxy.ServeHTTP(w, r)
		return
	}

	// Wrap the writer so the log line can report what the upstream answered.
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	h.proxy.ServeHTTP(rec, r)

	target := r.URL.Path
	if h.verbose {
		// The query string can carry tokens and signatures, so it is only
		// logged when explicitly asked for.
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
	}
	h.logger.Printf("proxy %s %s -> %d (%d bytes, %s)",
		r.Method, target, rec.status, rec.written, time.Since(start).Round(time.Millisecond))
}

// statusRecorder remembers the status code and body size written by the proxy.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if !s.wrote {
		s.status = status
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

// Flush keeps streaming responses working through the wrapper.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *handler) serveFLV(w http.ResponseWriter, r *http.Request, req flvRequest) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported by this server", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "video/x-flv")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if r.Method == http.MethodHead {
		return
	}

	opts := h.opts
	opts.PerRequestHeader = req.headers
	if opts.Logger == nil {
		opts.Logger = func(string, ...any) {}
	}

	// hlsflv writes through a flushWriter so every segment reaches the client
	// immediately instead of waiting in net/http's output buffer.
	client, err := hlsflv.New(req.target, &flushWriter{w: w, f: flusher}, opts)
	if err != nil {
		// The response has already started; report on stderr and stop.
		opts.Logger("blrechelper: %v", err)
		return
	}

	start := time.Now()
	err = client.Run(r.Context())
	stats := client.Stats()
	opts.Logger("blrechelper: finished %s after %s: %d segments, %d tags, %.1f MiB (%v)",
		req.target, time.Since(start).Round(time.Millisecond), stats.Segments, stats.Samples,
		float64(stats.Bytes)/(1<<20), err)
}

// usageText is the human readable help shown when the FLV endpoint is opened
// directly.
func usageText() string {
	return `blrechelper - HLS to FLV gateway

Get an FLV stream with the query form, where the playlist URL is percent
encoded so that any query string it carries survives intact:

  http://127.0.0.1:8080/index.flv?url=<urlencoded HLS playlist URL>

For example:

  http://127.0.0.1:8080/index.flv?url=https%3A%2F%2Fexample.com%2Fstream.m3u8

Any other request is reverse proxied unchanged to the configured upstream.
`
}

// flushWriter forwards writes to the response and flushes them immediately.
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if n > 0 {
		fw.f.Flush()
	}
	return n, err
}
