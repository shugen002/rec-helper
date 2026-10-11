// Package mp4 parses the subset of ISO base media file format (ISO/IEC 14496-12)
// needed to remux fragmented MP4 / CMAF segments into FLV.
//
// It understands the initialisation segment (ftyp + moov) and media segments
// (styp + moof + mdat), which is exactly what HLS delivers when EXT-X-MAP is
// used. Only what the remuxer needs is decoded; everything else is skipped.
package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Box type codes.
const (
	boxFTYP = "ftyp"
	boxMOOV = "moov"
	boxMVHD = "mvhd"
	boxTRAK = "trak"
	boxTKHD = "tkhd"
	boxMDIA = "mdia"
	boxMDHD = "mdhd"
	boxHDLR = "hdlr"
	boxMINF = "minf"
	boxSTBL = "stbl"
	boxSTSD = "stsd"
	boxMVEX = "mvex"
	boxTREX = "trex"
	boxMOOF = "moof"
	boxMFHD = "mfhd"
	boxTRAF = "traf"
	boxTFHD = "tfhd"
	boxTFDT = "tfdt"
	boxTRUN = "trun"
	boxMDAT = "mdat"
	boxSTTS = "stts"
	boxCTTS = "ctts"
	boxSTSS = "stss"
	boxESDS = "esds"
	boxWVTT = "wvtt"
	boxSTPP = "stpp"
	boxTX3G = "tx3g"
	boxCENC = "cenc"
	boxSINF = "sinf"
	boxPSSH = "pssh"
)

// tfhd flags.
const (
	tfhdBaseDataOffset         = 0x000001
	tfhdSampleDescriptionIndex = 0x000002
	tfhdDefaultSampleDuration  = 0x000008
	tfhdDefaultSampleSize      = 0x000010
	tfhdDefaultSampleFlags     = 0x000020
	tfhdDefaultBaseIsMoof      = 0x020000
)

// trun flags.
const (
	trunDataOffset       = 0x000001
	trunFirstSampleFlags = 0x000004
	trunSampleDuration   = 0x000100
	trunSampleSize       = 0x000200
	trunSampleFlags      = 0x000400
	trunSampleCTO        = 0x000800
)

// Combined sample flags (ISO/IEC 14496-12 section 8.8.3.1). The field packs the
// sample dependency values in the top 16 bits and the individual flags in the
// bottom 16 bits:
//
//	bits 31-28 is_leading            bits 15-14 sample_has_redundancy
//	bits 27-26 sample_depends_on     bits 13-12 sample_padding_value
//	bits 25-24 sample_is_depended_on  bit     11-10 sample_degradation_priority
//	bit     16 sample_is_non_sync_sample
const (
	// sampleDependsOnMask isolates sample_depends_on from the flag word.
	sampleDependsOnMask = 0x03000000
	// sampleDependsOnShift shifts the masked value into place.
	sampleDependsOnShift = 24
	// sampleDependsOnNo means the sample does not depend on others, which is
	// what a keyframe reports.
	sampleDependsOnNo = 2
	// sampleDependsOnYes means the sample depends on others.
	sampleDependsOnYes = 1
	// sampleFlagIsNonSync marks a sample that is explicitly not a sync sample.
	sampleFlagIsNonSync = 0x00010000
	// sampleFlagDegradationPriority is the low 16 bit priority field.
	sampleFlagDegradationPriority = 0x0000FFFF
)

// Handler types.
const (
	HandlerVideo    = "vide"
	HandlerAudio    = "soun"
	HandlerSubtitle = "subt"
	HandlerText     = "text"
)

// ErrEncrypted reports a segment protected by CENC/Common Encryption, which
// cannot be represented in FLV.
var ErrEncrypted = errors.New("mp4: track is encrypted (sinf/pssh present)")

