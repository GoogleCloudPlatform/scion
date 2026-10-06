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
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestClassifyGateAnswer covers design t1-async-create-v11.md §3.8.2's
// claim/checkpoint answer table (B-2).
func TestClassifyGateAnswer(t *testing.T) {
	cases := []struct {
		name   string
		result *hubclient.AgentLaunchReportResult
		want   gateAction
	}{
		{"applied", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, gateContinue},
		{"duplicate", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultDuplicate}, gateContinue},
		{"completed", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}, gateCompleted},
		{"409 superseded", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonSuperseded}, gateAbortNoCleanup},
		{"409 other_owner", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonOtherOwner}, gateAbortNoCleanup},
		{"409 deleted", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonDeleted}, gateAbortCleanup},
		{"409 stopped", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonStopped}, gateAbortCleanup},
		{"409 timed_out", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonTimedOut}, gateAbortCleanup},
		{"409 lost", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonLost}, gateAbortCleanup},
		{"409 failed", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonFailed}, gateAbortCleanup},
		{"409 not_launched", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonNotLaunched}, gateAbortCleanup},
		{"403", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}, gateAbortCleanup},
		{"404 agent_launch_unknown", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusNotFound, Code: hubclient.AgentLaunchReportCodeUnknownLaunch}, gateAbortCleanup},
		{"400", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusBadRequest}, gateStopNoCleanup},
		{"401", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusUnauthorized}, gateStopNoCleanup},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyGateAnswer(c.result); got != c.want {
				t.Errorf("classifyGateAnswer(%+v) = %v, want %v", c.result, got, c.want)
			}
		})
	}
}

// TestShouldCleanupAfterFailureReport covers design §3.8.2 step 7 (F2(d)):
// every answer to a failed report cleans up except completed and 409
// superseded/other_owner.
func TestShouldCleanupAfterFailureReport(t *testing.T) {
	cases := []struct {
		name   string
		result *hubclient.AgentLaunchReportResult
		want   bool
	}{
		{"applied", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, true},
		{"completed", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}, false},
		{"409 superseded", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonSuperseded}, false},
		{"409 other_owner", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonOtherOwner}, false},
		{"409 timed_out (reaper refine)", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonTimedOut}, true},
		{"409 lost", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonLost}, true},
		{"403", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}, true},
		{"404 agent_launch_unknown", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusNotFound, Code: hubclient.AgentLaunchReportCodeUnknownLaunch}, true},
		{"400", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusBadRequest}, false},
		{"401", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusUnauthorized}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldCleanupAfterFailureReport(c.result); got != c.want {
				t.Errorf("shouldCleanupAfterFailureReport(%+v) = %v, want %v", c.result, got, c.want)
			}
		})
	}
}

func TestClassifyStartError(t *testing.T) {
	t.Run("ctx expired means launch_timeout regardless of the error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 0)
		defer cancel()
		<-ctx.Done()
		code, _ := classifyStartError(ctx, errors.New("some runtime error"))
		if code != "launch_timeout" {
			t.Fatalf("code = %q, want launch_timeout", code)
		}
	})

	t.Run("template not found", func(t *testing.T) {
		code, _ := classifyStartError(context.Background(), config.ErrTemplateNotFound)
		if code != "template_not_found" {
			t.Fatalf("code = %q, want template_not_found", code)
		}
	})

	t.Run("harness config not found", func(t *testing.T) {
		code, _ := classifyStartError(context.Background(), config.ErrHarnessConfigNotFound)
		if code != "template_not_found" {
			t.Fatalf("code = %q, want template_not_found", code)
		}
	})

	t.Run("required skill resolution failure", func(t *testing.T) {
		skillErr := &agent.SkillResolutionError{URI: "gh://owner/repo/my-skill@main", Code: agent.SkillErrCodeNotFound, Message: "skill not found"}
		code, message := classifyStartError(context.Background(), fmt.Errorf("start: %w", skillErr))
		if code != ErrCodeSkillResolution {
			t.Fatalf("code = %q, want %q", code, ErrCodeSkillResolution)
		}
		if !strings.Contains(message, "gh://owner/repo/my-skill@main") {
			t.Fatalf("message %q does not name the skill", message)
		}
	})

	t.Run("other errors are runtime_error", func(t *testing.T) {
		code, _ := classifyStartError(context.Background(), errors.New("boom"))
		if code != "runtime_error" {
			t.Fatalf("code = %q, want runtime_error", code)
		}
	})
}

