package hlsflv

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestParseRequestHeaderAllowList pins the allow list behaviour: the names
// origins key on are accepted and anything else is refused.
func TestParseRequestHeaderAllowList(t *testing.T) {
	allowed := []struct {
		spec string
		name string
		val  string
	}{
		{"User-Agent: Custom/1.0", "User-Agent", "Custom/1.0"},
		{"user-agent: lower/1", "User-Agent", "lower/1"},
		{"Referer: https://live.bilibili.com/", "Referer", "https://live.bilibili.com/"},
		{"referrer: https://live.bilibili.com/", "Referer", "https://live.bilibili.com/"},
		{"Origin: https://live.bilibili.com", "Origin", "https://live.bilibili.com"},
		{"Authorization: Bearer abc", "Authorization", "Bearer abc"},
		{"Cookie: SESSDATA=x", "Cookie", "SESSDATA=x"},
		{"Accept: */*", "Accept", "*/*"},
		{"Accept-Language: zh-CN", "Accept-Language", "zh-CN"},
		{"Accept-Encoding: gzip", "Accept-Encoding", "gzip"},
		// A value may itself contain colons.
		{"Referer: https://example.com/a?b=c", "Referer", "https://example.com/a?b=c"},
		// Surrounding whitespace is trimmed.
		{"  Referer :  https://example.com/  ", "Referer", "https://example.com/"},
	}
	for _, c := range allowed {
		got, err := ParseRequestHeader(c.spec)
		if err != nil {
			t.Errorf("ParseRequestHeader(%q): %v", c.spec, err)
			continue
		}
		if got.Name != c.name || got.Value != c.val {
			t.Errorf("ParseRequestHeader(%q) = %+v, want %s: %s", c.spec, got, c.name, c.val)
		}
	}

	rejected := []string{
		"Host: evil.example.com",
		"Content-Length: 0",
		"Content-Type: text/plain",
		"Connection: keep-alive",
		"Transfer-Encoding: chunked",
		"Upgrade: websocket",
		"X-Forwarded-For: 1.2.3.4",
		"Proxy-Authorization: x",
		"X-Custom: y",
		"no colon",
		": empty name",
		"   : empty name",
	}
	for _, spec := range rejected {
		if got, err := ParseRequestHeader(spec); err == nil {
			t.Errorf("ParseRequestHeader(%q) = %+v, want a rejection", spec, got)
		}
	}
}

func TestNewRequestHeaderRejectsLineBreaks(t *testing.T) {
	for _, value := range []string{"a\r\nX-Injected: 1", "a\nb", "a\rb"} {
		if got, err := NewRequestHeader("Referer", value); err == nil {
			t.Errorf("NewRequestHeader(Referer, %q) = %+v, want a rejection", value, got)
		}
	}
}

// recordingOrigin serves a minimal fMP4 stream and records the headers of every
// request it receives.
type recordingOrigin struct {
	mu       sync.Mutex
	requests map[string]http.Header
	handler  http.Handler
}

func (o *recordingOrigin) record(r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests[r.URL.Path] = r.Header.Clone()
}

func (o *recordingOrigin) headerFor(path string) (http.Header, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	h, ok := o.requests[path]
	return h, ok
}

func (o *recordingOrigin) paths() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, 0, len(o.requests))
	for p := range o.requests {
		out = append(out, p)
	}
	return out
}

