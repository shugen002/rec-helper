package m3u8

import (
	"bytes"
	"testing"
)

// biliPlaylist mirrors the fragmented MP4 playlist shape used by the bundled
// example stream, including unknown EXT-BILI-AUX tags that must be ignored.
const biliPlaylist = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-START:TIME-OFFSET=0
#EXT-X-MEDIA-SEQUENCE:424553031
#EXT-X-TARGETDURATION:1
#EXT-X-MAP:URI="h1791707283.m4s"
#EXT-BILI-AUX:2203147|K|101af5|480045fa
#EXTINF:1.00,101af5|480045fa
424553031.m4s
#EXT-BILI-AUX:220350e|N|fa21a|798ebf9c
#EXTINF:1.00,fa21a|798ebf9c
424553032.m4s
#EXT-BILI-AUX:22040d7|N|2884f|83513288
#EXTINF:0.17,2884f|83513288
424553033.m4s
`

func TestParseMediaPlaylist(t *testing.T) {
	pl, err := Parse([]byte(biliPlaylist))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if pl.Kind != KindMedia {
		t.Fatalf("kind = %v, want media", pl.Kind)
	}
	m := pl.Media
	if m.Version != 7 {
		t.Errorf("version = %d, want 7", m.Version)
	}
	if m.TargetDuration != 1 {
		t.Errorf("target duration = %v, want 1", m.TargetDuration)
	}
	if !m.HasMediaSequence || m.MediaSequence != 424553031 {
		t.Errorf("media sequence = %d (present %v), want 424553031", m.MediaSequence, m.HasMediaSequence)
	}
	if m.EndList {
		t.Error("a live playlist must not be marked as ended")
	}
	if m.Map == nil || m.Map.URI != "h1791707283.m4s" {
		t.Fatalf("EXT-X-MAP = %+v, want h1791707283.m4s", m.Map)
	}
	if m.Start == nil || m.Start.TimeOffset != 0 || !m.Start.TimeOffsetSet {
		t.Errorf("EXT-X-START = %+v, want a zero offset", m.Start)
	}
	if len(m.Segments) != 3 {
		t.Fatalf("segment count = %d, want 3", len(m.Segments))
	}

	want := []struct {
		uri      string
		duration float64
		title    string
		sequence uint64
	}{
		{"424553031.m4s", 1.00, "101af5|480045fa", 424553031},
		{"424553032.m4s", 1.00, "fa21a|798ebf9c", 424553032},
		{"424553033.m4s", 0.17, "2884f|83513288", 424553033},
	}
	for i, w := range want {
		got := m.Segments[i]
		if got.URI != w.uri {
			t.Errorf("segment %d URI = %q, want %q", i, got.URI, w.uri)
		}
		if got.Duration != w.duration {
			t.Errorf("segment %d duration = %v, want %v", i, got.Duration, w.duration)
		}
		if got.Title != w.title {
			t.Errorf("segment %d title = %q, want %q", i, got.Title, w.title)
		}
		if got.Sequence != w.sequence {
			t.Errorf("segment %d sequence = %d, want %d", i, got.Sequence, w.sequence)
		}
		if got.Map == nil || got.Map.URI != "h1791707283.m4s" {
			t.Errorf("segment %d does not inherit the playlist EXT-X-MAP", i)
		}
	}
}

func TestParseRejectsMissingHeader(t *testing.T) {
	if _, err := Parse([]byte("#EXTINF:1,\nseg.m4s\n")); err == nil {
		t.Fatal("expected an error when #EXTM3U is absent")
	}
}

func TestParseHandlesCRLFAndBOM(t *testing.T) {
	data := "\ufeff#EXTM3U\r\n#EXTINF:2.5,\r\nseg1.ts\r\n"
	pl, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if pl.Kind != KindMedia || len(pl.Media.Segments) != 1 {
		t.Fatalf("parsed %+v, want one media segment", pl)
	}
	if got := pl.Media.Segments[0]; got.URI != "seg1.ts" || got.Duration != 2.5 {
		t.Errorf("segment = %+v", got)
	}
}

func TestParseMasterPlaylist(t *testing.T) {
	const master = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="main",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2"
#EXT-X-STREAM-INF:BANDWIDTH=1280000,AVERAGE-BANDWIDTH=1000000,CODECS="avc1.64002a,mp4a.40.2",RESOLUTION=1920x1080,FRAME-RATE=60.000,AUDIO="aac"
high/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=256000,CODECS="avc1.42c015,mp4a.40.2"
low/index.m3u8
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=50000,CODECS="avc1.64002a",URI="iframe/index.m3u8"
#EXT-X-SESSION-DATA:DATA-ID="com.example.title",VALUE="Example",LANGUAGE="en"
`
	pl, err := Parse([]byte(master))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if pl.Kind != KindMaster {
		t.Fatalf("kind = %v, want master", pl.Kind)
	}
	if !pl.Master.IndependentSegments {
		t.Error("EXT-X-INDEPENDENT-SEGMENTS was not parsed")
	}
	if len(pl.Master.Variants) != 3 {
		t.Fatalf("variant count = %d, want 3", len(pl.Master.Variants))
	}

	v := pl.Master.Variants[0]
	if v.URI != "high/index.m3u8" {
		t.Errorf("variant 0 URI = %q", v.URI)
	}
	if v.Bandwidth != 1280000 {
		t.Errorf("variant 0 bandwidth = %d, want 1280000", v.Bandwidth)
	}
	if v.AverageBandwidth != 1000000 {
		t.Errorf("variant 0 average bandwidth = %d, want 1000000", v.AverageBandwidth)
	}
	// A comma inside a quoted attribute must not split the value.
	if v.Codecs != "avc1.64002a,mp4a.40.2" {
		t.Errorf("variant 0 codecs = %q, want the full quoted list", v.Codecs)
	}
	if v.Resolution != "1920x1080" {
		t.Errorf("variant 0 resolution = %q", v.Resolution)
	}
	if v.FrameRate != 60 {
		t.Errorf("variant 0 frame rate = %v, want 60", v.FrameRate)
	}
	if v.Audio != "aac" {
		t.Errorf("variant 0 audio group = %q, want aac", v.Audio)
	}
	if pl.Master.Variants[1].URI != "low/index.m3u8" {
		t.Errorf("variant 1 URI = %q", pl.Master.Variants[1].URI)
	}
	if !pl.Master.Variants[2].IFrameOnly || pl.Master.Variants[2].URI != "iframe/index.m3u8" {
		t.Errorf("variant 2 = %+v, want an iframe-only variant", pl.Master.Variants[2])
	}

	if len(pl.Master.Renditions) != 1 {
		t.Fatalf("rendition count = %d, want 1", len(pl.Master.Renditions))
	}
	r := pl.Master.Renditions[0]
	if r.Type != "AUDIO" || r.GroupID != "aac" || r.Name != "main" {
		t.Errorf("rendition = %+v", r)
	}
	if !r.Default || !r.Autoselect || r.Forced {
		t.Errorf("rendition flags = %+v, want default and autoselect", r)
	}
	if r.Channels != "2" {
		t.Errorf("rendition channels = %q, want 2", r.Channels)
	}

	if len(pl.Master.SessionData) != 1 {
		t.Fatalf("session data count = %d, want 1", len(pl.Master.SessionData))
	}
	if sd := pl.Master.SessionData[0]; sd.DataID != "com.example.title" || sd.Value != "Example" || sd.Language != "en" {
		t.Errorf("session data = %+v", sd)
	}
}

