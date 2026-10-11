// Package hlsflv converts an HLS stream into an FLV stream without invoking
// ffmpeg or any other external process.
//
// It fetches the playlist, follows the initialisation segment when the stream
// is fragmented MP4, downloads media segments in order and remuxes their
// samples into FLV tags. Live playlists are reloaded so playback continues
// indefinitely.
package hlsflv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shugen002/blrechelper/internal/flv"
	"github.com/shugen002/blrechelper/internal/m3u8"
	"github.com/shugen002/blrechelper/internal/mp4"
)

// DefaultUserAgent is used when Options.UserAgent is empty.
//
// Live providers, including the one in the bundled example, reject requests
// that do not look like a browser.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"

// maxMirroredLiveSegments caps how much of a live playlist is buffered when a
// client connects, so a late joiner is not forced through minutes of backlog.
const maxMirroredLiveSegments = 6

// allowedRequestHeaders lists the header names a caller may set per request.
//
// It is an allow list rather than a deny list because a passthrough header
// mechanism is otherwise an open relay for arbitrary header injection. The
// names here are the ones origins actually key on for hotlink protection, plus
// the credentials a private stream needs.
var allowedRequestHeaders = map[string]string{
	"user-agent":      "User-Agent",
	"referer":         "Referer",
	"referrer":        "Referer", // common misspelling, accepted for convenience
	"origin":          "Origin",
	"authorization":   "Authorization",
	"cookie":          "Cookie",
	"accept":          "Accept",
	"accept-language": "Accept-Language",
	"accept-encoding": "Accept-Encoding",
}

// allowedHeaderList is the allow list in a stable order, for error messages.
const allowedHeaderList = "User-Agent, Referer, Origin, Authorization, Cookie, Accept, Accept-Language, Accept-Encoding"

// RequestHeader is one header a caller asked to send upstream.
type RequestHeader struct {
	Name  string
	Value string
}

// ParseRequestHeader parses a "Name: Value" pair and rejects a name that is not
// on the allow list.
func ParseRequestHeader(spec string) (RequestHeader, error) {
	name, value, found := strings.Cut(spec, ":")
	if !found {
		return RequestHeader{}, fmt.Errorf("header %q must be written as Name: Value", spec)
	}
	return NewRequestHeader(name, value)
}

// NewRequestHeader builds a header, rejecting a name that is not allowed.
func NewRequestHeader(name, value string) (RequestHeader, error) {
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if name == "" {
		return RequestHeader{}, errors.New("header name is empty")
	}
	canonical, ok := allowedRequestHeaders[strings.ToLower(name)]
	if !ok {
		return RequestHeader{}, fmt.Errorf("header %q is not allowed; allowed names are %s", name, allowedHeaderList)
	}
	// A header value must not smuggle a line break into the request.
	if strings.ContainsAny(value, "\r\n") {
		return RequestHeader{}, fmt.Errorf("header %q has a value containing a line break", name)
	}
	return RequestHeader{Name: canonical, Value: value}, nil
}

// Options configures a Client.
type Options struct {
	// UserAgent is sent with every upstream request.
	UserAgent string
	// Referer is sent with every upstream request. Many CDNs require it.
	Referer string
	// Header holds extra headers merged into every upstream request.
	Header http.Header
	// PerRequestHeader holds headers supplied by the caller for this one
	// conversion. They are merged into every upstream request, playlist,
	// initialisation segment and media segment alike, and take precedence over
	// UserAgent, Referer and Header.
	PerRequestHeader []RequestHeader
	// HTTPClient performs upstream requests. A default client is used when nil.
	HTTPClient *http.Client
	// RequestTimeout bounds a single upstream request. Defaults to 20 seconds.
	RequestTimeout time.Duration
	// PlaylistInterval bounds how long to wait between playlist reloads.
	// Defaults to 1 second.
	PlaylistInterval time.Duration
	// MaxPlaylistReloads bounds reload attempts for one refresh. Defaults to 10.
	MaxPlaylistReloads int
	// StartFromLive skips most of the backlog of a live playlist.
	StartFromLive bool
	// Logger receives diagnostics. Nil disables logging.
	Logger func(format string, args ...any)
	// OnTag, when set, is called for every FLV tag that is produced. It is
	// intended for tests and diagnostics and must not retain the payload.
	OnTag func(TagInfo)
}

