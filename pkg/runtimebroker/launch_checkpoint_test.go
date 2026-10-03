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
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for the broker side of the async-launch runtime hooks (design
// t1-async-create-v11.md §3.8.2, §3.8.3, §3.8.4): runLaunch hands the
// runtime a Hub-answered pre-create checkpoint and records every created
// resource for CleanupLaunch.

var (
	hookSecret = agent.ResourceHandle{Kind: api.ResourceKindSecret, Namespace: "ns", Name: "scion-agent-a", UID: "uid-secret"}
	hookPod    = agent.ResourceHandle{Kind: api.ResourceKindPod, Namespace: "ns", Name: "a", UID: "uid-pod"}
)

// hookedManager's Start behaves like the Kubernetes runtime: a checkpoint
// before the Secret create, a checkpoint before the pod create, and a
// handle after each create. A checkpoint error stops it before the create.
type hookedManager struct {
	*asyncManager
}

func (m *hookedManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	if opts.Checkpoint == nil || opts.OnResourceCreated == nil {
		return nil, errors.New("runLaunch did not pass the runtime hooks")
	}
	if err := opts.Checkpoint(ctx, "secrets"); err != nil {
		return nil, fmt.Errorf("create secret: %w", err)
	}
	opts.OnResourceCreated(hookSecret)
	if err := opts.Checkpoint(ctx, "pod_create"); err != nil {
		return nil, fmt.Errorf("create pod: %w", err)
	}
	opts.OnResourceCreated(hookPod)
	return m.asyncManager.Start(ctx, opts)
}

func (m *hookedManager) cleanupHandles() []agent.ResourceHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cleanupLast
}

func checkpointSteps(rtb *mockRuntimeBrokerService) []string {
	var steps []string
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateCheckpoint {
			steps = append(steps, r.Report.Step)
		}
	}
	return steps
}

func terminalReports(rtb *mockRuntimeBrokerService) []*hubclient.AgentLaunchReport {
	var out []*hubclient.AgentLaunchReport
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			out = append(out, r.Report)
		}
	}
	return out
}

func stale409(reason string) *hubclient.AgentLaunchReportResult {
	return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: reason}
}

var appliedAnswer = &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}

// runHookedLaunch runs runLaunch to completion with checkpointAnswer
// deciding each checkpoint report's answer (every other report is applied).
func runHookedLaunch(t *testing.T, ctx context.Context, checkpointAnswer func(step string) (*hubclient.AgentLaunchReportResult, error)) (*hookedManager, *mockRuntimeBrokerService, *launchRecord) {
	t.Helper()
	mgr := &hookedManager{newAsyncManager()}
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if req.State == hubclient.AgentLaunchReportStateCheckpoint {
			return checkpointAnswer(req.Step)
		}
		return appliedAnswer, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	rec := newLaunchRecord("L-hooks", "agent-hooks", store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancel)
	lc := launchCtx{opts: api.StartOptions{Name: "agent-hooks"}, mgr: mgr, key: launchKey{Slug: "agent-hooks"}}

	done := make(chan struct{})
	go func() {
		srv.runLaunch(ctx, rec, lc)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("runLaunch did not return")
	}
	return mgr, rtb, rec
}

func TestRunLaunch_CheckpointBeforeEachCreate_HandlesRecorded(t *testing.T) {
	mgr, rtb, rec := runHookedLaunch(t, context.Background(), func(string) (*hubclient.AgentLaunchReportResult, error) {
		return appliedAnswer, nil
	})
	if got := checkpointSteps(rtb); len(got) != 2 || got[0] != "secrets" || got[1] != "pod_create" {
		t.Fatalf("checkpoint steps = %q, want [secrets pod_create]", got)
	}
	var lastSeq int64
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateCheckpoint {
			if r.Report.Seq <= lastSeq {
				t.Fatalf("checkpoint seq %d not increasing (previous %d)", r.Report.Seq, lastSeq)
			}
			lastSeq = r.Report.Seq
		}
	}
	if got := rec.HandlesSnapshot(); len(got) != 2 || got[0] != hookSecret || got[1] != hookPod {
		t.Fatalf("recorded handles = %+v", got)
	}
	terms := terminalReports(rtb)
	if len(terms) != 1 || terms[0].State != hubclient.AgentLaunchReportStateSucceeded {
		t.Fatalf("terminals = %+v, want one succeeded", terms)
	}
	if n := mgr.CleanupCallCount(); n != 0 {
		t.Fatalf("cleanup ran %d times on a successful launch", n)
	}
}