// TestTerminalContext covers a deadline several minutes in the past still
// leaving a live context, because the TTL is deadline + 10 minutes, not the
// deadline itself.
func TestTerminalContext(t *testing.T) {
	ctx, cancel := terminalContext(time.Now().Add(-5 * time.Minute))
	defer cancel()
	if ctx.Err() != nil {
		t.Fatalf("expected a live context 5 minutes past the deadline (TTL = deadline+10min), got %v", ctx.Err())
	}
}

func TestTerminalContext_ExpiredPastTheTTL(t *testing.T) {
	ctx, cancel := terminalContext(time.Now().Add(-15 * time.Minute))
	defer cancel()
	if ctx.Err() == nil {
		t.Fatal("expected an expired context 15 minutes past the deadline (beyond the 10 min TTL)")
	}
}

// TestRunLaunch_TerminalTTLExpiresWithNoAnswer_NoCleanup exercises
// runLaunch's succeeded-report path end to end (not just terminalContext in
// isolation) when the Hub never gives a definitive answer before the
// terminal TTL runs out: design §3.8.2 step 7's "no definitive answer means
// no cleanup" rule applies here too, the same as a failed report. rec's
// Deadline is set far enough in the past that terminalContext's TTL
// (deadline+10min) is already expired, so SendTerminal's retry loop exits
// on its first attempt instead of the test waiting out a real 10-minute
// window.
func TestRunLaunch_TerminalTTLExpiresWithNoAnswer_NoCleanup(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
		// The terminal (succeeded) report never gets a definitive answer.
		return nil, errors.New("simulated unreachable")
	}

	rec := newLaunchRecord("L-ttl", "agent-ttl", store.LaunchKindCreate, "", time.Now().Add(-15*time.Minute), func() {})
	lc := launchCtx{
		opts: api.StartOptions{Name: "agent-ttl"},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-ttl"},
	}

	done := make(chan struct{})
	go func() {
		srv.runLaunch(context.Background(), rec, lc)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runLaunch did not return; the already-expired terminal TTL should make SendTerminal give up immediately")
	}

	if n := mgr.StartCallCount(); n != 1 {
		t.Fatalf("expected Start to be called once, got %d", n)
	}
	if n := mgr.CleanupCallCount(); n != 0 {
		t.Fatalf("expected no cleanup when the terminal TTL expired with no definitive answer, got %d calls", n)
	}
}

func TestShouldCleanupAfterSucceededReport(t *testing.T) {
	cases := []struct {
		name   string
		result *hubclient.AgentLaunchReportResult
		want   bool
	}{
		{"applied", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, false},
		{"duplicate", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultDuplicate}, false},
		{"completed", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}, false},
		{"409 superseded", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonSuperseded}, false},
		{"409 other_owner", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonOtherOwner}, false},
		{"409 timed_out", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonTimedOut}, true},
		{"409 lost", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonLost}, true},
		{"403", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}, true},
		{"404 agent_launch_unknown", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusNotFound, Code: hubclient.AgentLaunchReportCodeUnknownLaunch}, true},
		{"400", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusBadRequest}, false},
		{"401", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusUnauthorized}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldCleanupAfterSucceededReport(c.result); got != c.want {
				t.Errorf("shouldCleanupAfterSucceededReport(%+v) = %v, want %v", c.result, got, c.want)
			}
		})
	}
}

// TestCleanupAbortedLaunch_DeletesFilesWhenMarkerMatches and
// TestCleanupAbortedLaunch_KeepsFilesWhenMarkerMismatched cover
// cleanupAbortedLaunch actually removing the agent's files when its own
// marker write is still the current one, and leaving them alone when a
// newer launch's marker write has superseded it (design §3.8.4; this is
// also the two-brokers-sharing-an-agents-root case, since the marker
// mechanism is broker-agnostic file state, not in-memory state).
func TestCleanupAbortedLaunch_DeletesFilesWhenMarkerMatches(t *testing.T) {
	projectDir := t.TempDir()
	agentDir := filepath.Join(projectDir, "agents", "agent-x")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeLaunchMarker(projectDir, false, "agent-x", "L1"); err != nil {
		t.Fatal(err)
	}

	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	rec := newLaunchRecord("L1", "agent-id", store.LaunchKindCreate, "", time.Now().Add(time.Minute), func() {})
	lc := launchCtx{
		opts: api.StartOptions{Name: "agent-x", ProjectPath: projectDir},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-x"},
	}

	srv.cleanupAbortedLaunch(mgr, rec, lc)

	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent directory to be removed (marker matched), stat err = %v", err)
	}
	if n := mgr.CleanupCallCount(); n != 1 {
		t.Fatalf("expected CleanupLaunch to be called once, got %d", n)
	}
}

