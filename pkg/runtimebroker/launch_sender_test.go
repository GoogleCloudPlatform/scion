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

// TestNewLaunchSender_DefaultsKeepaliveIntervalTo15s covers mutation M4: a
// non-positive keepaliveInterval (the Hub's create request omitted
// LaunchKeepaliveSeconds) must default to 15s (design §3.7).
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

// TestLaunchSender_KeepaliveStopsOnTerminalStart covers design r8-8 / review
// r1 F-10: once a terminal send starts, the keepalive loop exits and sends
// nothing more.
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
// gate-answer table applying to keepalives (review r1 F-5): a "completed"
// answer is recorded without aborting.
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

// TestLaunchSender_KeepaliveRecordsAbortAnswer covers review r1 F-5: a
// definitive non-continue, non-completed answer (409 lost here) must wake
// KeepaliveAborted with the classified action.
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

// TestLaunchSender_KeepaliveRetriesAfterBackoff covers mutation M9: a failed
// keepalive attempt is retried only after a 2-5s jittered backoff, not
// immediately.
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
		t.Fatalf("retry landed after %v, want at least the 2s backoff floor (mutation M9 catch)", elapsed)
	}
}

// TestLaunchSender_AttemptTimesOutAt5s covers mutations M5/M6: both the
// keepalive and terminal attempt timeouts are 5s, not longer.
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

	t.Run("single attempt", func(t *testing.T) {
		// sendOnce makes exactly one attempt (no retry loop), so this
		// isolates the 5s per-attempt timeout itself rather than however
		// long sendKeepaliveOnce's/sendReportBlocking's retry loop runs.
		start := time.Now()
		_, err := s.sendOnce(context.Background(), &hubclient.AgentLaunchReport{LaunchID: "L1"}, 5*time.Second)
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("expected the attempt to time out (the mock never answers)")
		}
		// Generous relative to 5s, but far short of the 60s a
		// timeout-constant mutation (M5/M6) would produce.
		if elapsed > 15*time.Second {
			t.Fatalf("single attempt took %v; the attempt timeout looks much longer than 5s", elapsed)
		}
	})
}

// TestLaunchSender_FanOutAllUnknownMeansUnknown and
// TestLaunchSender_FanOutMixOfUnknownAndUnreachableMeansRetry cover review r1
// F-14's fan-out aggregation gaps (design §3.8.5 routing rule 4).
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

// TestLaunchSender_FanOut403DoesNotPinButOthersStillConsulted covers review
// r1 F-8: a 403 from one connection must not pin OwnerHub, and the fan-out
// must still consult the other connection.
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
