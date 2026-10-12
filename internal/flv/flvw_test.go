package flv

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// tag is one decoded FLV tag.
type tag struct {
	typ  byte
	ts   uint32
	body []byte
}

// walkTags validates the framing of an FLV stream and returns its tags.
func walkTags(t *testing.T, stream []byte) []tag {
	t.Helper()
	if len(stream) < 13 {
		t.Fatalf("stream is only %d bytes long", len(stream))
	}
	if !bytes.Equal(stream[0:3], []byte("FLV")) {
		t.Fatalf("signature = %q, want FLV", stream[0:3])
	}
	if stream[3] != 1 {
		t.Errorf("version = %d, want 1", stream[3])
	}
	if got := binary.BigEndian.Uint32(stream[5:9]); got != headerSize {
		t.Errorf("data offset = %d, want %d", got, headerSize)
	}
	if got := binary.BigEndian.Uint32(stream[9:13]); got != 0 {
		t.Errorf("first PreviousTagSize = %d, want 0", got)
	}

	var tags []tag
	off := 13
	for off < len(stream) {
		if off+11 > len(stream) {
			t.Fatalf("truncated tag header at %d", off)
		}
		typ := stream[off]
		size := int(stream[off+1])<<16 | int(stream[off+2])<<8 | int(stream[off+3])
		ts := uint32(stream[off+4])<<16 | uint32(stream[off+5])<<8 |
			uint32(stream[off+6]) | uint32(stream[off+7])<<24
		if stream[off+8] != 0 || stream[off+9] != 0 || stream[off+10] != 0 {
			t.Errorf("tag at %d has a non zero stream ID", off)
		}
		if off+11+size+4 > len(stream) {
			t.Fatalf("tag at %d declares %d bytes but only %d remain", off, size, len(stream)-off-11-4)
		}
		body := stream[off+11 : off+11+size]
		if prev := binary.BigEndian.Uint32(stream[off+11+size:]); prev != uint32(11+size) {
			t.Errorf("tag at %d has PreviousTagSize %d, want %d", off, prev, 11+size)
		}
		tags = append(tags, tag{typ: typ, ts: ts, body: body})
		off += 11 + size + 4
	}
	return tags
}

// scriptBody returns the payload of the single script tag of a stream.
func scriptBody(t *testing.T, stream []byte) []byte {
	t.Helper()
	var bodies [][]byte
	for _, tg := range walkTags(t, stream) {
		if tg.typ == TagScript {
			bodies = append(bodies, tg.body)
		}
	}
	if len(bodies) != 1 {
		t.Fatalf("script tag count = %d, want 1", len(bodies))
	}
	return bodies[0]
}

// ctsOf decodes the signed 24 bit composition time of a video tag body.
func ctsOf(body []byte) int32 {
	v := int32(body[2])<<16 | int32(body[3])<<8 | int32(body[4])
	if v >= 0x800000 {
		v -= 0x1000000
	}
	return v
}

// ---------------------------------------------------------------------------
// AMF metadata
//
// A property is encoded as keyLength(2) key type(1) value, and the object ends
// with a zero length key followed by the end marker. These tests assert the
// exact byte sequence, because a wrong ordering silently corrupts the stream
// for demuxers that parse onMetaData.
// ---------------------------------------------------------------------------

// metaBuilder assembles the expected AMF0 payload.
type metaBuilder struct {
	b   []byte
	num int
}

func newMetaBuilder() *metaBuilder {
	m := &metaBuilder{}
	m.b = append(m.b, amf0String)
	m.str("onMetaData")
	m.b = append(m.b, amf0ECMAArray)
	m.b = append(m.b, 0, 0, 0, 0) // array count placeholder
	return m
}

func (m *metaBuilder) str(s string) {
	m.b = append(m.b, byte(len(s)>>8), byte(len(s)))
	m.b = append(m.b, s...)
}

func (m *metaBuilder) number(key string, v float64) {
	m.num++
	m.str(key)
	m.b = append(m.b, amf0Number)
	bits := math.Float64bits(v)
	m.b = append(m.b,
		byte(bits>>56), byte(bits>>48), byte(bits>>40), byte(bits>>32),
		byte(bits>>24), byte(bits>>16), byte(bits>>8), byte(bits))
}

