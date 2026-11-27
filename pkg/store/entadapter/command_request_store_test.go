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

//go:build !no_sqlite

package entadapter

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/execrunoutput"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func newCommandRequestTestStore(t *testing.T) (*CompositeStore, string, string) {
	t.Helper()
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projID := uuid.New().String()
	if err := cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Slug: "cr-proj-" + projID[:8],
		Name: "CR Project",
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	agentID := uuid.New().String()
	if err := cs.CreateAgent(ctx, &store.Agent{
		ID:        agentID,
		Slug:      "cr-agent",
		Name:      "cr-agent",
		ProjectID: projID,
		Phase:     "running",
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return cs, projID, agentID
}

func makeCommandRequestPromptInput(t *testing.T, projID, agentID, approverID, key, script string, update bool, now time.Time) *store.CommandRequestPromptInput {
	t.Helper()
	req, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
		Key:         key,
		Description: "Run " + key,
		Script:      script,
	})
	if err != nil {
		t.Fatalf("NormalizeCommandRequest: %v", err)
	}
	msgID := uuid.New().String()
	return &store.CommandRequestPromptInput{
		Message: &store.Message{
			ID:          msgID,
			ProjectID:   projID,
			Sender:      "agent:cr-agent",
			SenderID:    agentID,
			Recipient:   "user:alice@example.com",
			RecipientID: approverID,
			ThreadID:    "dm:agent:" + agentID + ":user:" + approverID,
		},
		Intro:        "Please approve this command.",
		ProjectName:  "CR Project",
		ApproverHint: "alice@example.com",
		AgentID:      agentID,
		AgentName:    "cr-agent",
		ApproverID:   approverID,
		Request:      req,
		Update:       update,
		Now:          now,
	}
}

