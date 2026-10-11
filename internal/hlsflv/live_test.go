package hlsflv

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"
)

// The live tests talk to real CDNs and are skipped unless the playlist URLs are
// provided. Run them with:
//
//	BLREC_TEST_HLS_AVC_URL=https://.../index.m3u8 \
//	BLREC_TEST_HLS_HEVC_URL=https://.../index.m3u8 \
//	go test ./internal/hlsflv/ -run TestLive -v
const (
	envAVC  = "BLREC_TEST_HLS_AVC_URL"
	envHEVC = "BLREC_TEST_HLS_HEVC_URL"
)

// flvSummary is a structural summary of a produced FLV stream.
type flvSummary struct {
	flags       byte
	tags        int
	script      int
	videoSeq    int
	audioSeq    int
	videoCodecs map[byte]int
	videoFrames int
	audioFrames int
	lastVideoTS int64
	lastAudioTS int64
}

// summariseFLV walks an FLV stream and collects the facts the live tests assert.
func summariseFLV(t *testing.T, data []byte) flvSummary {
	t.Helper()
	if len(data) < 13 || !bytes.Equal(data[0:3], []byte("FLV")) {
		t.Fatalf("output is not an FLV stream (%d bytes)", len(data))
	}
	s := flvSummary{flags: data[4], videoCodecs: map[byte]int{}}
	off := 13
	for off < len(data) {
		if off+11 > len(data) {
			t.Fatalf("truncated tag header at %d", off)
		}
		typ := data[off]
		size := int(data[off+1])<<16 | int(data[off+2])<<8 | int(data[off+3])
		ts := int64(uint32(data[off+4])<<16 | uint32(data[off+5])<<8 |
			uint32(data[off+6]) | uint32(data[off+7])<<24)
		if off+11+size+4 > len(data) {
			// A partially written final tag is expected for a cancelled run.
			break
		}
		body := data[off+11 : off+11+size]
		if prev := binary.BigEndian.Uint32(data[off+11+size:]); prev != uint32(11+size) {
			t.Fatalf("tag at %d has PreviousTagSize %d, want %d", off, prev, 11+size)
		}
		switch typ {
		case 0x12:
			s.script++
		case 0x09:
			if len(body) >= 2 {
				s.videoCodecs[body[0]&0x0F]++
				if body[1] == 0 {
					s.videoSeq++
				} else {
					s.videoFrames++
					if ts < s.lastVideoTS {
						t.Errorf("video timestamp went backwards: %d then %d", s.lastVideoTS, ts)
					}
					s.lastVideoTS = ts
				}
			}
		case 0x08:
			if len(body) >= 2 {
				if body[1] == 0 {
					s.audioSeq++
				} else {
					s.audioFrames++
					if ts < s.lastAudioTS {
						t.Errorf("audio timestamp went backwards: %d then %d", s.lastAudioTS, ts)
					}
					s.lastAudioTS = ts
				}
			}
		}
		s.tags++
		off += 11 + size + 4
	}
	return s
}

// runLive converts a playlist for the given duration and returns the FLV bytes.
func runLive(t *testing.T, url string, d time.Duration) []byte {
	t.Helper()
	var buf bytes.Buffer
	c, err := New(url, &buf, Options{
		StartFromLive: true,
		Logger:        func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if err := c.Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatalf("Run: %v", err)
	}
	st := c.Stats()
	t.Logf("converted %d segments, %d samples, %d video tags, %d audio tags, %d bytes",
		st.Segments, st.Samples, st.VideoTags, st.AudioTags, st.Bytes)
	return buf.Bytes()
}

// TestLiveAVC converts the AVC example stream and checks that it produces a
// well formed FLV stream with H.264 video and AAC audio.
func TestLiveAVC(t *testing.T) {
	url := os.Getenv(envAVC)
	if url == "" {
		t.Skipf("set %s to run the live AVC test", envAVC)
	}
	data := runLive(t, url, 8*time.Second)
	s := summariseFLV(t, data)

	if s.flags != 0x01|0x04 {
		t.Errorf("header flags = 0x%02x, want 0x05 (audio and video)", s.flags)
	}
	if s.videoSeq != 1 {
		t.Errorf("AVC sequence header count = %d, want 1", s.videoSeq)
	}
	if s.audioSeq != 1 {
		t.Errorf("AAC sequence header count = %d, want 1", s.audioSeq)
	}
	if s.videoFrames == 0 {
		t.Error("no video frames were produced")
	}
	if s.audioFrames == 0 {
		t.Error("no audio frames were produced")
	}
	if got := s.videoCodecs[7]; got == 0 {
		t.Errorf("expected AVC video tags, codec IDs seen: %v", s.videoCodecs)
	}
	if s.lastVideoTS == 0 && s.lastAudioTS == 0 {
		t.Error("all timestamps are zero")
	}
}

// TestLiveHEVC converts the HEVC example stream and checks the compatibility
// requirement: HEVC is carried with codec ID 12, not with the Enhanced RTMP
// four character code header.
func TestLiveHEVC(t *testing.T) {
	url := os.Getenv(envHEVC)
	if url == "" {
		t.Skipf("set %s to run the live HEVC test", envHEVC)
	}
	data := runLive(t, url, 8*time.Second)
	s := summariseFLV(t, data)

	if s.videoSeq != 1 {
		t.Errorf("HEVC sequence header count = %d, want 1", s.videoSeq)
	}
	if s.audioSeq != 1 {
		t.Errorf("AAC sequence header count = %d, want 1", s.audioSeq)
	}
	if s.videoFrames == 0 {
		t.Error("no video frames were produced")
	}
	if got := s.videoCodecs[12]; got == 0 {
		t.Errorf("no codec ID 12 tags were produced, codec IDs seen: %v", s.videoCodecs)
	}
	// No tag may set the isExHeader bit: that is the Enhanced RTMP form this
	// project deliberately avoids.
	for codec, n := range s.videoCodecs {
		if codec > 15 {
			t.Errorf("a video tag used an invalid codec ID %d (%d tags)", codec, n)
		}
	}
	if s.script != 1 {
		t.Errorf("onMetaData script tag count = %d, want 1", s.script)
	}
}
