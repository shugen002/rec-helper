package main

import (
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

func TestTargetURLQueryForm(t *testing.T) {
	cases := []struct {
		name    string
		request string
		want    string
	}{
		{
			name:    "encoded playlist URL",
			request: "http://127.0.0.1:8080/index.flv?url=" + url.QueryEscape("https://example.com/stream.m3u8"),
			want:    "https://example.com/stream.m3u8",
		},
		{
			// The key advantage of the query form: a playlist URL that carries
			// its own query string survives intact.
			name:    "playlist URL with its own query string",
			request: "http://127.0.0.1:8080/index.flv?url=" + url.QueryEscape("https://example.com/live.m3u8?token=abc&sign=deadbeef&expire=99"),
			want:    "https://example.com/live.m3u8?token=abc&sign=deadbeef&expire=99",
		},
		{
			name:    "extra parameters alongside url",
			request: "http://127.0.0.1:8080/index.flv?foo=bar&url=" + url.QueryEscape("https://example.com/a.m3u8") + "&baz=1",
			want:    "https://example.com/a.m3u8",
		},
		{
			name:    "https upstream",
			request: "http://127.0.0.1:8080/index.flv?url=" + url.QueryEscape("http://example.com/a.m3u8"),
			want:    "http://example.com/a.m3u8",
		},
		{
			name:    "short aliases are accepted",
			request: "http://127.0.0.1:8080/index.flv?u=" + url.QueryEscape("https://example.com/a.m3u8"),
			want:    "https://example.com/a.m3u8",
		},
		{
			name:    "src alias is accepted",
			request: "http://127.0.0.1:8080/index.flv?src=" + url.QueryEscape("https://example.com/a.m3u8"),
			want:    "https://example.com/a.m3u8",
		},
		{
			name:    "url takes precedence over an alias",
			request: "http://127.0.0.1:8080/index.flv?u=" + url.QueryEscape("https://example.com/first.m3u8") + "&url=" + url.QueryEscape("https://example.com/second.m3u8"),
			want:    "https://example.com/second.m3u8",
		},
		{
			name:    "root path also works with the query form",
			request: "http://127.0.0.1:8080/?url=" + url.QueryEscape("https://example.com/a.m3u8"),
			want:    "https://example.com/a.m3u8",
		},
		{
			name:    "unencoded but escaped upstream URL still works",
			request: "http://127.0.0.1:8080/index.flv?url=https%3A%2F%2Fexample.com%2Fa.m3u8",
			want:    "https://example.com/a.m3u8",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := targetURL(request(t, c.request))
			if err != nil {
				t.Fatalf("targetURL: %v", err)
			}
			if got != c.want {
				t.Errorf("targetURL = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTargetURLPathForm(t *testing.T) {
	cases := []struct {
		name    string
		request string
		want    string
	}{
		{
			name:    "plain path form",
			request: "http://127.0.0.1:8080/https://example.com/stream.m3u8",
			want:    "https://example.com/stream.m3u8",
		},
		{
			name:    "collapsed double slash",
			request: "http://127.0.0.1:8080/https:/example.com/stream.m3u8",
			want:    "https://example.com/stream.m3u8",
		},
		{
			name:    "http scheme",
			request: "http://127.0.0.1:8080/http://example.com/stream.m3u8",
			want:    "http://example.com/stream.m3u8",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := targetURL(request(t, c.request))
			if err != nil {
				t.Fatalf("targetURL: %v", err)
			}
			if got != c.want {
				t.Errorf("targetURL = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTargetURLRejections(t *testing.T) {
	cases := []struct {
		name    string
		request string
	}{
		{"no target at all", "http://127.0.0.1:8080/"},
		{"index without a url parameter", "http://127.0.0.1:8080/index.flv"},
		{"empty url parameter", "http://127.0.0.1:8080/index.flv?url="},
		{"blank url parameter", "http://127.0.0.1:8080/index.flv?url=%20%20"},
		{"unsupported scheme in query", "http://127.0.0.1:8080/index.flv?url=" + url.QueryEscape("file:///etc/passwd")},
		{"no host in query", "http://127.0.0.1:8080/index.flv?url=" + url.QueryEscape("https:///stream.m3u8")},
		{"relative target in query", "http://127.0.0.1:8080/index.flv?url=" + url.QueryEscape("/stream.m3u8")},
		{"unsupported scheme in path", "http://127.0.0.1:8080/ftp://example.com/a.m3u8"},
		{"relative path", "http://127.0.0.1:8080/stream.m3u8"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, err := targetURL(request(t, c.request)); err == nil {
				t.Errorf("targetURL = %q, want an error", got)
			}
		})
	}
}

// TestIndexServesUsageOnlyWithoutTarget checks that visiting the index does not
// try to start a stream, and that adding a target switches to streaming.
func TestIndexServesUsage(t *testing.T) {
	h := &handler{}
	for _, path := range []string{"/", "/index.flv"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, request(t, "http://127.0.0.1:8080"+path))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("%s: content type = %q, want text/plain", path, ct)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "index.flv?url=") {
			t.Errorf("%s: usage does not document the query form:\n%s", path, body)
		}
		if !strings.Contains(body, "8080/<HLS playlist URL>") {
			t.Errorf("%s: usage does not document the path form:\n%s", path, body)
		}
	}
}

func TestBadRequestShowsUsage(t *testing.T) {
	h := &handler{}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, "http://127.0.0.1:8080/index.flv?url="+url.QueryEscape("file:///x")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "index.flv?url=") {
		t.Errorf("a bad request must include usage:\n%s", rec.Body.String())
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := &handler{}
	r := request(t, "http://127.0.0.1:8080/index.flv?url="+url.QueryEscape("https://example.com/a.m3u8"))
	r.Method = http.MethodPost
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow = %q, want \"GET, HEAD\"", allow)
	}
}

// TestHeadDoesNotStream checks that a HEAD request returns the streaming
// headers without opening an upstream connection.
func TestHeadDoesNotStream(t *testing.T) {
	h := &handler{}
	r := request(t, "http://127.0.0.1:8080/index.flv?url="+url.QueryEscape("https://example.invalid/a.m3u8"))
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
}