func TestCommandRequestStore_CreateVersioningUpdateAndExpiry(t *testing.T) {
	ctx := context.Background()
	cs, projID, agentID := newCommandRequestTestStore(t)
	approverID := uuid.New().String()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// v1 created cleanly.
	res1, err := cs.CreateCommandRequestPrompt(ctx, makeCommandRequestPromptInput(t, projID, agentID, approverID, "deploy-preview", "echo v1", false, now))
	if err != nil {
		t.Fatalf("CreateCommandRequestPrompt v1: %v", err)
	}
	if res1.Message.Type != messages.TypeCommandRequest {
		t.Errorf("message type = %q, want %q", res1.Message.Type, messages.TypeCommandRequest)
	}
	if res1.Request.Version != 1 || res1.Request.Status != store.CommandRequestStatusPending {
		t.Fatalf("v1 = %+v", res1.Request)
	}
	if res1.Request.IntroLength != len("Please approve this command.") {
		t.Errorf("IntroLength = %d, want %d", res1.Request.IntroLength, len("Please approve this command."))
	}

	// Repeat without --update fails with CommandRequestKeyPendingError.
	_, err = cs.CreateCommandRequestPrompt(ctx, makeCommandRequestPromptInput(t, projID, agentID, approverID, "DEPLOY-PREVIEW", "echo v1b", false, now.Add(time.Second)))
	var pendingErr *store.CommandRequestKeyPendingError
	if !errors.As(err, &pendingErr) {
		t.Fatalf("expected CommandRequestKeyPendingError, got %v", err)
	}
	if pendingErr.Version != 1 || pendingErr.RowID != res1.Request.ID {
		t.Errorf("pendingErr = %+v", pendingErr)
	}

	// Repeat with --update creates v2 and marks v1 replaced.
	res2, err := cs.CreateCommandRequestPrompt(ctx, makeCommandRequestPromptInput(t, projID, agentID, approverID, "deploy-preview", "echo v2", true, now.Add(2*time.Second)))
	if err != nil {
		t.Fatalf("CreateCommandRequestPrompt v2: %v", err)
	}
	if res2.Request.Version != 2 || res2.Replaced == nil || res2.Replaced.ID != res1.Request.ID {
		t.Fatalf("v2 = %+v, replaced = %+v", res2.Request, res2.Replaced)
	}
	v1After, err := cs.GetCommandRequest(ctx, res1.Request.ID)
	if err != nil {
		t.Fatalf("GetCommandRequest v1: %v", err)
	}
	if v1After.Status != store.CommandRequestStatusReplaced || v1After.ReplacedByID != res2.Request.ID {
		t.Errorf("v1 after update = %+v", v1After)
	}

	// Expire pending v2 after 24h.
	expired, err := cs.ExpireCommandRequests(ctx, now.Add(25*time.Hour), 100)
	if err != nil {
		t.Fatalf("ExpireCommandRequests: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != res2.Request.ID || expired[0].Status != store.CommandRequestStatusExpired {
		t.Fatalf("expired = %+v", expired)
	}

	// Repost after terminal (expired) without --update allocates v3 (RepostTerminalOK=true).
	res3, err := cs.CreateCommandRequestPrompt(ctx, makeCommandRequestPromptInput(t, projID, agentID, approverID, "deploy-preview", "echo v3", false, now.Add(26*time.Hour)))
	if err != nil {
		t.Fatalf("CreateCommandRequestPrompt v3: %v", err)
	}
	if res3.Request.Version != 3 {
		t.Errorf("v3 version = %d, want 3", res3.Request.Version)
	}
}

func TestCommandRequestStore_ApproveCASIdempotencyAndRunLifecycle(t *testing.T) {
	ctx := context.Background()
	cs, projID, agentID := newCommandRequestTestStore(t)
	approverID := uuid.New().String()
	execAgentID := uuid.New().String()
	execProjID := uuid.New().String()
	now := time.Now().UTC().Truncate(time.Microsecond)

	created, err := cs.CreateCommandRequestPrompt(ctx, makeCommandRequestPromptInput(t, projID, agentID, approverID, "gcloud-list", "gcloud config list", false, now))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	reqRow := created.Request

	// Wrong hash -> conflict command_hash_mismatch, row stays pending.
	badHash, err := cs.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID:     reqRow.ID,
		CommandSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		UserID:        approverID,
		Surface:       msgaction.CommandRequestSurfaceCLI,
		ExecAgentID:   execAgentID,
		ExecProjectID: execProjID,
		LeaseUntil:    now.Add(6 * time.Minute),
		Now:           now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("approve bad hash err: %v", err)
	}
	if badHash.ConflictCode != msgaction.ConflictCmdHashMismatch || badHash.Run != nil {
		t.Fatalf("badHash = %+v", badHash)
	}

	// Expired -> conflict expired, row stays pending.
	expRes, err := cs.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID:     reqRow.ID,
		CommandSHA256: reqRow.CommandSHA256,
		UserID:        approverID,
		Surface:       msgaction.CommandRequestSurfaceCLI,
		ExecAgentID:   execAgentID,
		ExecProjectID: execProjID,
		LeaseUntil:    now.Add(25 * time.Hour),
		Now:           now.Add(25 * time.Hour),
	})
	if err != nil {
		t.Fatalf("approve expired err: %v", err)
	}
	if expRes.ConflictCode != msgaction.ConflictExpired || expRes.Run != nil {
		t.Fatalf("expRes = %+v", expRes)
	}

	// Valid approve -> approved + queued run.
	appRes, err := cs.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID:      reqRow.ID,
		CommandSHA256:  reqRow.CommandSHA256,
		UserID:         approverID,
		UserName:       "Alice",
		UserEmail:      "alice@example.com",
		Surface:        msgaction.CommandRequestSurfaceCLI,
		IdempotencyKey: "idem-1",
		ExecAgentID:    execAgentID,
		ExecProjectID:  execProjID,
		ShareOutput:    false,
		LeaseUntil:     now.Add(6 * time.Minute),
		Now:            now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("approve valid err: %v", err)
	}
	if appRes.Conflict != nil || appRes.Replayed || appRes.Request.Status != store.CommandRequestStatusApproved || appRes.Run == nil {
		t.Fatalf("appRes = %+v", appRes)
	}
	runID := appRes.Run.ID
	if appRes.Run.Status != store.CommandRunStatusQueued || appRes.Run.RequestID != reqRow.ID {
		t.Fatalf("run = %+v", appRes.Run)
	}

	// Idempotent replay with same user + idempotency key returns Replayed=true.
	replayRes, err := cs.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID:      reqRow.ID,
		CommandSHA256:  reqRow.CommandSHA256,
		UserID:         approverID,
		Surface:        msgaction.CommandRequestSurfaceCLI,
		IdempotencyKey: "idem-1",
		ExecAgentID:    execAgentID,
		ExecProjectID:  execProjID,
		LeaseUntil:     now.Add(6 * time.Minute),
		Now:            now.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatalf("replay err: %v", err)
	}
	if !replayRes.Replayed || replayRes.Run == nil || replayRes.Run.ID != runID {
		t.Fatalf("replayRes = %+v", replayRes)
	}

	// Second approve with different idempotency key -> already_resolved conflict.
	dupRes, err := cs.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID:      reqRow.ID,
		CommandSHA256:  reqRow.CommandSHA256,
		UserID:         approverID,
		Surface:        msgaction.CommandRequestSurfaceCLI,
		IdempotencyKey: "idem-2",
		ExecAgentID:    execAgentID,
		ExecProjectID:  execProjID,
		LeaseUntil:     now.Add(6 * time.Minute),
		Now:            now.Add(4 * time.Second),
	})
	if err != nil {
		t.Fatalf("dup approve err: %v", err)
	}
	if dupRes.ConflictCode != msgaction.ConflictAlreadyResolved || dupRes.Run != nil {
		t.Fatalf("dupRes = %+v", dupRes)
	}

	// Run transitions: queued -> waking -> dispatching -> running -> succeeded + action_run output.
	ok, err := cs.TransitionCommandRunToWaking(ctx, runID, store.CommandRunWakeResume)
	if err != nil || !ok {
		t.Fatalf("TransitionCommandRunToWaking = (%v, %v)", ok, err)
	}
	// Duplicate waking transition fails (status is waking, not queued).
	ok, err = cs.TransitionCommandRunToWaking(ctx, runID, store.CommandRunWakeResume)
	if err != nil || ok {
		t.Fatalf("second waking should fail: (%v, %v)", ok, err)
	}

	ok, err = cs.TransitionCommandRunToDispatching(ctx, &store.CommandRunDispatchInput{
		RunID:        runID,
		LaunchID:     "launch-1",
		NotAfter:     now.Add(1 * time.Minute),
		LeaseUntil:   now.Add(12 * time.Minute),
		DispatchedAt: now.Add(5 * time.Second),
	})
	if err != nil || !ok {
		t.Fatalf("TransitionCommandRunToDispatching = (%v, %v)", ok, err)
	}

	// Sweeper expired query sees run once now > lease_until.
	expiredRuns, err := cs.ListExpiredCommandRuns(ctx, now.Add(13*time.Minute), 10)
	if err != nil || len(expiredRuns) != 1 || expiredRuns[0].ID != runID {
		t.Fatalf("ListExpiredCommandRuns = (%+v, %v)", expiredRuns, err)
	}

	ok, err = cs.TransitionCommandRunToRunning(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("TransitionCommandRunToRunning = (%v, %v)", ok, err)
	}

	// Oversize output rejected before CAS.
	exitZero := 0
	durMS := int64(420)
	outBytes := int64(12)
	wonOver, err := cs.FinishCommandRun(ctx, &store.FinishCommandRunInput{
		RunID:        runID,
		Status:       store.CommandRunStatusSucceeded,
		ExitCode:     &exitZero,
		DurationMS:   &durMS,
		OutputBytes:  &outBytes,
		OutputStatus: "captured",
		Output:       bytes.Repeat([]byte("x"), store.MaxExecRunOutputBytes+1),
		FinishedAt:   now.Add(6 * time.Second),
	})
	if !errors.Is(err, store.ErrInvalidInput) || wonOver {
		t.Fatalf("oversize finish = (%v, %v), want (false, ErrInvalidInput)", wonOver, err)
	}

	// First finish wins and inserts exec_run_outputs with owner_kind=action_run.
	won1, err := cs.FinishCommandRun(ctx, &store.FinishCommandRunInput{
		RunID:        runID,
		Status:       store.CommandRunStatusSucceeded,
		ExitCode:     &exitZero,
		DurationMS:   &durMS,
		OutputBytes:  &outBytes,
		OutputStatus: "captured",
		Output:       []byte("hello world\n"),
		FinishedAt:   now.Add(6 * time.Second),
	})
	if err != nil || !won1 {
		t.Fatalf("FinishCommandRun 1 = (%v, %v)", won1, err)
	}

	// Second finish loses cleanly and does not overwrite.
	won2, err := cs.FinishCommandRun(ctx, &store.FinishCommandRunInput{
		RunID:        runID,
		Status:       store.CommandRunStatusUnknown,
		Reason:       "lease_expired",
		OutputStatus: "lost",
		Output:       []byte("loser"),
		FinishedAt:   now.Add(7 * time.Second),
	})
	if err != nil || won2 {
		t.Fatalf("FinishCommandRun 2 = (%v, %v)", won2, err)
	}

	// Verify exec_run_outputs row has owner_kind=action_run and content matches winner.
	runUID, _ := uuid.Parse(runID)
	outRow, err := cs.client.ExecRunOutput.Query().
		Where(
			execrunoutput.IDEQ(runUID),
			execrunoutput.OwnerKindEQ(execrunoutput.OwnerKindActionRun),
		).
		Only(ctx)
	if err != nil {
		t.Fatalf("query exec_run_outputs: %v", err)
	}
	if string(outRow.Content) != "hello world\n" || outRow.ProjectID.String() != execProjID {
		t.Errorf("outRow = %+v", outRow)
	}

	// Outcome delivery claim is once-only unless cleared.
	claimAt := now.Add(8 * time.Second)
	claimed, err := cs.ClaimCommandRunOutcomeDelivery(ctx, runID, claimAt)
	if err != nil || !claimed {
		t.Fatalf("ClaimCommandRunOutcomeDelivery 1 = (%v, %v)", claimed, err)
	}
	claimed2, err := cs.ClaimCommandRunOutcomeDelivery(ctx, runID, claimAt.Add(time.Second))
	if err != nil || claimed2 {
		t.Fatalf("ClaimCommandRunOutcomeDelivery 2 = (%v, %v)", claimed2, err)
	}

	// ListAgentCommandRequests returns item + run.
	listRes, err := cs.ListAgentCommandRequests(ctx, &store.ListAgentCommandRequestsOptions{
		AgentID: agentID,
		Key:     "GCLOUD-LIST",
	})
	if err != nil {
		t.Fatalf("ListAgentCommandRequests: %v", err)
	}
	if len(listRes.Items) != 1 || listRes.Runs[reqRow.ID] == nil || listRes.Runs[reqRow.ID].Status != store.CommandRunStatusSucceeded {
		t.Fatalf("listRes = %+v", listRes)
	}
}

