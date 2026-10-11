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
			proxy: proxy,
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
}

// flvPath is the endpoint that converts an HLS playlist to FLV. It is only a
// conversion request when it also carries a target parameter, so a bare visit
// and a proxied request for the same path are not mistaken for one another.
const flvPath = "/index.flv"

// urlQueryKeys are the query parameter names accepted as the upstream playlist
// URL. The first one that is present wins.
var urlQueryKeys = []string{"url", "u", "src"}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only an FLV request is handled here. Everything else, including a
	// proxied request whose own query string happens to contain "url", is
	// forwarded untouched.
	if target, ok := flvTarget(r); ok {
		h.serveFLV(w, r, target)
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

// flvTarget reports whether the request asks for an FLV conversion and returns
// the upstream playlist URL. The path must match exactly, so a proxied request
// for another endpoint cannot be captured just because it has a url parameter.
func flvTarget(r *http.Request) (string, bool) {
	if r.URL.Path != flvPath {
		return "", false
	}
	q := r.URL.Query()
	for _, key := range urlQueryKeys {
		v := strings.TrimSpace(q.Get(key))
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return v, false
		}
		return u.String(), true
	}
	return "", false
}

// serveProxy forwards a request unchanged to the configured upstream.
func (h *handler) serveProxy(w http.ResponseWriter, r *http.Request) {
	if h.proxy == nil {
		http.NotFound(w, r)
		return
	}
	h.proxy.ServeHTTP(w, r)
}

func (h *handler) serveFLV(w http.ResponseWriter, r *http.Request, target string) {
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
	if opts.Logger == nil {
		opts.Logger = func(string, ...any) {}
	}

	// hlsflv writes through a flushWriter so every segment reaches the client
	// immediately instead of waiting in net/http's output buffer.
	client, err := hlsflv.New(target, &flushWriter{w: w, f: flusher}, opts)
	if err != nil {
		// The response has already started; report on stderr and stop.
		opts.Logger("blrechelper: %v", err)
		return
	}

	start := time.Now()
	err = client.Run(r.Context())
	stats := client.Stats()
	opts.Logger("blrechelper: finished %s after %s: %d segments, %d tags, %.1f MiB (%v)",
		target, time.Since(start).Round(time.Millisecond), stats.Segments, stats.Samples,
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
