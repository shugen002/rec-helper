package hlsflv

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Synthetic fMP4 construction, mirroring the shapes the mp4 package parses.
// ---------------------------------------------------------------------------

func box(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:8], typ)
	copy(out[8:], body)
	return out
}

func full(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	vf := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append([][]byte{vf}, payload...)...)
}

func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

// initSegment builds an ftyp + moov with one video and one audio track.
func initSegment() []byte {
	avcC := []byte{0x01, 0x64, 0x00, 0x2A, 0xFF}
	videoEntry := box("avc1", func() []byte {
		body := make([]byte, 78)
		binary.BigEndian.PutUint16(body[24:26], 1920)
		binary.BigEndian.PutUint16(body[26:28], 1080)
		return body
	}(), box("avcC", avcC))

	audioBody := make([]byte, 28)
	binary.BigEndian.PutUint16(audioBody[16:18], 2)
	binary.BigEndian.PutUint32(audioBody[24:28], 48000<<16)
	// esds carrying an AudioSpecificConfig of 0x11 0x90 (AAC-LC, 48 kHz, stereo).
	esds := func() []byte {
		asc := []byte{0x11, 0x90}
		dcd := append([]byte{0x04, 0x80, 0x80, 0x80, 0x0d + byte(len(asc)), 0x40}, make([]byte, 12)...)
		dcd = append(dcd, 0x05, 0x80, 0x80, 0x80, byte(len(asc)))
		dcd = append(dcd, asc...)
		es := append([]byte{0x03, 0x80, 0x80, 0x80, byte(3 + len(dcd)), 0x00, 0x01, 0x00}, dcd...)
		return append([]byte{0, 0, 0, 0}, es...)
	}()
	audioEntry := box("mp4a", audioBody, box("esds", esds))

	makeTrack := func(id uint32, handler string, timescale uint32, entry []byte) []byte {
		tkhdBody := make([]byte, 80)
		binary.BigEndian.PutUint32(tkhdBody[12:16], id)
		mdhdBody := make([]byte, 20)
		binary.BigEndian.PutUint32(mdhdBody[12:16], timescale)
		hdlrBody := make([]byte, 12)
		copy(hdlrBody[8:12], handler)
		stbl := box("stbl", full("stsd", 0, 0, u32(1), entry))
		minf := box("minf", stbl)
		mdia := box("mdia", box("mdhd", mdhdBody), box("hdlr", hdlrBody), minf)
		return box("trak", box("tkhd", tkhdBody), mdia)
	}

	video := makeTrack(2, "vide", 90000, videoEntry)
	audio := makeTrack(1, "soun", 48000, audioEntry)
	mvex := box("mvex",
		full("trex", 0, 0, u32(1), u32(1), u32(1024), u32(0), u32(0)),
		full("trex", 0, 0, u32(2), u32(1), u32(3000), u32(0), u32(0x02000000)),
	)
	return append(box("ftyp", []byte("isom"), u32(1), []byte("isomavc1")), box("moov", video, audio, mvex)...)
}

// mediaSegment builds one moof + mdat pair for a track.
func mediaSegment(trackID uint32, samples [][]byte, baseDecodeTime uint64, defaultDuration uint32, firstFlags uint32) []byte {
	var payload, table []byte
	for _, s := range samples {
		payload = append(payload, s...)
		table = append(table, u32(uint32(len(s)))...)
		table = append(table, u32(0)...)
	}

	const tfhdSize, tfdtSize = 8 + 4 + 4 + 4, 8 + 4 + 8
	trunSize := 8 + 4 + 4 + 4 + 4 + len(table)
	moofSize := (8 + 4 + 4) + (8 + tfhdSize + tfdtSize + trunSize) + 8
	dataOffset := moofSize + 8

	tfhd := full("tfhd", 0, tfhdDefaultSampleDurationFlag|tfhdDefaultBaseIsMoof,
		u32(trackID), u32(defaultDuration))
	tfdt := full("tfdt", 1, 0, u64(baseDecodeTime))
	trunBody := append(u32(uint32(len(samples))), u32(uint32(dataOffset))...)
	trunBody = append(trunBody, u32(firstFlags)...)
	trunBody = append(trunBody, table...)
	trun := full("trun", 0, trunFlagsUsed, trunBody)

	moof := box("moof", full("mfhd", 0, 0, u32(1)), box("traf", tfhd, tfdt, trun))
	if len(moof) != moofSize {
		panic("mediaSegment: size calculation is wrong")
	}
	return append(moof, box("mdat", payload)...)
}