func TestCleanupAbortedLaunch_KeepsFilesWhenMarkerMismatched(t *testing.T) {
	projectDir := t.TempDir()
	agentDir := filepath.Join(projectDir, "agents", "agent-y")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	// A newer launch's marker write has superseded this one.
	if err := writeLaunchMarker(projectDir, false, "agent-y", "L-newer"); err != nil {
		t.Fatal(err)
	}

	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	rec := newLaunchRecord("L-old", "agent-id", store.LaunchKindCreate, "", time.Now().Add(time.Minute), func() {})
	lc := launchCtx{
		opts: api.StartOptions{Name: "agent-y", ProjectPath: projectDir},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-y"},
	}

	srv.cleanupAbortedLaunch(mgr, rec, lc)

	if _, err := os.Stat(agentDir); err != nil {
		t.Fatalf("expected the agent directory to survive a marker mismatch, stat err = %v", err)
	}
}

// TestRunLaunch_WaitsForSupersededRecordStillCleaningUp exercises
// WaitSuperseded through two real runLaunch goroutines for the same key
// (design §3.8.2 step 5.2, F5), not just the registry's Begin/Finish
// bookkeeping in isolation: a new launch for a key still held by an older
// launch that is itself still blocked inside CleanupLaunch must not write
// its marker or call Start until the older launch's cleanup actually
// finishes.
func TestRunLaunch_WaitsForSupersededRecordStillCleaningUp(t *testing.T) {
	key := launchKey{Slug: "agent-shared"}

	oldMgr := newAsyncManager()
	oldMgr.setStartErr(errors.New("boom"))
	cleanupBlock := make(chan struct{})
	oldMgr.setCleanupBlock(cleanupBlock)

	newMgr := newAsyncManager()

	srv, rtb := newAsyncTestServer(t, oldMgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
		if req.LaunchID == "L-old" && req.State == hubclient.AgentLaunchReportStateFailed {
			// 403 is one of the design's listed cleanup reasons, so the old
			// launch's failure drives it into cleanupAbortedLaunch, which
			// blocks on cleanupBlock above.
			return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}, nil
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	oldRec := newLaunchRecord("L-old", "agent-old", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
	oldSupersededDone := srv.launchRegistry.Begin(key, oldRec)
	if oldSupersededDone != nil {
		t.Fatal("expected no superseded record for the first Begin")
	}
	oldLc := launchCtx{opts: api.StartOptions{Name: "agent-shared"}, mgr: oldMgr, key: key}

	oldDone := make(chan struct{})
	go func() {
		srv.runLaunch(context.Background(), oldRec, oldLc)
		close(oldDone)
	}()

	if !waitUntil(t, 2*time.Second, func() bool { return oldMgr.CleanupCallCount() >= 1 }) {
		t.Fatal("expected the old launch to reach CleanupLaunch (and block there)")
	}

	// The old launch is now blocked inside CleanupLaunch, so its runLaunch
	// goroutine has not returned and Finish has not closed its done channel
	// yet. Begin for the same key must hand the new launch that still-open
	// channel.
	newRec := newLaunchRecord("L-new", "agent-new", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
	newSupersededDone := srv.launchRegistry.Begin(key, newRec)
	if newSupersededDone == nil {
		t.Fatal("expected Begin to return the old record's still-open done channel")
	}
	newLc := launchCtx{
		opts:           api.StartOptions{Name: "agent-shared"},
		mgr:            newMgr,
		key:            key,
		supersededDone: newSupersededDone,
	}

	newDone := make(chan struct{})
	go func() {
		srv.runLaunch(context.Background(), newRec, newLc)
		close(newDone)
	}()

	// Give the new launch's claim (answered applied, fast) a moment to clear
	// so it would already be waiting at WaitSuperseded if nothing blocked it,
	// then confirm it has NOT called Start while the old launch is still
	// cleaning up.
	time.Sleep(200 * time.Millisecond)
	if n := newMgr.StartCallCount(); n != 0 {
		t.Fatalf("expected the new launch to wait for the superseded record before calling Start, got %d calls", n)
	}

	// Let the old launch's cleanup finish, which closes its done channel and
	// unblocks the new launch's WaitSuperseded.
	close(cleanupBlock)

	select {
	case <-oldDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the old launch did not finish after its cleanup was unblocked")
	}

	if !waitUntil(t, 2*time.Second, func() bool { return newMgr.StartCallCount() >= 1 }) {
		t.Fatal("expected the new launch to call Start once the superseded record finished")
	}

	select {
	case <-newDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the new launch did not finish")
	}
}

// TestRunLaunch_LocalCancelDuringWaitSuperseded_SendsNoTerminal covers a
// local stop/delete (launchRegistry.CancelLocal) waking a launch that is
// still blocked in WaitSuperseded on a predecessor that never finishes: the
// same rule as a locally cancelled claim applies here too (design's
// local-cancel case) -- no terminal, and since this happens before the
// marker would even be written, no marker either.
func TestRunLaunch_LocalCancelDuringWaitSuperseded_SendsNoTerminal(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	key := launchKey{Slug: "agent-cancel-wait-superseded"}
	oldRec := newLaunchRecord("L-old-cancel-wait", "agent-old-cancel-wait", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
	srv.launchRegistry.Begin(key, oldRec) // never released by this test

	projectDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newRec := newLaunchRecord("L-new-cancel-wait", "agent-new-cancel-wait", store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancel)
	supersededDone := srv.launchRegistry.Begin(key, newRec)
	if supersededDone == nil {
		t.Fatal("expected Begin to return the predecessor's still-open done channel")
	}
	lc := launchCtx{
		opts:           api.StartOptions{Name: "agent-cancel-wait-superseded", ProjectPath: projectDir},
		mgr:            mgr,
		key:            key,
		supersededDone: supersededDone,
	}

	done := make(chan struct{})
	go func() {
		srv.runLaunch(ctx, newRec, lc)
		close(done)
	}()

	if !waitUntil(t, 2*time.Second, func() bool { return len(rtb.getLaunchReports()) >= 1 }) {
		t.Fatal("expected the claim to be sent before WaitSuperseded blocks")
	}
	// Give runLaunch a moment to actually reach WaitSuperseded (the
	// predecessor is never released, so it can only be here or further
	// back) before cancelling it locally.
	time.Sleep(100 * time.Millisecond)
	newRec.CancelLocal()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runLaunch did not return after a local cancel during WaitSuperseded")
	}

	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called when locally cancelled during WaitSuperseded, got %d calls", n)
	}
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			t.Fatalf("a local cancel during WaitSuperseded must send no terminal, got %+v", r.Report)
		}
	}
	if readLaunchMarker(projectDir, false, "agent-cancel-wait-superseded") != "" {
		t.Fatal("expected no marker to have been written (cancelled before reaching that step)")
	}
}