// TestPerRequestHeadersReachEveryUpstreamRequest is the behaviour the query
// parameters promise: the headers apply to the playlist, the initialisation
// segment and the media segments alike, because hotlink protection usually
// checks all three.
func TestPerRequestHeadersReachEveryUpstreamRequest(t *testing.T) {
	o := newOrigin()
	rec := &recordingOrigin{requests: map[string]http.Header{}}
	inner := o.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()

	opts := Options{
		PerRequestHeader: []RequestHeader{
			{Name: "User-Agent", Value: "CustomAgent/1.0"},
			{Name: "Referer", Value: "https://live.bilibili.com/123"},
			{Name: "Origin", Value: "https://live.bilibili.com"},
			{Name: "Cookie", Value: "SESSDATA=abc"},
		},
	}
	runClient(t, srv.URL+"/index.m3u8", opts)

	for _, path := range []string{"/index.m3u8", "/init.mp4", "/seg0.m4s", "/seg1.m4s"} {
		h, ok := rec.headerFor(path)
		if !ok {
			t.Errorf("%s was never requested (saw %v)", path, rec.paths())
			continue
		}
		if got := h.Get("User-Agent"); got != "CustomAgent/1.0" {
			t.Errorf("%s User-Agent = %q, want CustomAgent/1.0", path, got)
		}
		if got := h.Get("Referer"); got != "https://live.bilibili.com/123" {
			t.Errorf("%s Referer = %q, want the caller value", path, got)
		}
		if got := h.Get("Origin"); got != "https://live.bilibili.com" {
			t.Errorf("%s Origin = %q, want the caller value", path, got)
		}
		if got := h.Get("Cookie"); got != "SESSDATA=abc" {
			t.Errorf("%s Cookie = %q, want SESSDATA=abc", path, got)
		}
	}
}

// TestPerRequestHeaderBeatsOptionDefaults checks the precedence rule: a caller
// supplied header replaces the client wide default.
func TestPerRequestHeaderBeatsOptionDefaults(t *testing.T) {
	o := newOrigin()
	rec := &recordingOrigin{requests: map[string]http.Header{}}
	inner := o.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()

	opts := Options{
		UserAgent: "DefaultAgent/9.9",
		Referer:   "https://default.example/",
		Header:    http.Header{"X-Static": []string{"from-option"}},
		PerRequestHeader: []RequestHeader{
			{Name: "User-Agent", Value: "CallerAgent/1.0"},
			{Name: "Referer", Value: "https://caller.example/"},
		},
	}
	runClient(t, srv.URL+"/index.m3u8", opts)

	h, ok := rec.headerFor("/index.m3u8")
	if !ok {
		t.Fatal("the playlist was never requested")
	}
	if got := h.Get("User-Agent"); got != "CallerAgent/1.0" {
		t.Errorf("User-Agent = %q, want the caller value", got)
	}
	if got := h.Get("Referer"); got != "https://caller.example/" {
		t.Errorf("Referer = %q, want the caller value", got)
	}
	// Headers set through Options.Header must survive alongside.
	if got := h.Get("X-Static"); got != "from-option" {
		t.Errorf("X-Static = %q, want it preserved", got)
	}
}

// TestEmptyPerRequestHeaderClearsDefault checks that an explicitly empty value
// suppresses a default header instead of sending an empty one.
func TestEmptyPerRequestHeaderClearsDefault(t *testing.T) {
	o := newOrigin()
	rec := &recordingOrigin{requests: map[string]http.Header{}}
	inner := o.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()

	opts := Options{
		Referer:          "https://default.example/",
		PerRequestHeader: []RequestHeader{{Name: "Referer", Value: ""}},
	}
	runClient(t, srv.URL+"/index.m3u8", opts)

	h, ok := rec.headerFor("/index.m3u8")
	if !ok {
		t.Fatal("the playlist was never requested")
	}
	if values, present := h["Referer"]; present && len(values) > 0 && values[0] != "" {
		t.Errorf("Referer = %q, want it absent when cleared", values)
	}
}

// TestPerRequestHeadersDoNotLeakIntoProxying is a guard: the header overrides
// belong to the FLV conversion, so they must not become a general way to set
// headers on the reverse proxy path.
func TestPerRequestHeadersDoNotLeakIntoProxying(t *testing.T) {
	// applyRequestHeaders is only reachable from the HLS fetcher; assert its
	// semantics directly so the boundary is explicit.
	req, err := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "default")
	req.Header.Set("Referer", "https://default.example/")
	applyRequestHeaders(req, []RequestHeader{
		{Name: "User-Agent", Value: "caller"},
		{Name: "Referer", Value: ""},
	})
	if got := req.Header.Get("User-Agent"); got != "caller" {
		t.Errorf("User-Agent = %q, want caller", got)
	}
	if values, present := req.Header["Referer"]; present && len(values) > 0 && values[0] != "" {
		t.Errorf("Referer = %q, want it removed", values)
	}
}
