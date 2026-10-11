// Package flv writes the Flash Video container format (Adobe Flash Video File
// Format Specification, version 10.1 and its Enhanced RTMP extensions).
//
// Only what an HLS remuxer needs is implemented: the file header, AVC and
// HEVC video tags, AAC, MP3, Opus and FLAC audio tags and an AMF0 onMetaData
// script tag.
package flv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Signature and version of the file header.
const (
	signature = "FLV"
	version   = 0x01
	// headerFlagAudio marks the presence of audio tags.
	headerFlagAudio = 0x01
	// headerFlagVideo marks the presence of video tags.
	headerFlagVideo = 0x04
	// headerSize is the fixed 9 byte file header plus the 4 byte first
	// previous-tag-size field.
	headerSize = 9
)

// Tag types.
const (
	TagAudio  = 0x08
	TagVideo  = 0x09
	TagScript = 0x12
)

// Video codec IDs for the legacy (non enhanced) video tag format.
const (
	CodecH263         = 2
	CodecScreenVideo  = 3
	CodecVP6          = 4
	CodecVP6Alpha     = 5
	CodecScreenVideo2 = 6
	CodecAVC          = 7
	// CodecHEVC is the HEVC codec ID used by the enhanced FLV specification
	// and by players that predate Enhanced RTMP.
	CodecHEVC = 12
)

// Frame types for the video tag.
const (
	FrameKey        = 1
	FrameInter      = 2
	FrameDisposable = 3
	FrameGenerated  = 4
	FrameCommand    = 5
)

// AVCPacketType values. HEVC uses the same packet type values in the legacy
// codec ID 12 layout.
const (
	AVCSequenceHeader = 0
	AVCNALU           = 1
	AVCEndOfSequence  = 2
)

// HEVC packet types, identical in meaning to the AVC ones.
const (
	HEVCSequenceHeader = AVCSequenceHeader
	HEVCNALU           = AVCNALU
	HEVCEndOfSequence  = AVCEndOfSequence
)

// AACPacketType values.
const (
	AACSequenceHeader = 0
	AACRaw            = 1
)

// Sound format values for the audio tag.
const (
	SoundMP3   = 2
	SoundAAC   = 10
	SoundSpeex = 11
	SoundOpus  = 13
	SoundFLAC  = 14
)

// SoundRate values (the legacy field is only meaningful for MP3).
const (
	Rate5500  = 0
	Rate11000 = 1
	Rate22000 = 2
	Rate44100 = 3
)

// SoundSize and SoundType values.
const (
	Size8Bit  = 0
	Size16Bit = 1
	Mono      = 0
	Stereo    = 1
)

// MaxTimestamp is the largest value representable in the 24 bit timestamp
// field of a tag header, plus the 8 bit timestamp extension.
const MaxTimestamp = 0xFFFFFFFF

// ErrTimestampOverflow is returned when a timestamp cannot be represented.
var ErrTimestampOverflow = errors.New("flv: timestamp exceeds the 32 bit tag field")

// Writer serialises FLV tags onto an io.Writer.
//
// A Writer is not safe for concurrent use; the caller serialises access.
type Writer struct {
	w   io.Writer
	buf [32]byte

	flags       byte
	wroteHeader bool
	// bytesWritten counts everything emitted after the file header.
	bytesWritten int64
}

// NewWriter returns a Writer that emits to w. The file header is written on
// the first tag, once the tracks are known.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// Header writes the 9 byte FLV file header followed by the first
// previous-tag-size field. It must be called before any tag is written; it is
// called automatically by the tag writers when needed.
func (w *Writer) Header() error {
	if w.wroteHeader {
		return nil
	}
	hdr := [headerSize + 4]byte{}
	copy(hdr[0:3], signature)
	hdr[3] = version
	hdr[4] = w.flags
	binary.BigEndian.PutUint32(hdr[5:9], headerSize)
	// Bytes 9..12 are the first PreviousTagSize, which is always zero.
	if _, err := w.w.Write(hdr[:]); err != nil {
		return err
	}
	w.wroteHeader = true
	return nil
}

// SetFlags declares which media types the stream will contain. It has no
// effect after the header has been written.
func (w *Writer) SetFlags(audio, video bool) {
	if w.wroteHeader {
		return
	}
	if audio {
		w.flags |= headerFlagAudio
	}
	if video {
		w.flags |= headerFlagVideo
	}
}

