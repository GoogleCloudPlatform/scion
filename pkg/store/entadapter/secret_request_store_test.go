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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func newSecretRequestTestStore(t *testing.T) (*CompositeStore, string, string) {
	t.Helper()
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projID := uuid.New().String()
	if err := cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Slug: "sr-proj-" + projID[:8],
		Name: "SR Project",
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	agentID := uuid.New().String()
	if err := cs.CreateAgent(ctx, &store.Agent{
		ID:        agentID,
		Slug:      "sr-agent",
		Name:      "sr-agent",
		ProjectID: projID,
		Phase:     "running",
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return cs, projID, agentID
}

func makeSecretRequestPromptInput(projID, agentID string, update bool, now time.Time, reqs ...messages.SecretRequest) *store.SecretRequestPromptInput {
	msgID := uuid.New().String()
	return &store.SecretRequestPromptInput{
		Message: &store.Message{
			ID:        msgID,
			ProjectID: projID,
			Sender:    "agent:sr-agent",
			SenderID:  agentID,
			Recipient: "user:alice@example.com",
			ThreadID:  "dm:agent:" + agentID + ":user:" + uuid.New().String(),
		},
		Intro:         "Need credentials",
		ProjectName:   "SR Project",
		StandaloneURL: "https://hub.example/secret-requests/" + msgID,
		AgentID:       agentID,
		AgentName:     "sr-agent",
		Requests:      reqs,
		Update:        update,
		Now:           now,
	}
}

func TestSecretRequestStore_CreateVersioningAndUpdate(t *testing.T) {
	ctx := context.Background()
	cs, projID, agentID := newSecretRequestTestStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	// v1 created cleanly
	res1, err := cs.CreateSecretRequestPrompt(ctx, makeSecretRequestPromptInput(projID, agentID, false, now,
		messages.SecretRequest{Key: "GITHUB_TOKEN", KeyNorm: "GITHUB_TOKEN", Description: "PAT", SecretType: "environment"},
		messages.SecretRequest{Key: "DEPLOY_KEY", KeyNorm: "DEPLOY_KEY", Description: "SSH", SecretType: "file", Target: "~/.ssh/id_ed25519"},
	))
	if err != nil {
		t.Fatalf("CreateSecretRequestPrompt v1: %v", err)
	}
	if len(res1.Requests) != 2 {
		t.Fatalf("want 2 requests, got %d", len(res1.Requests))
	}
	if res1.Requests[0].Row.Version != 1 || res1.Requests[1].Row.Version != 1 {
		t.Fatalf("want v1 for both, got %d and %d", res1.Requests[0].Row.Version, res1.Requests[1].Row.Version)
	}
	if res1.Message.Type != messages.TypeSecretRequest {
		t.Fatalf("message type = %q, want %q", res1.Message.Type, messages.TypeSecretRequest)
	}

	// Re-sending pending key without --update fails with SecretRequestKeyPendingError
	_, err = cs.CreateSecretRequestPrompt(ctx, makeSecretRequestPromptInput(projID, agentID, false, now.Add(time.Second),
		messages.SecretRequest{Key: "github_token", KeyNorm: "GITHUB_TOKEN", Description: "PAT2", SecretType: "environment"},
	))
	var pendingErr *store.SecretRequestKeyPendingError
	if !errors.As(err, &pendingErr) {
		t.Fatalf("want SecretRequestKeyPendingError, got %v", err)
	}
	if len(pendingErr.Existing) != 1 || pendingErr.Existing[0].Version != 1 {
		t.Fatalf("unexpected pendingErr.Existing: %+v", pendingErr.Existing)
	}

	// Re-sending with --update replaces ONLY the named key (GITHUB_TOKEN -> v2), leaving DEPLOY_KEY pending
	res2, err := cs.CreateSecretRequestPrompt(ctx, makeSecretRequestPromptInput(projID, agentID, true, now.Add(2*time.Second),
		messages.SecretRequest{Key: "GITHUB_TOKEN", KeyNorm: "GITHUB_TOKEN", Description: "PAT v2", SecretType: "environment"},
	))
	if err != nil {
		t.Fatalf("CreateSecretRequestPrompt v2 (--update): %v", err)
	}
	if res2.Requests[0].Row.Version != 2 {
		t.Fatalf("want v2, got %d", res2.Requests[0].Row.Version)
	}
	if res2.Requests[0].Replaced == nil || res2.Requests[0].Replaced.Status != store.SecretRequestStatusReplaced {
		t.Fatalf("expected replaced v1, got %+v", res2.Requests[0].Replaced)
	}

	v1Row, err := cs.GetSecretRequest(ctx, res1.Requests[0].Row.ID)
	if err != nil {
		t.Fatalf("GetSecretRequest v1: %v", err)
	}
	if v1Row.Status != store.SecretRequestStatusReplaced || v1Row.ReplacedByID != res2.Requests[0].Row.ID {
		t.Fatalf("v1 not marked replaced: status=%s replacedBy=%s", v1Row.Status, v1Row.ReplacedByID)
	}
	deployRow, err := cs.GetSecretRequest(ctx, res1.Requests[1].Row.ID)
	if err != nil {
		t.Fatalf("GetSecretRequest deploy: %v", err)
	}
	if deployRow.Status != store.SecretRequestStatusPending {
		t.Fatalf("DEPLOY_KEY should remain pending, got %s", deployRow.Status)
	}
}

func TestSecretRequestStore_FulfillClaimFinishRollbackAndTerminalRepost(t *testing.T) {
	ctx := context.Background()
	cs, projID, agentID := newSecretRequestTestStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	res, err := cs.CreateSecretRequestPrompt(ctx, makeSecretRequestPromptInput(projID, agentID, false, now,
		messages.SecretRequest{Key: "API_KEY", KeyNorm: "API_KEY", Description: "key", SecretType: "environment"},
	))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	row := res.Requests[0].Row

	// Claim succeeds
	claim, err := cs.ClaimSecretRequestsForFulfill(ctx, &store.ClaimSecretRequestsInput{
		MessageID:      row.MessageID,
		RowIDs:         []string{row.ID},
		UserID:         "user-1",
		UserName:       "Alice",
		UserEmail:      "alice@example.com",
		Surface:        store.SecretRequestSurfaceStandalonePage,
		IdempotencyKey: "idem-1",
		Now:            now.Add(time.Second),
	})
	if err != nil || claim.Conflict != nil || len(claim.Claimed) != 1 {
		t.Fatalf("claim failed: err=%v claim=%+v", err, claim)
	}

	// Second concurrent claim sees submitting conflict
	claim2, err := cs.ClaimSecretRequestsForFulfill(ctx, &store.ClaimSecretRequestsInput{
		MessageID: row.MessageID,
		RowIDs:    []string{row.ID},
		UserID:    "user-2",
		UserName:  "Bob",
		Now:       now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("claim2 err: %v", err)
	}
	if claim2.Conflict == nil || claim2.Conflict.Status != store.SecretRequestStatusFulfilling {
		t.Fatalf("want fulfilling conflict, got %+v", claim2)
	}

	// --update during fulfilling fails with SecretRequestSubmittingError
	_, err = cs.CreateSecretRequestPrompt(ctx, makeSecretRequestPromptInput(projID, agentID, true, now.Add(2*time.Second),
		messages.SecretRequest{Key: "API_KEY", KeyNorm: "API_KEY", Description: "key2", SecretType: "environment"},
	))
	var subErr *store.SecretRequestSubmittingError
	if !errors.As(err, &subErr) {
		t.Fatalf("want SecretRequestSubmittingError, got %v", err)
	}

	// Rollback returns to pending
	n, err := cs.RollbackSecretRequestClaim(ctx, &store.RollbackSecretRequestClaimInput{
		MessageID: row.MessageID,
		RowIDs:    []string{row.ID},
		UserID:    "user-1",
		ClaimedAt: claim.ClaimedAt,
	})
	if err != nil || n != 1 {
		t.Fatalf("rollback: n=%d err=%v", n, err)
	}

	// Re-claim and finish
	claim3, err := cs.ClaimSecretRequestsForFulfill(ctx, &store.ClaimSecretRequestsInput{
		MessageID:      row.MessageID,
		RowIDs:         []string{row.ID},
		UserID:         "user-1",
		UserName:       "Alice",
		UserEmail:      "alice@example.com",
		Surface:        store.SecretRequestSurfaceStandalonePage,
		IdempotencyKey: "idem-1",
		Now:            now.Add(3 * time.Second),
	})
	if err != nil || claim3.Conflict != nil {
		t.Fatalf("claim3: err=%v claim=%+v", err, claim3)
	}
	fin, err := cs.FinishSecretRequestFulfill(ctx, &store.FinishSecretRequestFulfillInput{
		MessageID:      row.MessageID,
		RowIDs:         []string{row.ID},
		UserID:         "user-1",
		ClaimedAt:      claim3.ClaimedAt,
		Surface:        store.SecretRequestSurfaceStandalonePage,
		IdempotencyKey: "idem-1",
		SecretVersions: map[string]int{row.ID: 4},
		Now:            now.Add(4 * time.Second),
	})
	if err != nil || fin.Conflict != nil || len(fin.Rows) != 1 || fin.Rows[0].Status != store.SecretRequestStatusFulfilled || fin.Rows[0].SecretVersion != 4 {
		t.Fatalf("finish: err=%v fin=%+v", err, fin)
	}

	// Idempotent replay by same user returns Replayed=true
	replay, err := cs.ClaimSecretRequestsForFulfill(ctx, &store.ClaimSecretRequestsInput{
		MessageID:      row.MessageID,
		RowIDs:         []string{row.ID},
		UserID:         "user-1",
		IdempotencyKey: "idem-1",
		Now:            now.Add(5 * time.Second),
	})
	if err != nil || !replay.Replayed {
		t.Fatalf("want replayed=true, got err=%v replay=%+v", err, replay)
	}

	// Plain send after fulfill automatically creates v2 without --update (S1)
	res2, err := cs.CreateSecretRequestPrompt(ctx, makeSecretRequestPromptInput(projID, agentID, false, now.Add(6*time.Second),
		messages.SecretRequest{Key: "API_KEY", KeyNorm: "API_KEY", Description: "rotate", SecretType: "environment"},
	))
	if err != nil {
		t.Fatalf("re-ask after fulfill: %v", err)
	}
	if res2.Requests[0].Row.Version != 2 {
		t.Fatalf("want v2 after fulfill, got %d", res2.Requests[0].Row.Version)
	}
	// v1 keeps fulfilled status while linking replaced_by_id
	v1After, err := cs.GetSecretRequest(ctx, row.ID)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if v1After.Status != store.SecretRequestStatusFulfilled || v1After.ReplacedByID != res2.Requests[0].Row.ID {
		t.Fatalf("v1 should stay fulfilled with replaced_by_id set, got status=%s replacedBy=%s", v1After.Status, v1After.ReplacedByID)
	}
}

func TestSecretRequestStore_WithdrawDismissSweeperAndDeleteHooks(t *testing.T) {
	ctx := context.Background()
	cs, projID, agentID := newSecretRequestTestStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Create 2-key card
	if _, err := cs.CreateSecretRequestPrompt(ctx, makeSecretRequestPromptInput(projID, agentID, false, now,
		messages.SecretRequest{Key: "K1", KeyNorm: "K1", SecretType: "environment"},
		messages.SecretRequest{Key: "K2", KeyNorm: "K2", SecretType: "environment"},
	)); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Withdraw K1 with wholeSet=true withdraws both K1 and K2
	wd, err := cs.WithdrawSecretRequests(ctx, &store.WithdrawSecretRequestsInput{
		AgentID:  agentID,
		Keys:     []string{"K1"},
		WholeSet: true,
		Reason:   "not needed",
		Now:      now.Add(time.Second),
	})
	if err != nil || len(wd.Withdrawn) != 2 {
		t.Fatalf("withdraw wholeSet: err=%v wd=%+v", err, wd)
	}

	// Repeat withdraw reports alreadyWithdrawn
	wd2, err := cs.WithdrawSecretRequests(ctx, &store.WithdrawSecretRequestsInput{
		AgentID: agentID,
		Keys:    []string{"K1"},
		Now:     now.Add(2 * time.Second),
	})
	if err != nil || len(wd2.AlreadyWithdrawn) != 1 || len(wd2.Withdrawn) != 0 {
		t.Fatalf("repeat withdraw: err=%v wd2=%+v", err, wd2)
	}

	// Create v2 and dismiss subset
	res2, err := cs.CreateSecretRequestPrompt(ctx, makeSecretRequestPromptInput(projID, agentID, false, now.Add(3*time.Second),
		messages.SecretRequest{Key: "K1", KeyNorm: "K1", SecretType: "environment"},
		messages.SecretRequest{Key: "K2", KeyNorm: "K2", SecretType: "environment"},
	))
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	dsm, err := cs.DismissSecretRequests(ctx, &store.DismissSecretRequestsInput{
		MessageID: res2.Requests[0].Row.MessageID,
		RowIDs:    []string{res2.Requests[0].Row.ID},
		UserID:    "user-bob",
		UserName:  "Bob",
		Reason:    "use shared",
		Surface:   store.SecretRequestSurfaceNative,
		Now:       now.Add(4 * time.Second),
	})
	if err != nil || len(dsm.Dismissed) != 1 {
		t.Fatalf("dismiss subset: err=%v dsm=%+v", err, dsm)
	}

	// Sweeper recovers stale claim on K2, then expires it
	claim, err := cs.ClaimSecretRequestsForFulfill(ctx, &store.ClaimSecretRequestsInput{
		MessageID: res2.Requests[1].Row.MessageID,
		RowIDs:    []string{res2.Requests[1].Row.ID},
		UserID:    "user-alice",
		Now:       now.Add(5 * time.Second),
	})
	if err != nil || len(claim.Claimed) != 1 {
		t.Fatalf("claim K2: %v", err)
	}
	rec, err := cs.RecoverStaleSecretRequestClaims(ctx, now.Add(5*time.Second+msgaction.SecretRequestClaimTTL), 10)
	if err != nil || len(rec) != 1 || rec[0].Status != store.SecretRequestStatusPending {
		t.Fatalf("recover stale claim: err=%v rec=%+v", err, rec)
	}
	exp, err := cs.ExpireSecretRequests(ctx, now.Add(msgaction.SecretRequestTTL+time.Hour), 10)
	if err != nil || len(exp) != 1 || exp[0].Status != store.SecretRequestStatusExpired {
		t.Fatalf("expire: err=%v exp=%+v", err, exp)
	}

	// Create v3 and test hard-delete withdraw (rows retained) + project delete (rows removed)
	res3, err := cs.CreateSecretRequestPrompt(ctx, makeSecretRequestPromptInput(projID, agentID, false, now.Add(10*time.Second),
		messages.SecretRequest{Key: "K3", KeyNorm: "K3", SecretType: "environment"},
	))
	if err != nil {
		t.Fatalf("create K3: %v", err)
	}
	hd, err := cs.WithdrawAgentSecretRequestsOnDelete(ctx, agentID, now.Add(11*time.Second))
	if err != nil || len(hd) != 1 || hd[0].WithdrawnReason != "agent deleted" || hd[0].WithdrawnBy != store.SecretRequestWithdrawnByHub {
		t.Fatalf("hard-delete withdraw: err=%v hd=%+v", err, hd)
	}
	// Row still exists!
	if _, err := cs.GetSecretRequest(ctx, res3.Requests[0].Row.ID); err != nil {
		t.Fatalf("row should be retained after agent delete: %v", err)
	}
	// Project delete removes all rows for project
	deleted, err := cs.DeleteProjectSecretRequests(ctx, projID)
	if err != nil || deleted < 5 {
		t.Fatalf("DeleteProjectSecretRequests: deleted=%d err=%v", deleted, err)
	}
	if _, err := cs.GetSecretRequest(ctx, res3.Requests[0].Row.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound after project delete, got %v", err)
	}
}

// TestSecretRequestStore_FinishConflictRollsBackEarlierRows verifies N1: when a
// multi-key card's FinishSecretRequestFulfill loses the CAS on a later row,
// earlier rows updated in the same transaction are rolled back.
func TestSecretRequestStore_FinishConflictRollsBackEarlierRows(t *testing.T) {
	ctx := context.Background()
	cs, projID, agentID := newSecretRequestTestStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	res, err := cs.CreateSecretRequestPrompt(ctx, makeSecretRequestPromptInput(projID, agentID, false, now,
		messages.SecretRequest{Key: "M1", KeyNorm: "M1", SecretType: "environment"},
		messages.SecretRequest{Key: "M2", KeyNorm: "M2", SecretType: "environment"},
	))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r1, r2 := res.Requests[0].Row, res.Requests[1].Row
	claim, err := cs.ClaimSecretRequestsForFulfill(ctx, &store.ClaimSecretRequestsInput{
		MessageID: r1.MessageID,
		RowIDs:    []string{r1.ID, r2.ID},
		UserID:    "user-1",
		UserName:  "Alice",
		Now:       now.Add(time.Second),
	})
	if err != nil || claim.Conflict != nil || len(claim.Claimed) != 2 {
		t.Fatalf("claim: err=%v claim=%+v", err, claim)
	}

	// Simulate r2 losing its claim (rolled back to pending) while r1 stays fulfilling.
	if _, err := cs.RollbackSecretRequestClaim(ctx, &store.RollbackSecretRequestClaimInput{
		MessageID: r1.MessageID,
		RowIDs:    []string{r2.ID},
		UserID:    "user-1",
		ClaimedAt: claim.ClaimedAt,
	}); err != nil {
		t.Fatalf("rollback r2: %v", err)
	}

	fin, err := cs.FinishSecretRequestFulfill(ctx, &store.FinishSecretRequestFulfillInput{
		MessageID:      r1.MessageID,
		RowIDs:         []string{r1.ID, r2.ID},
		UserID:         "user-1",
		ClaimedAt:      claim.ClaimedAt,
		SecretVersions: map[string]int{r1.ID: 1, r2.ID: 1},
		Now:            now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("finish err: %v", err)
	}
	if fin.Conflict == nil || fin.Conflict.ID != r2.ID {
		t.Fatalf("expected conflict on r2, got %+v", fin)
	}
	// Critical N1 check: r1 MUST NOT have been committed as fulfilled.
	r1After, err := cs.GetSecretRequest(ctx, r1.ID)
	if err != nil {
		t.Fatalf("get r1: %v", err)
	}
	if r1After.Status != store.SecretRequestStatusFulfilling {
		t.Fatalf("r1 should remain fulfilling after tx rollback, got %s", r1After.Status)
	}
}

// TestSecretRequestStore_FinishRequiresMatchingClaimedAt verifies that
// FinishSecretRequestFulfill checks ClaimedAtEQ in its compare-and-set: after
// a stale claim is recovered and the SAME user claims again at a later
// ClaimedAt, finishing with the first ClaimedAt must conflict.
func TestSecretRequestStore_FinishRequiresMatchingClaimedAt(t *testing.T) {
	ctx := context.Background()
	cs, projID, agentID := newSecretRequestTestStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	res, err := cs.CreateSecretRequestPrompt(ctx, makeSecretRequestPromptInput(projID, agentID, false, now,
		messages.SecretRequest{Key: "CLAIM_TS_KEY", KeyNorm: "CLAIM_TS_KEY", SecretType: "environment"},
	))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	row := res.Requests[0].Row

	// 1. First claim by user-1 at t1.
	t1 := now.Add(time.Second)
	claim1, err := cs.ClaimSecretRequestsForFulfill(ctx, &store.ClaimSecretRequestsInput{
		MessageID: row.MessageID,
		RowIDs:    []string{row.ID},
		UserID:    "user-1",
		UserName:  "Alice",
		Now:       t1,
	})
	if err != nil || claim1.Conflict != nil || len(claim1.Claimed) != 1 {
		t.Fatalf("claim1: err=%v claim=%+v", err, claim1)
	}

	// 2. Recover stale claim past ClaimTTL.
	rec, err := cs.RecoverStaleSecretRequestClaims(ctx, t1.Add(msgaction.SecretRequestClaimTTL+time.Second), 10)
	if err != nil || len(rec) != 1 {
		t.Fatalf("recover: err=%v rec=%+v", err, rec)
	}

	// 3. Second claim by the SAME user-1 at t2 > t1.
	t2 := t1.Add(msgaction.SecretRequestClaimTTL + 2*time.Second)
	claim2, err := cs.ClaimSecretRequestsForFulfill(ctx, &store.ClaimSecretRequestsInput{
		MessageID: row.MessageID,
		RowIDs:    []string{row.ID},
		UserID:    "user-1",
		UserName:  "Alice",
		Now:       t2,
	})
	if err != nil || claim2.Conflict != nil || len(claim2.Claimed) != 1 {
		t.Fatalf("claim2: err=%v claim=%+v", err, claim2)
	}
	if claim2.ClaimedAt.Equal(claim1.ClaimedAt) {
		t.Fatalf("expected distinct ClaimedAt timestamps, both %v", claim1.ClaimedAt)
	}

	// 4. Finish using the stale first ClaimedAt MUST conflict (fails if ClaimedAtEQ is removed).
	fin, err := cs.FinishSecretRequestFulfill(ctx, &store.FinishSecretRequestFulfillInput{
		MessageID:      row.MessageID,
		RowIDs:         []string{row.ID},
		UserID:         "user-1",
		ClaimedAt:      claim1.ClaimedAt,
		SecretVersions: map[string]int{row.ID: 1},
		Now:            t2.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("finish with stale ClaimedAt err: %v", err)
	}
	if fin.Conflict == nil {
		t.Fatalf("expected conflict when finishing with stale ClaimedAt, got success: %+v", fin)
	}
	got, err := cs.GetSecretRequest(ctx, row.ID)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if got.Status != store.SecretRequestStatusFulfilling {
		t.Fatalf("expected row to remain fulfilling, got %q", got.Status)
	}
}
