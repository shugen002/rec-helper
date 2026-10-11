package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// request builds a GET request the way net/http would deliver it to a handler.
func request(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("NewRequest(%q): %v", rawURL, err)
	}
	return r
}

// echo is what the test upstream recorded about the request it received.
type echo struct {
	Method  string
	Path    string
	RawQ    string
	Query   url.Values
	Body    string
	Headers http.Header
	Host    string
}

// newEchoProxy builds a handler whose proxy points at an upstream that echoes
// the request back.
func newEchoProxy(t *testing.T) (*handler, *echo) {
	t.Helper()
	got := &echo{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.Method = r.Method
		got.Path = r.URL.Path
		got.RawQ = r.URL.RawQuery
		got.Query = r.URL.Query()
		got.Body = string(body)
		got.Headers = r.Header.Clone()
		got.Host = r.Host

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusTeapot)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"path":  r.URL.Path,
			"query": r.URL.RawQuery,
		})
	}))
	t.Cleanup(upstream.Close)

	proxy, err := newReverseProxy(upstream.URL)
	if err != nil {
		t.Fatalf("newReverseProxy: %v", err)
	}
	return &handler{proxy: proxy}, got
}

// ---------------------------------------------------------------------------
// FLV routing
// ---------------------------------------------------------------------------

func TestFLVTargetAcceptsQueryForm(t *testing.T) {
	cases := []struct {
		name    string
		request string
		want    string
	}{
		{
			name:    "encoded playlist URL",
			request: "http://gw/index.flv?url=" + url.QueryEscape("https://example.com/stream.m3u8"),
			want:    "https://example.com/stream.m3u8",
		},
		{
			// The whole point of the query form.
			name:    "playlist URL carrying its own query string",
			request: "http://gw/index.flv?url=" + url.QueryEscape("https://example.com/live.m3u8?token=abc&sign=deadbeef"),
			want:    "https://example.com/live.m3u8?token=abc&sign=deadbeef",
		},
		{
			name:    "extra parameters alongside url",
			request: "http://gw/index.flv?foo=bar&url=" + url.QueryEscape("https://example.com/a.m3u8") + "&baz=1",
			want:    "https://example.com/a.m3u8",
		},
		{
			name:    "short alias",
			request: "http://gw/index.flv?u=" + url.QueryEscape("https://example.com/a.m3u8"),
			want:    "https://example.com/a.m3u8",
		},
		{
			name:    "src alias",
			request: "http://gw/index.flv?src=" + url.QueryEscape("https://example.com/a.m3u8"),
			want:    "https://example.com/a.m3u8",
		},
		{
			name:    "url wins over an alias",
			request: "http://gw/index.flv?u=" + url.QueryEscape("https://example.com/first.m3u8") + "&url=" + url.QueryEscape("https://example.com/second.m3u8"),
			want:    "https://example.com/second.m3u8",
		},
		{
			name:    "http upstream",
			request: "http://gw/index.flv?url=" + url.QueryEscape("http://example.com/a.m3u8"),
			want:    "http://example.com/a.m3u8",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := flvTarget(request(t, c.request))
			if !ok {
				t.Fatal("flvTarget reported that this is not an FLV request")
			}
			if got != c.want {
				t.Errorf("flvTarget = %q, want %q", got, c.want)
			}
		})
	}
}