// TestRunLaunch_LocalCancelDuringStart_SendsNoTerminal covers a local
// stop/delete waking Start via ctx' cancellation: Manager.Start returns
// context.Canceled (not a real deadline), which must be treated like the
// claim's and WaitSuperseded's local-cancel cases -- no terminal sent,
// distinct from classifyStartError's launch_timeout/runtime_error paths.
func TestRunLaunch_LocalCancelDuringStart_SendsNoTerminal(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{}) // Start blocks until ctx' is cancelled
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	rec := newLaunchRecord("L-cancel-during-start", "agent-cancel-during-start", store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancel)
	lc := launchCtx{
		opts: api.StartOptions{Name: "agent-cancel-during-start"},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-cancel-during-start"},
	}

	done := make(chan struct{})
	go func() {
		srv.runLaunch(ctx, rec, lc)
		close(done)
	}()

	if !waitUntil(t, 2*time.Second, func() bool { return mgr.StartCallCount() >= 1 }) {
		t.Fatal("expected Start to be called (and then blocked)")
	}
	rec.CancelLocal()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runLaunch did not return after a local cancel during Start")
	}

	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			t.Fatalf("a local cancel during Start must send no terminal, got %+v", r.Report)
		}
	}
}

// TestRunLaunch_AbortRecordedDuringDownload_NoStart covers the IsAborted
// check right after the GCS download step: the download is held open via
// launchCtx.downloadWorkspaceFromGCS (a test-only override; beginAsyncLaunch
// never sets it, so production always uses Server.downloadWorkspaceFromGCS)
// long enough for a keepalive abort to be recorded while it is still
// running, so by the time the download returns the abort is already there
// for this check -- not the identical ones before it (the claim already
// succeeded and WaitSuperseded/the marker step are both instantaneous here,
// well before the keepalive's first attempt at the 1s mark) and not a later
// one, since nothing follows this check but Start.
func TestRunLaunch_AbortRecordedDuringDownload_NoStart(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)

	abortAnswered := make(chan struct{})
	var abortAnsweredOnce sync.Once
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
		abortAnsweredOnce.Do(func() { close(abortAnswered) })
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonDeleted}, nil
	}

	releaseDownload := make(chan struct{})
	downloadStarted := make(chan struct{})
	var downloadStartedOnce sync.Once

	rec := newLaunchRecord("L-guard-d", "agent-guard-d", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
	lc := launchCtx{
		req:  CreateAgentRequest{LaunchKeepaliveSeconds: 1},
		opts: api.StartOptions{Name: "agent-guard-d"},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-guard-d"},
		downloadWorkspaceFromGCS: func(ctx context.Context, req CreateAgentRequest, opts api.StartOptions) (api.StartOptions, string, string, error) {
			downloadStartedOnce.Do(func() { close(downloadStarted) })
			select {
			case <-releaseDownload:
			case <-ctx.Done():
			}
			return opts, "", "", nil
		},
	}

	done := make(chan struct{})
	go func() {
		srv.runLaunch(context.Background(), rec, lc)
		close(done)
	}()

	select {
	case <-downloadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the download to start")
	}
	select {
	case <-abortAnswered:
	case <-time.After(3 * time.Second):
		t.Fatal("expected the keepalive's abort-triggering answer")
	}
	// Let recordKeepaliveAbort finish well before the download returns.
	time.Sleep(100 * time.Millisecond)
	close(releaseDownload)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runLaunch did not return after the download finished with an abort already recorded")
	}

	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called when the abort was already recorded during the download, got %d calls", n)
	}
	if !waitUntil(t, 2*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("expected the already-recorded abort to trigger cleanup once the download returns")
	}
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			t.Fatalf("an abort recorded during the download must send no terminal, got %+v", r.Report)
		}
	}
}

