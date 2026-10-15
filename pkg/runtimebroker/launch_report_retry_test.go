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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// unreachableErrText is the transport error a hub that never answers
// returns; it must not reach the report-retry logs.
const unreachableErrText = "dial tcp: hub-unreachable-canary"

// unansweredTerminals answers claims and progress, never terminals.
func unansweredTerminals(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
	if req.State == hubclient.AgentLaunchReportStateFailed || req.State == hubclient.AgentLaunchReportStateSucceeded {
		return nil, errors.New(unreachableErrText)
	}
	return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
}

// TestRunLaunch_UnansweredTerminalKeepsJournalRecord covers ptone/scion#5186
// on a running broker: a launch whose terminal report gets no hub answer
// within its TTL cleans nothing up and keeps its terminal_pending record for
// the next boot to resend, for a failed and for a succeeded launch.
func TestRunLaunch_UnansweredTerminalKeepsJournalRecord(t *testing.T) {
	for _, c := range []struct {
		name      string
		startErr  error
		succeeded bool
	}{
		{"failed", errors.New("runtime refused"), false},
		{"succeeded", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			mgr := &failingCleanupManager{recFakeManager: newRecFakeManager()}
			if c.startErr != nil {
				mgr.setStartErr(c.startErr)
			}
			srv, rtb := newAsyncTestServer(t, mgr)
			rtb.launchReportFunc = unansweredTerminals

			projectDir := t.TempDir()
			if err := writeLaunchMarker(projectDir, false, "agent-ua", "L-ua"); err != nil {
				t.Fatal(err)
			}
			// A deadline 15 minutes ago leaves the terminal no TTL at all.
			rec := newLaunchRecord("L-ua", "agent-id-ua", store.LaunchKindCreate, "", time.Now().Add(-15*time.Minute), func() {})
			rec.journal = srv.launchJournal.admit(rec, journalRecord{Slug: "agent-ua", AgentName: "agent-ua", RuntimeIdentity: "mock"})
			if rec.journal == nil {
				t.Fatal("launch not journalled")
			}
			srv.trackAsyncLaunch(rec)
			srv.runLaunch(context.Background(), rec, launchCtx{
				opts: api.StartOptions{Name: "agent-ua", ProjectPath: projectDir},
				mgr:  mgr,
				key:  launchKey{Slug: "agent-ua"},
			})

			if n := mgr.CleanupCalls(); n != 0 {
				t.Fatalf("an unanswered terminal must not clean up, got %d clean-up calls", n)
			}
			jr := readJournalFileT(t, rec.journal.path)
			if jr.State != "terminal_pending" || jr.Terminal == nil || jr.Terminal.Succeeded != c.succeeded {
				t.Fatalf("an unanswered terminal did not keep its terminal_pending record: %+v", jr)
			}
			// The launch marker is kept too: a retried report that confirms
			// clean-up must still pass the marker check before deleting files.
			if !launchMarkerMatches(projectDir, false, "agent-ua", "L-ua") {
				t.Fatal("the launch marker was removed although the terminal report got no answer")
			}
		})
	}
}

func writeUnansweredRecord(t *testing.T, srv *Server, jr journalRecord) string {
	t.Helper()
	srv.launchTimingOverride = fastLaunchTimings(fastTestKeepaliveInterval)
	srv.launchRecoveryTTL = 300 * time.Millisecond
	return writeDeadRecord(t, srv, jr)
}

// TestRecoverLaunches_UnansweredReportKeptForRetry covers recovery: a record
// whose report gets no hub answer is kept as terminal_pending with one more
// report attempt; an unfinished launch keeps the broker_restarted terminal
// it sent, and a decided terminal stays unchanged (a succeeded is never
// turned into a failure). Nothing is cleaned up.
func TestRecoverLaunches_UnansweredReportKeptForRetry(t *testing.T) {
	t.Run("launching", func(t *testing.T) {
		mgr := &failingCleanupManager{recFakeManager: newRecFakeManager()}
		srv, rtb := newAsyncTestServer(t, mgr)
		rtb.launchReportFunc = unansweredTerminals
		path := writeUnansweredRecord(t, srv, launchingRecord("LU1"))

		runRecovery(t, srv)

		jr := readJournalFileT(t, path)
		if jr.State != "terminal_pending" || jr.ReportAttempts != 1 || jr.ReportFirstFailedAt.IsZero() ||
			jr.Terminal == nil || jr.Terminal.ErrorCode != api.LaunchCodeBrokerRestarted {
			t.Fatalf("an unanswered recovery report did not keep the record with its broker_restarted terminal: %+v", jr)
		}
		if mgr.CleanupCalls() != 0 {
			t.Fatal("an unanswered report must not clean up")
		}
	})
	t.Run("terminal_pending succeeded", func(t *testing.T) {
		mgr := &failingCleanupManager{recFakeManager: newRecFakeManager()}
		srv, rtb := newAsyncTestServer(t, mgr)
		rtb.launchReportFunc = unansweredTerminals
		rec := launchingRecord("LU2")
		rec.State = journalTerminalPending.String()
		rec.Terminal = &journalTerminal{Succeeded: true, AgentInfo: &hubclient.AgentLaunchReportInfo{ID: "c-LU2", RunID: "run-LU2"}}
		rec.ReportAttempts = 1
		first := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
		rec.ReportFirstFailedAt = first
		path := writeUnansweredRecord(t, srv, rec)

		runRecovery(t, srv)

		jr := readJournalFileT(t, path)
		if jr.State != "terminal_pending" || jr.ReportAttempts != 2 || !jr.ReportFirstFailedAt.Equal(first) {
			t.Fatalf("an unanswered retry did not count one more attempt from the first failure: %+v", jr)
		}
		if jr.Terminal == nil || !jr.Terminal.Succeeded || jr.Terminal.AgentInfo == nil || jr.Terminal.AgentInfo.RunID != "run-LU2" {
			t.Fatalf("an unanswered retry changed the decided succeeded terminal: %+v", jr.Terminal)
		}
	})
}