// TestFLVTargetIsNotCapturedByOtherEndpoints is the important one: an API
// request that happens to carry a "url" parameter of its own must not be
// treated as an FLV request.
func TestFLVTargetIsNotCapturedByOtherEndpoints(t *testing.T) {
	cases := []struct {
		name    string
		request string
	}{
		{"api request with a url parameter", "http://gw/room/v1/Room/get_info?url=https%3A%2F%2Fexample.com%2Fx"},
		{"api request with a u parameter", "http://gw/x/y?u=https%3A%2F%2Fexample.com%2Fx"},
		{"other path with a playlist looking url", "http://gw/other.flv?url=" + url.QueryEscape("https://example.com/a.m3u8")},
		{"nested path ending in index.flv", "http://gw/api/index.flv?url=" + url.QueryEscape("https://example.com/a.m3u8")},
		{"index.flv without a target", "http://gw/index.flv"},
		{"index.flv with other parameters", "http://gw/index.flv?foo=bar"},
		{"empty target", "http://gw/index.flv?url="},
		{"blank target", "http://gw/index.flv?url=%20%20"},
		{"unsupported scheme", "http://gw/index.flv?url=" + url.QueryEscape("file:///etc/passwd")},
		{"no host", "http://gw/index.flv?url=" + url.QueryEscape("https:///a.m3u8")},
		{"relative target", "http://gw/index.flv?url=" + url.QueryEscape("/a.m3u8")},
		{"root", "http://gw/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := flvTarget(request(t, c.request)); ok {
				t.Errorf("flvTarget = %q, want it to report a non FLV request", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Reverse proxying
// ---------------------------------------------------------------------------

func TestProxyForwardsRequestUnchanged(t *testing.T) {
	h, got := newEchoProxy(t)

	const raw = "http://gw/room/v1/Room/get_info?room_id=12345&url=" + "%68%74%74%70%73%3A%2F%2Fexample.com%2Fx"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, raw))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want the upstream status 418", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want the upstream content type", ct)
	}
	if rec.Header().Get("X-Upstream") != "yes" {
		t.Error("the upstream response headers were not passed through")
	}

	if got.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", got.Method)
	}
	if got.Path != "/room/v1/Room/get_info" {
		t.Errorf("path = %q, want /room/v1/Room/get_info", got.Path)
	}
	// The query string, including a url parameter that belongs to the API,
	// must arrive unchanged.
	if got.Query.Get("room_id") != "12345" {
		t.Errorf("room_id = %q, want 12345", got.Query.Get("room_id"))
	}
	if got.Query.Get("url") != "https://example.com/x" {
		t.Errorf("url = %q, want https://example.com/x", got.Query.Get("url"))
	}
}

func TestProxyKeepsTargetBasePath(t *testing.T) {
	// A configured target with its own base path must be preserved.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.URL.Path))
	}))
	defer upstream.Close()

	proxy, err := newReverseProxy(upstream.URL + "/base")
	if err != nil {
		t.Fatalf("newReverseProxy: %v", err)
	}
	h := &handler{proxy: proxy}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, "http://gw/x/y?z=1"))
	if got := rec.Body.String(); got != "/base/x/y" {
		t.Errorf("upstream path = %q, want /base/x/y", got)
	}
}

func TestProxySetsForwardedHeaders(t *testing.T) {
	h, got := newEchoProxy(t)

	r := request(t, "http://gw/a/b?c=1")
	// A request built in memory has no peer address, and SetXForwarded can only
	// derive X-Forwarded-For from one, so supply it.
	r.RemoteAddr = "203.0.113.7:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if v := got.Headers.Get("X-Forwarded-For"); v != "203.0.113.7" {
		t.Errorf("X-Forwarded-For = %q, want 203.0.113.7", v)
	}
	if v := got.Headers.Get("X-Forwarded-Host"); v != "gw" {
		t.Errorf("X-Forwarded-Host = %q, want gw", v)
	}
	if v := got.Headers.Get("X-Forwarded-Proto"); v != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want http", v)
	}
}

func TestProxyDropsGatewayReferer(t *testing.T) {
	h, got := newEchoProxy(t)

	r := request(t, "http://gw/a")
	r.Header.Set("Referer", "http://gw/index.flv")
	r.Header.Set("Origin", "http://gw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if v := got.Headers.Get("Referer"); v != "" {
		t.Errorf("Referer = %q, want it dropped when it points back at the gateway", v)
	}
	if v := got.Headers.Get("Origin"); v != "" {
		t.Errorf("Origin = %q, want it dropped when it points back at the gateway", v)
	}
}

func TestProxyKeepsForeignReferer(t *testing.T) {
	h, got := newEchoProxy(t)

	r := request(t, "http://gw/a")
	r.Header.Set("Referer", "https://live.bilibili.com/123")
	r.Header.Set("Origin", "https://live.bilibili.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if v := got.Headers.Get("Referer"); v != "https://live.bilibili.com/123" {
		t.Errorf("Referer = %q, want it preserved", v)
	}
	if v := got.Headers.Get("Origin"); v != "https://live.bilibili.com" {
		t.Errorf("Origin = %q, want it preserved", v)
	}
}

