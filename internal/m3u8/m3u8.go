// Package m3u8 implements a parser for HLS playlists as described by
// RFC 8216 (HTTP Live Streaming).
//
// Both master playlists (variant streams) and media playlists (segments)
// are supported, along with the fMP4/CMAF specific tags used by the
// example stream: EXT-X-MAP and EXT-X-BYTERANGE.
package m3u8

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Kind distinguishes the two playlist flavours.
type Kind int

const (
	// KindMedia is a playlist that lists media segments.
	KindMedia Kind = iota
	// KindMaster is a playlist that lists variant streams.
	KindMaster
	// KindUnknown is returned when neither EXTINF nor EXT-X-STREAM-INF is present.
	KindUnknown
)

func (k Kind) String() string {
	switch k {
	case KindMedia:
		return "media"
	case KindMaster:
		return "master"
	default:
		return "unknown"
	}
}

// Encryption holds the EXT-X-KEY state that applies to a segment.
type Encryption struct {
	Method        string // NONE, AES-128, SAMPLE-AES
	URI           string
	IV            []byte
	KeyFormat     string
	KeyFormatVers string
}

// Encrypted reports whether the segment is encrypted with a supported method.
func (e Encryption) Encrypted() bool {
	return e.Method != "" && e.Method != "NONE"
}

// Key returns the effective initialisation vector.
//
// When EXT-X-KEY omits IV, RFC 8216 section 5.2 says the media sequence
// number of the segment is used as a big endian 128 bit value.
func (e Encryption) Key(mediaSequence uint64) []byte {
	if len(e.IV) > 0 {
		return e.IV
	}
	iv := make([]byte, 16)
	for i := 0; i < 8; i++ {
		iv[15-i] = byte(mediaSequence >> (8 * i))
	}
	return iv
}

// MediaSegment is a single entry of a media playlist.
type MediaSegment struct {
	// URI is the segment URI exactly as written in the playlist.
	URI string
	// Duration is the EXTINF duration in seconds.
	Duration float64
	// Title is the optional EXTINF title.
	Title string
	// Sequence is the EXT-X-MEDIA-SEQUENCE number of this segment.
	Sequence uint64
	// ByteRange is the EXT-X-BYTERANGE length, 0 when the whole resource is used.
	ByteRange int64
	// ByteRangeOffset is the resolved absolute offset of the byte range.
	ByteRangeOffset int64
	// Discontinuity marks an EXT-X-DISCONTINUITY before this segment.
	Discontinuity bool
	// ProgramDateTime is the EXT-X-PROGRAM-DATE-TIME in Unix nanoseconds, 0 if absent.
	ProgramDateTime int64
	// Map is the initialisation segment that applies to this segment.
	Map *InitSegment
	// Key is the encryption that applies to this segment.
	Key Encryption
}

// InitSegment describes an EXT-X-MAP initialisation segment.
type InitSegment struct {
	URI             string
	ByteRange       int64
	ByteRangeOffset int64
}

// MediaPlaylist is a parsed media playlist.
type MediaPlaylist struct {
	Kind Kind

	Version                  int
	TargetDuration           float64
	MediaSequence            uint64
	DiscontinuitySequence    uint64
	HasMediaSequence         bool
	HasDiscontinuitySequence bool
	EndList                  bool
	IndependentSegments      bool
	Type                     string // EXT-X-PLAYLIST-TYPE: VOD or EVENT
	Start                    *StartTag

	// Map is the playlist level EXT-X-MAP (applies to all segments unless overridden).
	Map *InitSegment
	// Key is the playlist level EXT-X-KEY (applies to all segments unless overridden).
	Key Encryption

	Segments []MediaSegment
}

// StartTag is the EXT-X-START tag.
type StartTag struct {
	TimeOffset    float64
	Precise       bool
	TimeOffsetSet bool
}

// Variant is one EXT-X-STREAM-INF entry of a master playlist.
type Variant struct {
	URI              string
	Bandwidth        int
	AverageBandwidth int
	Codecs           string
	Resolution       string
	FrameRate        float64
	HDCPLevel        string
	Audio            string
	Video            string
	Subtitles        string
	ClosedCaptions   string
	// IFrameOnly marks EXT-X-I-FRAME-STREAM-INF entries.
	IFrameOnly bool
}