// The flag values the synthetic segments use. They are duplicated here rather
// than exported so the test pins the wire format itself.
const (
	tfhdDefaultSampleDurationFlag = 0x000008
	tfhdDefaultBaseIsMoof         = 0x020000
	trunFlagsUsed                 = 0x000001 | 0x000004 | 0x000200 | 0x000800
)

// ---------------------------------------------------------------------------
// Test origin
// ---------------------------------------------------------------------------

// origin serves a two segment live playlist from memory.
type origin struct {
	init     []byte
	seg0     []byte
	seg1     []byte
	mu       sync.Mutex
	requests map[string]int
}

func (o *origin) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		o.count("/index.m3u8")
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MEDIA-SEQUENCE:1\n#EXT-X-TARGETDURATION:1\n"+
			"#EXT-X-MAP:URI=\"init.mp4\"\n"+
			"#EXTINF:1.00,\nseg0.m4s\n"+
			"#EXTINF:1.00,\nseg1.m4s\n"+
			"#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, r *http.Request) {
		o.count("/init.mp4")
		w.Write(o.init)
	})
	mux.HandleFunc("/seg0.m4s", func(w http.ResponseWriter, r *http.Request) {
		o.count("/seg0.m4s")
		w.Write(o.seg0)
	})
	mux.HandleFunc("/seg1.m4s", func(w http.ResponseWriter, r *http.Request) {
		o.count("/seg1.m4s")
		w.Write(o.seg1)
	})
	return mux
}

func (o *origin) count(path string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests[path]++
}

func (o *origin) countOf(path string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.requests[path]
}