// TagInfo describes one produced FLV tag.
type TagInfo struct {
	// Type is the FLV tag type: 8 audio, 9 video, 18 script.
	Type byte
	// Timestamp is the tag timestamp in milliseconds.
	Timestamp int64
	// Keyframe is meaningful for video tags.
	Keyframe bool
	// Size is the payload size in bytes.
	Size int
}

func (o *Options) applyDefaults() {
	if o.UserAgent == "" {
		o.UserAgent = DefaultUserAgent
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 20 * time.Second
	}
	if o.PlaylistInterval <= 0 {
		o.PlaylistInterval = time.Second
	}
	if o.MaxPlaylistReloads <= 0 {
		o.MaxPlaylistReloads = 10
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{
			// Redirects are followed by default; upstream CDNs use them.
			Timeout: 0,
		}
	}
}

func (o *Options) logf(format string, args ...any) {
	if o.Logger != nil {
		o.Logger(format, args...)
	}
}

// Stats summarises a conversion.
type Stats struct {
	Segments  int
	Samples   int
	Bytes     int64
	VideoTags int
	AudioTags int
	StartedAt time.Time
	EndedAt   time.Time
}

// Client converts one HLS stream into FLV.
type Client struct {
	opts Options

	// base is the playlist URL, used to resolve every relative reference.
	base *url.URL

	// tracks holds the current initialisation info, replaced whenever a new
	// EXT-X-MAP appears.
	info        *mp4.Info
	video       *mp4.Track
	audio       *mp4.Track
	mapURL      string
	mapRange    int64
	mapRangeOff int64

	w   *flv.Writer
	out io.Writer

	seen map[string]bool

	clocks map[uint32]*trackClock

	// frameRate is the nominal video frame rate for onMetaData, refined from
	// observed sample durations.
	frameRate      float64
	frameRateVotes int

	videoSeq, audioSeq bool

	// tag, when non-nil, observes every produced FLV tag.
	tag func(TagInfo)

	stats Stats
}

