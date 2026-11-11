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

package portforward

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

type connectResult struct {
	connected bool
	err       error
}

// runScripted runs m.run with connect answering from script (the last
// entry repeats) until n delays were requested, and returns the delays.
// No real time passes: the fake timer fires at once.
func runScripted(t *testing.T, m *Manager, script []connectResult, n int) []time.Duration {
	t.Helper()
	m.backoff.Rand = func() float64 { return 0 }
	if script != nil {
		m.connect = func(context.Context) (bool, error) {
			r := script[0]
			if len(script) > 1 {
				script = script[1:]
			}
			return r.connected, r.err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delays []time.Duration
	m.after = func(d time.Duration) <-chan time.Time {
		delays = append(delays, d)
		if len(delays) >= n {
			cancel()
			return nil // never fires; run returns on ctx.Done
		}
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	done := make(chan struct{})
	go func() {
		m.run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
	return delays
}

func secs(ns ...int) []time.Duration {
	out := make([]time.Duration, len(ns))
	for i, n := range ns {
		out[i] = time.Duration(n) * time.Second
	}
	return out
}

var errDial = errors.New("dial tcp: connection refused")

func TestRun_ConnectFailuresBackOffAndCap(t *testing.T) {
	got := runScripted(t, NewManager(nil), []connectResult{{false, errDial}}, 9)
	want := secs(5, 10, 20, 40, 80, 160, 300, 300, 300)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
}

func TestRun_BackoffResetsAfterConnectedSession(t *testing.T) {
	script := []connectResult{
		{false, errDial}, {false, errDial}, {false, errDial},
		{true, errors.New("session closed")},
		{false, errDial},
	}
	got := runScripted(t, NewManager(nil), script, 5)
	want := secs(5, 10, 20, 5, 10)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
}

func TestRun_UnauthorizedRetriesAtCap(t *testing.T) {
	err401 := &scionhub.StatusError{StatusCode: http.StatusUnauthorized, Body: "websocket: bad handshake"}
	script := []connectResult{{false, err401}, {false, err401}, {false, err401}, {true, errors.New("closed")}}
	got := runScripted(t, NewManager(nil), script, 4)
	want := []time.Duration{maxReconnectDelay, maxReconnectDelay, maxReconnectDelay, reconnectDelay}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
}

// TestRun_Unauthorized401FromHub drives the real dial path against a hub
// that answers the upgrade with 401.
func TestRun_Unauthorized401FromHub(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	m := NewManager(scionhub.NewClientWithConfig(srv.URL, "agent-credential", "agent-1"))
	connected, err := m.runOnce(context.Background())
	if connected || !scionhub.IsUnauthorized(err) {
		t.Fatalf("runOnce = (%v, %v), want (false, 401)", connected, err)
	}

	got := runScripted(t, m, nil, 3)
	want := []time.Duration{maxReconnectDelay, maxReconnectDelay, maxReconnectDelay}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
	if n := hits.Load(); n != 4 {
		t.Fatalf("hub requests = %d, want 4 (one per attempt)", n)
	}
}

// A refused upgrade with a status other than 401 (here 403) maps to a
// StatusError carrying the response body, and backs off exponentially
// rather than jumping to the cap.
func TestRun_ForbiddenHandshakeBacksOffExponentially(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	m := NewManager(scionhub.NewClientWithConfig(srv.URL, "agent-credential", "agent-1"))
	_, err := m.runOnce(context.Background())
	var se *scionhub.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusForbidden || !strings.Contains(se.Body, "forbidden") {
		t.Fatalf("runOnce error = %#v, want a 403 StatusError with the response body", err)
	}
	if scionhub.IsUnauthorized(err) {
		t.Fatal("a 403 must not be treated as a 401")
	}

	got := runScripted(t, m, nil, 3)
	if want := secs(5, 10, 20); !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
}

func TestRun_LogsOnlyOnStateChange(t *testing.T) {
	var errs int
	orig := logError
	logError = func(string, ...interface{}) { errs++ }
	t.Cleanup(func() { logError = orig })

	err401 := &scionhub.StatusError{StatusCode: http.StatusUnauthorized}
	script := []connectResult{
		// One line on entering backoff.
		{false, errDial}, {false, errDial}, {false, errDial},
		// One line on entering 401 mode.
		{false, err401}, {false, err401}, {false, err401},
	}
	runScripted(t, NewManager(nil), script, 6)
	if errs != 2 {
		t.Fatalf("error log lines = %d, want 2", errs)
	}
}