func TestProxyForwardsRequestBodies(t *testing.T) {
	h, got := newEchoProxy(t)

	body := "a=1&b=2"
	r, err := http.NewRequest(http.MethodPost, "http://gw/api/submit", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if got.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.Method)
	}
	if got.Body != body {
		t.Errorf("body = %q, want %q", got.Body, body)
	}
	if ct := got.Headers.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("content type = %q, want it forwarded", ct)
	}
}

func TestProxyForwardsUpstreamErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer upstream.Close()

	proxy, err := newReverseProxy(upstream.URL)
	if err != nil {
		t.Fatalf("newReverseProxy: %v", err)
	}
	h := &handler{proxy: proxy}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, "http://gw/missing"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 from the upstream", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "nope") {
		t.Errorf("body = %q, want the upstream body", rec.Body.String())
	}
}

func TestProxyDisabledReturnsNotFound(t *testing.T) {
	h := &handler{}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, "http://gw/anything"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when proxying is disabled", rec.Code)
	}
}

func TestNewReverseProxyValidation(t *testing.T) {
	if p, err := newReverseProxy(""); err != nil || p != nil {
		t.Errorf("empty target: proxy = %v, err = %v; want nil, nil", p, err)
	}
	if p, err := newReverseProxy("   "); err != nil || p != nil {
		t.Errorf("blank target: proxy = %v, err = %v; want nil, nil", p, err)
	}
	for _, target := range []string{"file:///etc/passwd", "ftp://example.com", "https:///x", "://bad", "/relative"} {
		if _, err := newReverseProxy(target); err == nil {
			t.Errorf("newReverseProxy(%q) succeeded, want an error", target)
		}
	}
	for _, target := range []string{"https://api.live.bilibili.com", "http://127.0.0.1:9000/base"} {
		if _, err := newReverseProxy(target); err != nil {
			t.Errorf("newReverseProxy(%q): %v", target, err)
		}
	}
}

// ---------------------------------------------------------------------------
// The FLV endpoint itself
// ---------------------------------------------------------------------------

func TestFLVEndpointUsageWithoutTarget(t *testing.T) {
	h, got := newEchoProxy(t)

	// A bare GET of the endpoint shows help instead of proxying.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, "http://gw/index.flv"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q, want text/plain", ct)
	}
	if !strings.Contains(rec.Body.String(), "index.flv?url=") {
		t.Errorf("usage does not document the query form:\n%s", rec.Body.String())
	}
	if got.Path != "" {
		t.Errorf("a bare GET must not reach the upstream, but it received %q", got.Path)
	}
}

// TestFLVEndpointInvalidTargetIsProxied documents the deliberate behaviour: a
// target that is not a usable URL is not a conversion request, so the request
// takes the normal proxying path.
func TestFLVEndpointInvalidTargetIsProxied(t *testing.T) {
	h, got := newEchoProxy(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, "http://gw/index.flv?url="+url.QueryEscape("file:///etc/passwd")))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want the upstream status 418 (proxied)", rec.Code)
	}
	if got.Path != "/index.flv" {
		t.Errorf("upstream path = %q, want /index.flv", got.Path)
	}
	if got.Query.Get("url") != "file:///etc/passwd" {
		t.Errorf("upstream url parameter = %q, want it unchanged", got.Query.Get("url"))
	}
}

func TestFLVEndpointRejectsBadMethod(t *testing.T) {
	h, _ := newEchoProxy(t)

	target := "http://gw/index.flv?url=" + url.QueryEscape("https://example.com/a.m3u8")
	r := request(t, target)
	r.Method = http.MethodPost
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow = %q, want \"GET, HEAD\"", allow)
	}
}

func TestFLVEndpointHeadDoesNotFetch(t *testing.T) {
	h, got := newEchoProxy(t)

	target := "http://gw/index.flv?url=" + url.QueryEscape("https://example.invalid/a.m3u8")
	r := request(t, target)
	r.Method = http.MethodHead
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/x-flv" {
		t.Errorf("content type = %q, want video/x-flv", ct)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a HEAD response must have no body, got %d bytes", rec.Body.Len())
	}
	if got.Path != "" {
		t.Errorf("HEAD must not reach the upstream, but it received %q", got.Path)
	}
}

func TestNewUpstreamTransport(t *testing.T) {
	tr := newUpstreamTransport()
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout must be set so a stalled upstream cannot hang forever")
	}
	tr.CloseIdleConnections()
}