// Track describes one track of an initialisation segment.
type Track struct {
	ID        uint32
	Handler   string
	Timescale uint32
	Duration  uint64 // in Timescale units, 0 for live

	// Sample entry.
	Format     string // avc1, hvc1, mp4a, Opus, ...
	Width      uint16
	Height     uint16
	Channels   uint16
	SampleRate uint32
	SampleSize uint16

	// Codec configuration payloads, taken verbatim from the sample entry.
	AvcC                []byte // AVCDecoderConfigurationRecord
	HvcC                []byte // HEVCDecoderConfigurationRecord
	Av1C                []byte
	VpcC                []byte
	AudioSpecificConfig []byte // from esds DecoderSpecificInfo
	OpusDOps            []byte
	FLACStreamInfo      []byte
	CodecString         string

	Encrypted bool

	// audioObjectTypeIndication is the MPEG-4 object type from esds; the
	// audioObjectType (AAC profile) is decoded from the AudioSpecificConfig.
	audioObjectTypeIndication byte
	audioObjectType           uint8

	// trex defaults.
	trexDefaultDuration uint32
	trexDefaultSize     uint32
	trexDefaultFlags    uint32

	// Sample description index of the (single) sample entry we understand.
	sampleDescriptionIndex uint32
}

// Audio reports whether the track carries audio.
func (t *Track) Audio() bool { return t.Handler == HandlerAudio }

// Video reports whether the track carries video.
func (t *Track) Video() bool { return t.Handler == HandlerVideo }

// Info holds a parsed initialisation segment.
type Info struct {
	Tracks []*Track

	byID map[uint32]*Track
}

// TrackByID returns the track with the given ID.
func (i *Info) TrackByID(id uint32) *Track {
	if i == nil {
		return nil
	}
	return i.byID[id]
}

// RequiredTracks returns the first video and first audio track, either of which
// may be nil.
func (i *Info) RequiredTracks() (video, audio *Track) {
	for _, t := range i.Tracks {
		if video == nil && t.Video() && t.playable() {
			video = t
		}
		if audio == nil && t.Audio() && t.playable() {
			audio = t
		}
	}
	return video, audio
}

// Playable reports whether the track uses a codec FLV can carry.
func (t *Track) playable() bool {
	switch strings.ToLower(t.Format) {
	case "avc1", "avc3", "hvc1", "hev1":
		return t.AvcC != nil || t.HvcC != nil
	case "mp4a":
		return t.AudioSpecificConfig != nil
	case "opus", "flac", "fLaC":
		return true
	}
	return false
}

// TrackSupported reports whether FLV can carry this track's codec.
func (t *Track) TrackSupported() bool { return t.playable() }

// Sample is one decoded media sample.
type Sample struct {
	TrackID uint32
	Data    []byte
	// DTS is the decode timestamp in track timescale units, rebased so the
	// first parsed frame of the stream starts at 0.
	DTS int64
	// CTS is the composition time offset (PTS - DTS) in track timescale units.
	CTS int64
	// Duration in track timescale units.
	Duration uint32
	// Keyframe reports whether the sample is a sync sample.
	Keyframe bool
}

// ParseInit parses an initialisation segment (typically the EXT-X-MAP resource).
func ParseInit(data []byte) (*Info, error) {
	info := &Info{byID: map[uint32]*Track{}}
	var moov []byte
	for _, b := range splitBoxes(data, 0, len(data)) {
		switch b.typ {
		case boxMOOV:
			moov = b.body(data)
		case boxFTYP, "styp", "free", "skip", "sidx", "emsg", "prft":
			// ignored
		}
	}
	if moov == nil {
		return nil, errors.New("mp4: no moov box in initialisation segment")
	}
	if err := info.parseMoov(moov); err != nil {
		return nil, err
	}
	if len(info.Tracks) == 0 {
		return nil, errors.New("mp4: moov contains no tracks")
	}
	return info, nil
}

func (i *Info) parseMoov(moov []byte) error {
	for _, b := range splitBoxes(moov, 0, len(moov)) {
		switch b.typ {
		case boxMVHD:
			// Nothing needed; per track timescales are used instead.
		case boxTRAK:
			t, err := parseTrak(b.body(moov))
			if err != nil {
				// Skip tracks whose sample entry we do not understand rather
				// than failing the whole stream.
				continue
			}
			if t == nil {
				continue
			}
			i.Tracks = append(i.Tracks, t)
			i.byID[t.ID] = t
		case boxMVEX:
			i.parseMvex(b.body(moov))
		}
	}
	return nil
}

