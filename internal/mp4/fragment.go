package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// FragmentOptions controls how a media segment is interpreted.
type FragmentOptions struct {
	// Offset is the absolute position of data[0] inside the whole resource.
	// It is needed to resolve absolute base data offsets when a moof uses
	// the base-data-offset-present flag.
	Offset int64
	// Adjust maps a raw decode timestamp (track timescale units) to the
	// timestamp written to the output. When nil, the value is passed through.
	Adjust func(trackID uint32, dts int64) int64
}

// Fragment is the decoded result of one media segment.
type Fragment struct {
	// Samples in file order, interleaved across tracks.
	Samples []Sample
	// Sequence is the mfhd sequence number, 0 when absent.
	Sequence uint32
	// SawMoof reports whether the segment carried fragment metadata.
	SawMoof bool
}

// ParseFragment decodes a media segment against the given initialisation info.
//
// The segment must contain one or more moof/mdat pairs; samples are located by
// resolving each trun against its tfhd, honouring default-base-is-moof,
// explicit base data offsets, relative data offsets and per-sample or default
// values for duration, size, flags and composition time.
func ParseFragment(info *Info, data []byte, opts FragmentOptions) (*Fragment, error) {
	if info == nil {
		return nil, errors.New("mp4: nil initialisation info")
	}
	frag := &Fragment{}
	boxes := splitBoxes(data, 0, len(data))
	for idx := 0; idx < len(boxes); idx++ {
		b := boxes[idx]
		if b.typ != boxMOOF {
			continue
		}
		frag.SawMoof = true

		// The mdat that belongs to this moof is the next mdat before any
		// following moof.
		var mdat []byte
		var mdatOffset int64
		for j := idx + 1; j < len(boxes); j++ {
			if boxes[j].typ == boxMDAT {
				mdat = boxes[j].body(data)
				mdatOffset = opts.Offset + int64(boxes[j].off) + int64(boxes[j].size-len(mdat))
				break
			}
			if boxes[j].typ == boxMOOF {
				break
			}
		}

		moofStart := opts.Offset + int64(b.off)
		moofBody := b.body(data)
		if err := parseMoof(info, moofBody, moofStart, mdat, mdatOffset, opts, frag); err != nil {
			return frag, err
		}
	}
	if !frag.SawMoof {
		return nil, errors.New("mp4: media segment contains no moof box")
	}
	return frag, nil
}

func parseMoof(info *Info, moof []byte, moofStart int64, mdat []byte, mdatOffset int64, opts FragmentOptions, frag *Fragment) error {
	for _, b := range splitBoxes(moof, 0, len(moof)) {
		switch b.typ {
		case boxMFHD:
			body := b.body(moof)
			if len(body) >= 8 {
				frag.Sequence = binary.BigEndian.Uint32(body[4:8])
			}
		case boxTRAF:
			if err := parseTraf(info, b.body(moof), moofStart, mdat, mdatOffset, opts, frag); err != nil {
				return err
			}
		}
	}
	return nil
}

// trafDefaults carries the value inheritance performed by tfhd.
type trafDefaults struct {
	track                 *Track
	baseDataOffset        int64
	hasBaseDataOffset     bool
	defaultSampleDuration uint32
	defaultSampleSize     uint32
	defaultSampleFlags    uint32
	baseDecodeTime        uint64
	hasTFDT               bool
}