func newOrigin() *origin {
	// Two video samples per segment and one audio sample per segment.
	seg0 := append(
		mediaSegment(2, [][]byte{{0, 0, 0, 1, 0x65, 0xAA}, {0, 0, 0, 1, 0x41, 0xBB}}, 900000, 3000, 0x02000000),
		mediaSegment(1, [][]byte{{0xDE, 0xAD}}, 48000, 1024, 0)...)
	seg1 := append(
		mediaSegment(2, [][]byte{{0, 0, 0, 1, 0x41, 0xCC}, {0, 0, 0, 1, 0x41, 0xDD}}, 906000, 3000, 0x01000000),
		mediaSegment(1, [][]byte{{0xBE, 0xEF}}, 49024, 1024, 0)...)
	return &origin{
		init:     initSegment(),
		seg0:     seg0,
		seg1:     seg1,
		requests: map[string]int{},
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// runClient converts the playlist and returns the produced FLV bytes.
func runClient(t *testing.T, url string, opts Options) []byte {
	t.Helper()
	var buf bytes.Buffer
	c, err := New(url, &buf, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return buf.Bytes()
}

func TestRunProducesValidFLV(t *testing.T) {
	o := newOrigin()
	srv := httptest.NewServer(o.handler())
	defer srv.Close()

	out := runClient(t, srv.URL+"/index.m3u8", Options{})

	if !bytes.HasPrefix(out, []byte("FLV\x01")) {
		t.Fatalf("output does not start with an FLV header: % x", out[:min(9, len(out))])
	}
	// Both media types must be declared.
	if got := out[4]; got != 0x01|0x04 {
		t.Errorf("header flags = 0x%02x, want 0x05", got)
	}

	type tag struct {
		typ  byte
		ts   uint32
		body []byte
	}
	var tags []tag
	off := 13
	for off < len(out) {
		if off+11 > len(out) {
			t.Fatalf("truncated tag header at %d", off)
		}
		typ := out[off]
		size := int(out[off+1])<<16 | int(out[off+2])<<8 | int(out[off+3])
		ts := uint32(out[off+4])<<16 | uint32(out[off+5])<<8 | uint32(out[off+6]) | uint32(out[off+7])<<24
		if off+11+size+4 > len(out) {
			t.Fatalf("tag at %d overruns the stream", off)
		}
		if prev := binary.BigEndian.Uint32(out[off+11+size:]); prev != uint32(11+size) {
			t.Fatalf("tag at %d PreviousTagSize = %d, want %d", off, prev, 11+size)
		}
		tags = append(tags, tag{typ: typ, ts: ts, body: out[off+11 : off+11+size]})
		off += 11 + size + 4
	}

	var script, videoSeq, audioSeq int
	for _, tg := range tags {
		switch tg.typ {
		case 0x12:
			script++
		case 0x09:
			if len(tg.body) >= 2 && tg.body[1] == 0 {
				videoSeq++
			}
		case 0x08:
			if len(tg.body) >= 2 && tg.body[1] == 0 {
				audioSeq++
			}
		}
	}
	if script != 1 {
		t.Errorf("script tag count = %d, want 1", script)
	}
	if videoSeq != 1 {
		t.Errorf("AVC sequence header count = %d, want 1", videoSeq)
	}
	if audioSeq != 1 {
		t.Errorf("AAC sequence header count = %d, want 1", audioSeq)
	}
	if o.countOf("/init.mp4") != 1 {
		t.Errorf("initialisation segment was fetched %d times, want 1", o.countOf("/init.mp4"))
	}
	if o.countOf("/seg0.m4s") != 1 || o.countOf("/seg1.m4s") != 1 {
		t.Errorf("segment fetches = %d, %d; want 1 each",
			o.countOf("/seg0.m4s"), o.countOf("/seg1.m4s"))
	}

	// The AAC priming frame is dropped, and both video samples of each segment
	// must appear, interleaved by presentation time.
	var audio, video int
	var lastAudio, lastVideo uint32
	for _, tg := range tags {
		switch tg.typ {
		case 0x08:
			if len(tg.body) >= 2 && tg.body[1] == 0 {
				continue
			}
			if tg.ts < lastAudio {
				t.Errorf("audio timestamp went backwards: %d then %d", lastAudio, tg.ts)
			}
			lastAudio = tg.ts
			audio++
		case 0x09:
			if len(tg.body) >= 2 && tg.body[1] == 0 {
				continue
			}
			if tg.ts < lastVideo {
				t.Errorf("video timestamp went backwards: %d then %d", lastVideo, tg.ts)
			}
			lastVideo = tg.ts
			video++
		}
	}
	if video != 4 {
		t.Errorf("video sample count = %d, want 4", video)
	}
	// Exactly one priming frame is dropped for the whole stream, so one of the
	// two audio samples survives.
	if audio != 1 {
		t.Errorf("audio sample count = %d, want 1 (one priming frame is dropped)", audio)
	}
}

func TestRunRejectsUnsupportedScheme(t *testing.T) {
	for _, u := range []string{"file:///tmp/x.m3u8", "ftp://example.com/x.m3u8", "/relative.m3u8", "not a url"} {
		if _, err := New(u, &bytes.Buffer{}, Options{}); err == nil {
			t.Errorf("New(%q) succeeded, want an error", u)
		}
	}
}

func TestRunResolvesRelativeReferences(t *testing.T) {
	o := newOrigin()
	// Every resource is served under /live/, and the playlist references them
	// relatively, so the client must resolve against the playlist URL.
	srv := httptest.NewServer(http.StripPrefix("/live", o.handler()))
	defer srv.Close()

	out := runClient(t, srv.URL+"/live/index.m3u8", Options{})
	if !bytes.HasPrefix(out, []byte("FLV")) {
		t.Fatal("expected an FLV stream")
	}
	if o.countOf("/init.mp4") != 1 {
		t.Error("the initialisation segment was not fetched")
	}
	if o.countOf("/seg0.m4s") != 1 || o.countOf("/seg1.m4s") != 1 {
		t.Error("both segments must be fetched")
	}
}

func TestRunFollowsMasterPlaylist(t *testing.T) {
	o := newOrigin()
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=256000,CODECS=\"avc1.42c015,mp4a.40.2\"\nlow/index.m3u8\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=1280000,CODECS=\"avc1.64002a,mp4a.40.2\"\nhigh/index.m3u8\n")
	})
	// Both variants resolve to the same media, so either way the media is
	// fetched; the point is that a master playlist is followed at all.
	mux.Handle("/low/", http.StripPrefix("/low", o.handler()))
	mux.Handle("/high/", http.StripPrefix("/high", o.handler()))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := runClient(t, srv.URL+"/master.m3u8", Options{})
	if o.countOf("/init.mp4") != 1 {
		t.Error("the selected variant was not fetched")
	}
	if !bytes.HasPrefix(out, []byte("FLV")) {
		t.Fatal("expected an FLV stream")
	}
}