// TestCommandRequestStore_R1_DispatchLeaseAndSweeperInterleaving verifies both
// halves of R1:
//  1. A stale sweeper finish carrying ExpectedStatuses={queued,waking} loses
//     once TransitionCommandRunToDispatching has committed.
//  2. TransitionCommandRunToDispatching refuses a run whose lease_until <= now.
func TestCommandRequestStore_R1_DispatchLeaseAndSweeperInterleaving(t *testing.T) {
	ctx := context.Background()
	cs, projID, agentID := newCommandRequestTestStore(t)
	approverID := uuid.New().String()
	execAgentID := uuid.New().String()
	execProjID := uuid.New().String()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Case 1: sweeper lists queued run, executor commits dispatching before
	// sweeper finishes -> sweeper finish with ExpectedStatuses={queued,waking}
	// must lose and leave the run dispatching.
	c1, err := cs.CreateCommandRequestPrompt(ctx, makeCommandRequestPromptInput(t, projID, agentID, approverID, "r1-interleave", "echo hi", false, now))
	if err != nil {
		t.Fatalf("create 1: %v", err)
	}
	app1, err := cs.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID:      c1.Request.ID,
		CommandSHA256:  c1.Request.CommandSHA256,
		UserID:         approverID,
		Surface:        msgaction.CommandRequestSurfaceCLI,
		IdempotencyKey: "r1-1",
		ExecAgentID:    execAgentID,
		ExecProjectID:  execProjID,
		LeaseUntil:     now.Add(6 * time.Minute),
		Now:            now,
	})
	if err != nil || app1.Run == nil {
		t.Fatalf("approve 1 = (%+v, %v)", app1, err)
	}
	wonDispatch, err := cs.TransitionCommandRunToDispatching(ctx, &store.CommandRunDispatchInput{
		RunID:        app1.Run.ID,
		LaunchID:     "launch-1",
		NotAfter:     now.Add(1 * time.Minute),
		LeaseUntil:   now.Add(12 * time.Minute),
		DispatchedAt: now.Add(time.Second),
	})
	if err != nil || !wonDispatch {
		t.Fatalf("TransitionCommandRunToDispatching = (%v, %v), want (true, nil)", wonDispatch, err)
	}
	wonSweep, err := cs.FinishCommandRun(ctx, &store.FinishCommandRunInput{
		RunID:            app1.Run.ID,
		Status:           store.CommandRunStatusNotStarted,
		ExpectedStatuses: []string{store.CommandRunStatusQueued, store.CommandRunStatusWaking},
		Reason:           "lease_expired",
		FinishedAt:       now.Add(2 * time.Second),
	})
	if err != nil || wonSweep {
		t.Fatalf("stale sweeper FinishCommandRun = (%v, %v), want (false, nil)", wonSweep, err)
	}
	got1, err := cs.GetCommandRun(ctx, app1.Run.ID)
	if err != nil {
		t.Fatalf("GetCommandRun 1: %v", err)
	}
	if got1.Status != store.CommandRunStatusDispatching || got1.FinishedAt != nil {
		t.Fatalf("run 1 = (status=%q finished=%v), want (dispatching, nil)", got1.Status, got1.FinishedAt)
	}

	// Case 2: TransitionCommandRunToDispatching refused when lease_until <= dispatchedAt.
	c2, err := cs.CreateCommandRequestPrompt(ctx, makeCommandRequestPromptInput(t, projID, agentID, approverID, "r1-expired-lease", "echo late", false, now))
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	app2, err := cs.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID:      c2.Request.ID,
		CommandSHA256:  c2.Request.CommandSHA256,
		UserID:         approverID,
		Surface:        msgaction.CommandRequestSurfaceCLI,
		IdempotencyKey: "r1-2",
		ExecAgentID:    execAgentID,
		ExecProjectID:  execProjID,
		LeaseUntil:     now.Add(5 * time.Second),
		Now:            now,
	})
	if err != nil || app2.Run == nil {
		t.Fatalf("approve 2 = (%+v, %v)", app2, err)
	}
	wonLate, err := cs.TransitionCommandRunToDispatching(ctx, &store.CommandRunDispatchInput{
		RunID:        app2.Run.ID,
		LaunchID:     "launch-1",
		NotAfter:     now.Add(2 * time.Minute),
		LeaseUntil:   now.Add(12 * time.Minute),
		DispatchedAt: now.Add(5 * time.Second), // == lease_until -> must be refused
	})
	if err != nil || wonLate {
		t.Fatalf("expired-lease TransitionCommandRunToDispatching = (%v, %v), want (false, nil)", wonLate, err)
	}
	got2, err := cs.GetCommandRun(ctx, app2.Run.ID)
	if err != nil {
		t.Fatalf("GetCommandRun 2: %v", err)
	}
	if got2.Status != store.CommandRunStatusQueued || got2.LaunchID != "" || got2.DispatchedAt != nil {
		t.Fatalf("run 2 mutated after refused dispatch: %+v", got2)
	}
}

