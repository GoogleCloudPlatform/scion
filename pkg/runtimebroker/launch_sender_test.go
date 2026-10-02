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
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func newTestLaunchSender(t *testing.T, rtb *mockRuntimeBrokerService, keepaliveInterval time.Duration) *launchSender {
	t.Helper()
	srv := newTestServer(t)
	srv.hubMu.Lock()
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "broker-a", HubClient: &stubBrokerHubClient{brokers: rtb}}
	srv.hubMu.Unlock()
	rec := newLaunchRecord("L1", "agent-1", "create", "hub-a", time.Now().Add(time.Hour), func() {})
	return newLaunchSender(srv, rec, "agent-1", "instance-1", keepaliveInterval)
}

// TestNewLaunchSender_DefaultsKeepaliveIntervalTo15s covers a non-positive
// keepaliveInterval (the Hub's create request omitted LaunchKeepaliveSeconds)
// defaulting to 15s (design §3.7).
func TestNewLaunchSender_DefaultsKeepaliveIntervalTo15s(t *testing.T) {
	rtb := &mockRuntimeBrokerService{}
	for _, in := range []time.Duration{0, -1} {
		s := newTestLaunchSender(t, rtb, in)
		if s.keepaliveInterval != 15*time.Second {
			t.Fatalf("keepaliveInterval = %v for input %v, want 15s", s.keepaliveInterval, in)
		}
	}
}

// TestLaunchSender_KeepaliveSendsPeriodically covers B-3: the keepalive runs
// on a cadence while nothing blocks it.
func TestLaunchSender_KeepaliveSendsPeriodically(t *testing.T) {
	rtb := &mockRuntimeBrokerService{}
	s := newTestLaunchSender(t, rtb, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartKeepalive(ctx)
	defer s.StopKeepalive()

	if !waitUntil(t, 2*time.Second, func() bool { return len(rtb.getLaunchReports()) >= 3 }) {
		t.Fatalf("expected at least 3 keepalives, got %d", len(rtb.getLaunchReports()))
	}
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State != hubclient.AgentLaunchReportStateProgress {
			t.Fatalf("expected every keepalive to be a progress report, got %s", r.Report.State)
		}
	}
}

// TestLaunchSender_KeepaliveStopsOnTerminalStart covers design §3.8.5: once
// a terminal send starts, the keepalive loop exits and sends nothing more.
func TestLaunchSender_KeepaliveStopsOnTerminalStart(t *testing.T) {
	rtb := &mockRuntimeBrokerService{}
	s := newTestLaunchSender(t, rtb, 15*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartKeepalive(ctx)

	if !waitUntil(t, time.Second, func() bool { return len(rtb.getLaunchReports()) >= 1 }) {
		t.Fatal("expected at least one keepalive before the terminal")
	}

	if _, err := s.SendTerminal(context.Background(), true, "", "", "", nil); err != nil {
		t.Fatalf("SendTerminal: %v", err)
	}
	s.WaitKeepaliveStopped()

	countAtStop := len(rtb.getLaunchReports())
	time.Sleep(60 * time.Millisecond) // long enough for several more intervals if the loop were still running
	if got := len(rtb.getLaunchReports()); got != countAtStop {
		t.Fatalf("keepalive sent %d more reports after the terminal started", got-countAtStop)
	}
}

// TestLaunchSender_KeepaliveRecordsCompletedAnswer covers design §3.8.2's
// gate-answer table applying to keepalives: a "completed" answer is
// recorded without aborting.
func TestLaunchSender_KeepaliveRecordsCompletedAnswer(t *testing.T) {
	rtb := &mockRuntimeBrokerService{
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}, nil
		},
	}
	s := newTestLaunchSender(t, rtb, 15*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartKeepalive(ctx)
	defer s.StopKeepalive()

	if !waitUntil(t, time.Second, s.IsCompleted) {
		t.Fatal("expected IsCompleted() to become true")
	}
	select {
	case <-s.KeepaliveAborted():
		t.Fatal("a completed answer must not abort")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestLaunchSender_KeepaliveRecordsAbortAnswer covers a definitive
// non-continue, non-completed answer (409 lost here) waking KeepaliveAborted
// with the classified action.
func TestLaunchSender_KeepaliveRecordsAbortAnswer(t *testing.T) {
	rtb := &mockRuntimeBrokerService{
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonLost}, nil
		},
	}
	s := newTestLaunchSender(t, rtb, 15*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartKeepalive(ctx)
	defer s.StopKeepalive()

	select {
	case <-s.KeepaliveAborted():
	case <-time.After(time.Second):
		t.Fatal("expected KeepaliveAborted to close")
	}
	outcome := s.LastAbortOutcome()
	if outcome == nil || outcome.action != gateAbortCleanup {
		t.Fatalf("outcome = %+v, want gateAbortCleanup", outcome)
	}
	s.WaitKeepaliveStopped() // the loop must exit once aborted
}

// TestLaunchSender_KeepaliveRetriesAfterBackoff covers a failed keepalive
// attempt being retried only after a 2-5s jittered backoff, not immediately.
func TestLaunchSender_KeepaliveRetriesAfterBackoff(t *testing.T) {
	var attempts int32
	rtb := &mockRuntimeBrokerService{
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				return nil, errUnreachableForTest
			}
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		},
	}
	s := newTestLaunchSender(t, rtb, time.Hour) // long enough that only the retry drives the second attempt

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	s.sendKeepaliveOnce(ctx)
	elapsed := time.Since(start)

	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("expected exactly 2 attempts (1 failure + 1 retry), got %d", got)
	}
	if elapsed < 2*time.Second {
		t.Fatalf("retry landed after %v, want at least the 2s backoff floor", elapsed)
	}
}

