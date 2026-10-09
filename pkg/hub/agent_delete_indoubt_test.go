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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An in_doubt delete whose queued intent later succeeds ends deleted with
// one deleted event and no user action (ptone/scion#2882).

// newInDoubtFixture returns a deferred delete fixture whose DELETE ended
// in_doubt with its intent still outstanding, plus that intent and the
// delete's claim.
func newInDoubtFixture(t *testing.T, suffix string, setup func(f *deferredDeleteFixture)) (*deferredDeleteFixture, store.BrokerDispatch, int64) {
	t.Helper()
	setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 100 * time.Millisecond })
	f := newDeferredDeleteFixture(t, suffix, nil)
	if setup != nil {
		setup(f)
	}
	requireInDoubt(t, f, f.del(t, ""))
	intents := f.pendingDeleteIntents(t)
	require.Len(t, intents, 1, "the intent is still outstanding")
	require.Equal(t, store.DispatchStatePending, intents[0].State, "in_doubt because the intent never ran")
	claim := mustGetAgent(t, f.store, f.agent.ID).DeletionClaim
	require.NotZero(t, claim)
	return f, intents[0], claim
}

// drainOK runs the owning node's drain with the broker now answering the
// delete directly.
func (f *deferredDeleteFixture) drainOK(t *testing.T) {
	t.Helper()
	f.client.returnErr = nil
	f.srv.drainBrokerDispatch(context.Background(), f.agent.RuntimeBrokerID, nil)
}

func (f *deferredDeleteFixture) revokes() int {
	f.hooks.mu.Lock()
	defer f.hooks.mu.Unlock()
	return f.hooks.revokeCalls
}

func intentState(t *testing.T, s store.Store, id string) string {
	t.Helper()
	d, err := s.GetBrokerDispatch(context.Background(), id)
	require.NoError(t, err)
	return d.State
}

// requireDeletedOnce asserts exactly one deleted event and exactly one
// credential revoke.
func requireDeletedOnce(t *testing.T, f *deferredDeleteFixture) {
	t.Helper()
	assert.Equal(t, 1, f.pub.count("deleted"), "exactly one deleted event")
	assert.Equal(t, 1, f.revokes(), "exactly one credential revoke")
}

// requireStillInDoubt asserts the row still reads failed/in_doubt at claim
// and that nothing was finalized.
func requireStillInDoubt(t *testing.T, f *deferredDeleteFixture, claim int64) {
	t.Helper()
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
	assert.Equal(t, store.DeletionCodeInDoubt, got.DeletionCode)
	assert.Equal(t, claim, got.DeletionClaim)
	assert.True(t, got.DeletedAt.IsZero())
	assert.Zero(t, f.pub.count("deleted"))
	assert.Zero(t, f.revokes())
}

// insertClaimlessDeleteIntent writes a delete intent with no claim, as a
// project delete does, and returns it.
func insertClaimlessDeleteIntent(t *testing.T, f *deferredDeleteFixture) store.BrokerDispatch {
	t.Helper()
	raw, err := json.Marshal(&DeleteDispatchArgs{
		DeleteFiles: true,
		NotAfter:    time.Now().Add(time.Minute),
		Target: &DeleteIntentTarget{
			BrokerID: f.agent.RuntimeBrokerID, ProjectID: f.agent.ProjectID,
			Slug: f.agent.Slug, Runtime: f.agent.Runtime,
		},
	})
	require.NoError(t, err)
	d := store.BrokerDispatch{
		ID: uuid.NewString(), BrokerID: f.agent.RuntimeBrokerID, AgentID: f.agent.ID,
		ProjectID: f.agent.ProjectID, Op: brokerDispatchOpDelete, Args: string(raw),
	}
	require.NoError(t, f.store.InsertBrokerDispatch(context.Background(), &d))
	return d
}

func setInDoubtWrittenHook(t *testing.T, fn func(agentID string)) {
	t.Helper()
	old := inDoubtWrittenHook
	inDoubtWrittenHook = fn
	t.Cleanup(func() { inDoubtWrittenHook = old })
}