// A 409 that ends the launch (stopped here) at the pod checkpoint: the pod
// is never created, CleanupLaunch gets the Secret handle recorded so far,
// and no terminal is sent (the Hub already ended the launch).
func TestRunLaunch_CheckpointStaleAnswer_CleansUpRecordedHandlesNoTerminal(t *testing.T) {
	for _, reason := range []string{hubclient.AgentLaunchReportReasonStopped, hubclient.AgentLaunchReportReasonDeleted} {
		t.Run(reason, func(t *testing.T) {
			mgr, rtb, _ := runHookedLaunch(t, context.Background(), func(step string) (*hubclient.AgentLaunchReportResult, error) {
				if step == "pod_create" {
					return stale409(reason), nil
				}
				return appliedAnswer, nil
			})
			if n := mgr.StartCallCount(); n != 0 {
				t.Fatal("the pod create ran after the checkpoint ended the launch")
			}
			if n := mgr.CleanupCallCount(); n != 1 {
				t.Fatalf("cleanup calls = %d, want 1", n)
			}
			if got := mgr.cleanupHandles(); len(got) != 1 || got[0] != hookSecret {
				t.Fatalf("cleanup handles = %+v, want only the secret", got)
			}
			if terms := terminalReports(rtb); len(terms) != 0 {
				t.Fatalf("terminal sent after a stale checkpoint answer: %+v", terms)
			}
		})
	}
}

// superseded / other_owner: the owning launch holds the resource names, so
// nothing is cleaned up and no terminal is sent.
func TestRunLaunch_CheckpointSuperseded_NoCleanupNoTerminal(t *testing.T) {
	for _, reason := range []string{hubclient.AgentLaunchReportReasonSuperseded, hubclient.AgentLaunchReportReasonOtherOwner} {
		t.Run(reason, func(t *testing.T) {
			mgr, rtb, _ := runHookedLaunch(t, context.Background(), func(step string) (*hubclient.AgentLaunchReportResult, error) {
				if step == "pod_create" {
					return stale409(reason), nil
				}
				return appliedAnswer, nil
			})
			if n := mgr.StartCallCount(); n != 0 {
				t.Fatal("the pod create ran after the checkpoint ended the launch")
			}
			if n := mgr.CleanupCallCount(); n != 0 {
				t.Fatalf("cleanup ran %d times after %s", n, reason)
			}
			if terms := terminalReports(rtb); len(terms) != 0 {
				t.Fatalf("terminal sent after %s: %+v", reason, terms)
			}
		})
	}
}

// completed at a checkpoint: later checkpoints are skipped, Run continues,
// and succeeded is still sent (design §3.8.2 step 5.6).
func TestRunLaunch_CheckpointCompleted_SkipsLaterCheckpointsAndSucceeds(t *testing.T) {
	mgr, rtb, rec := runHookedLaunch(t, context.Background(), func(string) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}, nil
	})
	if got := checkpointSteps(rtb); len(got) != 1 || got[0] != "secrets" {
		t.Fatalf("checkpoint steps = %q, want only [secrets]", got)
	}
	if n := mgr.StartCallCount(); n != 1 {
		t.Fatalf("Run did not continue after completed (start calls %d)", n)
	}
	if got := rec.HandlesSnapshot(); len(got) != 2 {
		t.Fatalf("handles = %+v, want both creates", got)
	}
	terms := terminalReports(rtb)
	if len(terms) != 1 || terms[0].State != hubclient.AgentLaunchReportStateSucceeded {
		t.Fatalf("terminals = %+v, want one succeeded", terms)
	}
	if n := mgr.CleanupCallCount(); n != 0 {
		t.Fatalf("cleanup ran %d times", n)
	}
}

// A completed claim answer skips every runtime checkpoint.
func TestRunLaunch_ClaimCompleted_SkipsRuntimeCheckpoints(t *testing.T) {
	mgr := &hookedManager{newAsyncManager()}
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}, nil
		}
		return appliedAnswer, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	rec := newLaunchRecord("L-cc", "agent-cc", store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancel)
	srv.runLaunch(ctx, rec, launchCtx{opts: api.StartOptions{Name: "agent-cc"}, mgr: mgr, key: launchKey{Slug: "agent-cc"}})

	if got := checkpointSteps(rtb); len(got) != 0 {
		t.Fatalf("checkpoints sent after a completed claim: %q", got)
	}
	if n := mgr.StartCallCount(); n != 1 {
		t.Fatalf("start calls = %d, want 1", n)
	}
}

