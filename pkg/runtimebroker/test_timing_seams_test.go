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
	"testing"
	"time"
)

// Test-only timing seams. Each helper shortens one production wait so a
// test that has to sit through it (a PTY teardown, a retry backoff) runs in
// milliseconds instead of seconds, while still driving the same code path.
// Production defaults are pinned separately (TestLaunchSender_DefaultTimings,
// TestPTYShutdownDefaults, TestCommandWaitDelayDefault).

// fastPTYExitGracePeriod replaces processExitGracePeriod (3s) in tests whose
// PTY bridge teardown would otherwise wait it out: the tmux attach client a
// test bridge runs does not exit on the PTY hangup alone, so every such
// teardown reached SIGTERM only after the full grace period.
const fastPTYExitGracePeriod = 100 * time.Millisecond

// fastTmuxSessionPollInterval replaces tmuxSessionPollInterval (500ms) in
// the same tests: waitForTmuxSession's first poll only fires after one full
// interval, so every attach waited at least 500ms before starting its exec.
const fastTmuxSessionPollInterval = 20 * time.Millisecond

// useFastPTYTimings shortens processExitGracePeriod and
// tmuxSessionPollInterval for the calling test and restores them on
// cleanup. processTermTimeout is left at its default: it is only waited out
// when SIGTERM is ignored, so shortening it would only change which signal
// ends a test's exec, not how long the test takes. The TestWaitForTmuxSession_*
// tests deliberately keep the production poll interval.
//
// It must be called before the test starts any PTY bridge, so its restore
// runs (cleanups are LIFO) only after the test's own cleanups have waited
// for those bridges to exit.
func useFastPTYTimings(t *testing.T) {
	t.Helper()
	origGrace, origPoll := processExitGracePeriod, tmuxSessionPollInterval
	processExitGracePeriod = fastPTYExitGracePeriod
	tmuxSessionPollInterval = fastTmuxSessionPollInterval
	t.Cleanup(func() {
		processExitGracePeriod = origGrace
		tmuxSessionPollInterval = origPoll
	})
}

// fastLaunchTimings returns launch sender timings with the retry backoffs
// cut from seconds to tens of milliseconds. keepaliveInterval, when
// positive, also replaces the per-launch keepalive interval (whose smallest
// wire value is 1s); pass 0 to keep the interval the test asked for.
func fastLaunchTimings(keepaliveInterval time.Duration) *launchTimings {
	return &launchTimings{
		reportMinBackoff:    10 * time.Millisecond,
		reportMaxBackoff:    30 * time.Millisecond,
		keepaliveMinBackoff: 10 * time.Millisecond,
		keepaliveMaxBackoff: 30 * time.Millisecond,
		keepaliveInterval:   keepaliveInterval,
	}
}

// fastTestKeepaliveInterval stands in for "launchKeepaliveSeconds": 1 in the
// async-create tests that wait for one or two keepalives. It stays well above
// the few milliseconds the claim and Manager.Start take in those tests, so
// the ordering they rely on (claim answered, Start reached, then the
// keepalive answer) is unchanged.
const fastTestKeepaliveInterval = 250 * time.Millisecond

// TestLaunchSender_DefaultTimings pins the production backoffs a sender gets
// when its server has no launchTimingOverride (always the case outside
// tests), since the timing tests themselves run with fastLaunchTimings.
func TestLaunchSender_DefaultTimings(t *testing.T) {
	s := newTestLaunchSender(t, &mockRuntimeBrokerService{}, time.Hour)
	want := launchTimings{
		reportMinBackoff:    1 * time.Second,
		reportMaxBackoff:    10 * time.Second,
		keepaliveMinBackoff: 2 * time.Second,
		keepaliveMaxBackoff: 5 * time.Second,
	}
	if s.timings != want {
		t.Fatalf("default timings = %+v, want %+v", s.timings, want)
	}
	if s.keepaliveInterval != time.Hour {
		t.Fatalf("keepaliveInterval = %v, want the 1h passed in (no override)", s.keepaliveInterval)
	}
}

// TestLaunchSender_TimingOverrideReplacesKeepaliveInterval covers the seam
// itself: a positive override interval replaces the one passed in, and a
// zero one keeps it.
func TestLaunchSender_TimingOverrideReplacesKeepaliveInterval(t *testing.T) {
	srv := newTestServer(t)
	rec := newLaunchRecord("L1", "agent-1", "create", "hub-a", time.Now().Add(time.Hour), func() {})

	srv.launchTimingOverride = fastLaunchTimings(fastTestKeepaliveInterval)
	s := newLaunchSender(srv, rec, "agent-1", "instance-1", time.Second)
	if s.keepaliveInterval != fastTestKeepaliveInterval {
		t.Fatalf("keepaliveInterval = %v, want the override %v", s.keepaliveInterval, fastTestKeepaliveInterval)
	}
	if s.timings != *srv.launchTimingOverride {
		t.Fatalf("timings = %+v, want the override %+v", s.timings, *srv.launchTimingOverride)
	}

	srv.launchTimingOverride = fastLaunchTimings(0)
	s = newLaunchSender(srv, rec, "agent-1", "instance-1", time.Second)
	if s.keepaliveInterval != time.Second {
		t.Fatalf("keepaliveInterval = %v, want the 1s passed in (zero override interval)", s.keepaliveInterval)
	}
}

// TestPTYShutdownDefaults pins the production PTY teardown waits, since the
// PTY bridge tests run with useFastPTYTimings.
func TestPTYShutdownDefaults(t *testing.T) {
	if tmuxSessionPollInterval != 500*time.Millisecond {
		t.Errorf("tmuxSessionPollInterval = %v, want 500ms", tmuxSessionPollInterval)
	}
	if processExitGracePeriod != 3*time.Second {
		t.Errorf("processExitGracePeriod = %v, want 3s", processExitGracePeriod)
	}
	if processTermTimeout != 2*time.Second {
		t.Errorf("processTermTimeout = %v, want 2s", processTermTimeout)
	}
}