// TestLaunchSender_AttemptTimeoutConstants asserts the per-attempt timeout
// each report kind uses (design §3.10), rather than timing an actual
// attempt: claimAttemptTimeout, keepaliveAttemptTimeout and
// terminalAttemptTimeout are separate constants specifically so a change to
// one cannot silently change the others, and SendClaim/sendKeepaliveOnce/
// SendTerminal are confirmed to pass them (not a copied literal) by
// TestLaunchSender_EachReportKindUsesItsOwnAttemptTimeout below.
func TestLaunchSender_AttemptTimeoutConstants(t *testing.T) {
	for name, got := range map[string]time.Duration{
		"claimAttemptTimeout":     claimAttemptTimeout,
		"keepaliveAttemptTimeout": keepaliveAttemptTimeout,
		"terminalAttemptTimeout":  terminalAttemptTimeout,
	} {
		if got != 5*time.Second {
			t.Errorf("%s = %v, want 5s", name, got)
		}
	}
}

// TestLaunchSender_AttemptTimesOutAt5s confirms sendOnce (used by all three
// report kinds) actually enforces a per-attempt bound in the first place,
// using a generous outer margin so it is not timing-sensitive; the exact
// value is TestLaunchSender_AttemptTimeoutConstants's job.
func TestLaunchSender_AttemptTimesOutAt5s(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	rtb := &mockRuntimeBrokerService{
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			<-block // never answers within the test's lifetime
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		},
	}
	s := newTestLaunchSender(t, rtb, time.Hour)

	// sendOnce makes exactly one attempt (no retry loop), so this isolates
	// the per-attempt timeout itself rather than however long
	// sendKeepaliveOnce's/sendReportBlocking's retry loop runs.
	start := time.Now()
	_, err := s.sendOnce(context.Background(), &hubclient.AgentLaunchReport{LaunchID: "L1"}, claimAttemptTimeout)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the attempt to time out (the mock never answers)")
	}
	if elapsed > 15*time.Second {
		t.Fatalf("single attempt took %v, want roughly claimAttemptTimeout (5s)", elapsed)
	}
}