// The drained intent succeeds on an in_doubt row: the row is hard-deleted
// with one deleted event and one revoke, the intent completes, and the
// re-claim lands while the intent is still outstanding (so start cannot
// slip in between).
func TestInDoubtDelete_IntentSucceedsFinalizesHard(t *testing.T) {
	f, intent, _ := newInDoubtFixture(t, "idhard", nil)

	var mu sync.Mutex
	var reclaims int
	var outstandingAtReclaim []bool
	f.hooks.onDeletionWrite = func(pred store.DeletionPredicate) {
		if !slices.Contains(pred.Codes, store.DeletionCodeInDoubt) {
			return
		}
		out, err := f.store.HasOutstandingBrokerDispatch(context.Background(), f.agent.ID, brokerDispatchOpDelete)
		assert.NoError(t, err)
		mu.Lock()
		reclaims++
		outstandingAtReclaim = append(outstandingAtReclaim, out)
		mu.Unlock()
	}

	f.drainOK(t)

	assert.True(t, agentGone(t, f.store, f.agent.ID), "the in_doubt row is finalized")
	requireDeletedOnce(t, f)
	assert.Equal(t, store.DispatchStateDone, intentState(t, f.store, intent.ID))
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, reclaims, "one re-claim")
	assert.Equal(t, []bool{true}, outstandingAtReclaim, "the re-claim lands before the intent completes")
}

// A soft delete stays soft, from the stored request: the soft-delete
// retention is turned off after the in_doubt and the row is still
// soft-deleted.
func TestInDoubtDelete_IntentSucceedsKeepsSoft(t *testing.T) {
	f, intent, _ := newInDoubtFixture(t, "idsoft", func(f *deferredDeleteFixture) {
		f.srv.config.SoftDeleteRetention = time.Hour
	})
	require.True(t, mustGetAgent(t, f.store, f.agent.ID).ParseDeletionRequest().Soft)
	f.srv.config.SoftDeleteRetention = 0

	f.drainOK(t)

	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.False(t, got.DeletedAt.IsZero(), "soft-deleted, not hard-deleted")
	assert.Equal(t, store.DeletionStateNone, got.DeletionState, "the soft finish clears the marker")
	requireDeletedOnce(t, f)
	assert.Equal(t, store.DispatchStateDone, intentState(t, f.store, intent.ID))
}

// A user retry that finishes the delete first wins; the old intent then
// drains without a second finalize.
func TestInDoubtDelete_UserRetryFirstWins(t *testing.T) {
	f, _, _ := newInDoubtFixture(t, "idretry", nil)

	f.client.returnErr = nil
	r := f.del(t, "")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	require.True(t, agentGone(t, f.store, f.agent.ID))

	f.drainOK(t)

	requireDeletedOnce(t, f)
}

// A force delete that finishes first wins; the old intent then drains
// without a second finalize.
func TestInDoubtDelete_ForceFirstWins(t *testing.T) {
	f, _, _ := newInDoubtFixture(t, "idforce", nil)

	r := f.del(t, "?force=true")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	require.True(t, agentGone(t, f.store, f.agent.ID))

	f.drainOK(t)

	requireDeletedOnce(t, f)
}

// The re-claim is pinned to the intent's claim: once a newer claim holds
// the row (here one that also ended in_doubt), the older claim's re-claim
// does nothing, and its intent drains without finalizing.
func TestInDoubtDelete_ReclaimNeedsCurrentClaim(t *testing.T) {
	f, _, claim := newInDoubtFixture(t, "idnewer", nil)
	ctx := context.Background()
	n, err := f.store.UpdateAgentDeletion(ctx, f.agent.ID, store.DeletionPredicate{Claim: &claim},
		store.DeletionFields{BumpClaim: true})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	plan, err := f.srv.reclaimInDoubtDeletion(ctx, f.agent.ID, claim, false, nil)
	require.NoError(t, err)
	assert.Nil(t, plan, "an older claim cannot re-claim")
	requireStillInDoubt(t, f, claim+1)

	f.drainOK(t)
	requireStillInDoubt(t, f, claim+1)
}

// While the engine still holds a live claim, the drained intent's success
// is left to the engine: it reads the intent done and finalizes, once.
func TestInDoubtDelete_LiveClaimLeftToEngine(t *testing.T) {
	setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 10 * time.Second })
	f := newDeferredDeleteFixture(t, "idlive", nil)

	var reclaims atomic.Int32
	f.hooks.onDeletionWrite = func(pred store.DeletionPredicate) {
		if slices.Contains(pred.Codes, store.DeletionCodeInDoubt) {
			reclaims.Add(1)
		}
	}
	ch := deleteAsync(t, f.srv, "/api/v1/agents/"+f.agent.ID, nil)
	require.Eventually(t, func() bool { return len(f.pendingDeleteIntents(t)) == 1 },
		5*time.Second, 10*time.Millisecond, "the delete intent was never written")
	require.Equal(t, store.DeletionStateDeleting, mustGetAgent(t, f.store, f.agent.ID).DeletionState)

	f.drainOK(t)

	r := waitDelete(t, ch, 10*time.Second)
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.True(t, agentGone(t, f.store, f.agent.ID))
	requireDeletedOnce(t, f)
	assert.LessOrEqual(t, reclaims.Load(), int32(1), "at most one (missed) re-claim attempt")
}

