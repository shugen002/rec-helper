package hlsflv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shugen002/rec-helper/internal/mp4"
)

// countingWriter discards output but counts bytes, so a conversion can be run
// without holding the whole stream in memory.
type countingWriter struct {
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// TestLiveNoVideoFrameLoss is the regression test for frame loss: every video
// sample the parser finds must come out as exactly one video tag.
//
// The check is done by fetching the real segments once, parsing them with the
// same code the client uses, and comparing that count with the tags the client
// actually produces for the same input.
func TestLiveNoVideoFrameLoss(t *testing.T) {
	url := firstNonEmpty(os.Getenv(envAVC), os.Getenv(envHEVC))
	if url == "" {
		t.Skipf("set %s or %s to run the live frame loss test", envAVC, envHEVC)
	}
	if !checkPlaylistReachable(t, url) {
		t.Skip("the example stream is no longer available")
	}

	// Fetch the playlist and every resource it references, once.
	fetched := map[string][]byte{}
	var mu sync.Mutex
	var order []string

	fetch := func(raw string) []byte {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatalf("building a request for %s: %v", raw, err)
		}
		req.Header.Set("User-Agent", DefaultUserAgent)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("fetching %s: %v", raw, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("fetching %s: %s", raw, resp.Status)
		}
		buf := make([]byte, 0, 1<<20)
		tmp := make([]byte, 1<<16)
		for {
			n, err := resp.Body.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				break
			}
		}
		return buf
	}

	playlist := string(fetch(url))
	base := url[:strings.LastIndex(url, "/")+1]

	var mapURI string
	var segs []string
	for _, ln := range strings.Split(playlist, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "#EXT-X-MAP:") {
			if i := strings.Index(ln, "URI=\""); i >= 0 {
				mapURI = ln[i+5:]
				if j := strings.Index(mapURI, "\""); j >= 0 {
					mapURI = mapURI[:j]
				}
			}
		}
		if strings.HasSuffix(ln, ".m4s") && !strings.HasPrefix(ln, "#") {
			segs = append(segs, ln)
		}
	}
	if mapURI == "" || len(segs) == 0 {
		t.Skip("the playlist has no EXT-X-MAP or no segments")
	}
	if len(segs) > 8 {
		segs = segs[:8]
	}

	mu.Lock()
	fetched[mapURI] = fetch(base + mapURI)
	for _, s := range segs {
		fetched[s] = fetch(base + s)
		order = append(order, s)
	}
	mu.Unlock()

	// Expected sample counts, from the same parser the client uses.
	info, err := mp4.ParseInit(fetched[mapURI])
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	video, _ := info.RequiredTracks()
	if video == nil {
		t.Skip("the stream has no video track")
	}
	expectedVideo := 0
	for _, s := range order {
		frag, err := mp4.ParseFragment(info, fetched[s], mp4.FragmentOptions{})
		if err != nil {
			t.Fatalf("ParseFragment(%s): %v", s, err)
		}
		for _, smp := range frag.Samples {
			if smp.TrackID == video.ID {
				expectedVideo++
			}
		}
	}
	if expectedVideo == 0 {
		t.Skip("no video samples were parsed")
	}
	t.Logf("parsed %d video samples from %d segments", expectedVideo, len(order))

	// Replay the same bytes through the full client.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if strings.HasSuffix(name, ".m3u8") {
			// A playlist that ends, so the run terminates by itself.
			io := []byte("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:1\n#EXT-X-MAP:URI=\"" + mapURI + "\"\n#EXT-X-ENDLIST\n")
			for _, s := range order {
				io = append(io, []byte("#EXTINF:1.00,\n"+s+"\n")...)
			}
			w.Write(io)
			return
		}
		mu.Lock()
		body, ok := fetched[name]
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()

	videoTags, keyTags := 0, 0
	opts := Options{
		Logger: func(string, ...any) {},
		// OnTag reports media tags only; decoder configuration records are not
		// reported, so every video tag here is one sample.
		OnTag: func(tag TagInfo) {
			if tag.Type != 0x09 {
				return
			}
			videoTags++
			if tag.Keyframe {
				keyTags++
			}
		},
	}
	out := &countingWriter{}
	c, err := New(srv.URL+"/index.m3u8", out, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatalf("Run: %v", err)
	}

	if videoTags != expectedVideo {
		t.Errorf("produced %d video sample tags, want %d: %d samples were lost",
			videoTags, expectedVideo, expectedVideo-videoTags)
	}
	t.Logf("produced %d video sample tags (%d keyframes), expected %d",
		videoTags, keyTags, expectedVideo)
}

// firstNonEmpty returns the first non empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
