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

package hub

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Finalizing an in_doubt delete whose intent later succeeds
// (ptone/scion#2882, design ptone/scion#2483 §9 item 3).
//
// An engine whose deferred wait ends with its intent still outstanding
// fails the delete in_doubt (failInDoubt), and deferredDeleteDeadline still
// lets that intent run. When it succeeds, the delete is re-claimed under a
// new claim, pinned to the in_doubt claim, and the engine finishes it
// (revoke, the single-transaction finalize, one deleted event):
//
//   - the drain (finalizeInDoubtDelete) re-claims straight to finalizing
//     before the intent is marked done, so start stays refused throughout;
//     teardown ran, so the engine skips the dispatch;
//   - the engine itself (recheckInDoubt) covers the intent completing
//     between its outstanding read and its in_doubt write. It re-claims to
//     deleting and dispatches again, because a start may have been admitted
//     once the intent was done.
//
// A user retry, a force delete or any newer claim bumps the claim first, so
// the re-claim then matches nothing.

// inDoubtWrittenHook is a test seam: when non-nil it runs right after
// failInDoubt's write, before the recheck. Nil in production.
var inDoubtWrittenHook func(agentID string)

// reclaimInDoubtDeletion takes a new claim on agentID's failed/in_doubt
// delete at claim, keeping its stored request and prior. With redispatch
// the row moves to deleting and the plan dispatches again; otherwise it
// moves to finalizing and the plan skips the dispatch. matches, when
// non-nil, must accept the stored request. It returns nil when there is no
// such delete to re-claim (another claim, another state or code, a missing
// or unreadable request, or a request matches refuses).
func (s *Server) reclaimInDoubtDeletion(ctx context.Context, agentID string, claim int64, redispatch bool, matches func(store.DeletionRequestInfo) bool) (*agentDeletionPlan, error) {
	cur, err := s.store.GetAgent(ctx, agentID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil // already finalized
	}
	if err != nil {
		return nil, err
	}
	// The finalize is soft or hard as the original claim decided, never
	// as recomputed now: without that decision, leave the row in_doubt.
	var req store.DeletionRequestInfo
	if cur.DeletionRequest == "" || json.Unmarshal([]byte(cur.DeletionRequest), &req) != nil {
		s.agentLifecycleLog.Warn("in_doubt delete: no stored request; leaving it in_doubt",
			"agent_id", agentID, "claim", claim)
		return nil, nil
	}
	if matches != nil && !matches(req) {
		s.agentLifecycleLog.Warn("in_doubt delete: stored request does not match the intent; leaving it in_doubt",
			"agent_id", agentID, "claim", claim, "request", cur.DeletionRequest)
		return nil, nil
	}

	now := deleteClock()
	lease := now.Add(deleteLease)
	next := store.DeletionStateFinalizing
	empty := ""
	set := store.DeletionFields{
		State:         &next,
		BumpClaim:     true,
		LeaseAt:       &lease,
		Code:          &empty,
		Error:         &empty,
		ClearFailedAt: true,
	}
	if redispatch {
		next = store.DeletionStateDeleting
		// A fresh start time, so the new dispatch's classification counts
		// only intents completed after it.
		set.StartedAt = &now
	}
	pred := store.DeletionPredicate{
		Claim:         &claim,
		States:        []string{store.DeletionStateFailed},
		Codes:         []string{store.DeletionCodeInDoubt},
		DeletedAtNull: true,
	}
	n, err := s.store.UpdateAgentDeletion(ctx, agentID, pred, set)
	if err != nil || n == 0 {
		return nil, err
	}
	row, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if row.DeletionClaim != claim+1 {
		// Re-claimed by someone else already (only after our lease lapsed).
		return nil, nil
	}
	return &agentDeletionPlan{
		claim:        claim + 1,
		snapshot:     row,
		req:          req,
		prior:        row.ParseDeletionPrior(),
		skipDispatch: !redispatch,
	}, nil
}

// finalizeInDoubtDelete runs after a claimed delete intent succeeded on
// this node, before the drain marks it done: if the row still reads
// failed/in_doubt under the intent's claim, it re-claims the delete and
// waits for the engine to finish it. A claimless intent is not tied to a
// delete's claim and never finalizes one. Errors are logged; the row then
// stays in_doubt, and a retry or force still works.
func (s *Server) finalizeInDoubtDelete(ctx context.Context, agentID string, args *DeleteDispatchArgs) {
	if args == nil || args.Claim == 0 {
		return
	}
	plan, err := s.reclaimInDoubtDeletion(ctx, agentID, args.Claim, false, func(req store.DeletionRequestInfo) bool {
		return req.Soft == args.SoftDelete && req.DeleteFiles == args.DeleteFiles
	})
	if err != nil {
		s.agentLifecycleLog.Error("in_doubt delete: re-claim after the intent succeeded failed",
			"agent_id", agentID, "claim", args.Claim, "error", err)
		return
	}
	if plan == nil {
		return
	}
	s.agentLifecycleLog.Info("in_doubt delete: intent succeeded; finalizing",
		"agent_id", agentID, "intent_claim", args.Claim, "claim", plan.claim)
	s.events.PublishAgentStatus(ctx, plan.snapshot)
	out := <-s.runAgentDeletion(ctx, plan)
	s.logInDoubtOutcome(agentID, plan.claim, out)
}

// recheckInDoubt runs after failInDoubt's write. If the delete intent has
// meanwhile completed (none outstanding, one done since this claim
// started; the rule classifyDispatchFailure uses), the drain may have
// checked the row before the write landed and left it to this engine, so
// re-claim with a fresh dispatch and return that engine's outcome.
func (e *deletionEngine) recheckInDoubt() (deletionOutcome, bool) {
	s := e.s
	agentID := e.agentID()
	started := e.plan.snapshot.DeletionStartedAt
	if started == nil {
		return deletionOutcome{}, false
	}
	ctx, cancel := context.WithTimeout(e.base, deleteShortStep)
	outstanding, err := s.store.HasOutstandingBrokerDispatch(ctx, agentID, brokerDispatchOpDelete)
	completed := false
	if err == nil && !outstanding {
		completed, err = s.store.HasCompletedBrokerDispatchSince(ctx, agentID, brokerDispatchOpDelete, *started)
	}
	cancel()
	if err != nil || !completed {
		return deletionOutcome{}, false
	}
	ctx, cancel = context.WithTimeout(e.base, deleteShortStep)
	plan, err := s.reclaimInDoubtDeletion(ctx, agentID, e.plan.claim, true, nil)
	if err != nil || plan == nil {
		cancel()
		if err != nil {
			s.agentLifecycleLog.Error("in_doubt delete: re-claim after the recheck failed",
				"agent_id", agentID, "claim", e.plan.claim, "error", err)
		}
		return deletionOutcome{}, false
	}
	s.agentLifecycleLog.Info("in_doubt delete: intent completed during the in_doubt write; deleting again",
		"agent_id", agentID, "old_claim", e.plan.claim, "claim", plan.claim)
	s.events.PublishAgentStatus(ctx, plan.snapshot)
	cancel()
	out := <-s.runAgentDeletion(e.base, plan)
	s.logInDoubtOutcome(agentID, plan.claim, out)
	return out, true
}

func (s *Server) logInDoubtOutcome(agentID string, claim int64, out deletionOutcome) {
	s.agentLifecycleLog.Info("in_doubt delete: engine finished",
		"agent_id", agentID, "claim", claim, "outcome", int(out.kind), "code", out.code)
}