// Only a failed/in_doubt row is re-claimed: a rollback, a conflict, a
// finalizing failure, a live claim and a row with no marker are left as
// they are.
func TestInDoubtDelete_OnlyInDoubtIsReclaimed(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Second)
	future := now.Add(time.Minute)
	cases := []struct {
		name  string
		state string
		code  string
		lease *time.Time
	}{
		{"rollback runtime_error", store.DeletionStateFailed, store.DeletionCodeRuntimeError, nil},
		{"rollback conflict", store.DeletionStateFailed, store.DeletionCodeConflict, nil},
		{"finalizing revoke_failed", store.DeletionStateFinalizing, store.DeletionCodeRevokeFailed, &past},
		{"live deleting", store.DeletionStateDeleting, "", &future},
		{"no marker", store.DeletionStateNone, "", nil},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeferredDeleteFixture(t, "idonly-"+string(rune('a'+i)), nil)
			ctx := context.Background()
			st, code := tc.state, tc.code
			req := `{"deleteFiles":true,"removeBranch":true}`
			set := store.DeletionFields{BumpClaim: true, State: &st, Code: &code, Request: &req}
			if tc.lease != nil {
				set.LeaseAt = tc.lease
			}
			n, err := f.store.UpdateAgentDeletion(ctx, f.agent.ID, store.DeletionPredicate{}, set)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			before := mustGetAgent(t, f.store, f.agent.ID)

			plan, err := f.srv.reclaimInDoubtDeletion(ctx, f.agent.ID, before.DeletionClaim, false, nil)
			require.NoError(t, err)
			assert.Nil(t, plan)
			after := mustGetAgent(t, f.store, f.agent.ID)
			assert.Equal(t, before.DeletionClaim, after.DeletionClaim)
			assert.Equal(t, before.DeletionState, after.DeletionState)
			assert.Equal(t, before.DeletionCode, after.DeletionCode)
			assert.Equal(t, before.StateVersion, after.StateVersion, "no write")
		})
	}
}

// A revoke failure leaves the row finalizing with revoke_failed (read as
// failed); a retry then finishes the delete.
func TestInDoubtDelete_RevokeFailureIsRetryable(t *testing.T) {
	f, intent, _ := newInDoubtFixture(t, "idrevoke", nil)
	f.hooks.setRevokeErr(errors.New("credential store unavailable"))

	f.drainOK(t)

	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, store.DeletionStateFinalizing, got.DeletionState)
	assert.Equal(t, store.DeletionCodeRevokeFailed, got.DeletionCode)
	assert.Zero(t, f.pub.count("deleted"))
	assert.Equal(t, store.DispatchStateDone, intentState(t, f.store, intent.ID), "the intent did run")

	f.hooks.setRevokeErr(nil)
	r := f.del(t, "")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.True(t, agentGone(t, f.store, f.agent.ID))
	assert.Equal(t, 1, f.pub.count("deleted"))
}

// A finalize failure leaves the row finalizing with finalize_failed; a
// retry then finishes the delete (soft, so the deleted event follows the
// commit).
func TestInDoubtDelete_FinalizeFailureIsRetryable(t *testing.T) {
	f, _, _ := newInDoubtFixture(t, "idfinal", func(f *deferredDeleteFixture) {
		f.srv.config.SoftDeleteRetention = time.Hour
	})
	var once sync.Once
	old := agentDeletionFinalizeSeam
	t.Cleanup(func() { agentDeletionFinalizeSeam = old })
	agentDeletionFinalizeSeam = func(context.Context, store.Store, *store.Agent, store.DeletionFinalizeMode) error {
		var err error
		once.Do(func() { err = errors.New("injected finalize error") })
		return err
	}

	f.drainOK(t)

	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, store.DeletionStateFinalizing, got.DeletionState)
	assert.Equal(t, store.DeletionCodeFinalizeFailed, got.DeletionCode)
	assert.True(t, got.DeletedAt.IsZero())
	assert.Zero(t, f.pub.count("deleted"))

	r := f.del(t, "")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.False(t, mustGetAgent(t, f.store, f.agent.ID).DeletedAt.IsZero())
	assert.Equal(t, 1, f.pub.count("deleted"))
}