func TestCommandRunStore_ClaimCommandRunsForPoll(t *testing.T) {
	cs, projID, agentID := newCommandRequestTestStore(t)
	ctx := context.Background()
	execAgentID := uuid.New().String()
	execProjID := uuid.New().String()
	approverID := "user-poll-claim"
	now := time.Now().UTC().Truncate(time.Microsecond)

	mkRun := func(key string, lease time.Time, status string) *store.CommandRun {
		t.Helper()
		c, err := cs.CreateCommandRequestPrompt(ctx, makeCommandRequestPromptInput(t, projID, agentID, approverID, key, "echo "+key, false, now))
		if err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
		app, err := cs.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
			RequestID:      c.Request.ID,
			CommandSHA256:  c.Request.CommandSHA256,
			UserID:         approverID,
			Surface:        msgaction.CommandRequestSurfaceCLI,
			IdempotencyKey: "idem-" + key,
			ExecAgentID:    execAgentID,
			ExecProjectID:  execProjID,
			LeaseUntil:     lease,
			Now:            now,
		})
		if err != nil || app.Run == nil {
			t.Fatalf("approve %s: (%+v, %v)", key, app, err)
		}
		switch status {
		case store.CommandRunStatusQueued:
		case store.CommandRunStatusWaking:
			won, err := cs.TransitionCommandRunToWaking(ctx, app.Run.ID, store.CommandRunWakeResume)
			if err != nil || !won {
				t.Fatalf("waking %s: (%v, %v)", key, won, err)
			}
		case store.CommandRunStatusDispatching, store.CommandRunStatusRunning, store.CommandRunStatusSucceeded:
			won, err := cs.TransitionCommandRunToDispatching(ctx, &store.CommandRunDispatchInput{
				RunID:        app.Run.ID,
				LaunchID:     "launch-1",
				NotAfter:     now.Add(1 * time.Minute),
				LeaseUntil:   lease,
				DispatchedAt: now,
			})
			if err != nil || !won {
				t.Fatalf("dispatching %s: (%v, %v)", key, won, err)
			}
			if status == store.CommandRunStatusRunning || status == store.CommandRunStatusSucceeded {
				won, err := cs.TransitionCommandRunToRunning(ctx, app.Run.ID)
				if err != nil || !won {
					t.Fatalf("running %s: (%v, %v)", key, won, err)
				}
			}
			if status == store.CommandRunStatusSucceeded {
				won, err := cs.FinishCommandRun(ctx, &store.FinishCommandRunInput{
					RunID:            app.Run.ID,
					Status:           store.CommandRunStatusSucceeded,
					ExpectedStatuses: []string{store.CommandRunStatusRunning},
					FinishedAt:       now.Add(time.Second),
				})
				if err != nil || !won {
					t.Fatalf("finish %s: (%v, %v)", key, won, err)
				}
			}
		}
		return app.Run
	}

	// Eligible live runs: dispatching + running.
	rDisp := mkRun("live-disp", now.Add(10*time.Minute), store.CommandRunStatusDispatching)
	rRun := mkRun("live-run", now.Add(10*time.Minute), store.CommandRunStatusRunning)
	rOwned := mkRun("live-owned", now.Add(10*time.Minute), store.CommandRunStatusRunning)
	// Ineligible runs: queued, waking, expired, finished.
	_ = mkRun("queued", now.Add(10*time.Minute), store.CommandRunStatusQueued)
	_ = mkRun("waking", now.Add(10*time.Minute), store.CommandRunStatusWaking)
	_ = mkRun("expired", now.Add(5*time.Second), store.CommandRunStatusRunning)
	_ = mkRun("finished", now.Add(10*time.Minute), store.CommandRunStatusSucceeded)

	claimAt := now.Add(10 * time.Second)
	claimTTL := 10 * time.Second

	// Replica 1 claims excluding rOwned with limit 10 -> gets rDisp and rRun only.
	claimed1, err := cs.ClaimCommandRunsForPoll(ctx, claimAt, claimTTL, 10, []string{rOwned.ID})
	if err != nil {
		t.Fatalf("ClaimCommandRunsForPoll 1: %v", err)
	}
	if len(claimed1) != 2 {
		t.Fatalf("claimed1 len = %d, want 2 (%+v)", len(claimed1), claimed1)
	}
	gotIDs := map[string]bool{claimed1[0].ID: true, claimed1[1].ID: true}
	if !gotIDs[rDisp.ID] || !gotIDs[rRun.ID] {
		t.Fatalf("claimed1 IDs = %v, want %s and %s", gotIDs, rDisp.ID, rRun.ID)
	}
	for _, r := range claimed1 {
		if r.PollClaimedAt == nil || !r.PollClaimedAt.Equal(claimAt) {
			t.Fatalf("run %s poll_claimed_at = %v, want %v", r.ID, r.PollClaimedAt, claimAt)
		}
	}

	// Replica 2 calling within claimTTL gets nothing for those two runs (still excluding rOwned).
	claimed2, err := cs.ClaimCommandRunsForPoll(ctx, claimAt.Add(5*time.Second), claimTTL, 10, []string{rOwned.ID})
	if err != nil {
		t.Fatalf("ClaimCommandRunsForPoll 2: %v", err)
	}
	if len(claimed2) != 0 {
		t.Fatalf("claimed2 within TTL = %d, want 0", len(claimed2))
	}

	// Replica 2 calling after claimTTL elapses claims them again (respecting limit=1).
	later := claimAt.Add(claimTTL + time.Second)
	claimed3, err := cs.ClaimCommandRunsForPoll(ctx, later, claimTTL, 1, []string{rOwned.ID})
	if err != nil {
		t.Fatalf("ClaimCommandRunsForPoll 3: %v", err)
	}
	if len(claimed3) != 1 {
		t.Fatalf("claimed3 len = %d, want 1", len(claimed3))
	}
	// Second page at same timestamp gets the other one.
	claimed4, err := cs.ClaimCommandRunsForPoll(ctx, later, claimTTL, 1, []string{rOwned.ID})
	if err != nil {
		t.Fatalf("ClaimCommandRunsForPoll 4: %v", err)
	}
	if len(claimed4) != 1 || claimed4[0].ID == claimed3[0].ID {
		t.Fatalf("claimed4 = %+v, want the other run from %s", claimed4, claimed3[0].ID)
	}
}

