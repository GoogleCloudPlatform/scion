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

package hub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

func TestWriteGate(t *testing.T) {
	type step struct {
		advance   time.Duration
		action    string // "allow", "401", "403", "500", "200", "refused", "reset"
		wantPause bool   // for "allow"
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{name: "no refused refresh: 401s never pause", steps: []step{
			{action: "401"}, {action: "allow"}, {action: "401"}, {action: "allow"},
		}},
		{name: "refused refresh then 401 pauses for the backoff", steps: []step{
			{action: "refused"}, {action: "allow"}, {action: "401"},
			{action: "allow", wantPause: true},
			{advance: refusedWriteBackoff - time.Second, action: "allow", wantPause: true},
			{advance: time.Second, action: "allow"},
		}},
		{name: "refused refresh: non-401 errors do not pause", steps: []step{
			{action: "refused"}, {action: "403"}, {action: "allow"}, {action: "500"}, {action: "allow"},
		}},
		{name: "success clears the streak", steps: []step{
			{action: "refused"}, {action: "401"},
			{advance: refusedWriteBackoff, action: "allow"}, {action: "200"},
			{advance: refusedWriteStopAfter, action: "401"},
			{action: "allow", wantPause: true},
			{advance: refusedWriteBackoff, action: "allow"},
		}},
		{name: "401s continuing past the stop window stop writes", steps: []step{
			{action: "refused"}, {action: "401"},
			{advance: refusedWriteStopAfter - time.Second, action: "401"},
			{advance: refusedWriteBackoff, action: "allow"},
			{advance: time.Second, action: "401"},
			{action: "allow", wantPause: true},
			{advance: time.Hour, action: "allow", wantPause: true},
		}},
		{name: "reset reopens a stopped gate", steps: []step{
			{action: "refused"}, {action: "401"},
			{advance: refusedWriteStopAfter, action: "401"},
			{action: "allow", wantPause: true},
			{action: "reset"}, {action: "allow"},
			{action: "401"}, {action: "allow"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
			g := &writeGate{now: clk.now}
			for i, s := range tt.steps {
				clk.advance(s.advance)
				switch s.action {
				case "allow":
					err := g.allow()
					if paused := errors.Is(err, ErrHubWritesPaused); paused != s.wantPause {
						t.Fatalf("step %d: allow paused = %v, want %v (err %v)", i, paused, s.wantPause, err)
					}
				case "401":
					g.observe(http.StatusUnauthorized)
				case "403":
					g.observe(http.StatusForbidden)
				case "500":
					g.observe(http.StatusInternalServerError)
				case "200":
					g.observe(http.StatusOK)
				case "refused":
					g.refreshRefused()
				case "reset":
					g.reset()
				default:
					t.Fatalf("unknown action %q", s.action)
				}
			}
		})
	}
}

// fakeHub records the run-id header of every request and answers status
// writes with writeStatus and token refreshes with refreshStatus.
type fakeHub struct {
	mu            sync.Mutex
	runIDs        []string
	hasRunID      []bool
	writes        int
	writeStatus   int
	refreshStatus int
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := r.Header[http.CanonicalHeaderKey(RunIDHeader)]
	f.hasRunID = append(f.hasRunID, ok)
	f.runIDs = append(f.runIDs, r.Header.Get(RunIDHeader))
	if r.URL.Path == "/api/v1/agents/agent-1/token/refresh" {
		if f.refreshStatus == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"new-token","expires_at":"2030-01-01T00:00:00Z"}`))
			return
		}
		w.WriteHeader(f.refreshStatus)
		return
	}
	f.writes++
	w.WriteHeader(f.writeStatus)
}

func TestClientSendsRunIDHeader(t *testing.T) {
	tests := []struct {
		name  string
		runID string
	}{
		{name: "run id set", runID: "run-1"},
		{name: "run id unset", runID: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(SetTokenHome(t.TempDir()))
			fh := &fakeHub{writeStatus: http.StatusOK, refreshStatus: http.StatusOK}
			srv := httptest.NewServer(fh)
			defer srv.Close()
			c := NewClientWithConfig(srv.URL, "tok", "agent-1")
			c.runID = tt.runID
			c.maxRetries = 0

			if err := c.Heartbeat(context.Background()); err != nil {
				t.Fatalf("Heartbeat: %v", err)
			}
			if _, _, err := c.RefreshToken(context.Background()); err != nil {
				t.Fatalf("RefreshToken: %v", err)
			}
			if c.RunID() != tt.runID {
				t.Errorf("RunID() = %q, want %q", c.RunID(), tt.runID)
			}
			fh.mu.Lock()
			defer fh.mu.Unlock()
			if len(fh.runIDs) != 2 {
				t.Fatalf("hub saw %d requests, want 2", len(fh.runIDs))
			}
			for i := range fh.runIDs {
				if fh.hasRunID[i] != (tt.runID != "") || fh.runIDs[i] != tt.runID {
					t.Errorf("request %d: header present=%v value=%q, want value %q", i, fh.hasRunID[i], fh.runIDs[i], tt.runID)
				}
			}
		})
	}
}

func TestClientPausesWritesAfterRefusedRefresh(t *testing.T) {
	tests := []struct {
		name          string
		refreshStatus int
		wantPaused    bool
	}{
		{name: "refresh refused (401)", refreshStatus: http.StatusUnauthorized, wantPaused: true},
		{name: "refresh refused (403)", refreshStatus: http.StatusForbidden, wantPaused: true},
		{name: "refresh transient failure", refreshStatus: http.StatusBadGateway, wantPaused: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fh := &fakeHub{writeStatus: http.StatusUnauthorized, refreshStatus: tt.refreshStatus}
			srv := httptest.NewServer(fh)
			defer srv.Close()
			clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
			c := NewClientWithConfig(srv.URL, "tok", "agent-1")
			c.maxRetries = 0
			c.writes.now = clk.now
			ctx := context.Background()

			_, _, _ = c.RefreshToken(ctx)
			if err := c.Heartbeat(ctx); err == nil || errors.Is(err, ErrHubWritesPaused) {
				t.Fatalf("first write: err = %v, want a sent-and-refused error", err)
			}
			err := c.Heartbeat(ctx)
			if paused := errors.Is(err, ErrHubWritesPaused); paused != tt.wantPaused {
				t.Fatalf("second write paused = %v, want %v (err %v)", paused, tt.wantPaused, err)
			}
			wantWrites := 2
			if tt.wantPaused {
				wantWrites = 1
			}
			fh.mu.Lock()
			got := fh.writes
			fh.mu.Unlock()
			if got != wantWrites {
				t.Errorf("hub saw %d writes, want %d", got, wantWrites)
			}

			// A new token reopens the gate.
			c.SetToken("replacement")
			if err := c.Heartbeat(ctx); errors.Is(err, ErrHubWritesPaused) {
				t.Errorf("write after SetToken paused: %v", err)
			}
		})
	}
}

func TestClientSuccessfulRefreshResetsWriteGate(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	fh := &fakeHub{writeStatus: http.StatusUnauthorized, refreshStatus: http.StatusUnauthorized}
	srv := httptest.NewServer(fh)
	defer srv.Close()
	c := NewClientWithConfig(srv.URL, "tok", "agent-1")
	c.maxRetries = 0
	ctx := context.Background()

	_, _, _ = c.RefreshToken(ctx)
	_ = c.Heartbeat(ctx)
	if err := c.Heartbeat(ctx); !errors.Is(err, ErrHubWritesPaused) {
		t.Fatalf("write after refused refresh: err = %v, want paused", err)
	}
	fh.mu.Lock()
	fh.refreshStatus = http.StatusOK
	fh.mu.Unlock()
	if _, _, err := c.RefreshToken(ctx); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if err := c.Heartbeat(ctx); errors.Is(err, ErrHubWritesPaused) {
		t.Errorf("write after successful refresh paused: %v", err)
	}
}
