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
	"errors"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Backend-driven agent delete: predicates, the shared start gate, and the
// failed-marker clear (design ptone/scion#2483 §2.1, §2.4).

// brokerDispatchOpDelete is the broker_dispatch op for a cross-node delete.
const brokerDispatchOpDelete = "delete"

// deletionActive reports whether a delete holds a live lease on a
// (state ∈ {deleting, finalizing} && lease_at > now). An expired row is not
// active: it reads as failed in the guards, the view and the claim.
func deletionActive(a *store.Agent) bool {
	return a.DeletionActive(time.Now())
}

// deleteStopNoop reports whether a stop request is a 200 no-op because the
// agent is already going down: a delete is active, or the row is
// finalizing (teardown has run, even if the lease expired).
func deleteStopNoop(a *store.Agent) bool {
	if a == nil {
		return false
	}
	return deletionActive(a) || a.DeletionState == store.DeletionStateFinalizing
}

// deletedOrDeleteHeld reports whether an agent row that still exists is
// soft-deleted or held by a delete (deleteStopNoop). With a row that is
// gone, it is the "the agent is not live" rule shared by the created
// publish (publishAgentCreatedIfLive), the message-broker subscription on
// created (createdAgentLive) and the compensating delete of a landed run
// (compensateLandedRun). A failed delete, or a deleting row whose lease
// expired, does not count: that delete gave up.
func deletedOrDeleteHeld(a *store.Agent) bool {
	if a == nil {
		return false
	}
	return !a.DeletedAt.IsZero() || deleteStopNoop(a)
}

// deleteBlocksStart is the start-block predicate (design §2.1):
//
//	deletionActive(a)
//	|| a.DeletionState == "finalizing"            -- expired too: only retry/force
//	|| HasOutstandingBrokerDispatch(a.ID, delete) -- pending|in_progress intent
//
// The third term reads broker_dispatch rather than the deletion code, so it
// holds whatever the claim or retry history is, and also covers an engine
// that died before recording anything. It is evaluated only on the start
// entries and the engine's classification, never on list views.
func (s *Server) deleteBlocksStart(ctx context.Context, a *store.Agent) (bool, error) {
	if a == nil {
		return false, nil
	}
	// store.DeletionHoldsRow is deletionActive(a) || finalizing (even
	// expired); SetAgentRunID refuses under the same predicate.
	if a.DeletionHoldsRow(time.Now()) {
		return true, nil
	}
	return s.store.HasOutstandingBrokerDispatch(ctx, a.ID, brokerDispatchOpDelete)
}

// startEntry identifies which entry point is asking the start gate. Step 1
// (delete) answers every entry the same way; later steps (T1 async create,
// ptone/scion#2153) answer per entry.
type startEntry string

const (
	startEntryStart          startEntry = "start"
	startEntryRestart        startEntry = "restart"
	startEntryReincarnate    startEntry = "reincarnate"
	startEntryRestore        startEntry = "restore"
	startEntryCreateExisting startEntry = "create_existing" // POST /agents on an existing agent (resume/recreate)
	startEntryWake           startEntry = "wake"            // DM wake (any phase where a launch refusal applies, else suspended)
)

// startRefusal is the start gate's answer when it has something to say.
//
//   - HTTPStatus != 0: refuse. The entry writes HTTPStatus/Code/Message
//     (and Details) and does nothing else — no quota, no dispatch.
//   - HTTPStatus == 0 with Warnings: allow, and the entry attaches Warnings
//     to its success response (T1's per-entry "200 with Warnings" answer).
//
// A nil *startRefusal means "allowed, nothing to add".
type startRefusal struct {
	HTTPStatus int
	Code       string
	Message    string
	Details    map[string]interface{}
	Warnings   []string
	// InFlight marks the step 3 refusal (409 agent_launching): the agent's
	// create launch is in flight. Entries that answer 200 with the current
	// agent instead check it before writing the refusal.
	InFlight bool
	// launch marks a step 2 or 3 refusal. DM wake reports those in its
	// runtime-error shape.
	launch bool
}

// refuses reports whether r refuses the start.
func (r *startRefusal) refuses() bool {
	return r != nil && r.HTTPStatus != 0
}

// write writes a refusing r as the HTTP error response.
func (r *startRefusal) write(w http.ResponseWriter) {
	writeError(w, r.HTTPStatus, r.Code, r.Message, r.Details)
}