// The intent completes after the engine read it outstanding but before its
// in_doubt write landed, so the drain saw a live claim and left it to the
// engine. The engine's recheck after its write finds the intent done and
// re-claims with a fresh dispatch through the normal engine path; the
// DELETE answers 204.
func TestInDoubtDelete_EngineRecheckFinalizes(t *testing.T) {
	setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 100 * time.Millisecond })
	f := newDeferredDeleteFixture(t, "idrecheck", nil)
	var once sync.Once
	var fired atomic.Bool
	setInDoubtWrittenHook(t, func(agentID string) {
		if agentID != f.agent.ID {
			return
		}
		once.Do(func() {
			fired.Store(true)
			for _, d := range f.pendingDeleteIntents(t) {
				endIntent(t, f.store, d.ID, true)
			}
			f.client.returnErr = nil
			f.client.deleteCalled = false
		})
	})

	r := f.del(t, "")
	require.True(t, fired.Load(), "the in_doubt write ran the seam")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.True(t, agentGone(t, f.store, f.agent.ID))
	requireDeletedOnce(t, f)
	assert.True(t, f.client.deleteCalled, "the recheck re-dispatches the delete")
}

// The recheck uses the agent-wide rule (no outstanding delete intent, one
// completed since the delete started), which a claimless intent can
// satisfy while the engine's own intent failed. The re-claim re-dispatches
// rather than skipping teardown, so this is safe: the re-dispatch's own
// intent later drains and finalizes, once.
func TestInDoubtDelete_EngineRecheckClaimlessCompletionIsSafe(t *testing.T) {
	setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 100 * time.Millisecond })
	f := newDeferredDeleteFixture(t, "idclrace", nil)
	var once sync.Once
	var fired atomic.Bool
	var firstClaim int64
	setInDoubtWrittenHook(t, func(agentID string) {
		if agentID != f.agent.ID {
			return
		}
		once.Do(func() {
			fired.Store(true)
			firstClaim = mustGetAgent(t, f.store, f.agent.ID).DeletionClaim
			for _, d := range f.pendingDeleteIntents(t) {
				endIntent(t, f.store, d.ID, false)
			}
			cl := insertClaimlessDeleteIntent(t, f)
			endIntent(t, f.store, cl.ID, true)
		})
	})

	requireInDoubt(t, f, f.del(t, ""))
	require.True(t, fired.Load(), "the in_doubt write ran the seam")
	got := mustGetAgent(t, f.store, f.agent.ID)
	require.Equal(t, firstClaim+1, got.DeletionClaim, "the recheck re-claimed")
	intents := f.pendingDeleteIntents(t)
	require.Len(t, intents, 1, "the re-dispatch wrote its own intent")
	args, err := UnmarshalDeleteArgs(intents[0].Args)
	require.NoError(t, err)
	assert.Equal(t, firstClaim+1, args.Claim)
	assert.Zero(t, f.pub.count("deleted"))
	assert.Zero(t, f.revokes())

	f.drainOK(t)

	assert.True(t, agentGone(t, f.store, f.agent.ID))
	requireDeletedOnce(t, f)
}

// A claimless intent's success never finalizes an in_doubt row: it is not
// tied to the delete's claim.
func TestInDoubtDelete_ClaimlessIntentDoesNotFinalize(t *testing.T) {
	f, _, claim := newInDoubtFixture(t, "idclaimless", nil)
	d := insertClaimlessDeleteIntent(t, f)
	f.client.returnErr = nil

	_, err := f.srv.execDispatchDelete(context.Background(), d)
	require.NoError(t, err)

	requireStillInDoubt(t, f, claim)
}

// The finalize uses the delete's stored request. When it is missing or
// does not match what the intent ran (soft, delete-files), the row stays
// in_doubt for a retry or force; the intent still completes.
func TestInDoubtDelete_RequestMismatchStaysInDoubt(t *testing.T) {
	cases := []struct {
		name string
		req  string
	}{
		{"missing request", ""},
		{"soft differs", `{"deleteFiles":true,"removeBranch":true,"soft":true}`},
		{"delete files differ", `{"removeBranch":true}`},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, intent, claim := newInDoubtFixture(t, "idreq-"+string(rune('a'+i)), nil)
			req := tc.req
			n, err := f.store.UpdateAgentDeletion(context.Background(), f.agent.ID,
				store.DeletionPredicate{Claim: &claim}, store.DeletionFields{Request: &req})
			require.NoError(t, err)
			require.Equal(t, 1, n)

			f.drainOK(t)

			requireStillInDoubt(t, f, claim)
			assert.Equal(t, store.DispatchStateDone, intentState(t, f.store, intent.ID))
		})
	}
}
