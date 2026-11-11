//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

// leafHandleTimeout bounds every Handle call in this file, so a regression
// that blocks on the agent-info.json leaf (for example a plain read of a
// FIFO with no writer) fails on an assertion instead of hanging the run.
const leafHandleTimeout = 10 * time.Second

// statusRecorder is a minimal hub that records the status updates the
// HubHandler sends.
type statusRecorder struct {
	mu       sync.Mutex
	statuses []string
	other    int
}

func (s *statusRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/test-agent-id/status" {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		status, _ := payload["status"].(string)
		s.statuses = append(s.statuses, status)
	} else {
		s.other++
	}
	_, _ = w.Write([]byte(`{}`))
}

func (s *statusRecorder) snapshot() (statuses []string, other int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.statuses...), s.other
}

// newLeafTestHandler points HOME at a fresh temp dir and the hub client env
// at a recorder, using synthetic values only. It returns the handler, the
// recorder and HOME.
func newLeafTestHandler(t *testing.T) (*HubHandler, *statusRecorder, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	rec := &statusRecorder{}
	server := httptest.NewServer(rec)
	t.Cleanup(server.Close)

	scrubHubEnv(t)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "test-agent-id")

	handler := NewHubHandler()
	if handler == nil {
		t.Fatal("expected handler to be created")
	}
	return handler, rec, home
}

// handleToolEndBounded runs Handle(EventToolEnd) bounded by
// leafHandleTimeout. If it does not return in time, release (when non-nil)
// is called to unblock the handler before the test fails.
func handleToolEndBounded(t *testing.T, handler *HubHandler, release func()) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- handler.Handle(&hooks.Event{Name: hooks.EventToolEnd})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
	case <-time.After(leafHandleTimeout):
		if release != nil {
			release()
		}
		select {
		case <-done:
		case <-time.After(leafHandleTimeout):
		}
		t.Fatalf("Handle(%s) did not return within %v: the agent-info.json leaf blocked the handler", hooks.EventToolEnd, leafHandleTimeout)
	}
}

// assertSingleWorking asserts the handler sent exactly one status update,
// "working": the result for a missing agent-info.json on tool-end.
func assertSingleWorking(t *testing.T, rec *statusRecorder) {
	t.Helper()
	statuses, other := rec.snapshot()
	if other != 0 {
		t.Errorf("unexpected non-status hub requests: %d", other)
	}
	if len(statuses) != 1 || statuses[0] != "working" {
		t.Fatalf("status updates sent to hub = %q, want exactly [\"working\"] (same as a missing agent-info.json)", statuses)
	}
}

// A FIFO at agent-info.json is treated as unknown local activity, exactly
// like a missing file: the handler neither blocks on it nor skips the
// update. The test never opens the FIFO for reading.
func TestHubHandler_LocalActivityFIFOLeafTreatedAsMissing(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		handler, rec, _ := newLeafTestHandler(t)
		handleToolEndBounded(t, handler, nil)
		assertSingleWorking(t, rec)
	})

	t.Run("fifo", func(t *testing.T) {
		handler, rec, home := newLeafTestHandler(t)
		fifo := filepath.Join(home, "agent-info.json")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) {
				t.Skipf("FIFOs unsupported here (mkfifo: %v)", err)
			}
			t.Fatalf("mkfifo: %v", err)
		}

		release := func() {
			// Opening the write side (non-blocking, never the read side)
			// completes a reader stuck in open(2) on the FIFO; it then sees
			// EOF once this fd is closed, so the hung handler goroutine is
			// released instead of leaking past the test.
			fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if err != nil {
				t.Logf("open FIFO write side to release handler: %v", err)
				return
			}
			_ = syscall.Close(fd)
		}
		handleToolEndBounded(t, handler, release)
		assertSingleWorking(t, rec)
	})
}

// An agent-info.json with a second hardlink is refused even though it holds
// a sticky activity: the handler behaves as for a missing file and sends
// "working". The single-link control proves the same content is sticky
// when read.
func TestHubHandler_LocalActivityHardlinkedLeafRefused(t *testing.T) {
	sticky := []byte(`{"activity":"` + stickyActivity + `"}`)

	t.Run("single link", func(t *testing.T) {
		handler, rec, home := newLeafTestHandler(t)
		writeAgentInfo(t, home, sticky)

		handleToolEndBounded(t, handler, nil)
		statuses, other := rec.snapshot()
		if other != 0 {
			t.Errorf("unexpected non-status hub requests: %d", other)
		}
		if len(statuses) != 0 {
			t.Fatalf("status updates sent to hub = %q, want none (sticky local activity)", statuses)
		}
	})

	t.Run("hardlinked", func(t *testing.T) {
		handler, rec, home := newLeafTestHandler(t)
		linkSrc := filepath.Join(home, "other.json")
		if err := os.WriteFile(linkSrc, sticky, 0o600); err != nil {
			t.Fatalf("write other.json: %v", err)
		}
		if err := os.Link(linkSrc, filepath.Join(home, "agent-info.json")); err != nil {
			if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EXDEV) {
				t.Skipf("hardlinks unsupported here (link: %v)", err)
			}
			t.Fatalf("link: %v", err)
		}

		handleToolEndBounded(t, handler, nil)
		assertSingleWorking(t, rec)
	})
}