// dmError converts a refusing r for the DM wake path. Launch refusals
// (steps 2-3) keep wake's runtime-error shape; the delete refusal keeps its
// code.
func (r *startRefusal) dmError() *AgentDMError {
	if r.launch {
		return &AgentDMError{
			Code:       ErrCodeRuntimeError,
			Message:    r.Message,
			HTTPStatus: http.StatusBadGateway,
		}
	}
	return &AgentDMError{
		Code:       r.Code,
		Message:    r.Message,
		HTTPStatus: r.HTTPStatus,
		Details:    r.Details,
	}
}

// startGate is the shared start gate (design §2.1 "Start gate helper"),
// called after authz by every entry that can start an agent: start, restart
// (before the managed-runtime branch), reincarnate, restore,
// create-with-existing-agent and DM wake. The order is fixed and the first
// match wins:
//
//  1. deleteBlocksStart → 409 delete_in_progress (this design);
//     1b. soft-deleted, on start, restart and wake → 409 "agent is
//     deleted; restore it first";
//  2. IsIncompleteCreate → 409 agent_create_incomplete (T1 P1b-3);
//  3. IsInFlight, before the launch deadline → 409 agent_launching with
//     InFlight set (T1 P1b-3). Start, restart and create-existing answer
//     it with 200 and the current agent (plus Warnings); reincarnate writes
//     the 409; DM wake skips the wake.
//
// Delete comes first because the delete claim writes stopping, which also
// makes IsIncompleteCreate true, and "deleting" is the accurate message.
// Restore runs the same steps, but steps 2-3 never match there: both
// predicates are false on a soft-deleted row.
func (s *Server) startGate(ctx context.Context, a *store.Agent, entry startEntry) *startRefusal {
	// Step 1: delete in progress.
	blocked, err := s.deleteBlocksStart(ctx, a)
	if err != nil {
		// Fail closed: without the dispatch table we cannot rule out an
		// outstanding cross-node delete intent.
		s.agentLifecycleLog.Error("start gate: delete check failed",
			"agent_id", a.ID, "entry", string(entry), "error", err)
		return &startRefusal{
			HTTPStatus: http.StatusInternalServerError,
			Code:       ErrCodeInternalError,
			Message:    "could not check whether a delete is in progress for this agent",
		}
	}
	if blocked {
		return deleteInProgressRefusal(a.ID)
	}

	// Step 1b: a soft-deleted row is not started or woken in place; only
	// restore brings it back (ptone/scion#2550 P1). Without this the start
	// would pass the gate and fail later at beginRun, whose run-ID write
	// refuses a soft-deleted row, after quota was reserved. It comes after
	// step 1 because a restore is refused while a delete holds the row.
	if !a.DeletedAt.IsZero() && (entry == startEntryStart || entry == startEntryRestart || entry == startEntryWake) {
		return agentDeletedRefusal(a.ID)
	}

	// Steps 2-3: incomplete create, then in flight.
	return launchStartRefusal(a, time.Now())
}

// agentDeletedRefusal is the 409 answer to starting or waking a
// soft-deleted agent.
func agentDeletedRefusal(agentID string) *startRefusal {
	return &startRefusal{
		HTTPStatus: http.StatusConflict,
		Code:       ErrCodeConflict,
		Message:    "agent is deleted; restore it first",
		Details: map[string]interface{}{
			"agentId": agentID,
		},
	}
}

// deleteInProgressRefusal is the 409 delete_in_progress answer.
func deleteInProgressRefusal(agentID string) *startRefusal {
	return &startRefusal{
		HTTPStatus: http.StatusConflict,
		Code:       ErrCodeDeleteInProgress,
		Message:    "a delete is in progress for this agent; wait for it to finish, or force the delete",
		Details: map[string]interface{}{
			"agentId": agentID,
		},
	}
}

// deletedDuringCreateMessage is the message of the 409 a synchronous create
// answers when a delete won the race (writeDeletedDuringCreate).
const deletedDuringCreateMessage = "agent was deleted while it was being created"

// writeDeletedDuringCreate writes the 409 delete_in_progress answer to a
// synchronous create whose agent was deleted, or is held by a delete, by the
// time the dispatch returned (ptone/scion#3099). The code is the one start
// and restart answer when they lose to a delete mid-dispatch
// (deleteClaimedDuringDispatch), and is the same whether the delete still
// holds the row or has finished. The body carries no agent; warnings (the
// outcome of the compensating delete of a run that landed) go in details.
func writeDeletedDuringCreate(w http.ResponseWriter, agentID string, warnings []string) {
	writeDeleteWon(w, agentID, deletedDuringCreateMessage, warnings)
}

