// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtimebroker

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// levelRecorder is a slog.Handler that records the level and message of
// every record, at every level.
type levelRecorder struct {
	mu         sync.Mutex
	records    []slog.Record
	firstError chan struct{} // closed at the first Error record
	once       sync.Once
}

func newLevelRecorder() *levelRecorder { return &levelRecorder{firstError: make(chan struct{})} }

func (h *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *levelRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r)
	h.mu.Unlock()
	if r.Level >= slog.LevelError {
		h.once.Do(func() { close(h.firstError) })
	}
	return nil
}
func (h *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecorder) WithGroup(string) slog.Handler      { return h }

func (h *levelRecorder) errorMessages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		if r.Level >= slog.LevelError {
			out = append(out, r.Message)
		}
	}
	return out
}

func newLoggedCloseTestClient(endpoint string, rec *levelRecorder) *ControlChannelClient {
	cfg := DefaultControlChannelConfig()
	cfg.HubEndpoint = endpoint
	cfg.BrokerID = "b1"
	cfg.ReconnectInitial = time.Hour
	cfg.ReconnectMax = time.Hour
	return NewControlChannelClient(cfg, http.NotFoundHandler(), nil, "", slog.New(rec))
}

// TestControlChannelClient_CloseDuringDialLogsNoError: a connect that fails
// because the client was closed (here, Close ends a dial blocked on a Hub
// that never answers) is the ordinary end of the connect loop and logs no
// Error record; a connect that fails for another reason still does.
func TestControlChannelClient_CloseDuringDialLogsNoError(t *testing.T) {
	t.Run("closed", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		accepted := make(chan struct{}, 1)
		var held sync.Mutex
		var conns []net.Conn
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				held.Lock()
				conns = append(conns, conn)
				held.Unlock()
				select {
				case accepted <- struct{}{}:
				default:
				}
			}
		}()
		t.Cleanup(func() {
			held.Lock()
			defer held.Unlock()
			for _, conn := range conns {
				_ = conn.Close()
			}
		})

		rec := newLevelRecorder()
		c := newLoggedCloseTestClient("http://"+ln.Addr().String(), rec)
		connectDone := make(chan error, 1)
		go func() { connectDone <- c.Connect(context.Background()) }()
		waitChan(t, accepted, "the dial to reach the Hub")
		closeDone := make(chan error, 1)
		go func() { closeDone <- c.Close() }()
		_ = waitChan(t, closeDone, "Close to return")
		if err := waitChan(t, connectDone, "the connect loop to end after Close"); err == nil {
			t.Error("Connect returned nil after Close, want the cancellation error")
		}
		if msgs := rec.errorMessages(); len(msgs) != 0 {
			t.Errorf("a connect ended by Close (client closed) logged Error records: %q", msgs)
		}
	})

	t.Run("failed", func(t *testing.T) {
		attempted := make(chan struct{}, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case attempted <- struct{}{}:
			default:
			}
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)

		rec := newLevelRecorder()
		c := newLoggedCloseTestClient(srv.URL, rec)
		connectDone := make(chan error, 1)
		go func() { connectDone <- c.Connect(context.Background()) }()
		waitChan(t, attempted, "the first connect attempt")
		// The failure is logged before the loop waits out its backoff;
		// Close then ends the wait.
		waitChan(t, (<-chan struct{})(rec.firstError), "the Error record of the refused connect")
		_ = c.Close()
		if err := waitChan(t, connectDone, "the connect loop to end after Close"); err == nil {
			t.Error("Connect returned nil after Close, want the cancellation error")
		}
		if msgs := rec.errorMessages(); len(msgs) == 0 {
			t.Error("a connect refused by the Hub logged no Error record")
		}
	})
}