// TestRecoverLaunches_UnansweredReportRetryBound covers the bound: below it
// the record is kept with one more attempt; at the attempt bound or past
// the age bound recovery gives up, removes the record with no clean-up, and
// logs IDs and counters only, never the transport error.
func TestRecoverLaunches_UnansweredReportRetryBound(t *testing.T) {
	cases := []struct {
		name         string
		attempts     int
		firstAgo     time.Duration
		wantGiveUp   bool
		wantAttempts int
	}{
		{"below the bound", 2, time.Hour, false, 3},
		{"attempt bound", launchReportMaxAttempts - 1, time.Hour, true, 0},
		{"age bound", 1, launchReportMaxAge + time.Hour, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mgr := &failingCleanupManager{recFakeManager: newRecFakeManager()}
			srv, rtb := newAsyncTestServer(t, mgr)
			rtb.launchReportFunc = unansweredTerminals
			logs := captureLifecycleLog(srv)
			rec := launchingRecord("LU3")
			rec.State = journalTerminalPending.String()
			rec.Terminal = &journalTerminal{ErrorCode: api.LaunchCodeBrokerRestarted, Message: launchRestartedMessage}
			rec.ReportAttempts = c.attempts
			rec.ReportFirstFailedAt = time.Now().Add(-c.firstAgo)
			projectDir := t.TempDir()
			rec.ProjectPath = projectDir
			if err := writeLaunchMarker(projectDir, false, rec.Slug, rec.LaunchID); err != nil {
				t.Fatal(err)
			}
			path := writeUnansweredRecord(t, srv, rec)

			runRecovery(t, srv)

			if mgr.CleanupCalls() != 0 {
				t.Fatal("an unanswered report must not clean up")
			}
			out := logs.String()
			var retryLines []string
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "the terminal report got no hub answer") {
					retryLines = append(retryLines, line)
				}
			}
			for _, line := range retryLines {
				if strings.Contains(line, unreachableErrText) {
					t.Fatalf("a report-retry log carries the transport error: %s", line)
				}
			}
			if !c.wantGiveUp {
				jr := readJournalFileT(t, path)
				if jr.ReportAttempts != c.wantAttempts {
					t.Fatalf("an unanswered retry below the bound was not kept with %d attempts: %+v", c.wantAttempts, jr)
				}
				if !launchMarkerMatches(projectDir, false, rec.Slug, rec.LaunchID) {
					t.Fatal("the launch marker was removed while the record is kept for a retry")
				}
				return
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("recovery did not give up at the bound: the record is still there (%v)", err)
			}
			if launchMarkerMatches(projectDir, false, rec.Slug, rec.LaunchID) {
				t.Fatal("giving up left the launch marker behind")
			}
			given := false
			for _, line := range retryLines {
				if strings.Contains(line, "retries given up") && strings.Contains(line, "launch_id=LU3") {
					given = true
				}
			}
			if !given {
				t.Fatalf("no give-up log with the launch ID:\n%s", out)
			}
		})
	}
}

// TestRecoverLaunches_RetriedReportAnswered covers the retry getting its
// answer: a terminal_pending record with earlier unanswered attempts is
// resent, the hub's 409 timed_out (it reaped the launch meanwhile) leads to
// the existing clean-up, and the record is removed.
func TestRecoverLaunches_RetriedReportAnswered(t *testing.T) {
	mgr := &failingCleanupManager{recFakeManager: newRecFakeManager()}
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonTimedOut}, nil
	}
	rec := launchingRecord("LU4")
	rec.State = journalTerminalPending.String()
	rec.Terminal = &journalTerminal{ErrorCode: api.LaunchCodeBrokerRestarted, Message: launchRestartedMessage}
	rec.ReportAttempts = 2
	rec.ReportFirstFailedAt = time.Now().Add(-2 * time.Hour)
	path := writeDeadRecord(t, srv, rec)

	runRecovery(t, srv)

	if n := len(terminalReports(rtb)); n != 1 {
		t.Fatalf("the kept report was not resent once, got %d", n)
	}
	if mgr.CleanupCalls() != 1 {
		t.Fatalf("the answered retry did not clean up (cleanup calls %d)", mgr.CleanupCalls())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the answered retry did not remove the record: %v", err)
	}
}
