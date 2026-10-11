package mp4

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// ---------------------------------------------------------------------------
// Box construction helpers. Tests build byte exact ISO BMFF structures so the
// parser is exercised without depending on any captured media.
// ---------------------------------------------------------------------------

// box builds a box with a 32 bit size and the four character type.
func box(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:8], typ)
	copy(out[8:], body)
	return out
}

// full builds a box whose body starts with a version and flags word.
func full(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	vf := make([]byte, 4)
	vf[0] = version
	vf[1] = byte(flags >> 16)
	vf[2] = byte(flags >> 8)
	vf[3] = byte(flags)
	return box(typ, append([][]byte{vf}, payload...)...)
}

// u16, u32 and u64 build big endian integers.
func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

// visualSampleEntry builds an avc1 sample entry with an avcC child.
func visualSampleEntry(typ string, width, height uint16, avcC []byte) []byte {
	body := make([]byte, 78)
	binary.BigEndian.PutUint16(body[24:26], width)
	binary.BigEndian.PutUint16(body[26:28], height)
	return box(typ, body, box("avcC", avcC))
}

// audioSampleEntry builds an mp4a sample entry with an esds child.
func audioSampleEntry(channels uint16, sampleRate uint32, asc []byte) []byte {
	body := make([]byte, 28)
	binary.BigEndian.PutUint16(body[16:18], channels)
	binary.BigEndian.PutUint16(body[18:20], 16)
	binary.BigEndian.PutUint32(body[24:28], sampleRate<<16)
	return box("mp4a", body, box("esds", esdsBody(0x40, asc)))
}

// esdsBody builds an MPEG-4 elementary stream descriptor with the given
// objectTypeIndication and DecoderSpecificInfo.
func esdsBody(objType byte, asc []byte) []byte {
	dcd := append([]byte{0x04, 0x80, 0x80, 0x80, 0x0d + byte(len(asc)), objType}, make([]byte, 12)...)
	dcd = append(dcd, 0x05, 0x80, 0x80, 0x80, byte(len(asc)))
	dcd = append(dcd, asc...)
	slen := 3 + len(dcd)
	es := append([]byte{0x03, 0x80, 0x80, 0x80, byte(slen), 0x00, 0x01, 0x00}, dcd...)
	vf := []byte{0x00, 0x00, 0x00, 0x00}
	return append(vf, es...)
}

// track builds a trak box with the given handler, timescale and sample entry.
func track(id uint32, handler string, timescale uint32, sampleEntry []byte) []byte {
	tkhdBody := make([]byte, 80)
	tkhdBody[0] = 0 // version 0
	// track ID sits at offset 12 of the body (after version/flags and times).
	binary.BigEndian.PutUint32(tkhdBody[12:16], id)
	tkhd := box("tkhd", tkhdBody)

	mdhdBody := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhdBody[12:16], timescale)
	hdlrBody := make([]byte, 12)
	copy(hdlrBody[8:12], handler)
	stsd := full("stsd", 0, 0, u32(1), sampleEntry)
	stbl := box("stbl", stsd)
	minf := box("minf", box("dinf", full("dref", 0, 0, u32(1), full("url ", 0, 1))), stbl)
	mdia := box("mdia", box("mdhd", mdhdBody), box("hdlr", hdlrBody), minf)
	return box("trak", tkhd, mdia)
}

// initSegment builds a complete ftyp + moov initialisation segment.
func initSegment() []byte {
	video := track(2, "vide", 90000, visualSampleEntry("avc1", 1920, 1080,
		[]byte{0x01, 0x64, 0x00, 0x2A, 0xFF, 0xE1, 0x00, 0x1E}))
	audio := track(1, "soun", 48000, audioSampleEntry(2, 48000, []byte{0x11, 0x90}))
	trex1 := full("trex", 0, 0, u32(1), u32(1), u32(1024), u32(0), u32(0))
	trex2 := full("trex", 0, 0, u32(2), u32(1), u32(0), u32(0), u32(0x01010000))
	mvex := box("mvex", full("mehd", 0, 0, u32(0)), trex1, trex2)
	ftyp := box("ftyp", []byte("isom"), u32(1), []byte("isomavc1mp42"))
	return append(ftyp, box("moov", full("mvhd", 0, 0, make([]byte, 96)), video, audio, mvex)...)
}

// ---------------------------------------------------------------------------
// Initialisation segment
// ---------------------------------------------------------------------------