// HeaderWritten reports whether the FLV file header has been emitted.
func (w *Writer) HeaderWritten() bool { return w.wroteHeader }

// WriteHeader emits the FLV file header with the media type flags already set.
//
// Callers that know the stream layout up front should use this instead of
// relying on the first tag to write the header, because a header written with
// zero flags tells players that the stream carries neither audio nor video.
func (w *Writer) WriteHeader() error {
	return w.Header()
}

// BytesWritten reports how many bytes have been emitted after the file header.
func (w *Writer) BytesWritten() int64 { return w.bytesWritten }

// Output returns the destination the writer emits to, so a caller that needs to
// batch tags can write them elsewhere first.
func (w *Writer) Output() io.Writer { return w.w }

// writeTag emits one FLV tag with the given type, timestamp and payload.
func (w *Writer) writeTag(typ byte, timestamp int64, payload []byte) error {
	if timestamp < 0 || timestamp > MaxTimestamp {
		return fmt.Errorf("%w: %d", ErrTimestampOverflow, timestamp)
	}
	ts := uint32(timestamp)
	if !w.wroteHeader {
		if err := w.Header(); err != nil {
			return err
		}
	}
	dataSize := len(payload)
	hdr := w.buf[:11]
	hdr[0] = typ
	// hdr[1:4] = data size, 24 bit big endian
	hdr[1] = byte(dataSize >> 16)
	hdr[2] = byte(dataSize >> 8)
	hdr[3] = byte(dataSize)
	// hdr[4:7] = timestamp low 24 bits, hdr[7] = timestamp high 8 bits
	hdr[4] = byte(ts >> 16)
	hdr[5] = byte(ts >> 8)
	hdr[6] = byte(ts)
	hdr[7] = byte(ts >> 24)
	// hdr[8:11] = stream ID, always zero for FLV
	if _, err := w.w.Write(hdr); err != nil {
		return err
	}
	if _, err := w.w.Write(payload); err != nil {
		return err
	}
	// PreviousTagSize = 11 + data size
	trailer := w.buf[:4]
	binary.BigEndian.PutUint32(trailer, uint32(11+dataSize))
	if _, err := w.w.Write(trailer); err != nil {
		return err
	}
	w.bytesWritten += int64(11 + dataSize + 4)
	return nil
}

// WriteAVCSequenceHeader writes the AVCDecoderConfigurationRecord, which must
// precede any AVC NALU tag.
func (w *Writer) WriteAVCSequenceHeader(record []byte) error {
	w.SetFlags(false, true)
	payload := make([]byte, 5+len(record))
	payload[0] = FrameKey<<4 | CodecAVC
	payload[1] = AVCSequenceHeader
	// payload[2:5] = composition time, zero for a sequence header
	copy(payload[5:], record)
	return w.writeTag(TagVideo, 0, payload)
}

// WriteAVCNALU writes one AVC access unit.
//
// cts is the composition time offset (PTS minus DTS) in milliseconds and may
// be negative. It is stored as a signed 24 bit value.
func (w *Writer) WriteAVCNALU(data []byte, timestamp int64, cts int32, keyframe bool) error {
	if len(data) == 0 {
		return nil
	}
	w.SetFlags(false, true)
	if cts > 0x7FFFFF {
		cts = 0x7FFFFF
	} else if cts < -0x800000 {
		cts = -0x800000
	}
	payload := make([]byte, 5+len(data))
	if keyframe {
		payload[0] = FrameKey<<4 | CodecAVC
	} else {
		payload[0] = FrameInter<<4 | CodecAVC
	}
	payload[1] = AVCNALU
	payload[2] = byte(cts >> 16)
	payload[3] = byte(cts >> 8)
	payload[4] = byte(cts)
	copy(payload[5:], data)
	return w.writeTag(TagVideo, timestamp, payload)
}

// WriteHEVCSequenceHeader writes an HEVCDecoderConfigurationRecord.
//
// HEVC uses the legacy FLV video tag layout with codec ID 12 rather than the
// Enhanced RTMP four character code form, for compatibility with players that
// do not implement Enhanced RTMP.
func (w *Writer) WriteHEVCSequenceHeader(record []byte) error {
	w.SetFlags(false, true)
	payload := make([]byte, 5+len(record))
	payload[0] = FrameKey<<4 | CodecHEVC
	payload[1] = HEVCSequenceHeader
	// payload[2:5] = composition time, zero for a sequence header
	copy(payload[5:], record)
	return w.writeTag(TagVideo, 0, payload)
}

