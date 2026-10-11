package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
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

func TestParseFLVRequestAcceptsQueryForm(t *testing.T) {
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
			got, ok, err := parseFLVRequest(request(t, c.request))
			if err != nil {
				t.Fatalf("parseFLVRequest: %v", err)
			}
			if !ok {
				t.Fatal("parseFLVRequest reported that this is not an FLV request")
			}
			if got.target != c.want {
				t.Errorf("target = %q, want %q", got.target, c.want)
			}
		})
	}
}

// TestParseFLVRequestIsNotCapturedByOtherEndpoints is the important one: an API
// request that happens to carry a "url" parameter of its own must not be
// treated as an FLV request.
func TestParseFLVRequestIsNotCapturedByOtherEndpoints(t *testing.T) {
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
		{"repeated empty target", "http://gw/index.flv?url=&url="},
		{"root", "http://gw/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok, err := parseFLVRequest(request(t, c.request))
			if err != nil {
				t.Fatalf("parseFLVRequest: %v", err)
			}
			if ok {
				t.Errorf("parsed %+v, want it to report a non FLV request", got)
			}
		})
	}
}

// TestParseFLVRequestRejectsBadTarget checks that a request aimed at the FLV
// endpoint with an unusable target is reported as an error, so it is never
// silently proxied to the API upstream.
func TestParseFLVRequestRejectsBadTarget(t *testing.T) {
	cases := []struct {
		name    string
		request string
	}{
		{"unsupported scheme", "http://gw/index.flv?url=" + url.QueryEscape("file:///etc/passwd")},
		{"no host", "http://gw/index.flv?url=" + url.QueryEscape("https:///a.m3u8")},
		{"relative target", "http://gw/index.flv?url=" + url.QueryEscape("/a.m3u8")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok, err := parseFLVRequest(request(t, c.request)); err == nil || ok {
				t.Errorf("parseFLVRequest = ok:%v err:%v, want a non FLV request with an error", ok, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Per request headers
// ---------------------------------------------------------------------------

func TestParseFLVRequestHeaderParameters(t *testing.T) {
	raw := "http://gw/index.flv?url=" + url.QueryEscape("https://example.com/a.m3u8") +
		"&ua=" + url.QueryEscape("CustomAgent/1.0") +
		"&referer=" + url.QueryEscape("https://live.bilibili.com/") +
		"&origin=" + url.QueryEscape("https://live.bilibili.com") +
		"&header=" + url.QueryEscape("Authorization: Bearer token123") +
		"&header=" + url.QueryEscape("Cookie: SESSDATA=abc")

	got, ok, err := parseFLVRequest(request(t, raw))
	if err != nil {
		t.Fatalf("parseFLVRequest: %v", err)
	}
	if !ok {
		t.Fatal("expected an FLV request")
	}

	want := map[string]string{
		"User-Agent":    "CustomAgent/1.0",
		"Referer":       "https://live.bilibili.com/",
		"Origin":        "https://live.bilibili.com",
		"Authorization": "Bearer token123",
		"Cookie":        "SESSDATA=abc",
	}
	if len(got.headers) != len(want) {
		t.Fatalf("header count = %d, want %d (%+v)", len(got.headers), len(want), got.headers)
	}
	for _, h := range got.headers {
		w, ok := want[h.Name]
		if !ok {
			t.Errorf("unexpected header %q", h.Name)
			continue
		}
		if h.Value != w {
			t.Errorf("%s = %q, want %q", h.Name, h.Value, w)
		}
	}
}

func TestParseFLVRequestHeaderNameIsCanonicalised(t *testing.T) {
	raw := "http://gw/index.flv?url=" + url.QueryEscape("https://example.com/a.m3u8") +
		"&header=" + url.QueryEscape("user-agent: lower/1")
	got, _, err := parseFLVRequest(request(t, raw))
	if err != nil {
		t.Fatalf("parseFLVRequest: %v", err)
	}
	if len(got.headers) != 1 || got.headers[0].Name != "User-Agent" {
		t.Errorf("headers = %+v, want a single canonical User-Agent", got.headers)
	}
}

func TestParseFLVRequestRejectsDisallowedHeader(t *testing.T) {
	for _, spec := range []string{
		"Host: evil.example.com",
		"Content-Length: 0",
		"Connection: close",
		"X-Forwarded-For: 1.2.3.4",
		"Transfer-Encoding: chunked",
		"Upgrade: websocket",
		"no-colon-here",
		": empty name",
	} {
		t.Run(spec, func(t *testing.T) {
			raw := "http://gw/index.flv?url=" + url.QueryEscape("https://example.com/a.m3u8") +
				"&header=" + url.QueryEscape(spec)
			if _, _, err := parseFLVRequest(request(t, raw)); err == nil {
				t.Errorf("header %q was accepted, want a rejection", spec)
			}
		})
	}
}

func TestParseFLVRequestEmptyHeaderValueClearsDefault(t *testing.T) {
	raw := "http://gw/index.flv?url=" + url.QueryEscape("https://example.com/a.m3u8") +
		"&referer="
	got, ok, err := parseFLVRequest(request(t, raw))
	if err != nil {
		t.Fatalf("parseFLVRequest: %v", err)
	}
	if !ok {
		t.Fatal("expected an FLV request")
	}
	// An explicitly empty override must be preserved so the client can drop the
	// default Referer rather than send one the caller did not want.
	if len(got.headers) != 1 || got.headers[0].Name != "Referer" || got.headers[0].Value != "" {
		t.Errorf("headers = %+v, want one empty Referer override", got.headers)
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

// TestFLVEndpointInvalidTargetIsRejected checks that a request aimed at the FLV
// endpoint with an unusable target is answered with 400 and usage, rather than
// being quietly proxied to the API upstream.
func TestFLVEndpointInvalidTargetIsRejected(t *testing.T) {
	h, got := newEchoProxy(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, "http://gw/index.flv?url="+url.QueryEscape("file:///etc/passwd")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "index.flv?url=") {
		t.Errorf("a rejected target must include usage:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unsupported scheme") {
		t.Errorf("the response must explain the problem:\n%s", rec.Body.String())
	}
	if got.Path != "" {
		t.Errorf("a rejected FLV request must not reach the upstream, but it received %q", got.Path)
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

// newLoggingProxy builds a handler that proxies to an upstream returning the
// given status, and captures log lines.
func newLoggingProxy(t *testing.T, status int, verbose bool) (*handler, *bytes.Buffer) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte("hello"))
	}))
	t.Cleanup(upstream.Close)

	proxy, err := newReverseProxy(upstream.URL)
	if err != nil {
		t.Fatalf("newReverseProxy: %v", err)
	}
	logs := &bytes.Buffer{}
	return &handler{
		proxy:   proxy,
		logger:  log.New(logs, "", 0),
		verbose: verbose,
	}, logs
}

func TestProxyLogsPath(t *testing.T) {
	h, logs := newLoggingProxy(t, http.StatusOK, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, "http://gw/room/v1/Room/get_info?room_id=1"))

	line := logs.String()
	if !strings.Contains(line, "/room/v1/Room/get_info") {
		t.Errorf("log line does not contain the path: %q", line)
	}
	if !strings.Contains(line, "GET") {
		t.Errorf("log line does not contain the method: %q", line)
	}
	if !strings.Contains(line, "200") {
		t.Errorf("log line does not contain the status: %q", line)
	}
	if !strings.Contains(line, "5 bytes") {
		t.Errorf("log line does not contain the response size: %q", line)
	}
	// The query string may carry tokens, so it must not be logged by default.
	if strings.Contains(line, "room_id") {
		t.Errorf("log line leaked the query string: %q", line)
	}
}

func TestProxyLogsQueryStringWhenVerbose(t *testing.T) {
	h, logs := newLoggingProxy(t, http.StatusTeapot, true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, "http://gw/path/here?a=1&b=2"))

	line := logs.String()
	if !strings.Contains(line, "/path/here?a=1&b=2") {
		t.Errorf("verbose log line does not contain the query string: %q", line)
	}
	if !strings.Contains(line, "418") {
		t.Errorf("log line does not contain the upstream status: %q", line)
	}
}

func TestProxyLogsUpstreamErrorStatus(t *testing.T) {
	h, logs := newLoggingProxy(t, http.StatusNotFound, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, "http://gw/missing"))

	if !strings.Contains(logs.String(), "404") {
		t.Errorf("log line does not report the 404: %q", logs.String())
	}
}

func TestProxyLogsNothingWhenLoggerIsNil(t *testing.T) {
	// A handler without a logger must still proxy correctly.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	proxy, err := newReverseProxy(upstream.URL)
	if err != nil {
		t.Fatalf("newReverseProxy: %v", err)
	}

	h := &handler{proxy: proxy}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, "http://gw/x"))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("status = %d body = %q, want 200 ok", rec.Code, rec.Body.String())
	}
}