func (i *Info) parseMvex(mvex []byte) {
	byID := map[uint32]*Track{}
	for _, t := range i.Tracks {
		byID[t.ID] = t
	}
	for _, b := range splitBoxes(mvex, 0, len(mvex)) {
		if b.typ != boxTREX {
			continue
		}
		body := b.body(mvex)
		if len(body) < 24 {
			continue
		}
		id := binary.BigEndian.Uint32(body[4:8])
		t := byID[id]
		if t == nil {
			continue
		}
		t.trexDefaultDuration = binary.BigEndian.Uint32(body[12:16])
		t.trexDefaultSize = binary.BigEndian.Uint32(body[16:20])
		t.trexDefaultFlags = binary.BigEndian.Uint32(body[20:24])
	}
}

func parseTrak(trak []byte) (*Track, error) {
	t := &Track{}
	var mdia []byte
	for _, b := range splitBoxes(trak, 0, len(trak)) {
		switch b.typ {
		case boxTKHD:
			body := b.body(trak)
			if len(body) >= 4 {
				ver := body[0]
				if ver == 1 && len(body) >= 24 {
					t.ID = binary.BigEndian.Uint32(body[20:24])
				} else if len(body) >= 16 {
					t.ID = binary.BigEndian.Uint32(body[12:16])
				}
			}
		case boxMDIA:
			mdia = b.body(trak)
		}
	}
	if mdia == nil {
		return nil, errors.New("mp4: trak without mdia")
	}
	for _, b := range splitBoxes(mdia, 0, len(mdia)) {
		switch b.typ {
		case boxMDHD:
			body := b.body(mdia)
			if len(body) < 4 {
				break
			}
			if body[0] == 1 && len(body) >= 32 {
				t.Timescale = binary.BigEndian.Uint32(body[20:24])
				t.Duration = binary.BigEndian.Uint64(body[24:32])
			} else if len(body) >= 20 {
				t.Timescale = binary.BigEndian.Uint32(body[12:16])
				t.Duration = uint64(binary.BigEndian.Uint32(body[16:20]))
			}
		case boxHDLR:
			body := b.body(mdia)
			if len(body) >= 12 {
				t.Handler = string(body[8:12])
			}
		case boxMINF:
			if err := t.parseMinf(b.body(mdia)); err != nil {
				return nil, err
			}
		}
	}
	if t.ID == 0 {
		return nil, errors.New("mp4: trak without track ID")
	}
	if t.Timescale == 0 {
		t.Timescale = 1
	}
	return t, nil
}

func (t *Track) parseMinf(minf []byte) error {
	for _, b := range splitBoxes(minf, 0, len(minf)) {
		if b.typ != boxSTBL {
			continue
		}
		stbl := b.body(minf)
		for _, sb := range splitBoxes(stbl, 0, len(stbl)) {
			if sb.typ == boxSTSD {
				return t.parseStsd(sb, stbl)
			}
		}
	}
	return nil
}

// parseStsd walks the sample entries of an stsd box. stsd is the box itself
// (header included) and container is the buffer its offsets are relative to.
func (t *Track) parseStsd(stsd boxRef, container []byte) error {
	body := stsd.body(container)
	if len(body) < 8 {
		return errors.New("mp4: short stsd")
	}
	entries := binary.BigEndian.Uint32(body[4:8])
	// Sample entries start after the 4 byte version/flags and the 4 byte
	// entry count, i.e. 8 bytes into the stsd box body.
	base := stsd.off + stsd.size - len(body)
	off := base + 8
	for n := uint32(0); n < entries; n++ {
		size, typ, hdr, ok := readBoxHeader(container, off)
		if !ok || size < hdr {
			break
		}
		entry := container[off+hdr : off+size]
		switch typ {
		case "avc1", "avc3", "hvc1", "hev1", "av01", "vp09", "mp4v", "dvh1", "dvhe":
			t.parseVisualSampleEntry(typ, entry)
			t.sampleDescriptionIndex = n + 1
			return nil
		case "mp4a", "Opus", "fLaC", "flac", "ac-3", "ec-3", "alac", "mp3 ":
			t.parseAudioSampleEntry(typ, entry)
			t.sampleDescriptionIndex = n + 1
			return nil
		}
		off += size
	}
	return fmt.Errorf("mp4: unsupported sample entry in stsd for track %d", t.ID)
}