// TestLaunchSenderAndLaunchCtx_NoHTTPRequestField is design §6 B-6's
// type-level assertion: the types the launch goroutine is built from must
// never carry an *http.Request or http.Request field, so there is no code
// path that could read one.
func TestLaunchSenderAndLaunchCtx_NoHTTPRequestField(t *testing.T) {
	assertNoHTTPRequestField(t, reflect.TypeOf(launchSender{}))
	assertNoHTTPRequestField(t, reflect.TypeOf(launchCtx{}))
	assertNoHTTPRequestField(t, reflect.TypeOf(launchRecord{}))
}

func assertNoHTTPRequestField(t *testing.T, typ reflect.Type) {
	t.Helper()
	reqType := reflect.TypeOf(http.Request{})
	reqPtrType := reflect.TypeOf(&http.Request{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type == reqType || f.Type == reqPtrType {
			t.Fatalf("%s.%s has type %s; the launch goroutine must never retain *http.Request", typ.Name(), f.Name, f.Type)
		}
	}
}

// killedCLIError returns the error an exec-based runtime (docker, podman,
// apple) hands back when ctx' is cancelled while its CLI is running:
// exec.CommandContext kills the process and Wait reports the process's own
// *exec.ExitError ("signal: killed"), not context.Canceled, wrapped with %w
// the way pkg/runtime wraps command failures.
func killedCLIError(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "kill -9 $$").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an *exec.ExitError from a killed shell, got %T: %v", err, err)
	}
	wrapped := fmt.Errorf("failed to start container: %w", err)
	if errors.Is(wrapped, context.Canceled) {
		t.Fatal("the killed-CLI error must not match context.Canceled, or it does not exercise the ctx'-based check")
	}
	return wrapped
}

// assertNoTerminal fails the test if any succeeded or failed report was sent.
func assertNoTerminal(t *testing.T, rtb *mockRuntimeBrokerService, what string) {
	t.Helper()
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			t.Fatalf("%s must send no terminal, got %+v", what, r.Report)
		}
	}
}