func (m *metaBuilder) boolean(key string, v bool) {
	m.num++
	m.str(key)
	m.b = append(m.b, amf0Boolean)
	if v {
		m.b = append(m.b, 1)
	} else {
		m.b = append(m.b, 0)
	}
}

func (m *metaBuilder) text(key, v string) {
	m.num++
	m.str(key)
	m.b = append(m.b, amf0String)
	m.str(v)
}

func (m *metaBuilder) done() []byte {
	// The array count follows the 13 byte event name header.
	binary.BigEndian.PutUint32(m.b[14:18], uint32(m.num))
	m.b = append(m.b, 0, 0, amf0ObjectEnd)
	return m.b
}

// assertMetadata serialises md and compares the script payload byte for byte.
func assertMetadata(t *testing.T, md Metadata, want []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteMetadata(0, md); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	got := scriptBody(t, buf.Bytes())
	if !bytes.Equal(got, want) {
		t.Errorf("onMetaData payload mismatch\n got % x\nwant % x", got, want)
	}
}

func TestWriteMetadataKeyThenTypeOrder(t *testing.T) {
	// One number property. stereo is always emitted, so expect two properties,
	// and it comes after the number properties.
	m := newMetaBuilder()
	m.number("width", 1920)
	m.boolean("stereo", false)
	want := m.done()

	assertMetadata(t, Metadata{Width: 1920}, want)

	// Independently pin the layout so a future reordering cannot slip through:
	// after the 4 byte array count the first key length must appear, and the
	// type marker must follow the key rather than precede it.
	if got := want[14:18]; !bytes.Equal(got, []byte{0x00, 0x00, 0x00, 0x02}) {
		t.Fatalf("array count bytes = % x, want 00 00 00 02", got)
	}
	// bytes 18..19 are the key length, so the key itself starts at 20.
	if got := want[18:20]; !bytes.Equal(got, []byte{0x00, 0x05}) {
		t.Errorf("key length bytes = % x, want 00 05", got)
	}
	if string(want[20:25]) != "width" {
		t.Errorf("key bytes = %q, want \"width\"", want[20:25])
	}
	// The type marker follows the key; a value byte (0x40 for 1920.0) here
	// would mean the ordering regressed.
	if want[25] != amf0Number {
		t.Errorf("type marker at 25 = 0x%02x, want 0x%02x (number)", want[25], amf0Number)
	}
}

func TestWriteMetadataBooleanAndString(t *testing.T) {
	m := newMetaBuilder()
	m.boolean("stereo", true)
	m.text("encoder", "rec-helper")
	assertMetadata(t, Metadata{Stereo: true, Encoder: "rec-helper"}, m.done())
}

func TestWriteMetadataFull(t *testing.T) {
	m := newMetaBuilder()
	m.number("width", 1920)
	m.number("height", 1080)
	m.number("framerate", 60)
	m.number("videocodecid", CodecAVC)
	m.number("audiosamplerate", 48000)
	m.number("audiosamplesize", 16)
	m.number("audiochannels", 2)
	m.number("audiocodecid", SoundAAC)
	m.boolean("stereo", true)
	m.text("encoder", "rec-helper")
	assertMetadata(t, Metadata{
		Width:           1920,
		Height:          1080,
		FrameRate:       60,
		VideoCodecID:    CodecAVC,
		AudioSampleRate: 48000,
		AudioSampleSize: 16,
		AudioChannels:   2,
		AudioCodecID:    SoundAAC,
		Stereo:          true,
		Encoder:         "rec-helper",
	}, m.done())
}

// TestWriteMetadataOmitsDurationForLiveStream guards the rule that a live
// stream must not advertise a duration.
func TestWriteMetadataOmitsDurationForLiveStream(t *testing.T) {
	m := newMetaBuilder()
	m.boolean("stereo", true)
	m.text("encoder", "rec-helper")
	assertMetadata(t, Metadata{Stereo: true, Encoder: "rec-helper"}, m.done())
}

func TestWriteMetadataIncludesDurationWhenKnown(t *testing.T) {
	m := newMetaBuilder()
	m.number("duration", 12.5)
	m.boolean("stereo", true)
	m.text("encoder", "rec-helper")
	assertMetadata(t, Metadata{Duration: 12.5, Stereo: true, Encoder: "rec-helper"}, m.done())
}