// New creates a Client that writes FLV to out for the playlist at playlistURL.
func New(playlistURL string, out io.Writer, opts Options) (*Client, error) {
	opts.applyDefaults()
	base, err := url.Parse(playlistURL)
	if err != nil {
		return nil, fmt.Errorf("hlsflv: bad playlist URL %q: %w", playlistURL, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("hlsflv: unsupported URL scheme %q", base.Scheme)
	}
	if base.Host == "" {
		return nil, errors.New("hlsflv: playlist URL has no host")
	}
	return &Client{
		opts:   opts,
		base:   base,
		w:      flv.NewWriter(out),
		out:    out,
		seen:   map[string]bool{},
		clocks: map[uint32]*trackClock{},
		tag:    opts.OnTag,
	}, nil
}

// reportTag publishes a produced tag to the diagnostic hook, if any.
func (c *Client) reportTag(t TagInfo) {
	if c.tag != nil {
		c.tag(t)
	}
}

// Stats returns a snapshot of the conversion counters.
func (c *Client) Stats() Stats {
	s := c.stats
	return s
}

// Run performs the conversion until the stream ends, ctx is cancelled or an
// unrecoverable error occurs.
func (c *Client) Run(ctx context.Context) error {
	c.stats.StartedAt = time.Now()
	defer func() { c.stats.EndedAt = time.Now() }()

	playlist, resolved, err := c.load(ctx, c.base)
	if err != nil {
		return err
	}
	if playlist.Kind == m3u8.KindMaster {
		var variantURL *url.URL
		playlist, variantURL, err = c.selectVariant(ctx, playlist)
		if err != nil {
			return err
		}
		resolved = variantURL
	}
	if playlist.Media == nil {
		return errors.New("hlsflv: playlist contains no media segments")
	}
	c.base = resolved

	return c.follow(ctx, playlist.Media)
}

// load fetches and parses a playlist, returning the URL it was actually read
// from (after redirects) so relative references resolve correctly.
func (c *Client) load(ctx context.Context, u *url.URL) (*m3u8.Playlist, *url.URL, error) {
	body, finalURL, err := c.get(ctx, u.String(), 0, 0)
	if err != nil {
		return nil, nil, err
	}
	pl, err := m3u8.Parse(body)
	if err != nil {
		return nil, nil, fmt.Errorf("hlsflv: parsing playlist %s: %w", u, err)
	}
	return pl, finalURL, nil
}

// selectVariant picks the most suitable variant from a master playlist.
//
// Variants that FLV cannot carry are skipped, then the highest bandwidth
// variant is chosen, which is what a desktop browser would do.
func (c *Client) selectVariant(ctx context.Context, pl *m3u8.Playlist) (*m3u8.Playlist, *url.URL, error) {
	if pl.Master == nil || len(pl.Master.Variants) == 0 {
		return nil, nil, errors.New("hlsflv: master playlist without variants")
	}
	candidates := make([]m3u8.Variant, 0, len(pl.Master.Variants))
	for _, v := range pl.Master.Variants {
		if v.IFrameOnly || v.URI == "" {
			continue
		}
		if v.Codecs != "" && !codecsSupported(v.Codecs) {
			continue
		}
		candidates = append(candidates, v)
	}
	if len(candidates) == 0 {
		return nil, nil, errors.New("hlsflv: no playable variant in master playlist")
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Bandwidth > candidates[j].Bandwidth
	})
	var lastErr error
	for _, v := range candidates {
		ref, err := url.Parse(v.URI)
		if err != nil {
			lastErr = err
			continue
		}
		target := c.base.ResolveReference(ref)
		sub, finalURL, err := c.load(ctx, target)
		if err != nil {
			lastErr = err
			continue
		}
		if sub.Media != nil && len(sub.Media.Segments) > 0 {
			return sub, finalURL, nil
		}
		lastErr = fmt.Errorf("hlsflv: variant %s has no segments", target)
	}
	if lastErr == nil {
		lastErr = errors.New("hlsflv: no usable variant")
	}
	return nil, nil, lastErr
}

// follow processes segments in order, reloading the playlist as it goes.
func (c *Client) follow(ctx context.Context, pl *m3u8.MediaPlaylist) error {
	for {
		segments, err := c.pending(ctx, pl)
		if err != nil {
			return err
		}
		for _, seg := range segments {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := c.processSegment(ctx, pl, seg); err != nil {
				c.opts.logf("hlsflv: segment %s failed: %v", seg.URI, err)
			}
		}
		if pl.EndList {
			return nil
		}
		next, err := c.reload(ctx, pl)
		if err != nil {
			return err
		}
		pl = next
	}
}

