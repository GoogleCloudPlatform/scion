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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stop must not return while admitted work is still running, and returns 0
// once that work ends (ptone/scion#4869).
func TestBrokerWorkTracker_StopWaitsForAdmittedWork(t *testing.T) {
	var tr brokerWorkTracker
	_, done, ok := tr.begin()
	require.True(t, ok, "work is admitted before stop")

	res := make(chan int, 1)
	go func() { res <- tr.stop(context.Background(), time.Hour) }()

	<-tr.stoppingCh()
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
	go func() { res <- tr.stop(context.Background(), time.Hour) }()
	<-tr.stoppingCh()
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
	assert.Equal(t, 0, tr.stop(context.Background(), time.Hour), "a second stop returns without waiting")
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