func TestRunSkipsUnsupportedVariant(t *testing.T) {
	o := newOrigin()
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		// The only high bandwidth variant uses a codec FLV cannot carry.
		fmt.Fprint(w, "#EXTM3U\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=9000000,CODECS=\"av01.0.08M.08\"\nunsupported/index.m3u8\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=1280000,CODECS=\"avc1.64002a,mp4a.40.2\"\nplayable/index.m3u8\n")
	})
	mux.HandleFunc("/unsupported/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		t.Error("an unsupported variant must not be fetched")
	})
	mux.Handle("/playable/", http.StripPrefix("/playable", o.handler()))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := runClient(t, srv.URL+"/master.m3u8", Options{})
	if !bytes.HasPrefix(out, []byte("FLV")) {
		t.Fatal("expected an FLV stream from the playable variant")
	}
}

// TestRunDropsSegmentWithoutInitSegment checks that a playlist lacking
// EXT-X-MAP is reported per segment instead of killing the whole stream.
func TestRunDropsSegmentWithoutInitSegment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			// No EXT-X-MAP: a plain MPEG-TS playlist, which is unsupported.
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1.0,\nseg.ts\n#EXT-X-ENDLIST\n")
			return
		}
		w.Write([]byte("garbage"))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	c, err := New(srv.URL+"/index.m3u8", &buf, Options{Logger: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run must not fail the whole stream for one bad segment: %v", err)
	}
	// Nothing could be parsed, so no tag (and therefore no header) is written;
	// the important part is that Run returned normally. The segment was still
	// downloaded and counted before it was rejected.
	if st := c.Stats(); st.Segments != 1 {
		t.Errorf("Segments = %d, want 1 (downloaded but rejected)", st.Segments)
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %d bytes for a stream with no usable media, want 0", buf.Len())
	}
}