// Rendition is an EXT-X-MEDIA entry of a master playlist.
type Rendition struct {
	Type            string
	GroupID         string
	Name            string
	Language        string
	AssocLanguage   string
	Default         bool
	Autoselect      bool
	Forced          bool
	URI             string
	Characteristics string
	Channels        string
}

// MasterPlaylist is a parsed master playlist.
type MasterPlaylist struct {
	Version             int
	IndependentSegments bool
	Variants            []Variant
	Renditions          []Rendition
	SessionData         []SessionData
	Start               *StartTag
}

// SessionData is an EXT-X-SESSION-DATA entry.
type SessionData struct {
	DataID   string
	Value    string
	URI      string
	Language string
}

// Playlist is the result of parsing: exactly one of Media or Master is non-nil
// once Kind is known.
type Playlist struct {
	Kind   Kind
	Media  *MediaPlaylist
	Master *MasterPlaylist
}

// Parse parses playlist text. baseSequence seeds segment numbering when the
// playlist is reloaded and EXT-X-MEDIA-SEQUENCE is missing; pass 0 on first parse.
func Parse(data []byte) (*Playlist, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	lines := strings.Split(text, "\n")
	if len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "#EXTM3U") {
		return nil, errors.New("m3u8: missing #EXTM3U header")
	}

	media := &MediaPlaylist{}
	master := &MasterPlaylist{}
	cur := media

	var (
		sawExtinf    bool
		sawStreamInf bool
		sawMasterTag bool

		pendingDuration        float64
		pendingTitle           string
		pendingByteRange       int64
		pendingByteRangeOffset int64
		byteRangeOffsetSet     bool
		pendingDiscontinuity   bool
		pendingPDT             int64
		pendingMap             *InitSegment
		key                    Encryption
		keySet                 bool
		pendingVariant         *Variant

		nextByteRangeOffset int64
		lastByteRangeURI    string
		sequenceCounter     uint64
	)

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			uri := line
			switch {
			case pendingVariant != nil:
				pendingVariant.URI = uri
				master.Variants = append(master.Variants, *pendingVariant)
				pendingVariant = nil
			default:
				seg := MediaSegment{
					URI:             uri,
					Duration:        pendingDuration,
					Title:           pendingTitle,
					Sequence:        sequenceCounter,
					ByteRange:       pendingByteRange,
					Discontinuity:   pendingDiscontinuity,
					ProgramDateTime: pendingPDT,
					Map:             pendingMap,
					Key:             key,
				}
				if pendingByteRange > 0 {
					switch {
					case byteRangeOffsetSet:
						seg.ByteRangeOffset = pendingByteRangeOffset
					case lastByteRangeURI == uri && nextByteRangeOffset > 0:
						// A range without an offset continues directly after
						// the previous range of the same resource.
						seg.ByteRangeOffset = nextByteRangeOffset
					default:
						seg.ByteRangeOffset = 0
					}
					nextByteRangeOffset = seg.ByteRangeOffset + pendingByteRange
					lastByteRangeURI = uri
				} else {
					nextByteRangeOffset = 0
					lastByteRangeURI = ""
				}
				media.Segments = append(media.Segments, seg)
				sequenceCounter++
			}
			pendingDuration = 0
			pendingTitle = ""
			pendingByteRange = 0
			pendingByteRangeOffset = 0
			byteRangeOffsetSet = false
			pendingDiscontinuity = false
			pendingPDT = 0
			continue
		}

		// Split "#TAG:value".
		tag := line
		value := ""
		if i := strings.IndexByte(line, ':'); i >= 0 {
			tag = line[:i]
			value = line[i+1:]
		}

		switch tag {
		case "#EXTM3U":
			// already validated

		case "#EXT-X-VERSION":
			v, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("m3u8: bad EXT-X-VERSION %q: %w", value, err)
			}
			media.Version, master.Version = v, v

		case "#EXT-X-TARGETDURATION":
			f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				return nil, fmt.Errorf("m3u8: bad EXT-X-TARGETDURATION %q: %w", value, err)
			}
			media.TargetDuration = f

		case "#EXT-X-MEDIA-SEQUENCE":
			n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("m3u8: bad EXT-X-MEDIA-SEQUENCE %q: %w", value, err)
			}
			media.MediaSequence = n
			media.HasMediaSequence = true
			sequenceCounter = n

		case "#EXT-X-DISCONTINUITY-SEQUENCE":
			n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			if err == nil {
				media.DiscontinuitySequence = n
				media.HasDiscontinuitySequence = true
			}

		case "#EXT-X-ENDLIST":
			media.EndList = true

		case "#EXT-X-INDEPENDENT-SEGMENTS":
			media.IndependentSegments = true
			master.IndependentSegments = true

		case "#EXT-X-PLAYLIST-TYPE":
			media.Type = strings.TrimSpace(value)

		case "#EXT-X-DISCONTINUITY":
			pendingDiscontinuity = true

		case "#EXT-X-PROGRAM-DATE-TIME":
			if t, err := parseProgramDateTime(strings.TrimSpace(value)); err == nil {
				pendingPDT = t
			}

		case "#EXT-X-BYTERANGE":
			n, off, err := parseByteRange(value)
			if err != nil {
				return nil, err
			}
			pendingByteRange = n
			byteRangeOffsetSet = false
			if off >= 0 {
				pendingByteRangeOffset = off
				byteRangeOffsetSet = true
			}

		case "#EXT-X-MAP":
			attrs := parseAttributes(value)
			m := &InitSegment{URI: attrs["URI"]}
			if br := attrs["BYTERANGE"]; br != "" {
				n, off, err := parseByteRange(br)
				if err != nil {
					return nil, err
				}
				m.ByteRange = n
				if off >= 0 {
					m.ByteRangeOffset = off
				}
			}
			if m.URI == "" {
				return nil, errors.New("m3u8: EXT-X-MAP without URI")
			}
			media.Map = m
			pendingMap = m

		case "#EXT-X-KEY":
			attrs := parseAttributes(value)
			k := Encryption{
				Method:        strings.ToUpper(attrs["METHOD"]),
				URI:           attrs["URI"],
				KeyFormat:     attrs["KEYFORMAT"],
				KeyFormatVers: attrs["KEYFORMATVERSIONS"],
			}
			if iv := attrs["IV"]; iv != "" {
				b, err := parseHexIV(iv)
				if err != nil {
					return nil, err
				}
				k.IV = b
			}
			key = k
			keySet = true
			media.Key = k

		case "#EXTINF":
			d, title := parseExtinf(value)
			pendingDuration = d
			pendingTitle = title
			sawExtinf = true

		case "#EXT-X-STREAM-INF":
			attrs := parseAttributes(value)
			v := Variant{
				Bandwidth:        atoiDefault(attrs["BANDWIDTH"], 0),
				AverageBandwidth: atoiDefault(attrs["AVERAGE-BANDWIDTH"], 0),
				Codecs:           attrs["CODECS"],
				Resolution:       attrs["RESOLUTION"],
				FrameRate:        atofDefault(attrs["FRAME-RATE"], 0),
				HDCPLevel:        attrs["HDCP-LEVEL"],
				Audio:            attrs["AUDIO"],
				Video:            attrs["VIDEO"],
				Subtitles:        attrs["SUBTITLES"],
				ClosedCaptions:   attrs["CLOSED-CAPTIONS"],
			}
			pendingVariant = &v
			sawStreamInf = true
			sawMasterTag = true

		case "#EXT-X-I-FRAME-STREAM-INF":
			attrs := parseAttributes(value)
			v := Variant{
				Bandwidth:  atoiDefault(attrs["BANDWIDTH"], 0),
				Codecs:     attrs["CODECS"],
				Resolution: attrs["RESOLUTION"],
				URI:        attrs["URI"],
				IFrameOnly: true,
			}
			if v.URI != "" {
				master.Variants = append(master.Variants, v)
			}
			sawMasterTag = true

		case "#EXT-X-MEDIA":
			attrs := parseAttributes(value)
			r := Rendition{
				Type:            strings.ToUpper(attrs["TYPE"]),
				GroupID:         attrs["GROUP-ID"],
				Name:            attrs["NAME"],
				Language:        attrs["LANGUAGE"],
				AssocLanguage:   attrs["ASSOC-LANGUAGE"],
				Default:         strings.EqualFold(attrs["DEFAULT"], "YES"),
				Autoselect:      strings.EqualFold(attrs["AUTOSELECT"], "YES"),
				Forced:          strings.EqualFold(attrs["FORCED"], "YES"),
				URI:             attrs["URI"],
				Characteristics: attrs["CHARACTERISTICS"],
				Channels:        attrs["CHANNELS"],
			}
			master.Renditions = append(master.Renditions, r)

		case "#EXT-X-SESSION-DATA":
			attrs := parseAttributes(value)
			master.SessionData = append(master.SessionData, SessionData{
				DataID:   attrs["DATA-ID"],
				Value:    attrs["VALUE"],
				URI:      attrs["URI"],
				Language: attrs["LANGUAGE"],
			})

		case "#EXT-X-START":
			attrs := parseAttributes(value)
			st := &StartTag{
				TimeOffset:    atofDefault(attrs["TIME-OFFSET"], 0),
				Precise:       strings.EqualFold(attrs["PRECISE"], "YES"),
				TimeOffsetSet: attrs["TIME-OFFSET"] != "",
			}
			media.Start, master.Start = st, st

		default:
			// Unknown/ignored tags (EXT-X-ALLOW-CACHE, EXT-BILI-AUX, ...).
		}
		_ = cur
	}

	_ = keySet
	if sawExtinf {
		media.Kind = KindMedia
		return &Playlist{Kind: KindMedia, Media: media}, nil
	}
	if sawStreamInf || sawMasterTag || len(master.Variants) > 0 {
		return &Playlist{Kind: KindMaster, Master: master}, nil
	}
	return &Playlist{Kind: KindUnknown, Media: media}, nil
}

