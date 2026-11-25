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