func TestParseInitTracks(t *testing.T) {
	info, err := ParseInit(initSegment())
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	if len(info.Tracks) != 2 {
		t.Fatalf("track count = %d, want 2", len(info.Tracks))
	}

	if got := info.TrackByID(1); got == nil {
		t.Fatal("track 1 is missing")
	} else {
		if !got.Audio() {
			t.Errorf("track 1 handler = %q, want audio", got.Handler)
		}
		if got.Timescale != 48000 {
			t.Errorf("track 1 timescale = %d, want 48000", got.Timescale)
		}
		if got.Format != "mp4a" {
			t.Errorf("track 1 format = %q, want mp4a", got.Format)
		}
		if got.Channels != 2 {
			t.Errorf("track 1 channels = %d, want 2", got.Channels)
		}
		if got.SampleRate != 48000 {
			t.Errorf("track 1 sample rate = %d, want 48000", got.SampleRate)
		}
		if !bytes.Equal(got.AudioSpecificConfig, []byte{0x11, 0x90}) {
			t.Errorf("track 1 AudioSpecificConfig = % x, want 11 90", got.AudioSpecificConfig)
		}
		if got.CodecString != "mp4a.40.2" {
			t.Errorf("track 1 codec string = %q, want mp4a.40.2", got.CodecString)
		}
		if got.AudioObjectType() != 2 {
			t.Errorf("track 1 audio object type = %d, want 2", got.AudioObjectType())
		}
		if !got.TrackSupported() {
			t.Error("track 1 should be supported")
		}
	}

	if got := info.TrackByID(2); got == nil {
		t.Fatal("track 2 is missing")
	} else {
		if !got.Video() {
			t.Errorf("track 2 handler = %q, want video", got.Handler)
		}
		if got.Timescale != 90000 {
			t.Errorf("track 2 timescale = %d, want 90000", got.Timescale)
		}
		if got.Format != "avc1" {
			t.Errorf("track 2 format = %q, want avc1", got.Format)
		}
		if got.Width != 1920 || got.Height != 1080 {
			t.Errorf("track 2 size = %dx%d, want 1920x1080", got.Width, got.Height)
		}
		if got.CodecString != "avc1.64002a" {
			t.Errorf("track 2 codec string = %q, want avc1.64002a", got.CodecString)
		}
		if !got.TrackSupported() {
			t.Error("track 2 should be supported")
		}
	}

	video, audio := info.RequiredTracks()
	if video == nil || video.ID != 2 {
		t.Errorf("required video track = %v, want track 2", video)
	}
	if audio == nil || audio.ID != 1 {
		t.Errorf("required audio track = %v, want track 1", audio)
	}
}

func TestParseInitRejectsGarbage(t *testing.T) {
	if _, err := ParseInit([]byte("not an mp4 file at all")); err == nil {
		t.Fatal("expected an error for a non ISO BMFF payload")
	}
	if _, err := ParseInit(box("ftyp", []byte("isom"))); err == nil {
		t.Fatal("expected an error when moov is absent")
	}
}

func TestParseInitHEVC(t *testing.T) {
	hvcC := []byte{0x01, 0x01, 0x60, 0x00, 0x00, 0x00, 0x90, 0x00, 0x00, 0x00, 0x00, 0x00, 0x5A}
	entry := box("hvc1", make([]byte, 78), box("hvcC", hvcC))
	video := track(1, "vide", 90000, entry)
	data := append(box("ftyp", []byte("isom")), box("moov", video)...)

	info, err := ParseInit(data)
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	tr := info.TrackByID(1)
	if tr == nil {
		t.Fatal("track 1 is missing")
	}
	if tr.Format != "hvc1" {
		t.Errorf("format = %q, want hvc1", tr.Format)
	}
	if !bytes.Equal(tr.HvcC, hvcC) {
		t.Errorf("HvcC = % x, want % x", tr.HvcC, hvcC)
	}
	if !tr.TrackSupported() {
		t.Error("HEVC should be supported")
	}
}

func TestParseInitSkipsUnsupportedTrack(t *testing.T) {
	// A tx3g subtitle track must be ignored, not fatal.
	sub := track(3, "subt", 1000, box("tx3g", make([]byte, 20)))
	video := track(1, "vide", 90000, visualSampleEntry("avc1", 640, 360, []byte{1, 66, 0, 30}))
	data := append(box("ftyp", []byte("isom")), box("moov", sub, video)...)

	info, err := ParseInit(data)
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	if len(info.Tracks) != 1 || info.Tracks[0].ID != 1 {
		t.Fatalf("tracks = %v, want only the video track", info.Tracks)
	}
}

