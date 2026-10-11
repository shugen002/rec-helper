package hlsflv

import (
	"context"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestNewSessionIDFormat pins the shape of a session ID: eight hexadecimal
// characters.
func TestNewSessionIDFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, err := NewSessionID()
		if err != nil {
			t.Fatalf("NewSessionID: %v", err)
		}
		if len(id) != SessionIDLength {
			t.Fatalf("session ID %q has length %d, want %d", id, len(id), SessionIDLength)
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Fatalf("session ID %q is not hexadecimal: %v", id, err)
		}
		if id != strings.ToLower(id) {
			t.Fatalf("session ID %q must be lower case hexadecimal", id)
		}
		if seen[id] {
			t.Fatalf("session ID %q was generated twice in 200 draws", id)
		}
		seen[id] = true
	}
}

// TestSessionIDAppearsOnEveryLogLine checks that all lines a conversion writes
// carry the same session ID, which is what makes concurrent streams readable.
func TestSessionIDAppearsOnEveryLogLine(t *testing.T) {
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
	id := c.SessionID()
	if len(id) != SessionIDLength {
		t.Fatalf("SessionID = %q, want %d hex characters", id, SessionIDLength)
	}

	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	prefix := "[" + id + "] "
	lines := events.all()
	if len(lines) == 0 {
		t.Fatal("no event lines were logged")
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			t.Errorf("log line %q does not start with %q", line, prefix)
		}
	}
	// A successful run logs nothing to the progress logger.
	if len(progress.all()) != 0 {
		t.Errorf("progress logger got %v, want nothing", progress.all())
	}
}

// TestSessionIDsDifferBetweenStreams checks that two simultaneous conversions
// do not share an ID.
func TestSessionIDsDifferBetweenStreams(t *testing.T) {
	o := newOrigin()
	srv := httptest.NewServer(o.handler())
	defer srv.Close()

	var mu sync.Mutex
	ids := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf strings.Builder
			c, err := New(srv.URL+"/index.m3u8", &buf, Options{
				EventLogger: func(string, ...any) {},
			})
			if err != nil {
				return
			}
			mu.Lock()
			ids[c.SessionID()] = true
			mu.Unlock()
			_ = c.Run(context.Background())
		}()
	}
	wg.Wait()
	if len(ids) != 8 {
		t.Errorf("got %d distinct session IDs from 8 streams, want 8", len(ids))
	}
}
