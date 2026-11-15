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

package commands

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/liveness"
)

// newTestLiveWorkRecorder returns a recorder on a fake clock and a provider
// that returns *next on each sample.
func newTestLiveWorkRecorder(now *time.Time, next **liveness.LiveWork) (*liveWorkRecorder, func() *liveness.LiveWork) {
	r := newLiveWorkRecorder()
	r.now = func() time.Time { return *now }
	return r, r.record(func() *liveness.LiveWork { return *next })
}

func TestLiveWorkRecorder(t *testing.T) {
	now := watchdogLaunch
	var next *liveness.LiveWork
	r, sample := newTestLiveWorkRecorder(&now, &next)

	assert.False(t, r.Live(), "no sample yet (no Hub heartbeat): no live work")

	next = &liveness.LiveWork{Kind: liveness.KindProcess, SuppressStall: true}
	assert.Same(t, next, sample(), "the heartbeat still gets the sample")
	assert.True(t, r.Live())

	// A capped or stuck tool call no longer suppresses STALLED, but the
	// tool is still running: the turn is not over.
	next = &liveness.LiveWork{Kind: liveness.KindToolCall}
	sample()
	assert.True(t, r.Live(), "any live work counts")

	next = nil
	assert.Nil(t, sample())
	assert.False(t, r.Live(), "the latest sample found none")

	next = &liveness.LiveWork{Kind: liveness.KindProcess, SuppressStall: true}
	sample()
	now = now.Add(liveWorkFreshness - time.Second)
	assert.True(t, r.Live())
	now = now.Add(time.Second)
	assert.False(t, r.Live(), "a sample older than the freshness window is stale (heartbeats stopped)")
}

// End to end through the API-error watchdog: while the heartbeat's latest
// sample shows live work, a swallowed non-retryable API error is not
// reported as harness_error; once the work ends, it is.
func TestHarnessWatchdog_HeartbeatLiveWorkHoldsTheAPIErrorReport(t *testing.T) {
	f := newWatchdogFixture(t, "working")
	var next *liveness.LiveWork
	r, sample := newTestLiveWorkRecorder(&f.now, &next)
	f.cfg.LiveWork = r.Live

	f.outcome(time.Second, false, 401, "bad key")
	f.now = watchdogLaunch.Add(3 * time.Minute)
	next = &liveness.LiveWork{Kind: liveness.KindToolCall, SuppressStall: true}
	sample()
	var reported time.Time
	assert.False(t, f.check(3*time.Minute, &reported), "a tool call is still running")

	f.now = watchdogLaunch.Add(190 * time.Second)
	next = nil
	sample()
	assert.True(t, f.check(190*time.Second, &reported), "no live work: the error is reported")
	require.Len(t, f.reports, 1)
}

func TestNewHarnessWatchdogConfig_WiresLiveWork(t *testing.T) {
	live := false
	cfg := newHarnessWatchdogConfig(harnessWatchdogDeps{
		LaunchedAt: watchdogLaunch,
		AgentHome:  t.TempDir(),
		Getenv:     func(string) string { return "" },
		LiveWork:   func() bool { return live },
	})
	require.NotNil(t, cfg.LiveWork)
	assert.False(t, cfg.LiveWork())
	live = true
	assert.True(t, cfg.LiveWork())

	cfg = newHarnessWatchdogConfig(harnessWatchdogDeps{
		LaunchedAt: watchdogLaunch,
		AgentHome:  t.TempDir(),
		Getenv:     func(string) string { return "" },
	})
	assert.Nil(t, cfg.LiveWork, "no recorder: no live work, as before")
}