// pending returns the segments that have not been processed yet. The first
// call trims a live backlog; later calls only look at newly appended entries.
func (c *Client) pending(ctx context.Context, pl *m3u8.MediaPlaylist) ([]m3u8.MediaSegment, error) {
	segs := pl.Segments
	if len(segs) == 0 {
		return nil, nil
	}
	// Have any of the playlist's segments been processed already? If none have,
	// this is the first pass over the playlist.
	first := true
	for _, s := range segs {
		if c.seen[c.segmentKey(pl, s)] {
			first = false
			break
		}
	}
	if first {
		if !pl.EndList && c.opts.StartFromLive && len(segs) > maxMirroredLiveSegments {
			// Skip the buffered backlog and start near the live edge.
			segs = segs[len(segs)-maxMirroredLiveSegments:]
		}
	}
	out := make([]m3u8.MediaSegment, 0, len(segs))
	for _, s := range segs {
		if c.seen[c.segmentKey(pl, s)] {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// segmentKey uniquely identifies a segment across playlist reloads.
func (c *Client) segmentKey(pl *m3u8.MediaPlaylist, s m3u8.MediaSegment) string {
	seq := s.Sequence
	if seq == 0 && !pl.HasMediaSequence {
		// Playlists without EXT-X-MEDIA-SEQUENCE reuse the URI as identity.
		return s.URI
	}
	return strconv.FormatUint(seq, 10) + "\x00" + s.URI
}

// reload fetches a fresh copy of the playlist, waiting for it to change.
func (c *Client) reload(ctx context.Context, prev *m3u8.MediaPlaylist) (*m3u8.MediaPlaylist, error) {
	interval := time.Duration(prev.TargetDuration * float64(time.Second))
	if interval <= 0 {
		interval = c.opts.PlaylistInterval
	}
	// Poll somewhat faster than the target duration so no segment is missed,
	// but never hammer the origin.
	wait := interval / 2
	if wait < c.opts.PlaylistInterval {
		wait = c.opts.PlaylistInterval
	}
	if wait > 5*time.Second {
		wait = 5 * time.Second
	}

	var lastErr error
	for attempt := 0; attempt < c.opts.MaxPlaylistReloads; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		pl, finalURL, err := c.load(ctx, c.base)
		if err != nil {
			lastErr = err
			c.opts.logf("hlsflv: playlist reload failed: %v", err)
			continue
		}
		if pl.Media == nil || pl.Kind == m3u8.KindMaster {
			// The variant playlist changed shape; re-resolve from the master.
			if pl.Kind == m3u8.KindMaster {
				sub, variantURL, err := c.selectVariant(ctx, pl)
				if err != nil {
					lastErr = err
					continue
				}
				c.base = variantURL
				pl = sub
			}
		}
		if pl.Media == nil {
			lastErr = errors.New("hlsflv: reloaded playlist is not a media playlist")
			continue
		}
		c.base = finalURL
		if pl.Media.EndList || hasNewSegments(prev, pl.Media, c) {
			return pl.Media, nil
		}
		prev = pl.Media
	}
	if lastErr == nil {
		lastErr = errors.New("hlsflv: playlist did not change")
	}
	return nil, lastErr
}

func hasNewSegments(prev *m3u8.MediaPlaylist, next *m3u8.MediaPlaylist, c *Client) bool {
	for _, s := range next.Segments {
		if !c.seen[c.segmentKey(next, s)] {
			return true
		}
	}
	return false
}

// processSegment downloads one segment and emits its samples.
func (c *Client) processSegment(ctx context.Context, pl *m3u8.MediaPlaylist, seg m3u8.MediaSegment) error {
	key := c.segmentKey(pl, seg)
	// Mark as seen before downloading so a failure is not retried forever.
	c.seen[key] = true

	if seg.Key.Encrypted() {
		return fmt.Errorf("hlsflv: segment %s is encrypted with %s, which FLV cannot carry", seg.URI, seg.Key.Method)
	}

	u, err := c.resolve(seg.URI)
	if err != nil {
		return err
	}
	body, _, err := c.get(ctx, u.String(), seg.ByteRange, seg.ByteRangeOffset)
	if err != nil {
		// A transient download failure should be retried on the next pass
		// rather than silently skipped, which would leave a hole in the
		// timeline. Segments may only be dropped once they are unreachable,
		// so the marker is removed and the failure surfaced.
		delete(c.seen, key)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("hlsflv: fetching %s: %w", seg.URI, err)
	}
	c.stats.Segments++

	if err := c.ensureInit(ctx, pl, seg); err != nil {
		return err
	}
	if c.info == nil {
		return errors.New("hlsflv: no initialisation segment available")
	}

	frag, err := mp4.ParseFragment(c.info, body, mp4.FragmentOptions{})
	if err != nil {
		// A malformed or partially fetched segment must not kill the stream;
		// drop it and resynchronise on the next initialisation segment.
		return err
	}
	c.observeFrameRate(frag.Samples)
	if err := c.emit(frag.Samples); err != nil {
		return err
	}
	return nil
}

// ensureInit loads the initialisation segment when the mapping changed.
func (c *Client) ensureInit(ctx context.Context, pl *m3u8.MediaPlaylist, seg m3u8.MediaSegment) error {
	m := seg.Map
	if m == nil {
		m = pl.Map
	}
	if m == nil {
		return errors.New("hlsflv: media playlist has no EXT-X-MAP; MPEG-ms segments are not supported")
	}
	if c.info != nil && c.mapURL == m.URI && c.mapRange == m.ByteRange && c.mapRangeOff == m.ByteRangeOffset {
		return nil
	}
	u, err := c.resolve(m.URI)
	if err != nil {
		return err
	}
	body, _, err := c.get(ctx, u.String(), m.ByteRange, m.ByteRangeOffset)
	if err != nil {
		return fmt.Errorf("hlsflv: fetching initialisation segment %s: %w", m.URI, err)
	}
	info, err := mp4.ParseInit(body)
	if err != nil {
		return fmt.Errorf("hlsflv: parsing initialisation segment %s: %w", m.URI, err)
	}
	video, audio := info.RequiredTracks()
	if video == nil && audio == nil {
		return fmt.Errorf("hlsflv: initialisation segment %s has no FLV compatible tracks", m.URI)
	}
	// A new initialisation segment means a stream discontinuity: timestamps
	// must be rebased, and sequence headers must be re-sent for the new
	// decoder configuration.
	rebased := c.info != nil
	c.info, c.video, c.audio = info, video, audio
	c.mapURL, c.mapRange, c.mapRangeOff = m.URI, m.ByteRange, m.ByteRangeOffset
	if rebased {
		c.clocks = map[uint32]*trackClock{}
		c.videoSeq, c.audioSeq = false, false
		c.frameRateVotes = 0
	}
	// The file header carries one flag per media type and can only be written
	// once, so it must be emitted now that the track layout is known.
	c.w.SetFlags(c.audio != nil, c.video != nil)
	if err := c.w.WriteHeader(); err != nil {
		return fmt.Errorf("hlsflv: writing the FLV header: %w", err)
	}
	c.writeMetadata(seg)
	return c.writeSequenceHeaders()
}

// writeMetadata emits the onMetaData script tag describing the stream. It is
// written before the sequence headers of each new decoder configuration.
func (c *Client) writeMetadata(seg m3u8.MediaSegment) {
	if c.video == nil && c.audio == nil {
		return
	}
	md := flv.Metadata{Encoder: "blrechelper"}
	if c.video != nil {
		md.Width = int(c.video.Width)
		md.Height = int(c.video.Height)
		md.VideoCodecID = flv.CodecAVC
		md.FrameRate = c.frameRate
	}
	if c.audio != nil {
		md.AudioSampleRate = float64(c.audio.SampleRate)
		md.AudioSampleSize = 16
		md.AudioChannels = float64(c.audio.Channels)
		md.Stereo = c.audio.Channels > 1
		md.AudioCodecID = flv.SoundAAC
	}
	// The script tag timestamp is zero because it precedes all media.
	if err := c.w.WriteMetadata(0, md); err != nil {
		c.opts.logf("hlsflv: writing metadata: %v", err)
	}
}

// observeFrameRate derives the video frame rate from real sample durations and
// keeps the most common value seen so far.
//
// It only feeds the cosmetic framerate field of onMetaData; playback timing
// comes from per sample timestamps.
func (c *Client) observeFrameRate(samples []mp4.Sample) {
	if c.video == nil || c.video.Timescale == 0 {
		return
	}
	counts := map[int]int{}
	for i := range samples {
		s := &samples[i]
		if s.TrackID != c.video.ID || s.Duration == 0 {
			continue
		}
		fps := int((float64(c.video.Timescale)/float64(s.Duration) + 0.5))
		if fps > 0 && fps <= 240 {
			counts[fps]++
		}
	}
	for fps, n := range counts {
		if n > c.frameRateVotes {
			c.frameRateVotes = n
			c.frameRate = float64(fps)
		}
	}
}

// reportConfigTags publishes the metadata and sequence header tags.
func (c *Client) reportConfigTags(scriptSize int) {
	if c.tag == nil {
		return
	}
	if scriptSize > 0 {
		c.reportTag(TagInfo{Type: flv.TagScript, Size: scriptSize})
	}
	if c.videoSeq {
		c.reportTag(TagInfo{Type: flv.TagVideo, Keyframe: true})
	}
	if c.audioSeq {
		c.reportTag(TagInfo{Type: flv.TagAudio})
	}
}

// writeSequenceHeaders emits the decoder configuration records.
func (c *Client) writeSequenceHeaders() error {
	if c.video != nil {
		switch {
		case c.video.AvcC != nil:
			if err := c.w.WriteAVCSequenceHeader(c.video.AvcC); err != nil {
				return err
			}
			c.videoSeq = true
		case c.video.HvcC != nil:
			if err := c.w.WriteHEVCSequenceHeader(c.video.HvcC); err != nil {
				return err
			}
			c.videoSeq = true
		}
	}
	if c.audio != nil {
		switch {
		case strings.EqualFold(c.audio.Format, "mp4a") && c.audio.AudioSpecificConfig != nil:
			if err := c.w.WriteAACSequenceHeader(c.audio.AudioSpecificConfig); err != nil {
				return err
			}
			c.audioSeq = true
		case c.audio.OpusDOps != nil:
			if err := c.w.WriteOpusSequenceHeader(c.audio.OpusDOps); err != nil {
				return err
			}
			c.audioSeq = true
		}
	}
	return nil
}

// emit writes one segment's samples in presentation order.
//
// Samples must be interleaved by presentation timestamp for players that
// expect well ordered tags. Because the tracks of a stream may use different
// timescales and unrelated baseDecodeTime values, each track's timeline origin
// is pinned first and the comparison is then made in milliseconds.
func (c *Client) emit(samples []mp4.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	for i := range samples {
		tr := c.info.TrackByID(samples[i].TrackID)
		if tr == nil {
			continue
		}
		c.clockFor(samples[i].TrackID, tr.Timescale).setBase(samples[i].DTS)
	}

	ordered := make([]mp4.Sample, len(samples))
	copy(ordered, samples)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := &ordered[i], &ordered[j]
		at, bt := c.info.TrackByID(a.TrackID), c.info.TrackByID(b.TrackID)
		ats, bts := uint32(1), uint32(1)
		if at != nil && at.Timescale > 0 {
			ats = at.Timescale
		}
		if bt != nil && bt.Timescale > 0 {
			bts = bt.Timescale
		}
		return c.presentationMS(a, ats) < c.presentationMS(b, bts)
	})
	for i := range ordered {
		if err := c.writeSample(&ordered[i]); err != nil {
			return err
		}
	}
	return nil
}