// deletedWhileStartingMessage is the message of the 409 a synchronous start
// or restart answers when a delete won after the broker start landed
// (deleteWonAfterLanding). The row may be gone or soft-deleted, or only held
// by a delete that has not finished (and may still fail), so the message
// covers both.
const deletedWhileStartingMessage = "agent was deleted, or is being deleted, while it was starting"

// writeDeleteWon writes the 409 delete_in_progress answer to a synchronous
// create, start or restart that lost to a delete after its dispatch: no
// agent body, details.agentId, and details.warnings (the outcome of the
// compensating delete of a run that landed) when there are any.
func writeDeleteWon(w http.ResponseWriter, agentID, message string, warnings []string) {
	details := map[string]interface{}{"agentId": agentID}
	if len(warnings) > 0 {
		details["warnings"] = warnings
	}
	writeError(w, http.StatusConflict, ErrCodeDeleteInProgress, message, details)
}

// deleteClaimedDuringDispatch returns the delete_in_progress refusal when a
// start or restart dispatch failed because a delete claimed the agent after
// the start gate passed: beginRun's run-ID write is refused once a delete
// holds the row (store.ErrDeleteInProgress), so the start fails closed
// before reaching the broker, rather than starting a run the delete's
// snapshot does not name (ptone/scion#2550 P1 round 3).
func deleteClaimedDuringDispatch(err error, agentID string) *startRefusal {
	if !errors.Is(err, store.ErrDeleteInProgress) {
		return nil
	}
	return deleteInProgressRefusal(agentID)
}

// deleteWonAfterLanding reports whether a synchronous start or restart's
// broker start landed but a delete won while the broker call was in flight:
// the row is gone, or deletedOrDeleteHeld (the rule compensateLandedRun,
// which has already run inside the dispatch, applied to the same row). The
// caller then answers writeDeleteWon (ptone/scion#3255).
//
// A failed re-read reports false: the caller then writes the status as
// before and answers from the stored row. A failed delete, or a deleting row
// whose lease expired, leaves the agent live, so it reports false too.
func (s *Server) deleteWonAfterLanding(ctx context.Context, agentID string) bool {
	fresh, err := s.store.GetAgent(ctx, agentID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return true
	case err != nil:
		s.agentLifecycleLog.Warn("failed to re-read agent after the broker started it; cannot check for a delete",
			"agent_id", agentID, "error", err)
		return false
	default:
		return deletedOrDeleteHeld(fresh)
	}
}

// writeRunIntentError answers a failed running-intent write. A refusal
// because a delete holds the row (store.ErrDeleteInProgress) gets the same
// delete_in_progress body, details.agentId included, as every other
// delete_in_progress answer; anything else goes to writeErrorFromErr.
func writeRunIntentError(w http.ResponseWriter, err error, agentID string) {
	if refusal := deleteClaimedDuringDispatch(err, agentID); refusal != nil {
		refusal.write(w)
		return
	}
	writeErrorFromErr(w, err, "")
}

// clearFailedDeletion clears a failed delete marker after a successful
// start, stop, restart or reincarnate (design §2.1). A row that reads as
// failed counts: state=failed, or a deleting row whose lease has expired.
// Only the failed-marker columns are cleared; the claim epoch is kept so it
// stays monotonic. On success the in-memory agent is updated to match, so a
// following publish carries deletion:null. Best-effort: errors are logged.
//
// a must be the agent as loaded before the action: both predicates pin the
// claim observed then, so a newer delete that claimed (and failed) while the
// action ran keeps its marker and banner.
//
// Call it after the action's own writes: it bumps state_version.
func (s *Server) clearFailedDeletion(ctx context.Context, a *store.Agent) {
	if a == nil {
		return
	}
	s.clearFailedDeletionAtClaim(ctx, a, a.DeletionClaim)
}