// ---------------------------------------------------------------------------
// Media segments
// ---------------------------------------------------------------------------

// fragment builds one moof + mdat pair for a single track.
//
// The tfhd uses default-base-is-moof, so the trun data offset is relative to
// the start of the moof. Durations are absent from the trun, so the tfhd
// default duration must be used, and every sample carries a size and a
// composition time offset.
func fragment(trackID uint32, samples [][]byte, baseDecodeTime uint64, defaultDuration uint32, firstFlags uint32) []byte {
	var payload, sampleTable []byte
	for _, s := range samples {
		payload = append(payload, s...)
		sampleTable = append(sampleTable, u32(uint32(len(s)))...)
		sampleTable = append(sampleTable, u32(0)...) // composition time offset
	}

	const (
		tfhdSize = 8 + 4 + 4 + 4 // header + flags + track ID + default duration
		tfdtSize = 8 + 4 + 8     // header + version/flags + v1 base decode time
	)
	trunSize := 8 + 4 + 4 + 4 + 4 + len(sampleTable)
	moofSize := (8 + 4 + 4) + // mfhd: header + version/flags + sequence number
		(8 + tfhdSize + tfdtSize + trunSize) + // traf and its children
		8 // moof header
	// Samples live in the mdat payload, which starts 8 bytes into the mdat.
	dataOffset := moofSize + 8

	tfhdBody := append(u32(trackID), u32(defaultDuration)...)
	tfhd := full("tfhd", 0, tfhdDefaultSampleDuration|tfhdDefaultBaseIsMoof, tfhdBody)

	tfdt := full("tfdt", 1, 0, u64(baseDecodeTime))

	trunFlags := uint32(trunDataOffset | trunFirstSampleFlags | trunSampleSize | trunSampleCTO)
	trunBody := append(u32(uint32(len(samples))), u32(uint32(dataOffset))...)
	trunBody = append(trunBody, u32(firstFlags)...)
	trunBody = append(trunBody, sampleTable...)
	trun := full("trun", 0, trunFlags, trunBody)

	traf := box("traf", tfhd, tfdt, trun)
	moof := box("moof", full("mfhd", 0, 0, u32(1)), traf)

	if len(moof) != moofSize {
		panic("fragment: moof size calculation is wrong")
	}
	return append(moof, box("mdat", payload)...)
}

func patchU32(dst []byte, v uint32) {
	binary.BigEndian.PutUint32(dst, v)
}

func TestParseFragmentSamples(t *testing.T) {
	info, err := ParseInit(initSegment())
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}

	videoSamples := [][]byte{{0xAA, 0xBB}, {0xCC}, {0xDD, 0xEE, 0xFF}}
	// 3000 ticks at 90000 Hz is exactly 33 ms; samples 2 and 3 repeat it.
	const defaultDuration = 3000
	// first sample flags: depends_on=2 (IDR) and not non-sync.
	const firstFlags = 0x02000000

	seg := fragment(2, videoSamples, 900000, defaultDuration, firstFlags)
	frag, err := ParseFragment(info, seg, FragmentOptions{})
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if len(frag.Samples) != len(videoSamples) {
		t.Fatalf("sample count = %d, want %d", len(frag.Samples), len(videoSamples))
	}
	for i, want := range videoSamples {
		got := frag.Samples[i]
		if got.TrackID != 2 {
			t.Errorf("sample %d track = %d, want 2", i, got.TrackID)
		}
		if !bytes.Equal(got.Data, want) {
			t.Errorf("sample %d data = % x, want % x", i, got.Data, want)
		}
		if got.Duration != defaultDuration {
			t.Errorf("sample %d duration = %d, want %d", i, got.Duration, defaultDuration)
		}
		if wantDTS := int64(900000 + i*defaultDuration); got.DTS != wantDTS {
			t.Errorf("sample %d DTS = %d, want %d", i, got.DTS, wantDTS)
		}
		if !got.Keyframe {
			t.Errorf("sample %d should be a keyframe", i)
		}
	}
}