func TestRunRetriesTransientFailure(t *testing.T) {
	o := newOrigin()
	var fails int
	mux := o.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/seg0.m4s" {
			o.mu.Lock()
			fails++
			n := fails
			o.mu.Unlock()
			if n == 1 {
				// The first attempt fails with a server error.
				http.Error(w, "temporary", http.StatusInternalServerError)
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	out := runClient(t, srv.URL+"/index.m3u8", Options{Logger: func(string, ...any) {}})

	// After the retry both segments must contribute video samples.
	video := 0
	off := 13
	for off < len(out) {
		size := int(out[off+1])<<16 | int(out[off+2])<<8 | int(out[off+3])
		body := out[off+11 : off+11+size]
		if out[off] == 0x09 && len(body) >= 2 && body[1] == 1 {
			video++
		}
		off += 11 + size + 4
	}
	if video != 4 {
		t.Errorf("video sample count = %d, want 4 after a retried segment", video)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	o := newOrigin()
	// A live playlist that never ends, so the client has to be cancelled.
	mux := http.NewServeMux()
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MAP:URI=\"init.mp4\"\n"+
			"#EXTINF:1.00,\nseg0.m4s\n")
	})
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, r *http.Request) { w.Write(o.init) })
	mux.HandleFunc("/seg0.m4s", func(w http.ResponseWriter, r *http.Request) { w.Write(o.seg0) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var buf bytes.Buffer
	c, err := New(srv.URL+"/live.m3u8", &buf, Options{
		PlaylistInterval: 20 * time.Millisecond,
		Logger:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = c.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want context.DeadlineExceeded", err)
	}
	if buf.Len() == 0 {
		t.Error("expected some FLV output before cancellation")
	}
}

func TestClientStats(t *testing.T) {
	o := newOrigin()
	srv := httptest.NewServer(o.handler())
	defer srv.Close()

	var buf bytes.Buffer
	c, err := New(srv.URL+"/index.m3u8", &buf, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	st := c.Stats()
	if st.Segments != 2 {
		t.Errorf("Segments = %d, want 2", st.Segments)
	}
	// Two segments contribute four video samples, and one audio sample survives
	// because the stream has a single AAC priming frame.
	if st.Samples != 5 {
		t.Errorf("Samples = %d, want 5", st.Samples)
	}
	if st.Bytes == 0 {
		t.Error("Bytes must be non zero")
	}
	if st.StartedAt.IsZero() || st.EndedAt.IsZero() {
		t.Error("StartedAt and EndedAt must be recorded")
	}
}

func TestStartFromLiveTrimsBacklog(t *testing.T) {
	initData := initSegment()
	seg := mediaSegment(2, [][]byte{{0, 0, 0, 1, 0x41, 1}}, 0, 3000, 0)

	var mu sync.Mutex
	var requested []string

	// serveSegments records and serves any /segN.m4s request. ServeMux has no
	// prefix match without a trailing slash, so dispatch manually.
	var segHandler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requested = append(requested, r.URL.Path)
		mu.Unlock()
		w.Write(seg)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		// A live playlist never carries EXT-X-ENDLIST.
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:1\n#EXT-X-MAP:URI=\"init.mp4\"\n")
		for i := 0; i < 20; i++ {
			fmt.Fprintf(&b, "#EXTINF:1.00,\nseg%d.m4s\n", i)
		}
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, r *http.Request) { w.Write(initData) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/seg") && strings.HasSuffix(r.URL.Path, ".m4s") {
			segHandler(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	c, err := New(srv.URL+"/live.m3u8", &buf, Options{
		StartFromLive:      true,
		PlaylistInterval:   5 * time.Millisecond,
		MaxPlaylistReloads: 1,
		Logger:             func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// The playlist never ends, so the run is stopped by the context.
	if err := c.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requested) > maxMirroredLiveSegments {
		t.Errorf("fetched %d segments, want at most %d when starting at the live edge",
			len(requested), maxMirroredLiveSegments)
	}
	if len(requested) == 0 {
		t.Error("expected at least one segment to be fetched")
	}
}

// TestStartFromLiveDisabledFetchesBacklog is the counterpart: without the live
// option the whole playlist is mirrored from the beginning.
func TestStartFromLiveDisabledFetchesBacklog(t *testing.T) {
	initData := initSegment()
	seg := mediaSegment(2, [][]byte{{0, 0, 0, 1, 0x41, 1}}, 0, 3000, 0)

	var mu sync.Mutex
	var requested []string

	mux := http.NewServeMux()
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:1\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXT-X-ENDLIST\n")
		for i := 0; i < 20; i++ {
			fmt.Fprintf(&b, "#EXTINF:1.00,\nseg%d.m4s\n", i)
		}
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, r *http.Request) { w.Write(initData) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/seg") && strings.HasSuffix(r.URL.Path, ".m4s") {
			mu.Lock()
			requested = append(requested, r.URL.Path)
			mu.Unlock()
			w.Write(seg)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	c, err := New(srv.URL+"/live.m3u8", &buf, Options{
		Logger: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requested) != 20 {
		t.Errorf("fetched %d segments, want all 20 of the playlist", len(requested))
	}
}

// failWriter accepts a limited number of bytes and then fails, which is what a
// consumer that disconnects mid-stream looks like to the client.
type failWriter struct {
	limit int
	got   int
}

func (w *failWriter) Write(p []byte) (int, error) {
	if w.got >= w.limit {
		return 0, errors.New("connection reset by peer")
	}
	n := len(p)
	if w.got+n > w.limit {
		n = w.limit - w.got
	}
	w.got += n
	if n < len(p) {
		return n, errors.New("connection reset by peer")
	}
	return n, nil
}

// TestRunStopsWhenTheConsumerDisconnects covers the case where the output side
// fails: the conversion must stop at once instead of downloading the rest of a
// live playlist into a dead connection, which is what made a player appear to
// hang after it had already been stopped.
func TestRunStopsWhenTheConsumerDisconnects(t *testing.T) {
	o := newOrigin()
	srv := httptest.NewServer(o.handler())
	defer srv.Close()

	w := &failWriter{limit: 64}
	c, err := New(srv.URL+"/index.m3u8", w, Options{Logger: t.Logf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runErr := c.Run(context.Background())
	t.Logf("Run returned: %v (wrote %d bytes of a %d byte limit)", runErr, w.got, w.limit)
	if runErr == nil {
		t.Fatal("Run returned nil, want an error when the output is closed")
	}
	if !errors.Is(runErr, ErrClientGone) {
		t.Errorf("Run = %v, want ErrClientGone", runErr)
	}

	// Only the first segments should have been fetched; the rest of the
	// playlist must not be pulled into a dead connection.
	if n := o.countOf("/seg1.m4s"); n != 0 {
		t.Errorf("fetched /seg1.m4s %d times after the output closed, want 0", n)
	}
}

// TestRunStopsWhenOutputClosesMidLiveStream is the same check against an
// endless playlist, where a missing stop would loop forever.
func TestRunStopsWhenOutputClosesMidLiveStream(t *testing.T) {
	initData := initSegment()
	seg := mediaSegment(2, [][]byte{{0, 0, 0, 1, 0x41, 1}}, 0, 3000, 0)

	var mu sync.Mutex
	fetched := 0

	mux := http.NewServeMux()
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:1\n#EXT-X-MAP:URI=\"init.mp4\"\n"+
			"#EXTINF:1.00,\nseg0.m4s\n")
	})
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, r *http.Request) { w.Write(initData) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/seg") {
			mu.Lock()
			fetched++
			mu.Unlock()
			w.Write(seg)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	w := &failWriter{limit: 40}
	c, err := New(srv.URL+"/live.m3u8", w, Options{
		PlaylistInterval: 5 * time.Millisecond,
		Logger:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrClientGone) {
			t.Errorf("Run = %v, want ErrClientGone", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop on an endless playlist after the output closed")
	}

	mu.Lock()
	n := fetched
	mu.Unlock()
	if n > 3 {
		t.Errorf("kept fetching after the output closed: %d segment requests", n)
	}
}

func TestCodecsSupported(t *testing.T) {
	cases := []struct {
		codecs string
		want   bool
	}{
		{"avc1.64002a,mp4a.40.2", true},
		{"hvc1.1.6.L93.B0", true},
		{"hev1.1.6.L93.B0", true},
		{"opus", true},
		{"", true},
		{"av01.0.08M.08", false},
		{"vp09.00.10.08", false},
		{"avc1.42c015,av01.0.08M.08", false},
		{"ac-3", false},
	}
	for _, c := range cases {
		if got := codecsSupported(c.codecs); got != c.want {
			t.Errorf("codecsSupported(%q) = %v, want %v", c.codecs, got, c.want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