// parseVisualSampleEntry reads the 78 byte VisualSampleEntry then its children.
func (t *Track) parseVisualSampleEntry(typ string, body []byte) {
	const fixed = 78
	t.Format = typ
	if len(body) >= 28 {
		t.Width = binary.BigEndian.Uint16(body[24:26])
		t.Height = binary.BigEndian.Uint16(body[26:28])
	}
	if len(body) <= fixed {
		return
	}
	t.parseSampleEntryChildren(body[fixed:])
	t.buildVideoCodecString()
}

// parseAudioSampleEntry reads the 28 byte AudioSampleEntry then its children.
func (t *Track) parseAudioSampleEntry(typ string, body []byte) {
	const fixed = 28
	t.Format = typ
	if len(body) >= 28 {
		t.Channels = binary.BigEndian.Uint16(body[16:18])
		t.SampleSize = binary.BigEndian.Uint16(body[18:20])
		t.SampleRate = binary.BigEndian.Uint32(body[24:28]) >> 16
	}
	if len(body) <= fixed {
		return
	}
	children := body[fixed:]
	t.parseSampleEntryChildren(children)
	if typ == "mp4a" && t.AudioSpecificConfig == nil {
		// Some authors put the esds behind a version 1 QuickTime extension
		// (4 extra bytes); retry with that offset.
		if len(children) > 4 {
			t.parseSampleEntryChildren(children[4:])
		}
	}
	t.buildAudioCodecString()
}

func (t *Track) parseSampleEntryChildren(children []byte) {
	for _, b := range splitBoxes(children, 0, len(children)) {
		body := b.body(children)
		switch b.typ {
		case "avcC":
			t.AvcC = append([]byte(nil), body...)
		case "hvcC":
			t.HvcC = append([]byte(nil), body...)
		case "av1C":
			t.Av1C = append([]byte(nil), body...)
		case "vpcC":
			t.VpcC = append([]byte(nil), body...)
		case "dOps":
			t.OpusDOps = append([]byte(nil), body...)
		case "dfLa":
			t.FLACStreamInfo = append([]byte(nil), body...)
		case boxESDS:
			t.parseEsds(body)
		case boxSINF:
			t.Encrypted = true
		}
	}
}

// parseEsds extracts the DecoderSpecificInfo (AudioSpecificConfig) and the
// object type indication from an MPEG-4 elementary stream descriptor.
func (t *Track) parseEsds(b []byte) {
	if len(b) < 4 {
		return
	}
	i := 4 // version + flags
	for i < len(b) {
		tag := b[i]
		i++
		size := 0
		for i < len(b) {
			c := b[i]
			i++
			size = size<<7 | int(c&0x7F)
			if c&0x80 == 0 {
				break
			}
		}
		if size < 0 || i+size > len(b) {
			return
		}
		switch tag {
		case 0x03: // ES_Descriptor
			if size >= 3 {
				i += 3 // ES_ID + flags
			}
		case 0x04: // DecoderConfigDescriptor
			if size >= 13 {
				t.audioObjectTypeIndication = b[i]
				i += 13
			}
		case 0x05: // DecoderSpecificInfo
			t.AudioSpecificConfig = append([]byte(nil), b[i:i+size]...)
			i += size
		default:
			i += size
		}
	}
	t.audioObjectType = aacObjectType(t.AudioSpecificConfig)
}

// aacObjectType decodes audioObjectType from an AudioSpecificConfig.
func aacObjectType(asc []byte) uint8 {
	if len(asc) == 0 {
		return 2 // AAC-LC, by far the most common default
	}
	br := bitReader{data: asc}
	ot := uint8(br.read(5))
	if ot == 31 {
		ot = uint8(32 + br.read(6))
	}
	return ot
}