// presentationMS returns a sample's presentation timestamp in milliseconds
// relative to its track's pinned origin.
func (c *Client) presentationMS(s *mp4.Sample, timescale uint32) int64 {
	clk := c.clocks[s.TrackID]
	if clk == nil || !clk.hasBase {
		return s.DTS + s.CTS
	}
	return (s.DTS+s.CTS-clk.base)*1000/int64(timescale) + clk.offset
}

// clockFor returns the clock for a track, creating it when necessary.
func (c *Client) clockFor(trackID uint32, timescale uint32) *trackClock {
	clk := c.clocks[trackID]
	if clk == nil {
		clk = &trackClock{}
		c.clocks[trackID] = clk
	}
	if clk.timescale == 0 {
		clk.timescale = timescale
	}
	return clk
}

// writeSample converts one sample to an FLV tag.
func (c *Client) writeSample(s *mp4.Sample) error {
	tr := c.info.TrackByID(s.TrackID)
	if tr == nil {
		return nil
	}
	clk := c.clockFor(s.TrackID, tr.Timescale)

	// The very first AAC packet of a stream is the encoder priming frame: the
	// FLV specification says its timestamp is zero and it carries no audio, so
	// it is consumed to establish the timeline and then dropped. This happens
	// once per stream, not once per initialisation segment.
	if tr.Audio() && strings.EqualFold(tr.Format, "mp4a") && !clk.primingConsumed {
		clk.primingConsumed = true
		clk.toMilliseconds(s.DTS, s.CTS, tr.Timescale)
		return nil
	}

	ms, cts := clk.toMilliseconds(s.DTS, s.CTS, tr.Timescale)
	if ms < 0 {
		ms = 0
	}

	switch {
	case tr.Video() && tr.AvcC != nil:
		if err := c.w.WriteAVCNALU(s.Data, ms, cts, s.Keyframe); err != nil {
			return err
		}
		c.stats.VideoTags++
		c.reportTag(TagInfo{Type: flv.TagVideo, Timestamp: ms, Keyframe: s.Keyframe, Size: len(s.Data)})
	case tr.Video() && tr.HvcC != nil:
		if err := c.w.WriteHEVCNALU(s.Data, ms, cts, s.Keyframe); err != nil {
			return err
		}
		c.stats.VideoTags++
		c.reportTag(TagInfo{Type: flv.TagVideo, Timestamp: ms, Keyframe: s.Keyframe, Size: len(s.Data)})
	case tr.Audio() && strings.EqualFold(tr.Format, "mp4a"):
		if err := c.w.WriteAACRaw(s.Data, ms); err != nil {
			return err
		}
		c.stats.AudioTags++
		c.reportTag(TagInfo{Type: flv.TagAudio, Timestamp: ms, Size: len(s.Data)})
	case tr.Audio() && tr.OpusDOps != nil:
		if err := c.w.WriteOpusRaw(s.Data, ms); err != nil {
			return err
		}
		c.stats.AudioTags++
		c.reportTag(TagInfo{Type: flv.TagAudio, Timestamp: ms, Size: len(s.Data)})
	default:
		return nil
	}
	clk.samples++
	c.stats.Samples++
	c.stats.Bytes += int64(len(s.Data))
	return nil
}

