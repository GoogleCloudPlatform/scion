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
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// fakeReinitClock is a settable clock for HubConnection.now.
type fakeReinitClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeReinitClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeReinitClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// reinitBackoffFixture drives one connection's reinitialize and retry
// the way the credential watcher does, one tick at a time, on a fake
// clock and synchronously, so nothing sleeps.
type reinitBackoffFixture struct {
	t     *testing.T
	srv   *Server
	conn  *HubConnection
	clock *fakeReinitClock
}

const reinitBackoffConnName = "hub-backoff"

func newReinitBackoffFixture(t *testing.T) *reinitBackoffFixture {
	t.Helper()
	t.Setenv(transportauth.EnvTransportMode, "")
	t.Setenv(transportauth.EnvTransportAudience, "")

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.HeartbeatEnabled = false
	cfg.ControlChannelEnabled = false
	cfg.BrokerAuthEnabled = false
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})

	conn, err := srv.createHubConnection(reinitBackoffConnName, makeTestCreds(reinitBackoffConnName, "broker-1", "http://hub1.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeReinitClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	conn.now = clock.Now
	t.Cleanup(conn.close)
	return &reinitBackoffFixture{t: t, srv: srv, conn: conn, clock: clock}
}

// setFailing makes every reinitialize fail (unsupported transport mode) or
// succeed.
func (f *reinitBackoffFixture) setFailing(failing bool) {
	f.t.Helper()
	mode := ""
	if failing {
		mode = "iap"
	}
	f.t.Setenv(transportauth.EnvTransportMode, mode)
}

// request records a new credential set and applies it at once, as the
// watcher does when a connection's credentials change.
func (f *reinitBackoffFixture) request(brokerID string) error {
	f.t.Helper()
	f.conn.requestReinitialize(makeTestCreds(reinitBackoffConnName, brokerID, "http://hub1.example.com"))
	applied, err := f.conn.applyRequestedReinitialize(context.Background(), f.srv)
	if !applied {
		f.t.Fatalf("a newly requested credential set was not applied at once")
	}
	return err
}

// tick advances the clock by one watcher interval and, as an unchanged
// watcher tick does, retries a failed reinitialize if one is requested.
// It reports whether a retry ran, and its error.
func (f *reinitBackoffFixture) tick() (retried bool, err error) {
	f.t.Helper()
	f.clock.Advance(credentialWatchInterval)
	if !f.conn.retryFailedReinitialize() {
		return false, nil
	}
	applied, err := f.conn.applyRequestedReinitialize(context.Background(), f.srv)
	if !applied {
		f.t.Fatalf("a requested retry was not applied")
	}
	return true, err
}

// nextRetry ticks until a retry runs, failing the test after maxTicks,
// and returns the time since start at which it ran and its error.
func (f *reinitBackoffFixture) nextRetry(start time.Time, maxTicks int) (time.Duration, error) {
	f.t.Helper()
	for i := 0; i < maxTicks; i++ {
		if retried, err := f.tick(); retried {
			return f.clock.Now().Sub(start), err
		}
	}
	f.t.Fatalf("no retry within %d ticks", maxTicks)
	return 0, nil
}

// TestHubConnection_ReinitRetryBacksOff checks that a credential set whose
// reinitialize keeps failing is retried at growing intervals, from the
// watcher interval doubling up to reinitRetryMaxDelay, not on every tick.
func TestHubConnection_ReinitRetryBacksOff(t *testing.T) {
	f := newReinitBackoffFixture(t)
	f.setFailing(true)
	if err := f.request("broker-2"); err == nil {
		t.Fatal("reinitialize succeeded, want it to fail")
	}

	want := []time.Duration{
		10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	}
	maxTicks := int(reinitRetryMaxDelay/credentialWatchInterval) + 1
	last := f.clock.Now()
	for i, w := range want {
		gap, err := f.nextRetry(last, maxTicks)
		if err == nil {
			t.Fatalf("retry %d succeeded, want it to fail", i+1)
		}
		if gap != w {
			t.Errorf("retry %d ran %v after the previous attempt, want %v", i+1, gap, w)
		}
		last = f.clock.Now()
	}
}

// TestHubConnection_ReinitCredentialChangeRetriesAtOnce checks that while
// a failing set is backing off, a different credential set is applied at
// once, and that its own failures back off from the start.
func TestHubConnection_ReinitCredentialChangeRetriesAtOnce(t *testing.T) {
	f := newReinitBackoffFixture(t)
	f.setFailing(true)
	if err := f.request("broker-2"); err == nil {
		t.Fatal("reinitialize succeeded, want it to fail")
	}
	// Four retries: the next one is 160s away.
	for i := 0; i < 4; i++ {
		if _, err := f.nextRetry(f.clock.Now(), 100); err == nil {
			t.Fatal("retry succeeded, want it to fail")
		}
	}

	// A changed set is applied at once (request fails the test if not).
	if err := f.request("broker-3"); err == nil {
		t.Fatal("reinitialize of the changed set succeeded, want it to fail")
	}
	if f.conn.retryFailedReinitialize() {
		t.Fatal("a retry was requested at the instant the changed set failed, want one watcher interval later")
	}
	start := f.clock.Now()
	gap, err := f.nextRetry(start, 100)
	if err == nil {
		t.Fatal("retry of the changed set succeeded, want it to fail")
	}
	if gap != credentialWatchInterval {
		t.Errorf("first retry of the changed set ran %v after its failure, want %v (backoff restarted)", gap, credentialWatchInterval)
	}

	// The changed set is retried, not the earlier one.
	if got := f.conn.latestCredentials(); got == nil || got.BrokerID != "broker-3" {
		t.Errorf("latestCredentials = %+v, want the changed set", got)
	}
}

// TestHubConnection_ReinitSuccessResetsBackoff checks that a retry that
// succeeds clears the backoff: no further retry is requested, and a later
// failure backs off from the start again.
func TestHubConnection_ReinitSuccessResetsBackoff(t *testing.T) {
	f := newReinitBackoffFixture(t)
	f.setFailing(true)
	if err := f.request("broker-2"); err == nil {
		t.Fatal("reinitialize succeeded, want it to fail")
	}
	for i := 0; i < 4; i++ {
		if _, err := f.nextRetry(f.clock.Now(), 100); err == nil {
			t.Fatal("retry succeeded, want it to fail")
		}
	}

	f.setFailing(false)
	if _, err := f.nextRetry(f.clock.Now(), 100); err != nil {
		t.Fatalf("retry after the fix failed: %v", err)
	}
	if got := f.conn.snapshot().Credentials; got == nil || got.BrokerID != "broker-2" {
		t.Errorf("connection holds %+v after the successful retry, want the requested set", got)
	}
	f.conn.mu.RLock()
	failures, next := f.conn.reinitFailures, f.conn.reinitNextRetry
	f.conn.mu.RUnlock()
	if failures != 0 || !next.IsZero() {
		t.Errorf("after success, reinitFailures=%d reinitNextRetry=%v, want 0 and zero", failures, next)
	}
	for i := 0; i < 40; i++ {
		if retried, _ := f.tick(); retried {
			t.Fatalf("tick %d after a successful retry requested a retry", i+1)
		}
	}

	// A later failure starts from the watcher interval again.
	f.setFailing(true)
	if err := f.request("broker-3"); err == nil {
		t.Fatal("reinitialize succeeded, want it to fail")
	}
	if f.conn.retryFailedReinitialize() {
		t.Fatal("a retry was requested at the instant of the failure, want one watcher interval later")
	}
	gap, _ := f.nextRetry(f.clock.Now(), 100)
	if gap != credentialWatchInterval {
		t.Errorf("first retry after a new failure ran %v later, want %v", gap, credentialWatchInterval)
	}
}

// TestHubConnection_ReinitRetryTakenAtNearerTick checks a failure that
// falls between watcher ticks: its retry is taken at the tick nearer to
// when it falls due, and never more than half a tick early.
func TestHubConnection_ReinitRetryTakenAtNearerTick(t *testing.T) {
	for _, tc := range []struct {
		offset time.Duration // failure time after the previous tick
		want   time.Duration // first retry, after the previous tick
	}{
		// Due at 13s: the tick at 10s is nearer than the one at 20s.
		{3 * time.Second, 10 * time.Second},
		// Due at 17s: the tick at 20s is nearer.
		{7 * time.Second, 20 * time.Second},
	} {
		t.Run(tc.offset.String(), func(t *testing.T) {
			f := newReinitBackoffFixture(t)
			f.setFailing(true)
			tick0 := f.clock.Now()
			f.clock.Advance(tc.offset)
			if err := f.request("broker-2"); err == nil {
				t.Fatal("reinitialize succeeded, want it to fail")
			}
			due := tick0.Add(tc.offset + credentialWatchInterval)

			// Watcher ticks stay on the grid that started at tick0.
			f.clock.Advance(credentialWatchInterval - tc.offset)
			for i := 0; i < 10; i++ {
				if f.conn.retryFailedReinitialize() {
					now := f.clock.Now()
					if got := now.Sub(tick0); got != tc.want {
						t.Errorf("first retry at %v after the previous tick, want %v", got, tc.want)
					}
					if early := due.Sub(now); early > credentialWatchInterval/2 {
						t.Errorf("first retry taken %v before it was due, want at most %v", early, credentialWatchInterval/2)
					}
					return
				}
				f.clock.Advance(credentialWatchInterval)
			}
			t.Fatal("no retry within 10 ticks")
		})
	}
}

// TestHubConnection_ReinitFailureLogLevels checks the level each failure is
// logged at: Error for the first failure of a set and for a changed error,
// Debug for an identical repeat, including once retries reach the cap.
func TestHubConnection_ReinitFailureLogLevels(t *testing.T) {
	conn := &HubConnection{Name: "hub-levels"}
	errA := errors.New("reinitialize failed: a")
	errB := errors.New("reinitialize failed: b")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	record := func(err error, at time.Duration) reinitFailureReport {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return conn.recordReinitResultLocked(err, t0.Add(at))
	}
	steps := []struct {
		err  error
		at   time.Duration
		want slog.Level
	}{
		{errA, 0, slog.LevelError},
		{errA, 10 * time.Second, slog.LevelDebug},
		{errA, 30 * time.Second, slog.LevelDebug},
		{errA, 70 * time.Second, slog.LevelDebug},
		{errA, 150 * time.Second, slog.LevelDebug},
		{errA, 310 * time.Second, slog.LevelDebug},
		{errA, 610 * time.Second, slog.LevelDebug}, // at the cap
		{errA, 910 * time.Second, slog.LevelDebug},
		{errB, 1200 * time.Second, slog.LevelError}, // a different error
		{errB, 1490 * time.Second, slog.LevelDebug},
	}
	for i, s := range steps {
		r := record(s.err, s.at)
		if r.skip || r.level != s.want {
			t.Errorf("failure %d at %v: level=%v skip=%v, want %v", i+1, s.at, r.level, r.skip, s.want)
		}
		if r.failures != i+1 {
			t.Errorf("failure %d: consecutive failures = %d", i+1, r.failures)
		}
	}

	// A success resets: the next failure is a first failure again.
	if r := record(nil, 1600*time.Second); !r.skip {
		t.Errorf("success: report %+v, want nothing to log", r)
	}
	if r := record(errB, 1610*time.Second); r.level != slog.LevelError || r.failures != 1 || r.retryIn != credentialWatchInterval {
		t.Errorf("failure after success: %+v, want Error, 1 failure, retry in %v", r, credentialWatchInterval)
	}
}

// TestReinitRetryDelay checks the backoff schedule.
func TestReinitRetryDelay(t *testing.T) {
	for n, want := range map[int]time.Duration{
		1: 10 * time.Second, 2: 20 * time.Second, 3: 40 * time.Second, 4: 80 * time.Second,
		5: 160 * time.Second, 6: 5 * time.Minute, 7: 5 * time.Minute, 1000: 5 * time.Minute,
	} {
		if got := reinitRetryDelay(n); got != want {
			t.Errorf("reinitRetryDelay(%d) = %v, want %v", n, got, want)
		}
	}
}