func TestWriteMetadataExtrasAreSortedByName(t *testing.T) {
	// The writer emits number properties, then stereo, then the extra map in
	// sorted key order, then encoder.
	m := newMetaBuilder()
	m.boolean("stereo", true)
	m.number("audiodatarate", 128)
	m.number("videodatarate", 3000)
	m.text("encoder", "rec-helper")
	assertMetadata(t, Metadata{
		Stereo:  true,
		Encoder: "rec-helper",
		Extra:   map[string]float64{"videodatarate": 3000, "audiodatarate": 128},
	}, m.done())
}

// ---------------------------------------------------------------------------
// AVC video
// ---------------------------------------------------------------------------

func TestAVCSequenceHeaderAndNALUs(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetFlags(true, true)
	record := []byte{0x01, 0x64, 0x00, 0x2A, 0xFF}
	if err := w.WriteAVCSequenceHeader(record); err != nil {
		t.Fatalf("WriteAVCSequenceHeader: %v", err)
	}
	if err := w.WriteAVCNALU([]byte{0, 0, 0, 1, 0x65}, 1234, -7, true); err != nil {
		t.Fatalf("WriteAVCNALU: %v", err)
	}
	if err := w.WriteAACSequenceHeader([]byte{0x11, 0x90}); err != nil {
		t.Fatalf("WriteAACSequenceHeader: %v", err)
	}
	if err := w.WriteAACRaw([]byte{0xAA, 0xBB}, 1240); err != nil {
		t.Fatalf("WriteAACRaw: %v", err)
	}

	stream := buf.Bytes()
	if stream[4] != headerFlagAudio|headerFlagVideo {
		t.Errorf("flags = 0x%02x, want 0x%02x", stream[4], headerFlagAudio|headerFlagVideo)
	}
	tags := walkTags(t, stream)
	if len(tags) != 4 {
		t.Fatalf("tag count = %d, want 4", len(tags))
	}

	seq := tags[0]
	if seq.typ != TagVideo || seq.ts != 0 {
		t.Errorf("AVC sequence header: type=%d ts=%d", seq.typ, seq.ts)
	}
	if seq.body[0] != FrameKey<<4|CodecAVC {
		t.Errorf("AVC sequence header byte = 0x%02x, want 0x%02x", seq.body[0], FrameKey<<4|CodecAVC)
	}
	if seq.body[1] != AVCSequenceHeader {
		t.Errorf("AVC packet type = %d, want %d", seq.body[1], AVCSequenceHeader)
	}
	if !bytes.Equal(seq.body[5:], record) {
		t.Errorf("configuration record = % x, want % x", seq.body[5:], record)
	}

	nalu := tags[1]
	if nalu.typ != TagVideo || nalu.ts != 1234 {
		t.Errorf("AVC NALU: type=%d ts=%d, want 1234", nalu.typ, nalu.ts)
	}
	if nalu.body[1] != AVCNALU {
		t.Errorf("AVC packet type = %d, want %d", nalu.body[1], AVCNALU)
	}
	if got := ctsOf(nalu.body); got != -7 {
		t.Errorf("composition time = %d, want -7", got)
	}

	aacSeq := tags[2]
	if aacSeq.typ != TagAudio || aacSeq.ts != 0 {
		t.Errorf("AAC sequence header: type=%d ts=%d", aacSeq.typ, aacSeq.ts)
	}
	if aacSeq.body[0]>>4 != SoundAAC || aacSeq.body[1] != AACSequenceHeader {
		t.Errorf("AAC sequence header bytes = % x", aacSeq.body[:2])
	}

	aacRaw := tags[3]
	if aacRaw.typ != TagAudio || aacRaw.ts != 1240 {
		t.Errorf("AAC raw: type=%d ts=%d, want 1240", aacRaw.typ, aacRaw.ts)
	}
	if aacRaw.body[1] != AACRaw {
		t.Errorf("AAC packet type = %d, want %d", aacRaw.body[1], AACRaw)
	}
}

// ---------------------------------------------------------------------------
// HEVC video: compatibility requirement
// ---------------------------------------------------------------------------