// TestLaunchSender_EachReportKindUsesItsOwnAttemptTimeout covers SendClaim,
// sendKeepaliveOnce and SendTerminal each actually passing their own named
// constant (claimAttemptTimeout/keepaliveAttemptTimeout/
// terminalAttemptTimeout) down to the attempt's own context, rather than a
// literal that happens to also be 5s today: the mock reads the deadline on
// the ctx it is actually called with and the test asserts it is close to 5s
// from "now", which a 60s value at any one call site would fail clearly.
func TestLaunchSender_EachReportKindUsesItsOwnAttemptTimeout(t *testing.T) {
	assertFiveSecondDeadline := func(t *testing.T, label string, ctx context.Context) {
		t.Helper()
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("%s: attempt ctx has no deadline", label)
		}
		remaining := time.Until(deadline)
		if remaining < 3*time.Second || remaining > 7*time.Second {
			t.Fatalf("%s: attempt ctx deadline is %v out, want roughly 5s (claim/keepalive/terminalAttemptTimeout)", label, remaining)
		}
	}

	t.Run("claim", func(t *testing.T) {
		seen := make(chan context.Context, 1)
		rtb := &mockRuntimeBrokerService{
			launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
				return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
			},
		}
		rtb.ctxHook = func(ctx context.Context) { seen <- ctx }
		s := newTestLaunchSender(t, rtb, time.Hour)
		if _, err := s.SendClaim(context.Background()); err != nil {
			t.Fatalf("SendClaim: %v", err)
		}
		assertFiveSecondDeadline(t, "claim", <-seen)
	})

	t.Run("keepalive", func(t *testing.T) {
		seen := make(chan context.Context, 1)
		rtb := &mockRuntimeBrokerService{
			launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
				return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
			},
		}
		rtb.ctxHook = func(ctx context.Context) { seen <- ctx }
		s := newTestLaunchSender(t, rtb, time.Hour)
		s.sendKeepaliveOnce(context.Background())
		assertFiveSecondDeadline(t, "keepalive", <-seen)
	})

	t.Run("terminal", func(t *testing.T) {
		seen := make(chan context.Context, 1)
		rtb := &mockRuntimeBrokerService{
			launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
				return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
			},
		}
		rtb.ctxHook = func(ctx context.Context) { seen <- ctx }
		s := newTestLaunchSender(t, rtb, time.Hour)
		if _, err := s.SendTerminal(context.Background(), true, "", "", "", nil); err != nil {
			t.Fatalf("SendTerminal: %v", err)
		}
		assertFiveSecondDeadline(t, "terminal", <-seen)
	})
}

// TestLaunchSender_ClaimNeverAttemptsAfterAbortAlreadyRecorded covers
// sendReportBlocking's pre-attempt abortCh check: when a keepalive abort is
// already recorded before SendClaim (the only abortable caller) is ever
// invoked, SendClaim must return errAbortedByKeepalive immediately, without
// making even a single attempt -- not rely on discovering the abort only
// after a wasted round trip.
func TestLaunchSender_ClaimNeverAttemptsAfterAbortAlreadyRecorded(t *testing.T) {
	var attempts int32
	rtb := &mockRuntimeBrokerService{
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			atomic.AddInt32(&attempts, 1)
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		},
	}
	s := newTestLaunchSender(t, rtb, time.Hour)
	s.recordKeepaliveAbort(gateAbortCleanup, &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonDeleted})

	_, err := s.SendClaim(context.Background())
	if !errors.Is(err, errAbortedByKeepalive) {
		t.Fatalf("SendClaim error = %v, want errAbortedByKeepalive", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 0 {
		t.Fatalf("expected no attempts once the abort was already recorded, got %d", got)
	}
}

// TestLaunchSender_FanOutAllUnknownMeansUnknown and
// TestLaunchSender_FanOutMixOfUnknownAndUnreachableMeansRetry cover the
// fan-out aggregation (design §3.8.5 routing rule 4).
func TestLaunchSender_FanOutAllUnknownMeansUnknown(t *testing.T) {
	srv := newTestServer(t)
	unknownFunc := func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusNotFound, Code: hubclient.AgentLaunchReportCodeUnknownLaunch}, nil
	}
	hubA := &mockRuntimeBrokerService{launchReportFunc: unknownFunc}
	hubB := &mockRuntimeBrokerService{launchReportFunc: unknownFunc}
	srv.hubMu.Lock()
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "broker-a", HubClient: &stubBrokerHubClient{brokers: hubA}}
	srv.hubConnections["hub-b"] = &HubConnection{Name: "hub-b", BrokerID: "broker-b", HubClient: &stubBrokerHubClient{brokers: hubB}}
	srv.hubMu.Unlock()

	rec := newLaunchRecord("L1", "agent-1", "create", "", time.Now().Add(time.Hour), func() {})
	s := newLaunchSender(srv, rec, "agent-1", "instance-1", time.Hour)

	result, err := s.sendOnce(context.Background(), &hubclient.AgentLaunchReport{LaunchID: "L1"}, time.Second)
	if err != nil {
		t.Fatalf("sendOnce: %v (want a definitive unknown result)", err)
	}
	if result.HTTPStatus != http.StatusNotFound || result.Code != hubclient.AgentLaunchReportCodeUnknownLaunch {
		t.Fatalf("result = %+v, want 404 agent_launch_unknown", result)
	}
}

