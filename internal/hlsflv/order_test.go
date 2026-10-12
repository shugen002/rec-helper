package hlsflv

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	"github.com/shugen002/rec-helper/internal/mp4"
)

// flvBuffer collects the FLV stream so its tags can be inspected.
type flvBuffer struct {
	buf bytes.Buffer
}

func (w *flvBuffer) Write(p []byte) (int, error) { return w.buf.Write(p) }

// videoTagTimestamps walks an FLV stream and returns the timestamp of every
// video sample tag, in file order. Sequence headers are skipped.
func videoTagTimestamps(t *testing.T, stream []byte) []int64 {
	t.Helper()
	if len(stream) < 13 || !bytes.Equal(stream[0:3], []byte("FLV")) {
		t.Fatal("output is not an FLV stream")
	}
	var out []int64
	off := 13
	for off+11 <= len(stream) {
		typ := stream[off]
		size := int(stream[off+1])<<16 | int(stream[off+2])<<8 | int(stream[off+3])
		ts := int64(uint32(stream[off+4])<<16 | uint32(stream[off+5])<<8 |
			uint32(stream[off+6]) | uint32(stream[off+7])<<24)
		if off+11+size+4 > len(stream) {
			break
		}
		body := stream[off+11 : off+11+size]
		if typ == 0x09 && len(body) >= 2 && body[1] == 1 {
			out = append(out, ts)
		}
		off += 11 + size + 4
	}
	return out
}

// newEmitClient builds a client wired to a tag collector, for exercising emit
// directly.
func newEmitClient(t *testing.T, out *flvBuffer) *Client {
	t.Helper()
	info, err := mp4.ParseInit(initSegment())
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	collector := NewTagCollector()
	c := &Client{
		opts:      Options{},
		seen:      map[string]bool{},
		clocks:    map[uint32]*trackClock{},
		info:      info,
		w:         collector,
		collector: collector,
		out:       out,
	}
	return c
}

// TestEmitAcceptsNonMonotonicPresentationTimes makes the ordering rule explicit:
// with B frames the presentation order is not monotonic, and that must be
// accepted without reordering, because the tag timestamp is a decode timestamp.
func TestEmitAcceptsNonMonotonicPresentationTimes(t *testing.T) {
	out := &flvBuffer{}
	c := newEmitClient(t, out)

	// Decode order increases while presentation order does not: a later decoded
	// frame is presented earlier, which is the normal B frame pattern.
	samples := []mp4.Sample{
		{TrackID: 2, DTS: 0, CTS: 3000, Duration: 3000, Data: []byte{0, 0, 0, 1, 0x65}, Keyframe: true},
		{TrackID: 2, DTS: 3000, CTS: 3000, Duration: 3000, Data: []byte{0, 0, 0, 1, 0x41}},
		{TrackID: 2, DTS: 6000, CTS: 0, Duration: 3000, Data: []byte{0, 0, 0, 1, 0x41}},
	}
	if err := c.emit(samples); err != nil {
		t.Fatalf("emit rejected a valid B frame sequence: %v", err)
	}
	if err := c.flushTags(); err != nil {
		t.Fatalf("flushTags: %v", err)
	}

	got := videoTagTimestamps(t, out.buf.Bytes())
	want := []int64{0, 33, 66}
	if len(got) != len(want) {
		t.Fatalf("produced %d video tags, want %d", len(got), len(want))
	}
	// The tags must follow decode order, so the timestamps increase even though
	// the presentation times do not.
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tag %d timestamp = %d, want %d (decode order)", i, got[i], want[i])
		}
	}
}

// TestEmitRejectsNonMonotonicDecodeTimestamps checks the guard that turns a
// parser regression into an error instead of a corrupt stream.
func TestEmitRejectsNonMonotonicDecodeTimestamps(t *testing.T) {
	c := newEmitClient(t, &flvBuffer{})
	samples := []mp4.Sample{
		{TrackID: 2, DTS: 1000, Duration: 3000, Data: []byte{0, 0, 0, 1, 0x65}, Keyframe: true},
		{TrackID: 2, DTS: 500, Duration: 3000, Data: []byte{0, 0, 0, 1, 0x41}},
	}
	err := c.emit(samples)
	if err == nil {
		t.Fatal("emit accepted decode timestamps that go backwards")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("go backwards")) {
		t.Errorf("error = %v, want it to mention backwards timestamps", err)
	}
}

// TestEmitKeepsAudioAndVideoGroupsIntact documents that samples are written in
// parser order, which groups each track's samples rather than interleaving them.
// FLV does not require interleaving, and reordering is what broke B frames.
func TestEmitKeepsAudioAndVideoGroupsIntact(t *testing.T) {
	out := &flvBuffer{}
	c := newEmitClient(t, out)

	// The parser emits one track's samples after another, as the trun boxes are
	// laid out in the segment.
	samples := []mp4.Sample{
		{TrackID: 1, DTS: 0, Duration: 1024, Data: []byte{0xAA}},
		{TrackID: 1, DTS: 1024, Duration: 1024, Data: []byte{0xBB}},
		{TrackID: 2, DTS: 0, CTS: 0, Duration: 3000, Data: []byte{0, 0, 0, 1, 0x65}, Keyframe: true},
		{TrackID: 2, DTS: 3000, CTS: 0, Duration: 3000, Data: []byte{0, 0, 0, 1, 0x41}},
	}
	if err := c.emit(samples); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := c.flushTags(); err != nil {
		t.Fatalf("flushTags: %v", err)
	}

	// Walk the tags and confirm the order is audio, video, video. The first
	// audio sample is the AAC priming frame and is dropped, so only one audio
	// tag appears.
	stream := out.buf.Bytes()
	var kinds []byte
	off := 13
	for off+11 <= len(stream) {
		typ := stream[off]
		size := int(stream[off+1])<<16 | int(stream[off+2])<<8 | int(stream[off+3])
		if off+11+size+4 > len(stream) {
			break
		}
		body := stream[off+11 : off+11+size]
		if len(body) >= 2 && body[1] == 1 {
			kinds = append(kinds, typ)
		}
		off += 11 + size + 4
	}
	want := []byte{0x08, 0x09, 0x09}
	if !bytes.Equal(kinds, want) {
		t.Errorf("tag order = %v, want %v (parser order, not interleaved)", kinds, want)
	}
}

// TestProducedVideoTimestampsAreMonotonic runs the whole conversion and checks
// the produced stream's video timestamps never decrease.
func TestProducedVideoTimestampsAreMonotonic(t *testing.T) {
	o := newOrigin()
	srv := httptest.NewServer(o.handler())
	defer srv.Close()

	out := &flvBuffer{}
	c, err := New(srv.URL+"/index.m3u8", out, Options{
		Logger:      func(string, ...any) {},
		EventLogger: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stamps := videoTagTimestamps(t, out.buf.Bytes())
	if len(stamps) == 0 {
		t.Fatal("no video tags were produced")
	}
	for i := 1; i < len(stamps); i++ {
		if stamps[i] < stamps[i-1] {
			t.Fatalf("video timestamp went backwards at tag %d: %d then %d",
				i, stamps[i-1], stamps[i])
		}
	}
}