// resolve turns a playlist reference into an absolute URL.
func (c *Client) resolve(ref string) (*url.URL, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("hlsflv: bad reference %q: %w", ref, err)
	}
	return c.base.ResolveReference(u), nil
}

// applyRequestHeaders sets the caller supplied headers on a request. Set is
// used rather than Add so a caller value replaces the default.
func applyRequestHeaders(req *http.Request, headers []RequestHeader) {
	for _, h := range headers {
		if h.Value == "" {
			// An empty value means "do not send this header at all", which is
			// how a caller suppresses the default Referer.
			req.Header.Del(h.Name)
			continue
		}
		req.Header.Set(h.Name, h.Value)
	}
}

// get performs one upstream request, optionally restricted to a byte range,
// retrying transient failures.
// It returns the body and the final URL after redirects.
func (c *Client) get(ctx context.Context, rawURL string, length, offset int64) ([]byte, *url.URL, error) {
	var lastErr error
	for attempt := 0; attempt < requestAttempts; attempt++ {
		if attempt > 0 {
			// Back off briefly; a CDN hiccup is usually gone by the retry.
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 300 * time.Millisecond):
			}
		}
		body, finalURL, err := c.getOnce(ctx, rawURL, length, offset)
		if err == nil {
			return body, finalURL, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
	}
	return nil, nil, lastErr
}