// No definitive answer to a checkpoint before ctx' expires: the launch fails
// with hub_unreachable, and the failed report's applied answer cleans up the
// handles recorded so far.
func TestRunLaunch_CheckpointUnreachable_FailsHubUnreachableAndCleansUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	mgr, rtb, _ := runHookedLaunch(t, ctx, func(step string) (*hubclient.AgentLaunchReportResult, error) {
		if step == "pod_create" {
			return nil, errors.New("simulated unreachable")
		}
		return appliedAnswer, nil
	})
	terms := terminalReports(rtb)
	if len(terms) != 1 || terms[0].State != hubclient.AgentLaunchReportStateFailed {
		t.Fatalf("terminals = %+v, want one failed", terms)
	}
	if terms[0].ErrorCode != "hub_unreachable" {
		t.Fatalf("failed error code = %q, want hub_unreachable", terms[0].ErrorCode)
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatal("the pod create ran with no checkpoint answer")
	}
	if got := mgr.cleanupHandles(); mgr.CleanupCallCount() != 1 || len(got) != 1 || got[0] != hookSecret {
		t.Fatalf("cleanup calls %d with handles %+v, want one with the secret", mgr.CleanupCallCount(), got)
	}
}

// --- launchSender.Checkpoint directly ---

func TestLaunchSender_Checkpoint_Answers(t *testing.T) {
	cases := []struct {
		name        string
		answer      *hubclient.AgentLaunchReportResult
		wantErr     bool
		wantAbort   bool
		wantCleanup bool
	}{
		{"applied", appliedAnswer, false, false, false},
		{"duplicate", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultDuplicate}, false, false, false},
		{"stopped", stale409(hubclient.AgentLaunchReportReasonStopped), true, true, true},
		{"superseded", stale409(hubclient.AgentLaunchReportReasonSuperseded), true, true, false},
		{"forbidden", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rtb := &mockRuntimeBrokerService{launchReportFunc: func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
				return tc.answer, nil
			}}
			s := newTestLaunchSender(t, rtb, time.Hour)
			err := s.Checkpoint(context.Background(), "secrets")
			if (err != nil) != tc.wantErr {
				t.Fatalf("Checkpoint error = %v, wantErr %v", err, tc.wantErr)
			}
			if s.IsAborted() != tc.wantAbort {
				t.Fatalf("IsAborted = %v, want %v", s.IsAborted(), tc.wantAbort)
			}
			if tc.wantAbort {
				o := s.LastAbortOutcome()
				if o == nil || (o.action == gateAbortCleanup) != tc.wantCleanup {
					t.Fatalf("abort outcome = %+v, want cleanup %v", o, tc.wantCleanup)
				}
				// Once aborted, a later checkpoint fails without sending.
				before := len(rtb.getLaunchReports())
				if err := s.Checkpoint(context.Background(), "pod_create"); err == nil {
					t.Fatal("checkpoint after an abort must fail")
				}
				if len(rtb.getLaunchReports()) != before {
					t.Fatal("checkpoint after an abort sent a report")
				}
			}
			reps := rtb.getLaunchReports()
			if len(reps) == 0 || reps[0].Report.State != hubclient.AgentLaunchReportStateCheckpoint || reps[0].Report.Step != "secrets" {
				t.Fatalf("first report = %+v, want a secrets checkpoint", reps)
			}
			if s.CheckpointUnreachable() {
				t.Fatal("a definitive answer must not mark the hub unreachable")
			}
		})
	}
}

func TestLaunchSender_Checkpoint_CompletedSkipsLaterCheckpoints(t *testing.T) {
	rtb := &mockRuntimeBrokerService{launchReportFunc: func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}, nil
	}}
	s := newTestLaunchSender(t, rtb, time.Hour)
	if err := s.Checkpoint(context.Background(), "secrets"); err != nil {
		t.Fatalf("completed must let the create proceed: %v", err)
	}
	if err := s.Checkpoint(context.Background(), "pod_create"); err != nil {
		t.Fatal(err)
	}
	if n := len(rtb.getLaunchReports()); n != 1 {
		t.Fatalf("reports sent = %d, want 1 (later checkpoints skipped)", n)
	}
}

func TestLaunchSender_Checkpoint_UnreachableUntilDeadline(t *testing.T) {
	rtb := &mockRuntimeBrokerService{launchReportFunc: func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return nil, errors.New("simulated unreachable")
	}}
	s := newTestLaunchSender(t, rtb, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := s.Checkpoint(ctx, "pod_create")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Checkpoint error = %v, want a deadline", err)
	}
	if !s.CheckpointUnreachable() {
		t.Fatal("CheckpointUnreachable not recorded")
	}
	if s.IsAborted() {
		t.Fatal("an unreachable hub is not an abort answer")
	}
}