func parseTraf(info *Info, traf []byte, moofStart int64, mdat []byte, mdatOffset int64, opts FragmentOptions, frag *Fragment) error {
	d := trafDefaults{}
	var truns []boxRef
	for _, b := range splitBoxes(traf, 0, len(traf)) {
		body := b.body(traf)
		switch b.typ {
		case boxTFHD:
			if len(body) < 8 {
				return errors.New("mp4: short tfhd")
			}
			flags := binary.BigEndian.Uint32(body[0:4]) & 0xFFFFFF
			trackID := binary.BigEndian.Uint32(body[4:8])
			d.track = info.TrackByID(trackID)
			if d.track == nil {
				// Unknown track: keep parsing so other trafs still work.
				return nil
			}
			i := 8
			if flags&tfhdBaseDataOffset != 0 {
				if i+8 > len(body) {
					return errors.New("mp4: short tfhd base data offset")
				}
				d.baseDataOffset = int64(binary.BigEndian.Uint64(body[i : i+8]))
				d.hasBaseDataOffset = true
				i += 8
			}
			if flags&tfhdSampleDescriptionIndex != 0 {
				i += 4
			}
			if flags&tfhdDefaultSampleDuration != 0 {
				if i+4 > len(body) {
					return errors.New("mp4: short tfhd default duration")
				}
				d.defaultSampleDuration = binary.BigEndian.Uint32(body[i : i+4])
				i += 4
			}
			if flags&tfhdDefaultSampleSize != 0 {
				if i+4 > len(body) {
					return errors.New("mp4: short tfhd default size")
				}
				d.defaultSampleSize = binary.BigEndian.Uint32(body[i : i+4])
				i += 4
			}
			if flags&tfhdDefaultSampleFlags != 0 {
				if i+4 > len(body) {
					return errors.New("mp4: short tfhd default flags")
				}
				d.defaultSampleFlags = binary.BigEndian.Uint32(body[i : i+4])
				i += 4
			}
		case boxTFDT:
			if len(body) < 8 {
				continue
			}
			if body[0] == 1 {
				if len(body) >= 12 {
					d.baseDecodeTime = binary.BigEndian.Uint64(body[4:12])
					d.hasTFDT = true
				}
			} else {
				d.baseDecodeTime = uint64(binary.BigEndian.Uint32(body[4:8]))
				d.hasTFDT = true
			}
		case boxTRUN:
			truns = append(truns, b)
		}
	}
	if d.track == nil {
		return nil
	}
	if d.track.Encrypted {
		return fmt.Errorf("%w (track %d)", ErrEncrypted, d.track.ID)
	}
	summaries := make([]Sample, 0, 16)
	// cursor is an absolute position inside the resource. It starts after the
	// moof (or at the explicit base data offset) and, for a trun without a
	// data offset, continues at the end of the previous trun's samples.
	cursor := moofStart
	if d.hasBaseDataOffset {
		cursor = d.baseDataOffset
	}
	for _, tr := range truns {
		if err := parseTrun(d, tr.body(traf), moofStart, &cursor, mdat, mdatOffset, opts, &summaries); err != nil {
			return err
		}
	}
	frag.Samples = append(frag.Samples, summaries...)
	return nil
}