func TestLaunchSender_FanOutMixOfUnknownAndUnreachableMeansRetry(t *testing.T) {
	srv := newTestServer(t)
	hubA := &mockRuntimeBrokerService{
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusNotFound, Code: hubclient.AgentLaunchReportCodeUnknownLaunch}, nil
		},
	}
	hubB := &mockRuntimeBrokerService{
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			return nil, errUnreachableForTest
		},
	}
	srv.hubMu.Lock()
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "broker-a", HubClient: &stubBrokerHubClient{brokers: hubA}}
	srv.hubConnections["hub-b"] = &HubConnection{Name: "hub-b", BrokerID: "broker-b", HubClient: &stubBrokerHubClient{brokers: hubB}}
	srv.hubMu.Unlock()

	rec := newLaunchRecord("L1", "agent-1", "create", "", time.Now().Add(time.Hour), func() {})
	s := newLaunchSender(srv, rec, "agent-1", "instance-1", time.Hour)

	_, err := s.sendOnce(context.Background(), &hubclient.AgentLaunchReport{LaunchID: "L1"}, time.Second)
	if err == nil {
		t.Fatal("expected an error (a mix of unknown and unreachable means retry)")
	}
}

// TestLaunchSender_FanOut403DoesNotPinButOthersStillConsulted covers a 403
// from one connection not pinning OwnerHub, with the fan-out still
// consulting the other connection.
func TestLaunchSender_FanOut403DoesNotPinButOthersStillConsulted(t *testing.T) {
	srv := newTestServer(t)
	hubA := &mockRuntimeBrokerService{
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}, nil
		},
	}
	hubB := &mockRuntimeBrokerService{} // default: applied
	srv.hubMu.Lock()
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "broker-a", HubClient: &stubBrokerHubClient{brokers: hubA}}
	srv.hubConnections["hub-b"] = &HubConnection{Name: "hub-b", BrokerID: "broker-b", HubClient: &stubBrokerHubClient{brokers: hubB}}
	srv.hubMu.Unlock()

	rec := newLaunchRecord("L1", "agent-1", "create", "", time.Now().Add(time.Hour), func() {})
	s := newLaunchSender(srv, rec, "agent-1", "instance-1", time.Hour)

	result, err := s.sendOnce(context.Background(), &hubclient.AgentLaunchReport{LaunchID: "L1"}, time.Second)
	if err != nil {
		t.Fatalf("sendOnce: %v", err)
	}
	if result.Result != hubclient.AgentLaunchReportResultApplied {
		t.Fatalf("result = %+v, want the applied answer from hub-b", result)
	}
	if got := rec.OwnerHub(); got != "hub-b" {
		t.Fatalf("OwnerHub = %q, want hub-b (403 from hub-a must not pin)", got)
	}
}