// TestRunLaunch_LocalCancelDuringStart_ExitErrorSendsNoTerminal covers a
// local stop/delete during Start on an exec-based runtime: Start returns the
// killed CLI's *exec.ExitError rather than context.Canceled, and the launch
// must still be treated as locally cancelled (no terminal), not reported as
// runtime_error.
func TestRunLaunch_LocalCancelDuringStart_ExitErrorSendsNoTerminal(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{}) // Start blocks until ctx' is cancelled
	mgr.startCancelErr = killedCLIError(t)
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	rec := newLaunchRecord("L-cancel-exit-err", "agent-cancel-exit-err", store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancel)
	lc := launchCtx{
		opts: api.StartOptions{Name: "agent-cancel-exit-err"},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-cancel-exit-err"},
	}

	done := make(chan struct{})
	go func() {
		srv.runLaunch(ctx, rec, lc)
		close(done)
	}()

	if !waitUntil(t, 2*time.Second, func() bool { return mgr.StartCallCount() >= 1 }) {
		t.Fatal("expected Start to be called (and then blocked)")
	}
	rec.CancelLocal()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runLaunch did not return after a local cancel during Start")
	}
	assertNoTerminal(t, rtb, "a local cancel during Start that surfaces as an exec exit error")
}

// TestRunLaunch_LocalCancelDuringDownload_SendsNoTerminal covers a local
// stop/delete during the GCS download step, whose error (whatever its value)
// must not be reported as runtime_error either.
func TestRunLaunch_LocalCancelDuringDownload_SendsNoTerminal(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	downloadStarted := make(chan struct{})
	var downloadStartedOnce sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	rec := newLaunchRecord("L-cancel-download", "agent-cancel-download", store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancel)
	lc := launchCtx{
		opts: api.StartOptions{Name: "agent-cancel-download"},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-cancel-download"},
		downloadWorkspaceFromGCS: func(ctx context.Context, req CreateAgentRequest, opts api.StartOptions) (api.StartOptions, string, string, error) {
			downloadStartedOnce.Do(func() { close(downloadStarted) })
			<-ctx.Done()
			return opts, "failed to download workspace from GCS", "Failed to download workspace from GCS: interrupted",
				errors.New("failed to download workspace from GCS: interrupted")
		},
	}

	done := make(chan struct{})
	go func() {
		srv.runLaunch(ctx, rec, lc)
		close(done)
	}()

	select {
	case <-downloadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the download to start")
	}
	rec.CancelLocal()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runLaunch did not return after a local cancel during the download")
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called after a local cancel during the download, got %d calls", n)
	}
	assertNoTerminal(t, rtb, "a local cancel during the download")
}

// TestRunLaunch_DownloadValidatesWorkspaceDirBeforeCreatingIt covers
// runLaunch's own GCS download validating the workspace directory before it
// creates it, for a directory that became invalid after admission: the
// agent directory under WorktreeBase is a symlink to a directory outside it.
// The launch fails under the launching step, Start is never called, and
// nothing is created through the symlink.
func TestRunLaunch_DownloadValidatesWorkspaceDirBeforeCreatingIt(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}
	const name = "agent-download-invalid-dir"
	outside := symlinkedWorktreeAgentDir(t, srv, name)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newLaunchRecord("L-download-invalid-dir", name, store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancel)
	lc := launchCtx{
		req:  CreateAgentRequest{Name: name, WorkspaceStoragePath: "some/path"},
		opts: api.StartOptions{Name: name, ProjectPath: t.TempDir()},
		mgr:  mgr,
		key:  launchKey{Slug: name},
	}

	done := make(chan struct{})
	go func() {
		srv.runLaunch(ctx, rec, lc)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runLaunch did not return after an invalid workspace directory")
	}

	assertNoOutsideWorkspace(t, outside)
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called for an invalid workspace directory, got %d calls", n)
	}
	var terminals []*hubclient.AgentLaunchReport
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			terminals = append(terminals, r.Report)
		}
	}
	if len(terminals) != 1 {
		t.Fatalf("expected exactly one terminal, got %d: %+v", len(terminals), terminals)
	}
	got := terminals[0]
	if got.State != hubclient.AgentLaunchReportStateFailed || got.Step != "launching" {
		t.Fatalf("terminal = %+v, want failed under the launching step", got)
	}
	if got.ErrorCode != "runtime_error" {
		t.Fatalf("terminal error code = %q, want runtime_error", got.ErrorCode)
	}
	if !strings.Contains(got.Message, "invalid workspace directory") {
		t.Fatalf("terminal message = %q, want it to name the invalid workspace directory", got.Message)
	}
}