func TestCommandRequestStore_GetAndPurgeActionRunOutputs(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projID := uuid.NewString()
	agentID := uuid.NewString()
	execAgentID := uuid.NewString()
	execProjID := uuid.NewString()
	approverID := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)

	createFinishedRun := func(key string, finishedAt time.Time, output []byte) *store.CommandRun {
		t.Helper()
		c, err := cs.CreateCommandRequestPrompt(ctx, makeCommandRequestPromptInput(t, projID, agentID, approverID, key, "echo "+key, false, finishedAt.Add(-time.Minute)))
		if err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
		app, err := cs.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
			RequestID:     c.Request.ID,
			CommandSHA256: c.Request.CommandSHA256,
			UserID:        approverID,
			Surface:       msgaction.CommandRequestSurfaceCLI,
			ExecAgentID:   execAgentID,
			ExecProjectID: execProjID,
			LeaseUntil:    finishedAt.Add(5 * time.Minute),
			Now:           finishedAt.Add(-30 * time.Second),
		})
		if err != nil || app.Run == nil {
			t.Fatalf("approve %s: (%+v, %v)", key, app, err)
		}
		code := 0
		dur := int64(10)
		outLen := int64(len(output))
		won, err := cs.FinishCommandRun(ctx, &store.FinishCommandRunInput{
			RunID:        app.Run.ID,
			Status:       store.CommandRunStatusSucceeded,
			ExitCode:     &code,
			DurationMS:   &dur,
			OutputBytes:  &outLen,
			OutputStatus: "captured",
			Output:       output,
			FinishedAt:   finishedAt,
		})
		if err != nil || !won {
			t.Fatalf("finish %s: (%v, %v)", key, won, err)
		}
		return app.Run
	}

	oldRun := createFinishedRun("old-out", now.Add(-8*24*time.Hour), []byte("old-action-output"))
	recentRun := createFinishedRun("recent-out", now.Add(-2*24*time.Hour), []byte("recent-action-output"))

	// Seed a scheduled_exec output row older than 7 days to verify strict owner_kind isolation.
	schedRunID := uuid.New()
	if _, err := cs.client.ExecRunOutput.Create().
		SetID(schedRunID).
		SetOwnerKind(execrunoutput.OwnerKindScheduledExec).
		SetProjectID(uuid.MustParse(execProjID)).
		SetCaptureMode("tail").
		SetContent([]byte("old-scheduled-output")).
		SetCreatedAt(now.Add(-10 * 24 * time.Hour)).
		Save(ctx); err != nil {
		t.Fatalf("seed scheduled_exec output: %v", err)
	}

	// GetActionRunOutput reads action_run rows and refuses scheduled_exec rows.
	gotOld, err := cs.GetActionRunOutput(ctx, oldRun.ID)
	if err != nil || string(gotOld) != "old-action-output" {
		t.Fatalf("GetActionRunOutput(old) = (%q, %v), want old-action-output", string(gotOld), err)
	}
	if has, err := cs.HasActionRunOutput(ctx, oldRun.ID); err != nil || !has {
		t.Fatalf("HasActionRunOutput(old) = (%v, %v), want (true, nil)", has, err)
	}
	if has, err := cs.HasActionRunOutput(ctx, schedRunID.String()); err != nil || has {
		t.Fatalf("HasActionRunOutput(scheduled_exec) = (%v, %v), want (false, nil)", has, err)
	}
	if _, err := cs.GetActionRunOutput(ctx, schedRunID.String()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetActionRunOutput(scheduled_exec) err = %v, want ErrNotFound", err)
	}
	if _, err := cs.GetActionRunOutput(ctx, uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetActionRunOutput(missing) err = %v, want ErrNotFound", err)
	}

	// PurgeOldActionRunOutputs deletes only action_run rows older than 7 days.
	purged, err := cs.PurgeOldActionRunOutputs(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("PurgeOldActionRunOutputs: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	if has, err := cs.HasActionRunOutput(ctx, oldRun.ID); err != nil || has {
		t.Fatalf("HasActionRunOutput(old after purge) = (%v, %v), want (false, nil)", has, err)
	}
	if _, err := cs.GetActionRunOutput(ctx, oldRun.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old action_run output should be purged, got err=%v", err)
	}
	gotRecent, err := cs.GetActionRunOutput(ctx, recentRun.ID)
	if err != nil || string(gotRecent) != "recent-action-output" {
		t.Fatalf("recent action_run output should remain, got (%q, %v)", string(gotRecent), err)
	}
	// Scheduled output must be untouched by PurgeOldActionRunOutputs.
	schedRow, err := cs.client.ExecRunOutput.Get(ctx, schedRunID)
	if err != nil || string(schedRow.Content) != "old-scheduled-output" {
		t.Fatalf("scheduled_exec output must remain untouched, got (%+v, %v)", schedRow, err)
	}
}