func (t *Track) buildVideoCodecString() {
	switch strings.ToLower(t.Format) {
	case "avc1", "avc3":
		if len(t.AvcC) >= 4 {
			t.CodecString = fmt.Sprintf("%s.%02x%02x%02x", t.Format, t.AvcC[1], t.AvcC[2], t.AvcC[3])
		}
	case "hvc1", "hev1":
		if len(t.HvcC) >= 13 {
			profileSpace := t.HvcC[1] >> 6
			profileIDC := t.HvcC[1] & 0x1F
			compat := binary.BigEndian.Uint32(t.HvcC[2:6])
			tier := (t.HvcC[1] >> 5) & 0x01
			levelIDC := t.HvcC[12]
			prefix := ""
			if profileSpace > 0 {
				prefix = string(rune('A' + profileSpace - 1))
			}
			t.CodecString = fmt.Sprintf("%s.%s%d.%X.%c%d", t.Format, prefix, profileIDC, reverseBits32(compat), "LH"[tier], levelIDC)
		}
	case "av01":
		if len(t.Av1C) >= 4 {
			t.CodecString = fmt.Sprintf("av01.%d.%02d%c.%02d", t.Av1C[1]>>5, t.Av1C[1]&0x1F, "MH"[t.Av1C[1]>>5&1], t.Av1C[2]>>5)
		}
	default:
		t.CodecString = t.Format
	}
}

func (t *Track) buildAudioCodecString() {
	switch strings.ToLower(t.Format) {
	case "mp4a":
		objType := t.audioObjectType
		t.CodecString = fmt.Sprintf("mp4a.%02x.%d", t.audioObjectTypeIndication, objType)
	default:
		t.CodecString = t.Format
	}
}

// reverseBits32 reverses the bit order of a 32 bit value, as required when
// rendering the HEVC general_profile_compatibility_flags.
func reverseBits32(v uint32) uint32 {
	var r uint32
	for i := 0; i < 32; i++ {
		r = r<<1 | (v & 1)
		v >>= 1
	}
	return r
}

// AudioObjectType returns the AAC audioObjectType decoded from the
// AudioSpecificConfig, or 0 when the track is not AAC.
func (t *Track) AudioObjectType() uint8 {
	if !strings.EqualFold(t.Format, "mp4a") {
		return 0
	}
	return t.audioObjectType
}

// initReader is a view over the initialisation segment for reading compact
// sample tables, used to synthesise moof-less (progressive) segments.
func readBoxHeader(b []byte, off int) (size int, typ string, hdr int, ok bool) {
	if off+8 > len(b) {
		return 0, "", 0, false
	}
	size = int(binary.BigEndian.Uint32(b[off:]))
	typ = string(b[off+4 : off+8])
	hdr = 8
	if size == 1 {
		if off+16 > len(b) {
			return 0, "", 0, false
		}
		size = int(binary.BigEndian.Uint64(b[off+8:]))
		hdr = 16
	} else if size == 0 {
		size = len(b) - off
	}
	if size < hdr || off+size > len(b) {
		return 0, "", 0, false
	}
	return size, typ, hdr, true
}

type boxRef struct {
	typ  string
	off  int
	size int
}

// splitBoxes lists the child boxes of the byte range [start, end).
func splitBoxes(b []byte, start, end int) []boxRef {
	if end > len(b) {
		end = len(b)
	}
	var out []boxRef
	off := start
	for off+8 <= end {
		size, typ, _, ok := readBoxHeader(b, off)
		if !ok {
			break
		}
		out = append(out, boxRef{typ: typ, off: off, size: size})
		off += size
	}
	return out
}

func (r boxRef) body(b []byte) []byte {
	_, _, hdr, ok := readBoxHeader(b, r.off)
	if !ok {
		return nil
	}
	return b[r.off+hdr : r.off+r.size]
}

// FindBox returns the body of the first top level box of the given type.
func FindBox(data []byte, typ string) []byte {
	for _, b := range splitBoxes(data, 0, len(data)) {
		if b.typ == typ {
			return b.body(data)
		}
	}
	return nil
}

// IsFragmented reports whether the payload looks like an ISO BMFF file.
func IsFragmented(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	switch string(data[4:8]) {
	case boxFTYP, "styp", boxMOOF, boxMOOV, boxMDAT, "sidx":
		return true
	}
	return false
}
