// Command blrechelper converts an HLS stream to FLV over HTTP without invoking
// ffmpeg or any other external program.
//
// Usage:
//
//	blrechelper -addr 127.0.0.1:8080
//
// Then request:
//
//	http://127.0.0.1:8080/http://example.com/stream.m3u8
//
// The HLS URL is the request path after the leading slash, so the scheme's own
// slashes are preserved. A common shorthand is also accepted where the client
// collapses the double slash to a single one:
//
//	http://127.0.0.1:8080/http:/example.com/stream.m3u8
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/shugen002/blrechelper/internal/hlsflv"
)

func main() {
	var (
		addr     = flag.String("addr", "127.0.0.1:8080", "address to listen on")
		referer  = flag.String("referer", "", "Referer header sent to the origin; defaults to the origin root")
		ua       = flag.String("user-agent", hlsflv.DefaultUserAgent, "User-Agent header sent to the origin")
		timeout  = flag.Duration("request-timeout", 20*time.Second, "timeout for a single upstream request")
		liveOnly = flag.Bool("live", false, "skip the buffered backlog of a live playlist and start near the live edge")
		verbose  = flag.Bool("v", false, "log per segment progress to stderr")
	)
	flag.Usage = usage
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)

	srv := &http.Server{
		Addr: *addr,
		Handler: &handler{
			opts: hlsflv.Options{
				UserAgent:      *ua,
				Referer:        *referer,
				RequestTimeout: *timeout,
				StartFromLive:  *liveOnly,
				Logger: func(format string, args ...any) {
					if *verbose {
						logger.Printf(format, args...)
					}
				},
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
	logger.Printf("request an FLV stream with http://%s/http://host/path/index.m3u8", ln.Addr())
	logger.Printf("Or use the query parameter form: http://%s/index.flv?url=${urlencode(http://host/path/index.m3u8)}", ln.Addr())

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

func usage() {
	fmt.Fprintf(os.Stderr, `blrechelper - HLS to FLV gateway

Usage:
  %s [flags]

Then request:
  http://<addr>/<hls playlist url>

Flags:
`, os.Args[0])
	flag.PrintDefaults()
}

// handler serves FLV conversions.
type handler struct {
	opts hlsflv.Options
}

// indexPath and indexFile are the endpoints that serve usage rather than a
// stream, so a bare visit is not mistaken for a malformed target.
const (
	indexPath = "/"
	indexFile = "/index.flv"
)

// urlQueryKeys are the query parameter names accepted as the upstream playlist
// URL. The first one that is present wins.
var urlQueryKeys = []string{"url", "u", "src"}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Path
	if path == indexPath || path == indexFile {
		// The query form uses this path, so only show usage when no target was
		// supplied at all.
		if _, ok := queryTarget(r); !ok {
			h.index(w)
			return
		}
	}

	target, err := targetURL(r)
	if err != nil {
		http.Error(w, err.Error()+"\n\n"+usageText(), http.StatusBadRequest)
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

// usageText is the human readable help shown on the index page and on a bad
// request.
func usageText() string {
	return `blrechelper - HLS to FLV gateway

Request an FLV stream in either of these forms:

  http://127.0.0.1:8080/index.flv?url=<urlencoded HLS playlist URL>

  http://127.0.0.1:8080/<HLS playlist URL>

The query form is the safer one: the playlist URL is percent encoded, so any
query string it carries survives intact.

An http:/example.com/stream.m3u8 form (single slash) is also accepted, which
covers clients that collapse the double slash.
`
}

func (h *handler) index(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, usageText())
}

// queryTarget returns the upstream URL carried in the query string.
//
// It is also the hook that distinguishes a request for the usage page from a
// request for a stream on the index path.
func queryTarget(r *http.Request) (string, bool) {
	q := r.URL.Query()
	for _, key := range urlQueryKeys {
		if v := strings.TrimSpace(q.Get(key)); v != "" {
			return v, true
		}
	}
	return "", false
}

// targetURL extracts the upstream HLS URL from the request.
//
// The query form is preferred:
//
//	/index.flv?url=https%3A%2F%2Fexample.com%2Fstream.m3u8%3Ftoken%3Dabc
//
// The path form is kept for compatibility and accepts both the plain form and
// the single slash form that clients which collapse "//" produce:
//
//	/https://example.com/x.m3u8
//	/https:/example.com/x.m3u8
func targetURL(r *http.Request) (string, error) {
	if target, ok := queryTarget(r); ok {
		u, err := url.Parse(target)
		if err != nil {
			return "", fmt.Errorf("malformed upstream URL: %w", err)
		}
		if err := checkUpstreamURL(u); err != nil {
			return "", err
		}
		return u.String(), nil
	}

	raw := r.URL.EscapedPath()
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", fmt.Errorf("malformed request path: %w", err)
	}
	decoded = strings.TrimPrefix(decoded, "/")
	if decoded == "" {
		return "", errors.New("missing HLS playlist URL: pass it as ?url=<encoded URL>")
	}

	// Restore the authority separator when it was collapsed. A scheme is
	// always followed by "//", so a bare "scheme:/" must become "scheme://".
	if i := strings.Index(decoded, ":/"); i > 0 && !strings.HasPrefix(decoded[i:], "://") {
		decoded = decoded[:i+1] + "/" + decoded[i+1:]
	}

	u, err := url.Parse(decoded)
	if err != nil {
		return "", fmt.Errorf("malformed upstream URL %q: %w", decoded, err)
	}
	if err := checkUpstreamURL(u); err != nil {
		return "", err
	}
	return u.String(), nil
}

// checkUpstreamURL rejects anything that must not be fetched.
func checkUpstreamURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q: only http and https are allowed", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("upstream URL %q has no host", u.Redacted())
	}
	return nil
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