func TestLaunchSender_FanOutAll403(t *testing.T) {
	srv := newTestServer(t)
	forbidden := func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}, nil
	}
	hubA := &mockRuntimeBrokerService{launchReportFunc: forbidden}
	hubB := &mockRuntimeBrokerService{launchReportFunc: forbidden}
	srv.hubMu.Lock()
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "broker-a", HubClient: &stubBrokerHubClient{brokers: hubA}}
	srv.hubConnections["hub-b"] = &HubConnection{Name: "hub-b", BrokerID: "broker-b", HubClient: &stubBrokerHubClient{brokers: hubB}}
	srv.hubMu.Unlock()

	rec := newLaunchRecord("L1", "agent-1", "create", "", time.Now().Add(time.Hour), func() {})
	s := newLaunchSender(srv, rec, "agent-1", "instance-1", time.Hour)

	result, err := s.sendOnce(context.Background(), &hubclient.AgentLaunchReport{LaunchID: "L1"}, time.Second)
	if err != nil {
		t.Fatalf("sendOnce: %v (want a definitive 403 when every connection says 403)", err)
	}
	if result.HTTPStatus != http.StatusForbidden {
		t.Fatalf("result = %+v, want HTTPStatus=403", result)
	}
	if got := rec.OwnerHub(); got != "" {
		t.Fatalf("OwnerHub = %q, want unset (403 never pins)", got)
	}
}

// TestLaunchSender_FanOutAll401 is TestLaunchSender_FanOutAll403's
// counterpart for 401: when every connection answers 401, that is still a
// (non-pinning) definitive result, not errLaunchReportUnreachable.
func TestLaunchSender_FanOutAll401(t *testing.T) {
	srv := newTestServer(t)
	unauthorized := func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusUnauthorized}, nil
	}
	hubA := &mockRuntimeBrokerService{launchReportFunc: unauthorized}
	hubB := &mockRuntimeBrokerService{launchReportFunc: unauthorized}
	srv.hubMu.Lock()
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "broker-a", HubClient: &stubBrokerHubClient{brokers: hubA}}
	srv.hubConnections["hub-b"] = &HubConnection{Name: "hub-b", BrokerID: "broker-b", HubClient: &stubBrokerHubClient{brokers: hubB}}
	srv.hubMu.Unlock()

	rec := newLaunchRecord("L1", "agent-1", "create", "", time.Now().Add(time.Hour), func() {})
	s := newLaunchSender(srv, rec, "agent-1", "instance-1", time.Hour)

	result, err := s.sendOnce(context.Background(), &hubclient.AgentLaunchReport{LaunchID: "L1"}, time.Second)
	if err != nil {
		t.Fatalf("sendOnce: %v (want a definitive 401 when every connection says 401)", err)
	}
	if result.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("result = %+v, want HTTPStatus=401", result)
	}
	if got := rec.OwnerHub(); got != "" {
		t.Fatalf("OwnerHub = %q, want unset (401 never pins)", got)
	}
}

// TestLaunchSender_FanOut401DoesNotPinButOthersStillConsulted covers design
// §3.8.2's gate-answer table not listing 400/401 as a reason to prefer one
// connection over another: a 401 from a non-owning connection (e.g. during
// key rotation) must not win a fan-out over the connection that actually
// owns the launch.
func TestLaunchSender_FanOut401DoesNotPinButOthersStillConsulted(t *testing.T) {
	srv := newTestServer(t)
	hubAAnswered := make(chan struct{})
	hubA := &mockRuntimeBrokerService{
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			defer close(hubAAnswered)
			return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusUnauthorized}, nil
		},
	}
	hubB := &mockRuntimeBrokerService{
		// Answer only after hub-a has: sendOnce's fan-out loop processes
		// whichever result lands in resultsCh first, so without this
		// ordering the test would only catch a 401 that is allowed to pin
		// when hub-a's answer happens to land first, not deterministically.
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			<-hubAAnswered
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		},
	}
	srv.hubMu.Lock()
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "broker-a", HubClient: &stubBrokerHubClient{brokers: hubA}}
	srv.hubConnections["hub-b"] = &HubConnection{Name: "hub-b", BrokerID: "broker-b", HubClient: &stubBrokerHubClient{brokers: hubB}}
	srv.hubMu.Unlock()

	rec := newLaunchRecord("L1", "agent-1", "create", "", time.Now().Add(time.Hour), func() {})
	s := newLaunchSender(srv, rec, "agent-1", "instance-1", time.Hour)

	result, err := s.sendOnce(context.Background(), &hubclient.AgentLaunchReport{LaunchID: "L1"}, time.Second)
	if err != nil {
		t.Fatalf("sendOnce: %v", err)
	}
	if result.Result != hubclient.AgentLaunchReportResultApplied {
		t.Fatalf("result = %+v, want the applied answer from hub-b", result)
	}
	if got := rec.OwnerHub(); got != "hub-b" {
		t.Fatalf("OwnerHub = %q, want hub-b (401 from hub-a must not pin)", got)
	}
}

