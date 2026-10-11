package hlsflv

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// logRecorder collects event log lines.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logRecorder) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// find returns the first line containing substr.
func (l *logRecorder) find(substr string) string {
	for _, line := range l.all() {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}

// TestEventLogsCoverTheStreamLifecycle checks the three lines an operator needs:
// which playlist was fetched, what the stream contains, and how it ended.
func TestEventLogsCoverTheStreamLifecycle(t *testing.T) {
	o := newOrigin()
	srv := httptest.NewServer(o.handler())
	defer srv.Close()

	logs := &logRecorder{}
	var buf strings.Builder
	c, err := New(srv.URL+"/index.m3u8", &buf, Options{EventLogger: logs.logf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 1. The playlist URL.
	if line := logs.find("fetching playlist"); !strings.Contains(line, "/index.m3u8") {
		t.Errorf("no playlist log line, got: %v", logs.all())
	}

	// 2. The stream info: codecs, dimensions and sample rate.
	info := logs.find("stream info")
	for _, want := range []string{"avc1.64002a", "1920x1080", "mp4a.40.2", "48000Hz", "2ch"} {
		if !strings.Contains(info, want) {
			t.Errorf("stream info line %q does not mention %q", info, want)
		}
	}

	// 3. The end of the stream, with the reason and the totals.
	end := logs.find("stream ended")
	if !strings.Contains(end, "reason=end of stream") {
		t.Errorf("end line %q does not report the reason", end)
	}
	if !strings.Contains(end, "2 segments") {
		t.Errorf("end line %q does not report the segment count", end)
	}
}

// TestEventLogReportsCancellation checks that a cancelled conversion is
// distinguishable from one that ran to the end of the playlist.
func TestEventLogReportsCancellation(t *testing.T) {
	o := newOrigin()
	logs := &logRecorder{}
	var buf strings.Builder
	c, err := New(httptest.NewServer(o.handler()).URL+"/index.m3u8", &buf, Options{EventLogger: logs.logf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = c.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if line := logs.find("stream ended"); !strings.Contains(line, "reason=cancelled") {
		t.Errorf("end line %q does not report cancellation", line)
	}
}

// TestEventLogReportsClientDisconnect checks that a consumer going away is
// reported as such, which is how a player that stops is told apart from an
// upstream failure.
func TestEventLogReportsClientDisconnect(t *testing.T) {
	o := newOrigin()
	logs := &logRecorder{}
	w := &failWriter{limit: 32}
	c, err := New(httptest.NewServer(o.handler()).URL+"/index.m3u8", w, Options{
		EventLogger: logs.logf,
		Logger:      func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if line := logs.find("stream ended"); !strings.Contains(line, "reason=client disconnected") {
		t.Errorf("end line %q does not report the disconnect", line)
	}
}

// TestEventLogReportsUpstreamFailure checks that an unrecoverable error is
// reported with its message.
func TestEventLogReportsUpstreamFailure(t *testing.T) {
	logs := &logRecorder{}
	var buf strings.Builder
	c, err := New("http://127.0.0.1:1/index.m3u8", &buf, Options{
		EventLogger:    logs.logf,
		RequestTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded against an unreachable origin")
	}
	if line := logs.find("stream ended"); !strings.Contains(line, "reason=error:") {
		t.Errorf("end line %q does not report the error", line)
	}
}

// TestEventLogIsSeparateFromProgressLog checks that turning per segment
// progress off does not silence the lifecycle events.
func TestEventLogIsSeparateFromProgressLog(t *testing.T) {
	o := newOrigin()
	srv := httptest.NewServer(o.handler())
	defer srv.Close()

	events := &logRecorder{}
	progress := &logRecorder{}
	var buf strings.Builder
	c, err := New(srv.URL+"/index.m3u8", &buf, Options{
		EventLogger: events.logf,
		Logger:      progress.logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if events.find("stream info") == "" || events.find("stream ended") == "" {
		t.Error("lifecycle events must be reported through EventLogger")
	}
	// Nothing failed, so the progress logger stays quiet.
	if len(progress.all()) != 0 {
		t.Errorf("progress logger received %v, want nothing", progress.all())
	}
}