// TestHEVCUsesCodecID12 pins the compatibility requirement that HEVC is carried
// in the legacy FLV video tag layout with codec ID 12 rather than with the
// Enhanced RTMP four character code header.
func TestHEVCUsesCodecID12(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetFlags(false, true)
	record := []byte{0x01, 0x02, 0x03}
	if err := w.WriteHEVCSequenceHeader(record); err != nil {
		t.Fatalf("WriteHEVCSequenceHeader: %v", err)
	}
	if err := w.WriteHEVCNALU([]byte{0, 0, 0, 1, 0x26}, 500, 12, true); err != nil {
		t.Fatalf("WriteHEVCNALU: %v", err)
	}
	if err := w.WriteHEVCNALU([]byte{0, 0, 0, 1, 0x02}, 517, 0, false); err != nil {
		t.Fatalf("WriteHEVCNALU: %v", err)
	}

	tags := walkTags(t, buf.Bytes())
	if len(tags) != 3 {
		t.Fatalf("tag count = %d, want 3", len(tags))
	}

	for i, tg := range tags {
		if tg.typ != TagVideo {
			t.Fatalf("tag %d type = %d, want video", i, tg.typ)
		}
		if tg.body[0]&0x80 != 0 {
			t.Errorf("tag %d sets the isExHeader bit, which codec ID 12 must not use", i)
		}
		if got := tg.body[0] & 0x0F; got != CodecHEVC {
			t.Errorf("tag %d codec ID = %d, want %d", i, got, CodecHEVC)
		}
	}

	seq := tags[0]
	if got := seq.body[0] >> 4; got != FrameKey {
		t.Errorf("sequence header frame type = %d, want keyframe", got)
	}
	if seq.body[1] != HEVCSequenceHeader {
		t.Errorf("sequence header packet type = %d, want %d", seq.body[1], HEVCSequenceHeader)
	}
	if !bytes.Equal(seq.body[5:], record) {
		t.Errorf("configuration record = % x, want % x", seq.body[5:], record)
	}

	key := tags[1]
	if got := key.body[0] >> 4; got != FrameKey {
		t.Errorf("keyframe frame type = %d, want keyframe", got)
	}
	if key.body[1] != HEVCNALU {
		t.Errorf("keyframe packet type = %d, want %d", key.body[1], HEVCNALU)
	}
	if got := ctsOf(key.body); got != 12 {
		t.Errorf("keyframe composition time = %d, want 12", got)
	}
	if key.ts != 500 {
		t.Errorf("keyframe timestamp = %d, want 500", key.ts)
	}
	if !bytes.Equal(key.body[5:], []byte{0, 0, 0, 1, 0x26}) {
		t.Errorf("keyframe payload = % x", key.body[5:])
	}

	inter := tags[2]
	if got := inter.body[0] >> 4; got != FrameInter {
		t.Errorf("inter frame type = %d, want %d", got, FrameInter)
	}
	if got := ctsOf(inter.body); got != 0 {
		t.Errorf("inter frame composition time = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Header flags
// ---------------------------------------------------------------------------

func TestHeaderFlagsReflectStreamContent(t *testing.T) {
	cases := []struct {
		name  string
		audio bool
		video bool
		want  byte
	}{
		{"audio only", true, false, headerFlagAudio},
		{"video only", false, true, headerFlagVideo},
		{"both", true, true, headerFlagAudio | headerFlagVideo},
		{"neither", false, false, 0},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		w := NewWriter(&buf)
		w.SetFlags(c.audio, c.video)
		if err := w.WriteHeader(); err != nil {
			t.Fatalf("%s: WriteHeader: %v", c.name, err)
		}
		got := buf.Bytes()[4]
		if got != c.want {
			t.Errorf("%s: flags = 0x%02x, want 0x%02x", c.name, got, c.want)
		}
	}
}

// TestHeaderFlagsMustPrecedeFirstTag covers the bug where the header was
// written by the first tag before SetFlags had any effect, leaving a stream
// that declared neither audio nor video.
func TestHeaderFlagsMustPrecedeFirstTag(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetFlags(true, true)
	if err := w.WriteHeader(); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if err := w.WriteAVCSequenceHeader([]byte{1}); err != nil {
		t.Fatalf("WriteAVCSequenceHeader: %v", err)
	}
	if err := w.WriteAACSequenceHeader([]byte{1, 2}); err != nil {
		t.Fatalf("WriteAACSequenceHeader: %v", err)
	}
	if got := buf.Bytes()[4]; got != headerFlagAudio|headerFlagVideo {
		t.Errorf("flags = 0x%02x, want 0x%02x", got, headerFlagAudio|headerFlagVideo)
	}
}

func TestSetFlagsIsIgnoredAfterHeader(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteAVCSequenceHeader([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if !w.HeaderWritten() {
		t.Error("HeaderWritten must report true once a tag has been written")
	}
	w.SetFlags(true, false) // must not rewrite the header
	if got := buf.Bytes()[4]; got != headerFlagVideo {
		t.Errorf("flags = 0x%02x, want 0x%02x", got, headerFlagVideo)
	}
}

// ---------------------------------------------------------------------------
// Timestamps and robustness
// ---------------------------------------------------------------------------

func TestTimestampExtensionBits(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetFlags(false, true)
	const ts = 0x01234567
	if err := w.WriteAVCNALU([]byte{0, 0, 0, 1, 0x41}, ts, 0, false); err != nil {
		t.Fatalf("WriteAVCNALU: %v", err)
	}
	tags := walkTags(t, buf.Bytes())
	if len(tags) != 1 {
		t.Fatalf("tag count = %d, want 1", len(tags))
	}
	if tags[0].ts != ts {
		t.Errorf("timestamp = 0x%08x, want 0x%08x", tags[0].ts, ts)
	}
}

func TestTimestampOverflow(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetFlags(false, true)
	if err := w.WriteAVCNALU([]byte{1}, MaxTimestamp+1, 0, true); err == nil {
		t.Fatal("expected an error for a timestamp beyond the FLV field")
	}
	if err := w.WriteHEVCNALU([]byte{1}, -1, 0, true); err == nil {
		t.Fatal("expected an error for a negative timestamp")
	}
	if err := w.WriteAACRaw([]byte{1}, MaxTimestamp+1); err == nil {
		t.Fatal("expected an error for a timestamp beyond the FLV field")
	}
}

func TestCompositionTimeClamping(t *testing.T) {
	const over = 0x7FFFFF + 100
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetFlags(false, true)
	if err := w.WriteAVCNALU([]byte{0, 0, 0, 1, 0x41}, 10, over, false); err != nil {
		t.Fatalf("WriteAVCNALU: %v", err)
	}
	if err := w.WriteAVCNALU([]byte{0, 0, 0, 1, 0x41}, 20, -over, false); err != nil {
		t.Fatalf("WriteAVCNALU: %v", err)
	}
	tags := walkTags(t, buf.Bytes())
	if got := ctsOf(tags[0].body); got != 0x7FFFFF {
		t.Errorf("positive composition time = %d, want %d", got, 0x7FFFFF)
	}
	if got := ctsOf(tags[1].body); got != -0x800000 {
		t.Errorf("negative composition time = %d, want %d", got, -0x800000)
	}
}

func TestEmptyPayloadsAreSkipped(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetFlags(true, true)
	if err := w.WriteAVCNALU(nil, 0, 0, true); err != nil {
		t.Fatalf("WriteAVCNALU: %v", err)
	}
	if err := w.WriteAACRaw(nil, 0); err != nil {
		t.Fatalf("WriteAACRaw: %v", err)
	}
	if err := w.WriteHEVCNALU(nil, 0, 0, true); err != nil {
		t.Fatalf("WriteHEVCNALU: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %d bytes for empty payloads, want 0", buf.Len())
	}
}

func TestBytesWrittenCountsTags(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetFlags(true, true)
	if err := w.WriteHeader(); err != nil {
		t.Fatal(err)
	}
	if got := w.BytesWritten(); got != 0 {
		t.Errorf("BytesWritten before any tag = %d, want 0", got)
	}
	if err := w.WriteAVCSequenceHeader([]byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	// 11 byte tag header + 8 byte body + 4 byte PreviousTagSize
	if got := w.BytesWritten(); got != 23 {
		t.Errorf("BytesWritten = %d, want 23", got)
	}
}