func TestParseByteRange(t *testing.T) {
	const pl = `#EXTM3U
#EXT-X-VERSION:4
#EXT-X-MAP:URI="init.mp4",BYTERANGE="720@0"
#EXTINF:4.0,
#EXT-X-BYTERANGE:1000@720
segment.ts
#EXTINF:4.0,
#EXT-X-BYTERANGE:2000
segment.ts
`
	got, err := Parse([]byte(pl))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := got.Media
	if m.Map == nil || m.Map.ByteRange != 720 || m.Map.ByteRangeOffset != 0 {
		t.Fatalf("map byte range = %+v, want 720@0", m.Map)
	}
	if len(m.Segments) != 2 {
		t.Fatalf("segment count = %d, want 2", len(m.Segments))
	}
	if s := m.Segments[0]; s.ByteRange != 1000 || s.ByteRangeOffset != 720 {
		t.Errorf("segment 0 byte range = %d@%d, want 1000@720", s.ByteRange, s.ByteRangeOffset)
	}
	// An EXT-X-BYTERANGE without an offset continues after the previous range
	// for the same resource.
	if s := m.Segments[1]; s.ByteRange != 2000 || s.ByteRangeOffset != 1720 {
		t.Errorf("segment 1 byte range = %d@%d, want 2000@1720", s.ByteRange, s.ByteRangeOffset)
	}
}