// programDateTimeLayouts lists the timestamp shapes seen in the wild.
var programDateTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
}

// parseProgramDateTime converts an EXT-X-PROGRAM-DATE-TIME value into Unix
// nanoseconds. It returns an error when the value cannot be understood.
func parseProgramDateTime(v string) (int64, error) {
	for _, layout := range programDateTimeLayouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UnixNano(), nil
		}
	}
	return 0, fmt.Errorf("m3u8: bad EXT-X-PROGRAM-DATE-TIME %q", v)
}

// parseExtinf parses the value of an EXTINF tag.
func parseExtinf(v string) (float64, string) {
	dur, title, _ := strings.Cut(v, ",")
	f, err := strconv.ParseFloat(strings.TrimSpace(dur), 64)
	if err != nil {
		return 0, strings.TrimSpace(title)
	}
	return f, strings.TrimSpace(title)
}

// parseByteRange parses "n[@o]". A missing offset is reported as -1.
func parseByteRange(v string) (int64, int64, error) {
	v = strings.TrimSpace(v)
	lenPart, offPart, hasOff := strings.Cut(v, "@")
	n, err := strconv.ParseInt(strings.TrimSpace(lenPart), 10, 64)
	if err != nil || n < 0 {
		return 0, -1, fmt.Errorf("m3u8: bad byte range length %q", v)
	}
	if !hasOff {
		return n, -1, nil
	}
	off, err := strconv.ParseInt(strings.TrimSpace(offPart), 10, 64)
	if err != nil || off < 0 {
		return 0, -1, fmt.Errorf("m3u8: bad byte range offset %q", v)
	}
	return n, off, nil
}