// parseTrun decodes one trun, appending its samples to out.
//
// cursor is the absolute position in the resource of the next sample byte. A
// trun with the data-offset-present flag resets it relative to moofStart;
// otherwise decoding continues where the previous trun of the same traf
// stopped, which is what the specification requires.
func parseTrun(d trafDefaults, trun []byte, moofStart int64, cursor *int64, mdat []byte, mdatOffset int64, opts FragmentOptions, out *[]Sample) error {
	if len(trun) < 8 {
		return errors.New("mp4: short trun")
	}
	flags := binary.BigEndian.Uint32(trun[0:4]) & 0xFFFFFF
	count := int(binary.BigEndian.Uint32(trun[4:8]))
	i := 8

	var (
		firstSampleFlags uint32
		hasFirstFlags    bool
	)
	if flags&trunDataOffset != 0 {
		if i+4 > len(trun) {
			return errors.New("mp4: short trun data offset")
		}
		// The data offset is signed and relative to the containing moof.
		*cursor = moofStart + int64(int32(binary.BigEndian.Uint32(trun[i:i+4])))
		i += 4
	}
	if flags&trunFirstSampleFlags != 0 {
		if i+4 > len(trun) {
			return errors.New("mp4: short trun first sample flags")
		}
		firstSampleFlags = binary.BigEndian.Uint32(trun[i : i+4])
		hasFirstFlags = true
		i += 4
	}

	hasDur := flags&trunSampleDuration != 0
	hasSize := flags&trunSampleSize != 0
	hasFlags := flags&trunSampleFlags != 0
	hasCTO := flags&trunSampleCTO != 0

	dts := int64(d.baseDecodeTime)
	for n := 0; n < count; n++ {
		dur := d.defaultSampleDuration
		if hasDur {
			if i+4 > len(trun) {
				return truncatedTrun(count, n)
			}
			dur = binary.BigEndian.Uint32(trun[i : i+4])
			i += 4
			if dur == 0 {
				dur = d.defaultSampleDuration
			}
		}
		if dur == 0 {
			dur = d.track.trexDefaultDuration
		}
		size := d.defaultSampleSize
		if hasSize {
			if i+4 > len(trun) {
				return truncatedTrun(count, n)
			}
			size = binary.BigEndian.Uint32(trun[i : i+4])
			i += 4
			if size == 0 {
				size = d.defaultSampleSize
			}
		}
		if size == 0 {
			size = d.track.trexDefaultSize
		}
		sflags := d.defaultSampleFlags
		if hasFlags {
			if i+4 > len(trun) {
				return truncatedTrun(count, n)
			}
			sflags = binary.BigEndian.Uint32(trun[i : i+4])
			i += 4
		}
		if n == 0 && hasFirstFlags {
			sflags = firstSampleFlags
		}
		var cts uint32
		if hasCTO {
			if i+4 > len(trun) {
				return truncatedTrun(count, n)
			}
			cts = binary.BigEndian.Uint32(trun[i : i+4])
			i += 4
		}

		start := *cursor - mdatOffset
		end := start + int64(size)
		if size > 0 && (start < 0 || end > int64(len(mdat))) {
			return fmt.Errorf("mp4: sample %d of track %d addresses bytes [%d,%d) outside the %d byte mdat",
				n, d.track.ID, start, end, len(mdat))
		}
		var payload []byte
		if size > 0 {
			payload = mdat[start:end]
		}
		*cursor = *cursor + int64(size)

		outDTS := dts
		if opts.Adjust != nil {
			outDTS = opts.Adjust(d.track.ID, dts)
		}
		*out = append(*out, Sample{
			TrackID:  d.track.ID,
			Data:     payload,
			DTS:      outDTS,
			CTS:      int64(int32(cts)),
			Duration: dur,
			Keyframe: isSync(sflags),
		})
		dts += int64(dur)
	}
	return nil
}

func truncatedTrun(count, n int) error {
	return fmt.Errorf("mp4: trun declares %d samples but its payload is truncated after %d", count, n)
}

// isSync reports whether sample flags mark a sync sample.
//
// sample_depends_on == 2 means "does not depend on other samples", which is a
// keyframe; sample_is_non_sync_sample set means explicitly not a sync sample.
// Some muxers leave the flags zero, in which case they carry no information
// and the sample is treated as a keyframe, matching how tolerant players
// handle such streams.
func isSync(flags uint32) bool {
	if flags == 0 {
		return true
	}
	if flags&sampleFlagIsNonSync != 0 {
		return false
	}
	switch (flags & sampleDependsOnMask) >> sampleDependsOnShift {
	case sampleDependsOnNo:
		return true
	case sampleDependsOnYes:
		return false
	}
	// 0 means "unknown": fall back to the non-sync flag only.
	return true
}

// bitReader reads big endian bit fields.
type bitReader struct {
	data []byte
	pos  int // bit position
}

func (r *bitReader) read(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		byteIdx := r.pos >> 3
		if byteIdx >= len(r.data) {
			r.pos++
			v <<= 1
			continue
		}
		bit := (r.data[byteIdx] >> (7 - uint(r.pos&7))) & 1
		v = v<<1 | uint32(bit)
		r.pos++
	}
	return v
}