// requestAttempts bounds how often a single upstream request is retried.
const requestAttempts = 3

// getOnce performs a single upstream request.
func (c *Client) getOnce(ctx context.Context, rawURL string, length, offset int64) ([]byte, *url.URL, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", c.opts.UserAgent)
	req.Header.Set("Accept", "*/*")
	if c.opts.Referer != "" {
		req.Header.Set("Referer", c.opts.Referer)
	} else if req.URL.Host != c.base.Host {
		req.Header.Set("Referer", c.base.Scheme+"://"+c.base.Host+"/")
	}
	for k, vs := range c.opts.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// Headers supplied by the caller win, so a stream that needs a specific
	// User-Agent or Referer can be played from a plain URL.
	applyRequestHeaders(req, c.opts.PerRequestHeader)
	if length > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	}

	resp, err := c.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	default:
		return nil, nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	// A truncated read means the connection dropped mid-transfer; retrying is
	// worthwhile, and a partial segment must never be parsed.
	if resp.ContentLength >= 0 && int64(len(body)) != resp.ContentLength {
		return nil, nil, fmt.Errorf("truncated response: got %d of %d bytes", len(body), resp.ContentLength)
	}
	if length > 0 {
		if resp.StatusCode == http.StatusPartialContent {
			if int64(len(body)) != length {
				return nil, nil, fmt.Errorf("short range response: got %d of %d bytes", len(body), length)
			}
		} else if int64(len(body)) > offset+length {
			body = body[offset : offset+length]
		}
	}
	return body, resp.Request.URL, nil
}