// TestStatusRecorderKeepsStreaming checks that wrapping the writer does not
// break flushing, which a streaming upstream response depends on.
func TestStatusRecorderKeepsStreaming(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	if _, ok := http.ResponseWriter(sr).(http.Flusher); !ok {
		t.Fatal("the wrapped writer must still expose Flush")
	}
	sr.WriteHeader(http.StatusAccepted)
	sr.Write([]byte("abc"))
	sr.Flush()

	if sr.status != http.StatusAccepted {
		t.Errorf("status = %d, want 202", sr.status)
	}
	if sr.written != 3 {
		t.Errorf("written = %d, want 3", sr.written)
	}
	if rec.Code != http.StatusAccepted || rec.Body.String() != "abc" {
		t.Errorf("underlying recorder = %d %q", rec.Code, rec.Body.String())
	}
}

// TestStatusRecorderDefaultsTo200 covers a handler that writes a body without
// calling WriteHeader.
func TestStatusRecorderDefaultsTo200(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}
	sr.Write([]byte("body"))
	if sr.status != http.StatusOK {
		t.Errorf("status = %d, want 200", sr.status)
	}
}

func TestNewUpstreamTransport(t *testing.T) {
	tr := newUpstreamTransport()
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout must be set so a stalled upstream cannot hang forever")
	}
	tr.CloseIdleConnections()
}