// clearFailedDeletionAtClaim is clearFailedDeletion with the claim pinned
// explicitly, for callers (the reincarnate worker) whose a was re-read after
// the action and so may already carry a newer claim.
func (s *Server) clearFailedDeletionAtClaim(ctx context.Context, a *store.Agent, claim int64) {
	if a == nil || a.DeletionState == store.DeletionStateNone {
		return
	}
	// Keep the marker (and its banner and force affordance) while an older
	// delete intent is still outstanding: start stays blocked by it, so the
	// view must keep explaining why (in_doubt never expires from view).
	if outstanding, err := s.store.HasOutstandingBrokerDispatch(ctx, a.ID, brokerDispatchOpDelete); err != nil || outstanding {
		if err != nil {
			s.agentLifecycleLog.Warn("failed to check delete intents before clearing failed marker",
				"agent_id", a.ID, "error", err)
		}
		return
	}
	now := time.Now()
	none := store.DeletionStateNone
	empty := ""
	set := store.DeletionFields{
		State:          &none,
		Code:           &empty,
		Error:          &empty,
		Prior:          &empty,
		Request:        &empty,
		ClearLeaseAt:   true,
		ClearStartedAt: true,
		ClearFailedAt:  true,
	}
	preds := []store.DeletionPredicate{
		{Claim: &claim, States: []string{store.DeletionStateFailed}},
		// An abandoned deleting row reads as failed. A finalizing row is
		// never cleared here: teardown has run, only retry or force lifts it.
		{Claim: &claim, States: []string{store.DeletionStateDeleting}, LeaseExpiredBefore: &now},
	}
	for _, pred := range preds {
		n, err := s.store.UpdateAgentDeletion(ctx, a.ID, pred, set)
		if err != nil {
			s.agentLifecycleLog.Warn("failed to clear failed delete marker",
				"agent_id", a.ID, "error", err)
			return
		}
		if n > 0 {
			a.DeletionState = store.DeletionStateNone
			a.DeletionCode = ""
			a.DeletionError = ""
			a.DeletionPrior = ""
			a.DeletionRequest = ""
			a.DeletionLeaseAt = nil
			a.DeletionStartedAt = nil
			a.DeletionFailedAt = nil
			a.StateVersion++
			return
		}
	}
}

// settleLifecycleWrite runs after a start/stop/restart's UpdateAgentStatus
// succeeds. It clears a failed delete marker (pinned to the claim observed
// at load, see clearFailedDeletion) and then re-reads the row so the publish
// and the response reflect what was actually stored. A delete that claimed
// the row while the action dispatched makes the in-tx guard drop the phase
// write; without the re-read the handler would publish and return the
// requested phase with deletion:null while the row is deleting.
//
// Only the columns the in-tx guard and the clear can change are carried over
// from the re-read; in-memory fields the dispatch set are kept. If the re-read
// fails, a falls back to the requested phase (the pre-guard behaviour) and
// reloaded is false.
func (s *Server) settleLifecycleWrite(ctx context.Context, a *store.Agent, newPhase string) (reloaded bool) {
	a.Phase = newPhase
	s.clearFailedDeletion(ctx, a)
	if err := s.reloadGuardedColumns(ctx, a); err != nil {
		s.agentLifecycleLog.Warn("failed to re-read agent after lifecycle write",
			"agent_id", a.ID, "error", err)
		return false
	}
	return true
}

// reloadGuardedColumns re-reads a's row and copies the columns a concurrent
// delete (or a guarded store write) may have changed, so a following publish
// reports the row as stored rather than as the caller assumed it.
func (s *Server) reloadGuardedColumns(ctx context.Context, a *store.Agent) error {
	fresh, err := s.store.GetAgent(ctx, a.ID)
	if err != nil {
		return err
	}
	a.Phase = fresh.Phase
	a.Activity = fresh.Activity
	a.ContainerStatus = fresh.ContainerStatus
	a.ExitCode = fresh.ExitCode
	a.ExitReason = fresh.ExitReason
	a.Message = fresh.Message
	a.StateVersion = fresh.StateVersion
	a.DeletedAt = fresh.DeletedAt
	a.SoftDeleteOpID = fresh.SoftDeleteOpID
	a.DeletionState = fresh.DeletionState
	a.DeletionClaim = fresh.DeletionClaim
	a.DeletionLeaseAt = fresh.DeletionLeaseAt
	a.DeletionStartedAt = fresh.DeletionStartedAt
	a.DeletionFailedAt = fresh.DeletionFailedAt
	a.DeletionCode = fresh.DeletionCode
	a.DeletionError = fresh.DeletionError
	a.DeletionPrior = fresh.DeletionPrior
	a.DeletionRequest = fresh.DeletionRequest
	return nil
}

// publishAgentStatusFresh publishes a's status from a re-read of its row
// (design note F): a status write that raced a delete must not publish the
// phase it intended over the delete's stopping/deleted. Nothing is published
// when the row is gone or soft-deleted.
func (s *Server) publishAgentStatusFresh(ctx context.Context, a *store.Agent) {
	if err := s.reloadGuardedColumns(ctx, a); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("failed to re-read agent before status publish",
				"agent_id", a.ID, "error", err)
		}
		return
	}
	if !a.DeletedAt.IsZero() {
		return
	}
	s.events.PublishAgentStatus(ctx, a)
}