// TestLaunchSender_ClaimEndsBackoffWhenAbortRecorded covers SendClaim's
// backoff wait: with the hub unreachable, an abort recorded while the first
// attempt is in flight must end the claim from inside the backoff, well
// before reportMinBackoff (1s) elapses and the next attempt's own pre-check
// would notice it.
func TestLaunchSender_ClaimEndsBackoffWhenAbortRecorded(t *testing.T) {
	var attempts int32
	var s *launchSender
	rtb := &mockRuntimeBrokerService{
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				s.recordKeepaliveAbort(gateAbortCleanup, &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonDeleted})
			}
			return nil, errors.New("simulated unreachable")
		},
	}
	s = newTestLaunchSender(t, rtb, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := s.SendClaim(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, errAbortedByKeepalive) {
		t.Fatalf("SendClaim error = %v, want errAbortedByKeepalive", err)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("SendClaim took %v to notice the recorded abort; want it to end the backoff wait immediately (< 500ms, below reportMinBackoff)", elapsed)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("expected exactly one attempt, got %d", got)
	}
}

// TestLaunchSender_TerminalNotEndedByAbortRecordedDuringIt covers
// SendTerminal ignoring an abort recorded while it is retrying: from the
// terminal's first attempt on, only the terminal's own answer decides the
// outcome (design §3.8.5). Attempt 1 is unreachable and an abort is recorded
// during it; attempt 2 must still happen and its answer be returned.
func TestLaunchSender_TerminalNotEndedByAbortRecordedDuringIt(t *testing.T) {
	var attempts int32
	var s *launchSender
	rtb := &mockRuntimeBrokerService{
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				s.recordKeepaliveAbort(gateAbortCleanup, &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonDeleted})
				return nil, errors.New("simulated unreachable")
			}
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		},
	}
	s = newTestLaunchSender(t, rtb, time.Hour)

	// reportMaxBackoff (10s) plus slack for the second attempt.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := s.SendTerminal(ctx, false, "launching", "runtime_error", "boom", nil)
	if err != nil {
		t.Fatalf("SendTerminal error = %v, want the second attempt's answer (an abort recorded during the terminal must not end it)", err)
	}
	if result == nil || result.Result != hubclient.AgentLaunchReportResultApplied {
		t.Fatalf("SendTerminal result = %+v, want applied from the second attempt", result)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("expected exactly two attempts, got %d", got)
	}
}

// TestLaunchSender_KeepaliveAnswerAfterTerminalStartIsDropped covers a
// keepalive attempt that was already in flight when the terminal's first
// attempt started: its answer is dropped, so even an abort-classifying
// answer records nothing (design §3.8.5: from the terminal's first attempt
// on, only the terminal's answer decides cleanup).
func TestLaunchSender_KeepaliveAnswerAfterTerminalStartIsDropped(t *testing.T) {
	var s *launchSender
	rtb := &mockRuntimeBrokerService{
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			// The terminal starts while this keepalive attempt is in flight.
			s.markTerminalStarted()
			return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonDeleted}, nil
		},
	}
	s = newTestLaunchSender(t, rtb, time.Hour)

	s.sendKeepaliveOnce(context.Background())

	if s.IsAborted() {
		t.Fatal("a keepalive answer that arrived after the terminal started must not record an abort")
	}
	if got := s.LastAbortOutcome(); got != nil {
		t.Fatalf("LastAbortOutcome = %+v, want nil", got)
	}
}