func TestParseFragmentSyncFlagOverrides(t *testing.T) {
	info, err := ParseInit(initSegment())
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	seg := fragment(2, [][]byte{{1}, {2}}, 0, 3000, 0x02000000)
	frag, err := ParseFragment(info, seg, FragmentOptions{})
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if !frag.Samples[0].Keyframe {
		t.Error("the first sample must be a keyframe")
	}

	cases := []struct {
		name  string
		flags uint32
		want  bool
	}{
		{"absent flags", 0, true},
		{"depends_on=2 (IDR)", 0x02000000, true},
		{"depends_on=1", 0x01000000, false},
		{"non sync flag", 0x01010000, false},
		{"depends_on=2 with leading", 0x06000000, true},
		{"degradation priority only", 0x00000005, true},
	}
	for _, c := range cases {
		if got := isSync(c.flags); got != c.want {
			t.Errorf("%s: isSync(0x%08x) = %v, want %v", c.name, c.flags, got, c.want)
		}
	}
}

func TestParseFragmentAdjustHook(t *testing.T) {
	info, _ := ParseInit(initSegment())
	seg := fragment(2, [][]byte{{1}, {2}}, 1000, 3000, 0)
	frag, err := ParseFragment(info, seg, FragmentOptions{
		Adjust: func(trackID uint32, dts int64) int64 { return dts - 1000 },
	})
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if frag.Samples[0].DTS != 0 || frag.Samples[1].DTS != 3000 {
		t.Errorf("adjusted DTS = %d, %d, want 0, 3000", frag.Samples[0].DTS, frag.Samples[1].DTS)
	}
}

func TestParseFragmentOffsetOption(t *testing.T) {
	info, _ := ParseInit(initSegment())
	seg := fragment(2, [][]byte{{1, 2, 3}}, 0, 3000, 0)
	// An absolute offset must be handled; sample extraction is unaffected.
	if _, err := ParseFragment(info, seg, FragmentOptions{Offset: 4096}); err != nil {
		t.Fatalf("ParseFragment with offset: %v", err)
	}
}

func TestParseFragmentRejectsNonFragment(t *testing.T) {
	info, _ := ParseInit(initSegment())
	if _, err := ParseFragment(info, box("ftyp", []byte("isom")), FragmentOptions{}); err == nil {
		t.Fatal("expected an error for a segment without a moof box")
	}
}

func TestParseFragmentRejectsOutOfRangeSample(t *testing.T) {
	info, _ := ParseInit(initSegment())
	seg := fragment(2, [][]byte{{1, 2, 3}}, 0, 3000, 0)

	// Corrupt the single sample size in the trun so it exceeds the mdat.
	// trun box layout from its own start: size(4) type(4) version/flags(4)
	// count(4) dataOffset(4) firstSampleFlags(4) sampleSize(4) cts(4)
	idx := bytes.Index(seg, []byte("trun"))
	if idx < 0 {
		t.Fatal("trun not found")
	}
	sizeAt := idx - 4 + 8 + 4 + 4 + 4 + 4
	patchU32(seg[sizeAt:sizeAt+4], 1<<20)

	if _, err := ParseFragment(info, seg, FragmentOptions{}); err == nil {
		t.Fatal("expected an error for a sample outside the mdat")
	}
}

func TestParseFragmentMultipleSamplesAcrossTracks(t *testing.T) {
	info, _ := ParseInit(initSegment())
	// Two separate moof+mdat pairs, one per track.
	a := fragment(1, [][]byte{{0x11}, {0x22}}, 0, 1024, 0)
	b := fragment(2, [][]byte{{0x33}}, 0, 3000, 0)
	seg := append(a, b...)

	frag, err := ParseFragment(info, seg, FragmentOptions{})
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if len(frag.Samples) != 3 {
		t.Fatalf("sample count = %d, want 3", len(frag.Samples))
	}
	if frag.Samples[0].TrackID != 1 || frag.Samples[2].TrackID != 2 {
		t.Errorf("track IDs = %d, %d, %d; want 1, 1, 2",
			frag.Samples[0].TrackID, frag.Samples[1].TrackID, frag.Samples[2].TrackID)
	}
}

func TestIsFragmented(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"ftyp", box("ftyp", []byte("isom")), true},
		{"moof", box("moof"), true},
		{"styp", box("styp", []byte("msdh")), true},
		{"short", []byte{1, 2, 3}, false},
		{"other", box("free"), false},
	}
	for _, c := range cases {
		if got := IsFragmented(c.data); got != c.want {
			t.Errorf("%s: IsFragmented = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFindBox(t *testing.T) {
	data := box("ftyp", []byte("isom"))
	if got := FindBox(data, "ftyp"); string(got[:4]) != "isom" {
		t.Errorf("FindBox(ftyp) = %q", got)
	}
	if got := FindBox(data, "moov"); got != nil {
		t.Errorf("FindBox(moov) = % x, want nil", got)
	}
}
