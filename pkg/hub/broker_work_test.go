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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stoppingCh returns a channel that is closed once closeAdmission has run.
// Test plumbing for the tracker's stopping hook: it lets a test wait for
// admission to close without polling.
func (t *brokerWorkTracker) stoppingCh() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.initLocked()
	return t.stopping
}

// waitStopping waits until tr's admission is closed. If done is closed
// first, the code under test returned without closing admission, and the
// test fails by assertion instead of hanging.
func waitStopping(t *testing.T, tr *brokerWorkTracker, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-tr.stoppingCh():
	case <-done:
		t.Fatalf("%s returned without closing broker work admission", what)
	}
}

// stop must not return while admitted work is still running, and returns 0
// once that work ends (ptone/scion#4869).
func TestBrokerWorkTracker_StopWaitsForAdmittedWork(t *testing.T) {
	var tr brokerWorkTracker
	_, done, ok := tr.begin()
	require.True(t, ok, "work is admitted before stop")

	res := make(chan int, 1)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		res <- tr.stop(context.Background(), time.Hour)
	}()

	waitStopping(t, &tr, returned, "stop")
	select {
	case n := <-res:
		t.Fatalf("stop returned %d while admitted work was still running", n)
	default:
	}

	done()
	assert.Equal(t, 0, <-res, "every unit is joined")
}

// Work started with goTracked is joined the same way.
func TestBrokerWorkTracker_StopJoinsGoTracked(t *testing.T) {
	var tr brokerWorkTracker
	release := make(chan struct{})
	ran := make(chan struct{})
	require.True(t, tr.goTracked(func(context.Context) {
		<-release
		close(ran)
	}))

	res := make(chan int, 1)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		res <- tr.stop(context.Background(), time.Hour)
	}()
	waitStopping(t, &tr, returned, "stop")
	select {
	case n := <-res:
		t.Fatalf("stop returned %d while a goTracked function was still running", n)
	default:
	}

	close(release)
	assert.Equal(t, 0, <-res)
	select {
	case <-ran:
	default:
		t.Fatal("stop returned before the goTracked function finished")
	}
}

// Once stop has started, new work is refused and never runs.
func TestBrokerWorkTracker_StopRefusesNewWork(t *testing.T) {
	var tr brokerWorkTracker
	require.Equal(t, 0, tr.stop(context.Background(), time.Hour))

	_, _, ok := tr.begin()
	assert.False(t, ok, "begin after stop must refuse the work")
	assert.False(t, tr.goTracked(func(context.Context) {
		t.Error("goTracked after stop must not run its function")
	}))
	assert.Equal(t, 0, tr.stop(context.Background(), time.Hour), "a second stop with nothing running returns 0")
}

// closeAdmission refuses new work and sets closing, but leaves admitted
// work running until wait joins it.
func TestBrokerWorkTracker_CloseAdmissionThenWait(t *testing.T) {
	var tr brokerWorkTracker
	assert.False(t, tr.closing())
	workCtx, done, ok := tr.begin()
	require.True(t, ok)

	tr.closeAdmission()
	assert.True(t, tr.closing(), "running work sees that shutdown has begun")
	_, _, ok = tr.begin()
	assert.False(t, ok, "begin after closeAdmission must refuse the work")
	assert.NoError(t, workCtx.Err(), "closeAdmission does not cancel admitted work")
	assert.Equal(t, 1, tr.runningCount())

	done()
	assert.Equal(t, 0, tr.wait(context.Background(), time.Hour))
}

// The wait is bounded by the caller's context: stop returns the count still
// running and cancels the work context, so overrunning work stops.
func TestBrokerWorkTracker_StopBoundedByContext(t *testing.T) {
	var tr brokerWorkTracker
	workCtx, done, ok := tr.begin()
	require.True(t, ok)
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Equal(t, 1, tr.stop(ctx, time.Hour), "the unit still running is reported")
	assert.Error(t, workCtx.Err(), "overrunning work is cancelled")
}

// The wait is bounded by the grace as well.
func TestBrokerWorkTracker_StopBoundedByGrace(t *testing.T) {
	var tr brokerWorkTracker
	workCtx, done, ok := tr.begin()
	require.True(t, ok)
	defer done()

	assert.Equal(t, 1, tr.stop(context.Background(), 0))
	assert.Error(t, workCtx.Err(), "overrunning work is cancelled")
}

// After Shutdown, HandleUpgrade refuses the connection with 503 and
// errControlChannelClosed, and registers nothing, so a broker that
// reconnects during hub shutdown starts no connect work (ptone/scion#4869).
func TestControlChannelHandleUpgrade_RefusedAfterShutdown(t *testing.T) {
	m := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	m.Shutdown()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-brokers/connect", nil)
	sessionID, err := m.HandleUpgrade(rec, req, "broker-after-shutdown")

	require.ErrorIs(t, err, errControlChannelClosed)
	assert.Empty(t, sessionID)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.False(t, m.IsConnected("broker-after-shutdown"))
}

// Shutdown can run while HandleUpgrade is between its first closed check
// and registering the connection. The re-check under the lock must then
// refuse the connection and close it, so Shutdown, which no longer sees
// it, does not leave it open (ptone/scion#4869). The upgrader's
// CheckOrigin runs at exactly that point, so the test shuts down there.
func TestControlChannelHandleUpgrade_ShutdownDuringUpgradeClosesConnection(t *testing.T) {
	m := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	m.upgrader.CheckOrigin = func(*http.Request) bool {
		m.Shutdown()
		return true
	}

	errCh := make(chan error, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := m.HandleUpgrade(w, r, "broker-mid-upgrade")
		errCh <- err
	}))
	defer ts.Close()

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	require.NoError(t, err, "the websocket handshake itself completes")
	defer func() { _ = c.Close() }()

	require.ErrorIs(t, <-errCh, errControlChannelClosed)
	assert.False(t, m.IsConnected("broker-mid-upgrade"), "the refused connection must not be registered")

	// The hub closed the socket instead of sending the connected message.
	// The deadline only keeps a broken build from hanging; the assertion
	// does not depend on it.
	require.NoError(t, c.SetReadDeadline(time.Now().Add(time.Minute)))
	_, msg, err := c.ReadMessage()
	assert.Error(t, err, "the hub must close the connection, got message %q", msg)
}