// WriteHEVCNALU writes one HEVC access unit using codec ID 12.
//
// The payload is a length prefixed HEVC access unit, exactly as for AVC. The
// composition time offset is stored as a signed 24 bit value.
func (w *Writer) WriteHEVCNALU(data []byte, timestamp int64, cts int32, keyframe bool) error {
	if len(data) == 0 {
		return nil
	}
	w.SetFlags(false, true)
	if cts > 0x7FFFFF {
		cts = 0x7FFFFF
	} else if cts < -0x800000 {
		cts = -0x800000
	}
	payload := make([]byte, 5+len(data))
	if keyframe {
		payload[0] = FrameKey<<4 | CodecHEVC
	} else {
		payload[0] = FrameInter<<4 | CodecHEVC
	}
	payload[1] = HEVCNALU
	payload[2] = byte(cts >> 16)
	payload[3] = byte(cts >> 8)
	payload[4] = byte(cts)
	copy(payload[5:], data)
	return w.writeTag(TagVideo, timestamp, payload)
}

func frameTypeOf(keyframe bool) int {
	if keyframe {
		return FrameKey
	}
	return FrameInter
}

// WriteAACSequenceHeader writes the AudioSpecificConfig.
func (w *Writer) WriteAACSequenceHeader(asc []byte) error {
	w.SetFlags(true, false)
	payload := make([]byte, 2+len(asc))
	payload[0] = SoundAAC<<4 | Rate44100<<2 | Size16Bit<<1 | Stereo
	payload[1] = AACSequenceHeader
	copy(payload[2:], asc)
	return w.writeTag(TagAudio, 0, payload)
}

// WriteAACRaw writes one raw AAC frame (no ADTS header).
func (w *Writer) WriteAACRaw(data []byte, timestamp int64) error {
	if len(data) == 0 {
		return nil
	}
	w.SetFlags(true, false)
	payload := make([]byte, 2+len(data))
	payload[0] = SoundAAC<<4 | Rate44100<<2 | Size16Bit<<1 | Stereo
	payload[1] = AACRaw
	copy(payload[2:], data)
	return w.writeTag(TagAudio, timestamp, payload)
}

// WriteMP3 writes one MP3 frame.
func (w *Writer) WriteMP3(data []byte, timestamp int64, sampleRate, channels int) error {
	if len(data) == 0 {
		return nil
	}
	w.SetFlags(true, false)
	payload := make([]byte, 1+len(data))
	payload[0] = SoundMP3<<4 | mp3Rate(sampleRate)<<2 | Size16Bit<<1 | mp3Channels(channels)
	copy(payload[1:], data)
	return w.writeTag(TagAudio, timestamp, payload)
}

// WriteOpusSequenceHeader writes the Opus identification header carried in
// dOps, using the Enhanced RTMP "Opus" four character code.
func (w *Writer) WriteOpusSequenceHeader(dops []byte) error {
	w.SetFlags(true, false)
	payload := make([]byte, 5+len(dops))
	payload[0] = 0x80 | FrameKey<<4 | 0 // sequence start
	copy(payload[1:5], "Opus")
	copy(payload[5:], dops)
	return w.writeTag(TagAudio, 0, payload)
}

// WriteOpusRaw writes one Opus packet.
func (w *Writer) WriteOpusRaw(data []byte, timestamp int64) error {
	if len(data) == 0 {
		return nil
	}
	w.SetFlags(true, false)
	payload := make([]byte, 5+len(data))
	payload[0] = 0x80 | FrameKey<<4 | 3 // coded frames
	copy(payload[1:5], "Opus")
	copy(payload[5:], data)
	return w.writeTag(TagAudio, timestamp, payload)
}

func mp3Rate(sampleRate int) byte {
	switch {
	case sampleRate >= 44100:
		return Rate44100
	case sampleRate >= 22050:
		return Rate22000
	case sampleRate >= 11025:
		return Rate11000
	default:
		return Rate5500
	}
}

func mp3Channels(channels int) byte {
	if channels > 1 {
		return Stereo
	}
	return Mono
}

// Metadata describes the stream for the onMetaData script tag.
type Metadata struct {
	Duration        float64
	Width           int
	Height          int
	VideoDataRate   float64
	FrameRate       float64
	VideoCodecID    float64
	AudioSampleRate float64
	AudioSampleSize float64
	AudioChannels   float64
	AudioCodecID    float64
	Stereo          bool
	Encoder         string
	Extra           map[string]float64
}