// parseHexIV parses a 0x-prefixed hexadecimal initialisation vector.
func parseHexIV(v string) ([]byte, error) {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(v, "0x"), "0X"))
	if len(s)%2 != 0 {
		s = "0" + s
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("m3u8: bad IV %q: %w", v, err)
	}
	return b, nil
}

// parseAttributes splits an attribute list such as
// `BANDWIDTH=1280000,CODECS="avc1.64002a,mp4a.40.2"` into a map.
// Commas inside quoted strings are preserved.
func parseAttributes(v string) map[string]string {
	out := map[string]string{}
	var (
		key      strings.Builder
		val      strings.Builder
		inValue  bool
		inQuotes bool
	)
	flush := func() {
		k := strings.TrimSpace(key.String())
		if k != "" {
			out[k] = strings.TrimSpace(val.String())
		}
		key.Reset()
		val.Reset()
		inValue = false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '"':
			inQuotes = !inQuotes
		case c == '=' && !inValue && !inQuotes:
			inValue = true
		case c == ',' && !inQuotes:
			flush()
		default:
			if inValue {
				val.WriteByte(c)
			} else {
				key.WriteByte(c)
			}
		}
	}
	flush()
	return out
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func atofDefault(s string, def float64) float64 {
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return def
	}
	return f
}