// TestLaunchRecord_HandlesConcurrentAddAndSnapshot: the runtime adds
// handles from Manager.Start's goroutine while runLaunch may snapshot them
// (run under -race).
func TestLaunchRecord_HandlesConcurrentAddAndSnapshot(t *testing.T) {
	rec := newLaunchRecord("L-c", "agent-c", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
	const writers, perWriter = 4, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				rec.AddHandle(agent.ResourceHandle{Kind: api.ResourceKindSecret, Name: fmt.Sprintf("s-%d-%d", w, i), UID: "u"})
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		prev := 0
		for i := 0; i < 100; i++ {
			snap := rec.HandlesSnapshot()
			if len(snap) < prev {
				t.Errorf("snapshot shrank from %d to %d", prev, len(snap))
			}
			prev = len(snap)
			if len(snap) > 0 {
				snap[0].Name = "mutated" // a copy: must not affect the record
			}
		}
	}()
	wg.Wait()
	got := rec.HandlesSnapshot()
	if len(got) != writers*perWriter {
		t.Fatalf("handles = %d, want %d", len(got), writers*perWriter)
	}
	for _, h := range got {
		if h.Name == "mutated" {
			t.Fatal("a snapshot aliases the record's handles")
		}
	}
}

// blockingHookedManager's Start records a Secret handle after its checkpoint
// and then blocks until ctx' is cancelled, like a Kubernetes start waiting
// for its pod when a local stop or delete arrives.
type blockingHookedManager struct {
	*asyncManager
	started chan struct{}
}

func (m *blockingHookedManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	if err := opts.Checkpoint(ctx, "secrets"); err != nil {
		return nil, err
	}
	opts.OnResourceCreated(hookSecret)
	close(m.started)
	<-ctx.Done()
	return nil, fmt.Errorf("wait for pod: %w", ctx.Err())
}

// A local stop or delete (launchRecord.CancelLocal) ending Start: no
// terminal is sent, and the resources recorded so far are cleaned up on a
// live context, unless the launch already completed (claim or checkpoint
// answer). The agent files stay, even with the launch's marker in place:
// they belong to the local stop or delete handler.
func TestRunLaunch_LocalCancelDuringStart_CleansUpRecordedHandles(t *testing.T) {
	completedAnswer := &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}
	for _, tc := range []struct {
		name        string
		answer      func(req *hubclient.AgentLaunchReport) *hubclient.AgentLaunchReportResult
		wantCleanup int
	}{
		{"applied", func(*hubclient.AgentLaunchReport) *hubclient.AgentLaunchReportResult { return appliedAnswer }, 1},
		{"completed at the checkpoint", func(req *hubclient.AgentLaunchReport) *hubclient.AgentLaunchReportResult {
			if req.State == hubclient.AgentLaunchReportStateCheckpoint {
				return completedAnswer
			}
			return appliedAnswer
		}, 0},
		{"completed at the claim", func(req *hubclient.AgentLaunchReport) *hubclient.AgentLaunchReportResult {
			if claimState(req) {
				return completedAnswer
			}
			return appliedAnswer
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &blockingHookedManager{asyncManager: newAsyncManager(), started: make(chan struct{})}
			srv, rtb := newAsyncTestServer(t, mgr)
			rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
				return tc.answer(req), nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec := newLaunchRecord("L-lc", "agent-lc", store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancel)
			projectDir := t.TempDir()
			agentDir := filepath.Join(projectDir, "agents", "agent-lc")
			if err := os.MkdirAll(agentDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), nil, 0644); err != nil {
				t.Fatal(err)
			}
			lc := launchCtx{opts: api.StartOptions{Name: "agent-lc", ProjectPath: projectDir}, mgr: mgr, key: launchKey{Slug: "agent-lc"}}

			done := make(chan struct{})
			go func() {
				srv.runLaunch(ctx, rec, lc)
				close(done)
			}()
			select {
			case <-mgr.started:
			case <-time.After(10 * time.Second):
				t.Fatal("Start did not reach its blocking point")
			}
			// The marker holds this launch until runLaunch returns, so a
			// file cleanup in the cancel branch would delete the files.
			if !launchMarkerMatches(projectDir, false, "agent-lc", "L-lc") {
				t.Fatal("the launch marker does not hold this launch; the file check would prove nothing")
			}
			rec.CancelLocal()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("runLaunch did not return after the local cancel")
			}

			if n := mgr.CleanupCallCount(); n != tc.wantCleanup {
				t.Fatalf("cleanup calls = %d, want %d", n, tc.wantCleanup)
			}
			if tc.wantCleanup == 1 {
				if got := (&hookedManager{mgr.asyncManager}).cleanupHandles(); len(got) != 1 || got[0] != hookSecret {
					t.Fatalf("cleanup handles = %+v, want the recorded secret", got)
				}
				mgr.mu.Lock()
				ctxErr := mgr.cleanupCtxErr
				mgr.mu.Unlock()
				if ctxErr != nil {
					t.Fatalf("cleanup ran on a done context: %v", ctxErr)
				}
			}
			if _, err := os.Stat(agentDir); err != nil {
				t.Fatalf("agent files removed after a local cancel: %v", err)
			}
			if terms := terminalReports(rtb); len(terms) != 0 {
				t.Fatalf("terminal sent after a local cancel: %+v", terms)
			}
		})
	}
}