func TestParseEncryption(t *testing.T) {
	const pl = `#EXTM3U
#EXT-X-MEDIA-SEQUENCE:5
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x000102030405060708090A0B0C0D0E0F
#EXTINF:4.0,
seg0.ts
#EXT-X-KEY:METHOD=NONE
#EXTINF:4.0,
seg1.ts
`
	got, err := Parse([]byte(pl))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := got.Media
	if !m.Segments[0].Key.Encrypted() {
		t.Error("segment 0 should be encrypted")
	}
	if m.Segments[0].Key.Method != "AES-128" {
		t.Errorf("segment 0 method = %q", m.Segments[0].Key.Method)
	}
	wantIV := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	if !bytes.Equal(m.Segments[0].Key.IV, wantIV) {
		t.Errorf("segment 0 IV = % x, want % x", m.Segments[0].Key.IV, wantIV)
	}
	if m.Segments[1].Key.Encrypted() {
		t.Error("segment 1 should be clear after METHOD=NONE")
	}
}

func TestEncryptionDerivesIVFromMediaSequence(t *testing.T) {
	// RFC 8216 section 5.2: without an explicit IV the media sequence number is
	// used as a big endian 128 bit value.
	e := Encryption{Method: "AES-128", URI: "key.bin"}
	iv := e.Key(0x0102030405060708)
	want := []byte{0, 0, 0, 0, 0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8}
	if !bytes.Equal(iv, want) {
		t.Errorf("derived IV = % x, want % x", iv, want)
	}
	if got := e.Key(0); !bytes.Equal(got, make([]byte, 16)) {
		t.Errorf("sequence 0 derived IV = % x, want all zeroes", got)
	}

	// An explicit IV must take precedence over the derived one.
	explicit := Encryption{Method: "AES-128", IV: want}
	if got := explicit.Key(99); !bytes.Equal(got, want) {
		t.Errorf("explicit IV = % x, want % x", got, want)
	}
}

func TestParseDiscontinuityAndProgramDateTime(t *testing.T) {
	const pl = `#EXTM3U
#EXT-X-MEDIA-SEQUENCE:10
#EXTINF:4.0,
a.ts
#EXT-X-DISCONTINUITY
#EXT-X-PROGRAM-DATE-TIME:2024-01-02T03:04:05.500Z
#EXTINF:4.0,
b.ts
#EXT-X-ENDLIST
`
	got, err := Parse([]byte(pl))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := got.Media
	if !m.EndList {
		t.Error("EXT-X-ENDLIST was not parsed")
	}
	if m.Segments[0].Discontinuity {
		t.Error("segment 0 must not be marked discontinuous")
	}
	if !m.Segments[1].Discontinuity {
		t.Error("segment 1 must be marked discontinuous")
	}
	if m.Segments[1].ProgramDateTime == 0 {
		t.Error("segment 1 must carry a program date time")
	}
	if m.Segments[0].ProgramDateTime != 0 {
		t.Error("segment 0 must not carry a program date time")
	}
}

func TestParseDefaultIVIsNotShared(t *testing.T) {
	// Deriving an IV must not mutate the stored encryption state.
	e := Encryption{Method: "AES-128"}
	first := e.Key(1)
	second := e.Key(2)
	if bytes.Equal(first, second) {
		t.Error("different media sequences must derive different IVs")
	}
	if len(e.IV) != 0 {
		t.Error("deriving an IV must not populate the explicit IV field")
	}
}

func TestKindString(t *testing.T) {
	for _, c := range []struct {
		kind Kind
		want string
	}{
		{KindMedia, "media"},
		{KindMaster, "master"},
		{KindUnknown, "unknown"},
	} {
		if got := c.kind.String(); got != c.want {
			t.Errorf("Kind(%d).String() = %q, want %q", c.kind, got, c.want)
		}
	}
}