// TestRunLaunch_DeadlineDuringWaitSuperseded_FailsLaunchingWithLaunchTimeout
// covers ctx' reaching its deadline while the launch is still waiting on a
// superseded predecessor that never finishes: exactly one failed terminal,
// launch_timeout, under the launching step (the claim was already applied),
// and Start is never called.
func TestRunLaunch_DeadlineDuringWaitSuperseded_FailsLaunchingWithLaunchTimeout(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	key := launchKey{Slug: "agent-deadline-wait-superseded"}
	oldRec := newLaunchRecord("L-old-deadline-wait", "agent-old-deadline-wait", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
	srv.launchRegistry.Begin(key, oldRec) // never released by this test

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	newRec := newLaunchRecord("L-new-deadline-wait", "agent-new-deadline-wait", store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancel)
	supersededDone := srv.launchRegistry.Begin(key, newRec)
	if supersededDone == nil {
		t.Fatal("expected Begin to return the predecessor's still-open done channel")
	}
	projectDir := t.TempDir()
	lc := launchCtx{
		opts:           api.StartOptions{Name: "agent-deadline-wait-superseded", ProjectPath: projectDir},
		mgr:            mgr,
		key:            key,
		supersededDone: supersededDone,
	}

	done := make(chan struct{})
	go func() {
		srv.runLaunch(ctx, newRec, lc)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runLaunch did not return after ctx' reached its deadline during WaitSuperseded")
	}

	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called when ctx' expires during WaitSuperseded, got %d calls", n)
	}
	var terminals []*hubclient.AgentLaunchReport
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			terminals = append(terminals, r.Report)
		}
	}
	if len(terminals) != 1 {
		t.Fatalf("expected exactly one terminal, got %d: %+v", len(terminals), terminals)
	}
	got := terminals[0]
	if got.State != hubclient.AgentLaunchReportStateFailed {
		t.Fatalf("terminal state = %q, want failed", got.State)
	}
	if got.ErrorCode != "launch_timeout" {
		t.Fatalf("terminal error code = %q, want launch_timeout", got.ErrorCode)
	}
	if got.Step != "launching" {
		t.Fatalf("terminal step = %q, want launching (the claim was already applied)", got.Step)
	}
	if !strings.Contains(got.Message, "superseded launch") {
		t.Fatalf("terminal message = %q, want it to name the superseded launch", got.Message)
	}
	if readLaunchMarker(projectDir, false, "agent-deadline-wait-superseded") != "" {
		t.Fatal("expected no marker to have been written (the deadline hit before that step)")
	}
}

// TestRunLaunch_AbortRecordedAfterStartSelected_CleansUpWithoutTerminal
// covers the IsAborted check right after runLaunch's select has taken
// Start's result: an abort recorded at exactly that point (pinned with the
// launchCtx.afterStartSelected test seam, since a real keepalive can only
// land there by chance) must clean up once and send no terminal, rather than
// reporting the successful Start.
func TestRunLaunch_AbortRecordedAfterStartSelected_CleansUpWithoutTerminal(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	var hookCalls int
	rec := newLaunchRecord("L-abort-after-start", "agent-abort-after-start", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
	lc := launchCtx{
		opts: api.StartOptions{Name: "agent-abort-after-start"},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-abort-after-start"},
		afterStartSelected: func(sender *launchSender) {
			hookCalls++
			sender.recordKeepaliveAbort(gateAbortCleanup, &hubclient.AgentLaunchReportResult{
				HTTPStatus: http.StatusConflict,
				Code:       hubclient.AgentLaunchReportCodeStaleLaunch,
				Reason:     hubclient.AgentLaunchReportReasonDeleted,
			})
		},
	}

	done := make(chan struct{})
	go func() {
		srv.runLaunch(context.Background(), rec, lc)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runLaunch did not return")
	}

	if hookCalls != 1 {
		t.Fatalf("expected the afterStartSelected seam to run once, got %d", hookCalls)
	}
	if n := mgr.StartCallCount(); n != 1 {
		t.Fatalf("expected Start to be called once, got %d", n)
	}
	if n := mgr.CleanupCallCount(); n != 1 {
		t.Fatalf("expected exactly one cleanup for the abort recorded after Start was selected, got %d", n)
	}
	assertNoTerminal(t, rtb, "an abort recorded right after Start's result was selected")
}