func TestCommandRequestStore_CreateAndApproveMarkdownCommandRequest(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectID := uuid.New().String()
	agentID := uuid.New().String()
	msgID := uuid.New().String()
	execAgentID := uuid.New().String()
	execProjectID := uuid.New().String()
	script := "gcloud config list\n"
	hash := messages.CommandRequestCanonicalHash(script, "", messages.DefaultCommandRequestTimeoutS)
	now := time.Now().UTC().Truncate(time.Microsecond)

	in := &store.CreateMarkdownCommandRequestInput{
		ProjectID:      projectID,
		AgentID:        agentID,
		AgentName:      "dev-agent",
		MessageID:      msgID,
		ChatKey:        "dm:agent:" + agentID + ":user:user-alice",
		Script:         script,
		CommandSHA256:  hash,
		UserID:         "user-alice",
		UserName:       "Alice",
		UserEmail:      "alice@example.com",
		IdempotencyKey: "idem-md-1",
		ExecAgentID:    execAgentID,
		ExecProjectID:  execProjectID,
		ShareOutput:    false,
		LeaseUntil:     now.Add(6 * time.Minute),
		Now:            now,
	}

	// First call creates v1 approved row + queued run in one tx.
	res1, err := cs.CreateAndApproveMarkdownCommandRequest(ctx, in)
	if err != nil {
		t.Fatalf("CreateAndApproveMarkdownCommandRequest 1: %v", err)
	}
	if res1.Replayed || res1.Conflict != nil {
		t.Fatalf("expected fresh create, got replayed=%v conflict=%v", res1.Replayed, res1.Conflict)
	}
	if res1.Request == nil || res1.Run == nil {
		t.Fatalf("expected request and run, got %+v", res1)
	}
	wantKey := messages.MarkdownCommandRequestKey(msgID, hash)
	if res1.Request.Key != wantKey || res1.Request.Version != 1 {
		t.Fatalf("key/version = %q/v%d, want %q/v1", res1.Request.Key, res1.Request.Version, wantKey)
	}
	if res1.Request.Status != store.CommandRequestStatusApproved {
		t.Fatalf("status = %q, want approved", res1.Request.Status)
	}
	if res1.Request.Surface != msgaction.CommandRequestSurfaceMarkdownBlock {
		t.Fatalf("surface = %q, want markdown_block", res1.Request.Surface)
	}
	if res1.Request.ShareOutputRequested {
		t.Fatalf("share_output_requested = true, want false")
	}
	if res1.Run.RequestID != res1.Request.ID || res1.Run.Status != store.CommandRunStatusQueued {
		t.Fatalf("run = %+v, want queued run for %s", res1.Run, res1.Request.ID)
	}

	// Same user + same idempotency key replays cleanly.
	replay, err := cs.CreateAndApproveMarkdownCommandRequest(ctx, in)
	if err != nil {
		t.Fatalf("CreateAndApproveMarkdownCommandRequest replay: %v", err)
	}
	if !replay.Replayed || replay.Request.ID != res1.Request.ID || replay.Run.ID != res1.Run.ID {
		t.Fatalf("expected idempotent replay of %s/%s, got %+v", res1.Request.ID, res1.Run.ID, replay)
	}

	// Second click (different idempotency key) -> already_resolved conflict with existing run; never v2.
	in2 := *in
	in2.IdempotencyKey = "idem-md-2"
	dup, err := cs.CreateAndApproveMarkdownCommandRequest(ctx, &in2)
	if err != nil {
		t.Fatalf("CreateAndApproveMarkdownCommandRequest dup: %v", err)
	}
	if dup.Conflict == nil || dup.ConflictCode != msgaction.ConflictAlreadyResolved {
		t.Fatalf("expected already_resolved conflict, got %+v", dup)
	}
	if dup.Conflict.ID != res1.Request.ID {
		t.Fatalf("conflict row = %s, want %s", dup.Conflict.ID, res1.Request.ID)
	}
	if dup.ConflictRun == nil || dup.ConflictRun.ID != res1.Run.ID {
		t.Fatalf("conflict run = %+v, want %s", dup.ConflictRun, res1.Run.ID)
	}

	// ListAgentCommandRequests must still have only 1 row (v1, never v2).
	listed, err := cs.ListAgentCommandRequests(ctx, &store.ListAgentCommandRequestsOptions{
		AgentID: agentID,
		Key:     wantKey,
	})
	if err != nil {
		t.Fatalf("ListAgentCommandRequests: %v", err)
	}
	if len(listed.Items) != 1 {
		t.Fatalf("listed count = %d, want 1", len(listed.Items))
	}
}