// trackClock converts track timescale timestamps into a monotonic millisecond
// timeline that starts at zero.
//
// Each track is rebased on its own first sample, which makes the two tracks of
// a stream line up without depending on the absolute baseDecodeTime values,
// and avoids the 32 bit overflow an FLV timestamp field would otherwise hit on
// long running streams.
type trackClock struct {
	base    int64
	hasBase bool
	offset  int64
	lastOut int64
	samples int64

	// timescale of the track this clock belongs to.
	timescale uint32

	// primingConsumed records that the AAC priming frame of this stream has
	// already been dropped. It must survive an initialisation segment change,
	// because a stream has only one priming frame.
	primingConsumed bool
}

// maxClockJumpMS bounds a forward timestamp jump so a corrupt segment cannot
// push playback arbitrarily far ahead.
const maxClockJumpMS = 10000

// setBase pins the timeline origin of a track, if it is not pinned yet.
func (t *trackClock) setBase(dts int64) {
	if !t.hasBase {
		t.base = dts
		t.hasBase = true
		t.lastOut = -1
	}
}

// toMilliseconds returns the output timestamp in milliseconds and the
// composition time offset, both derived from track timescale units.
func (t *trackClock) toMilliseconds(dts, cts int64, timescale uint32) (int64, int32) {
	ts := int64(timescale)
	if ts <= 0 {
		ts = 1
	}
	t.setBase(dts)
	out := (dts-t.base)*1000/ts + t.offset

	if t.lastOut >= 0 {
		switch {
		case out < t.lastOut:
			// The source moved backwards. Teleport the track forward so that
			// timestamps stay strictly increasing, which FLV players require.
			t.offset += t.lastOut - out + 1
			out = t.lastOut + 1
		case out-t.lastOut > maxClockJumpMS:
			t.offset -= out - t.lastOut - 1
			out = t.lastOut + 1
		}
	}
	t.lastOut = out

	comp := cts * 1000 / ts
	return out, int32(comp)
}

// codecsSupported reports whether every codec in an RFC 6381 CODECS list can
// be carried in FLV.
func codecsSupported(codecs string) bool {
	for _, c := range strings.Split(codecs, ",") {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		switch {
		case strings.HasPrefix(c, "avc1"), strings.HasPrefix(c, "avc3"),
			strings.HasPrefix(c, "hvc1"), strings.HasPrefix(c, "hev1"),
			strings.HasPrefix(c, "mp4a"), strings.HasPrefix(c, "opus"),
			strings.HasPrefix(c, "flac"):
		default:
			return false
		}
	}
	return true
}