// WriteMetadata writes an AMF0 onMetaData script tag describing the stream.
//
// The durations map, when non-nil, supplies a "duration" property; a live
// stream must omit it because the value is undefined.
func (w *Writer) WriteMetadata(timestamp int64, md Metadata) error {
	type property struct {
		key  string
		kind byte // 0 number, 1 boolean, 2 string
		num  float64
		str  string
		bl   bool
	}
	var props []property
	addNumber := func(key string, v float64) {
		if v > 0 {
			props = append(props, property{key: key, kind: 0, num: v})
		}
	}
	addNumber("duration", md.Duration)
	addNumber("width", float64(md.Width))
	addNumber("height", float64(md.Height))
	addNumber("videodatarate", md.VideoDataRate)
	addNumber("framerate", md.FrameRate)
	addNumber("videocodecid", md.VideoCodecID)
	addNumber("audiosamplerate", md.AudioSampleRate)
	addNumber("audiosamplesize", md.AudioSampleSize)
	addNumber("audiochannels", md.AudioChannels)
	addNumber("audiocodecid", md.AudioCodecID)
	props = append(props, property{key: "stereo", kind: 1, bl: md.Stereo})
	for _, k := range sortedKeys(md.Extra) {
		addNumber(k, md.Extra[k])
	}
	if md.Encoder != "" {
		props = append(props, property{key: "encoder", kind: 2, str: md.Encoder})
	}

	// AMF0: a string naming the event followed by an ECMA array of properties.
	//
	// Every property is a key followed by a value with its type marker:
	//   keyLength(2) key type(1) value
	// The object is closed by a zero length key and the object end marker.
	buf := make([]byte, 0, 64+len(md.Encoder))
	buf = append(buf, amf0String)
	buf = append(buf, byte(len("onMetaData")>>8), byte(len("onMetaData")))
	buf = append(buf, "onMetaData"...)
	buf = append(buf, amf0ECMAArray)
	buf = append(buf, byte(len(props)>>24), byte(len(props)>>16), byte(len(props)>>8), byte(len(props)))
	for _, p := range props {
		n := len(p.key)
		buf = append(buf, byte(n>>8), byte(n))
		buf = append(buf, p.key...)
		switch p.kind {
		case 0:
			buf = append(buf, amf0Number)
			bits := math.Float64bits(p.num)
			buf = append(buf,
				byte(bits>>56), byte(bits>>48), byte(bits>>40), byte(bits>>32),
				byte(bits>>24), byte(bits>>16), byte(bits>>8), byte(bits))
		case 1:
			buf = append(buf, amf0Boolean)
			if p.bl {
				buf = append(buf, 1)
			} else {
				buf = append(buf, 0)
			}
		case 2:
			buf = append(buf, amf0String)
			if len(p.str) > 0xFFFF {
				return fmt.Errorf("flv: metadata string property %q is too long", p.key)
			}
			v := len(p.str)
			buf = append(buf, byte(v>>8), byte(v))
			buf = append(buf, p.str...)
		}
	}
	// A zero length key name terminates the object.
	buf = append(buf, 0, 0, amf0ObjectEnd)

	if !w.wroteHeader {
		if err := w.Header(); err != nil {
			return err
		}
	}
	return w.writeTag(TagScript, timestamp, buf)
}

// AMF0 type markers.
const (
	amf0Number    = 0x00
	amf0Boolean   = 0x01
	amf0String    = 0x02
	amf0Object    = 0x03
	amf0Null      = 0x05
	amf0ECMAArray = 0x08
	amf0ObjectEnd = 0x09
)

// appendAMFUint16 appends a big endian 16 bit value.
func appendAMFUint16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}

// appendAMFUint32 appends a big endian 32 bit value.
func appendAMFUint32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// appendAMFUint64 appends a big endian 64 bit value.
func appendAMFUint64(b []byte, v uint64) []byte {
	return append(b,
		byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
		byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// appendAMFString appends a length prefixed AMF0 string value (without the
// type marker, which the caller writes).
func appendAMFString(b []byte, s string) []byte {
	b = appendAMFUint16(b, uint16(len(s)))
	return append(b, s...)
}

func sortedKeys(m map[string]float64) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
