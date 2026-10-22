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

package entadapter

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ApplyLaunchReport implements store.AgentStore.ApplyLaunchReport, evaluating
// the ordered rule list in design t1-async-create-v11.md §3.7. It runs as a
// single hand-rolled transaction (see launch_store.go's file comment) holding
// a row lock on Postgres for its duration.
func (s *AgentStore) ApplyLaunchReport(ctx context.Context, agentID, brokerID string, r store.LaunchReport) (store.LaunchReportAnswer, store.Agent, error) {
	switch r.State {
	case store.LaunchReportStateClaim, store.LaunchReportStateCheckpoint, store.LaunchReportStateProgress,
		store.LaunchReportStateSucceeded, store.LaunchReportStateFailed:
	default:
		return store.LaunchReportAnswer{}, store.Agent{}, fmt.Errorf("%w: unknown launch report state %q", store.ErrInvalidInput, r.State)
	}
	if r.LaunchID == "" {
		// An empty LaunchID would coincide with a never-launched row's
		// current.LaunchID (also "", its zero value), letting step 2 below
		// mistake "this report names no launch" for "this report matches the
		// row's current launch" and fall through to applyLaunchReportActive
		// as if the row had an active launch it never had.
		return store.LaunchReportAnswer{}, store.Agent{}, fmt.Errorf("%w: launch report LaunchID must not be empty", store.ErrInvalidInput)
	}

	uid, err := parseUUID(agentID)
	if err != nil {
		return store.LaunchReportAnswer{}, store.Agent{}, err
	}

	ltx, err := s.beginLaunchTx(ctx)
	if err != nil {
		return store.LaunchReportAnswer{}, store.Agent{}, err
	}
	defer ltx.cleanup()
	isPG := s.dialect() == dialect.Postgres
	committed := false
	defer func() {
		if !committed {
			_ = ltx.tx.Rollback()
		}
	}()

	q := ltx.client.Agent.Query().Where(agent.IDEQ(uid))
	if isPG {
		q = q.ForUpdate()
	}
	current, err := q.Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			// Read-only exit: nothing was written, so roll back rather than
			// commit — a commit here would mask a Postgres commit failure on
			// a transaction that only ever did a FOR UPDATE read.
			_ = ltx.tx.Rollback()
			committed = true
			return store.LaunchReportAnswer{HTTPStatus: http.StatusNotFound, Code: store.LaunchReportCodeUnknownLaunch}, store.Agent{}, nil
		}
		return store.LaunchReportAnswer{}, store.Agent{}, err
	}

	// Step 1 (broker identity): brokerID != agent.RuntimeBrokerID, including
	// after reassignment.
	if current.RuntimeBrokerID != brokerID {
		_ = ltx.tx.Rollback()
		committed = true
		return store.LaunchReportAnswer{HTTPStatus: http.StatusForbidden}, *entAgentToStore(current), nil
	}

	// Step 2: r.LaunchID != agent.launch_id -> superseded. Covers both "this
	// launch has already been superseded by a newer one" and "this row has
	// never heard of this launch ID".
	if r.LaunchID != current.LaunchID {
		_ = ltx.tx.Rollback()
		committed = true
		return store.LaunchReportAnswer{HTTPStatus: http.StatusConflict, Code: store.LaunchReportCodeStaleLaunch, Reason: store.LaunchReportReasonSuperseded}, *entAgentToStore(current), nil
	}

	// Step 3: DeletedAt set -> deleted.
	if current.DeletedAt != nil {
		_ = ltx.tx.Rollback()
		committed = true
		return store.LaunchReportAnswer{HTTPStatus: http.StatusConflict, Code: store.LaunchReportCodeStaleLaunch, Reason: store.LaunchReportReasonDeleted}, *entAgentToStore(current), nil
	}

	// Step 4: launch_owner set and != r.InstanceID, for ANY state ->
	// other_owner. If the owner is empty and the report is non-terminal and
	// the launch is active, claim it for r.InstanceID (applied below,
	// together with whatever else this report causes).
	terminal := r.State == store.LaunchReportStateSucceeded || r.State == store.LaunchReportStateFailed
	if current.LaunchOwner != "" && current.LaunchOwner != r.InstanceID {
		_ = ltx.tx.Rollback()
		committed = true
		return store.LaunchReportAnswer{HTTPStatus: http.StatusConflict, Code: store.LaunchReportCodeStaleLaunch, Reason: store.LaunchReportReasonOtherOwner}, *entAgentToStore(current), nil
	}
	ownerToSet := ""
	if current.LaunchOwner == "" && !terminal && current.LaunchState == store.LaunchStateActive {
		ownerToSet = r.InstanceID
	}

	if current.LaunchState == store.LaunchStateEnded {
		answer, updated, wrote, err := s.applyLaunchReportEnded(ctx, ltx, current, r)
		if err != nil {
			return store.LaunchReportAnswer{}, store.Agent{}, err
		}
		if !wrote {
			// Read-only exit: nothing was written, so roll back. wrote is
			// decided next to each Save() call, independently of the
			// answer's Changed field (which is about publish semantics for
			// the P1a-ii caller, not about whether a write happened) — a
			// wrong Changed value must never be able to turn a real write
			// into a silently discarded one.
			_ = ltx.tx.Rollback()
			committed = true
			return answer, *entAgentToStore(updated), nil
		}
		if err := ltx.tx.Commit(); err != nil {
			return store.LaunchReportAnswer{}, store.Agent{}, fmt.Errorf("launch store: commit ApplyLaunchReport: %w", err)
		}
		committed = true
		return answer, *entAgentToStore(updated), nil
	}

	if current.LaunchState != store.LaunchStateActive {
		// current.LaunchState == "" (no launch has ever begun on this row).
		// Since r.LaunchID is rejected above when empty, and BeginLaunch is
		// the only writer of a non-empty launch_id, current.LaunchID must
		// also be "" here — meaning step 2 above (r.LaunchID != current.
		// LaunchID) already rejects every report that reaches this point as
		// superseded. This is a defensive, explicit check so "not ended" is
		// never silently assumed to mean "active".
		_ = ltx.tx.Rollback()
		committed = true
		return store.LaunchReportAnswer{HTTPStatus: http.StatusConflict, Code: store.LaunchReportCodeStaleLaunch, Reason: store.LaunchReportReasonSuperseded}, *entAgentToStore(current), nil
	}

	answer, updated, err := s.applyLaunchReportActive(ctx, ltx, current, r, ownerToSet)
	if err != nil {
		return store.LaunchReportAnswer{}, store.Agent{}, err
	}
	if err := ltx.tx.Commit(); err != nil {
		return store.LaunchReportAnswer{}, store.Agent{}, fmt.Errorf("launch store: commit ApplyLaunchReport: %w", err)
	}
	committed = true
	return answer, *entAgentToStore(updated), nil
}

// applyLaunchReportEnded handles design §3.7 step 5 (launch_state=ended). Its
// third return value, wrote, tells the caller whether to commit — it is set
// immediately next to each Save() call, never inferred from the returned
// answer's Changed field.
func (s *AgentStore) applyLaunchReportEnded(ctx context.Context, ltx *launchTx, current *ent.Agent, r store.LaunchReport) (store.LaunchReportAnswer, *ent.Agent, bool, error) {
	switch current.LaunchEndReason {
	case store.LaunchEndReasonSucceeded:
		// Any report -> completed, no row change.
		return store.LaunchReportAnswer{Result: store.LaunchReportResultCompleted}, current, false, nil

	case store.LaunchEndReasonRunningObserved:
		if r.State == store.LaunchReportStateSucceeded {
			upd := ltx.client.Agent.UpdateOneID(current.ID).
				SetLaunchEndReason(store.LaunchEndReasonSucceeded).
				SetStateVersion(current.StateVersion + 1)
			applySucceededAgentInfo(upd, current, r)
			updated, err := upd.Save(ctx)
			if err != nil {
				return store.LaunchReportAnswer{}, nil, false, mapError(err)
			}
			return store.LaunchReportAnswer{Result: store.LaunchReportResultCompleted, Changed: true}, updated, true, nil
		}
		// Any other report, including failed -> completed, no row change.
		return store.LaunchReportAnswer{Result: store.LaunchReportResultCompleted}, current, false, nil

	default:
		// failed, timed_out, lost, not_launched, superseded (as this row's
		// stored end reason).
		//
		// Refine: a failed report when the stored reason is timed_out/lost
		// and the phase is error (the reaper got there first) — the Hub has
		// just confirmed the agent never ran, so the broker must clean up.
		// For a start-kind launch the reaper can leave either phase: a
		// deadline or staleness reap writes its failure phase
		// (failureEndPhase, stopped for a restart or a stopped prior), and a
		// wind-down reap leaves an error row in error (ptone/scion#4669).
		// For a create launch both are error. A wind-down reap of a row the
		// user stopped also leaves stopped, with launch_error
		// launch_stopped; that is not the reaper's failure end, so it gets
		// 409 as a create launch (and the report before the reap) does.
		phase := state.Phase(current.Phase)
		reaperFailureEnd := phase == state.PhaseError ||
			(phase == failureEndPhase(current) && current.LaunchError != store.LaunchErrorLaunchStopped)
		if r.State == store.LaunchReportStateFailed &&
			(current.LaunchEndReason == store.LaunchEndReasonTimedOut || current.LaunchEndReason == store.LaunchEndReasonLost) &&
			reaperFailureEnd {
			upd := ltx.client.Agent.UpdateOneID(current.ID).
				SetStateVersion(current.StateVersion + 1)
			if r.ErrorCode != "" {
				// The cause qualifies this code, so it is written with
				// it; otherwise the reaper's code stands, with no cause.
				upd.SetLaunchError(r.ErrorCode).SetLaunchCause(r.Cause)
			}
			msg := formatFailureMessage(r.Step, r.Message)
			if msg != "" {
				upd.SetMessage(msg)
			}
			withFailedReportStep(upd, r)
			updated, err := upd.Save(ctx)
			if err != nil {
				return store.LaunchReportAnswer{}, nil, false, mapError(err)
			}
			return store.LaunchReportAnswer{Result: store.LaunchReportResultApplied, Changed: true}, updated, true, nil
		}
		return store.LaunchReportAnswer{HTTPStatus: http.StatusConflict, Code: store.LaunchReportCodeStaleLaunch, Reason: current.LaunchEndReason}, current, false, nil
	}
}

// applyLaunchReportActive handles design §3.7 step 6 (launch_state=active).
func (s *AgentStore) applyLaunchReportActive(ctx context.Context, ltx *launchTx, current *ent.Agent, r store.LaunchReport, ownerToSet string) (store.LaunchReportAnswer, *ent.Agent, error) {
	now, err := storeNow(ctx, ltx.tx, ltx.client.Driver().Dialect() == dialect.Postgres)
	if err != nil {
		return store.LaunchReportAnswer{}, nil, err
	}

	switch state.Phase(current.Phase) {
	case state.PhaseCreated, state.PhaseProvisioning, state.PhaseCloning, state.PhaseStarting:
		return s.applyLaunchReportPreRunning(ctx, ltx, current, r, ownerToSet, now)

	case state.PhaseRunning:
		upd := ltx.client.Agent.UpdateOneID(current.ID).
			SetLaunchLastReportAt(now).
			SetStateVersion(current.StateVersion + 1)
		if ownerToSet != "" {
			upd.SetLaunchOwner(ownerToSet)
		}
		if r.State == store.LaunchReportStateSucceeded {
			applySucceededAgentInfo(upd, current, r)
			upd.SetLaunchState(store.LaunchStateEnded).SetLaunchEndReason(store.LaunchEndReasonSucceeded)
			withStartEndClearsMemory(upd, current)
			updated, err := withLaunchEndSettlement(upd, current, store.LaunchEndReasonSucceeded, now).Save(ctx)
			if err != nil {
				return store.LaunchReportAnswer{}, nil, mapError(err)
			}
			return store.LaunchReportAnswer{Result: store.LaunchReportResultApplied, Changed: true}, updated, nil
		}
		// Non-terminal or failed: end the launch as running_observed. The
		// broker does not clean up on `completed` (§3.8.2).
		upd.SetLaunchState(store.LaunchStateEnded).SetLaunchEndReason(store.LaunchEndReasonRunningObserved)
		withStartEndClearsMemory(upd, current)
		updated, err := withLaunchEndSettlement(upd, current, store.LaunchEndReasonRunningObserved, now).Save(ctx)
		if err != nil {
			return store.LaunchReportAnswer{}, nil, mapError(err)
		}
		return store.LaunchReportAnswer{Result: store.LaunchReportResultCompleted, Changed: true}, updated, nil

	case state.PhaseStopped, state.PhaseStopping, state.PhaseSuspended, state.PhaseHalted:
		upd := ltx.client.Agent.UpdateOneID(current.ID).
			SetLaunchLastReportAt(now).
			SetLaunchState(store.LaunchStateEnded).
			SetLaunchEndReason(store.LaunchEndReasonFailed).
			SetLaunchError(store.LaunchErrorLaunchStopped).
			SetMessage("stopped during launch").
			SetStateVersion(current.StateVersion + 1)
		withStartEndClearsMemory(upd, current)
		if ownerToSet != "" {
			upd.SetLaunchOwner(ownerToSet)
		}
		updated, err := withLaunchEndSettlement(upd, current, store.LaunchEndReasonFailed, now).Save(ctx)
		if err != nil {
			return store.LaunchReportAnswer{}, nil, mapError(err)
		}
		return store.LaunchReportAnswer{HTTPStatus: http.StatusConflict, Code: store.LaunchReportCodeStaleLaunch, Reason: store.LaunchReportReasonStopped, Changed: true}, updated, nil

	case state.PhaseError:
		// Phase already moved to error by another writer (design §3.3: only
		// a phase=running write ends a launch automatically, so this row can
		// carry phase=error while its launch is still active).
		newLaunchError := current.LaunchError
		newLaunchCause := current.LaunchCause
		newMessage := current.Message
		if r.State == store.LaunchReportStateFailed {
			if r.ErrorCode != "" {
				newLaunchError = r.ErrorCode
				newLaunchCause = r.Cause
			} else if newLaunchError == "" {
				// Design §3.3: launch_error must not stay empty on this
				// launch-ending write. current.LaunchError may already be
				// non-empty (set by whoever moved phase to error); only
				// default it when it, too, is empty.
				newLaunchError = store.LaunchErrorAgentError
			}
			if msg := formatFailureMessage(r.Step, r.Message); msg != "" {
				newMessage = msg
			}
		} else if newLaunchError == "" {
			newLaunchError = store.LaunchErrorAgentError
		}
		upd := ltx.client.Agent.UpdateOneID(current.ID).
			SetLaunchLastReportAt(now).
			SetLaunchState(store.LaunchStateEnded).
			SetLaunchEndReason(store.LaunchEndReasonFailed).
			SetLaunchError(newLaunchError).
			SetLaunchCause(newLaunchCause).
			SetMessage(newMessage).
			SetStateVersion(current.StateVersion + 1)
		if r.State == store.LaunchReportStateFailed {
			withFailedReportStep(upd, r)
		}
		// A start-kind launch ends by its own failure rule even though
		// another writer moved the row to error first (ptone/scion#4669):
		// a restart ends stopped, a start restores a stopped prior or
		// remembers a suspended/created/provisioning/halted one.
		withStartFailureEnd(upd, current)
		if ownerToSet != "" {
			upd.SetLaunchOwner(ownerToSet)
		}
		updated, err := withLaunchEndSettlement(upd, current, store.LaunchEndReasonFailed, now).Save(ctx)
		if err != nil {
			return store.LaunchReportAnswer{}, nil, mapError(err)
		}
		if r.State == store.LaunchReportStateFailed {
			// The agent never ran: the broker must clean up.
			if err := releaseBrokerQuotaTx(ctx, ltx.client, current, now); err != nil {
				return store.LaunchReportAnswer{}, nil, err
			}
			return store.LaunchReportAnswer{Result: store.LaunchReportResultApplied, Changed: true}, updated, nil
		}
		return store.LaunchReportAnswer{HTTPStatus: http.StatusConflict, Code: store.LaunchReportCodeStaleLaunch, Reason: store.LaunchEndReasonFailed, Changed: true}, updated, nil

	default:
		// Exhaustive over state.Phase's 9 values; unreachable.
		return store.LaunchReportAnswer{}, nil, fmt.Errorf("launch store: unhandled phase %q", current.Phase)
	}
}

// applyLaunchReportPreRunning handles the normal-case phase bucket of design
// §3.7 step 6: created, provisioning, cloning, starting.
func (s *AgentStore) applyLaunchReportPreRunning(ctx context.Context, ltx *launchTx, current *ent.Agent, r store.LaunchReport, ownerToSet string, now time.Time) (store.LaunchReportAnswer, *ent.Agent, error) {
	switch r.State {
	case store.LaunchReportStateSucceeded:
		// Bypasses seq.
		upd := ltx.client.Agent.UpdateOneID(current.ID).
			SetPhase(string(state.PhaseRunning)).
			// The first running write: the exit fields describe the
			// previous run, as in UpdateAgentStatus (ptone/scion#5384).
			ClearExitCode().
			SetExitReason("").
			SetLaunchError("").
			SetLaunchCause("").
			SetLaunchState(store.LaunchStateEnded).
			SetLaunchEndReason(store.LaunchEndReasonSucceeded).
			SetLaunchLastReportAt(now).
			SetStateVersion(current.StateVersion + 1)
		applySucceededAgentInfo(upd, current, r)
		withStartEndClearsMemory(upd, current)
		if ownerToSet != "" {
			upd.SetLaunchOwner(ownerToSet)
		}
		updated, err := withLaunchEndSettlement(upd, current, store.LaunchEndReasonSucceeded, now).Save(ctx)
		if err != nil {
			return store.LaunchReportAnswer{}, nil, mapError(err)
		}
		return store.LaunchReportAnswer{Result: store.LaunchReportResultApplied, Changed: true}, updated, nil

	case store.LaunchReportStateFailed:
		// Bypasses seq.
		msg := formatFailureMessage(r.Step, r.Message)
		errorCode := r.ErrorCode
		if errorCode == "" {
			// Design §3.3: every create-launch end on an error/stopped phase
			// must leave launch_error non-empty (IsIncompleteCreate depends
			// on it). A broker's failed report always ends the launch here,
			// so an unclassified failure still needs a code.
			errorCode = store.LaunchErrorAgentError
		}
		upd := ltx.client.Agent.UpdateOneID(current.ID).
			SetPhase(string(state.PhaseError)).
			SetMessage(msg).
			SetLaunchError(errorCode).
			SetLaunchCause(r.Cause).
			SetLaunchState(store.LaunchStateEnded).
			SetLaunchEndReason(store.LaunchEndReasonFailed).
			SetLaunchLastReportAt(now).
			SetStateVersion(current.StateVersion + 1)
		withFailedReportStep(upd, r)
		// A start-kind launch ends in its failure phase (stopped for a
		// restart or a stopped prior) with its resume memory.
		withStartFailureEnd(upd, current)
		if ownerToSet != "" {
			upd.SetLaunchOwner(ownerToSet)
		}
		updated, err := withLaunchEndSettlement(upd, current, store.LaunchEndReasonFailed, now).Save(ctx)
		if err != nil {
			return store.LaunchReportAnswer{}, nil, mapError(err)
		}
		if err := releaseBrokerQuotaTx(ctx, ltx.client, current, now); err != nil {
			return store.LaunchReportAnswer{}, nil, err
		}
		return store.LaunchReportAnswer{Result: store.LaunchReportResultApplied, Changed: true}, updated, nil

	default:
		// claim / checkpoint / progress: ordered by seq.
		if r.Seq <= current.LaunchSeq {
			// Duplicate: still refresh last_report_at (keepalive), no bump,
			// no publish (the caller sees no material change).
			updated, err := ltx.client.Agent.UpdateOneID(current.ID).
				SetLaunchLastReportAt(now).
				Save(ctx)
			if err != nil {
				return store.LaunchReportAnswer{}, nil, mapError(err)
			}
			return store.LaunchReportAnswer{Result: store.LaunchReportResultDuplicate}, updated, nil
		}

		upd := ltx.client.Agent.UpdateOneID(current.ID).
			SetLaunchSeq(r.Seq).
			SetLaunchLastReportAt(now)
		// A report that changes none of step, phase or message is a
		// keepalive even though its seq advanced; Changed tracks that
		// so the P1a-ii caller knows not to publish or bump.
		changed := ownerToSet != ""
		if r.Step != "" && r.Step != current.LaunchStep {
			upd.SetLaunchStep(r.Step)
			changed = true
		}
		if r.Message != "" && r.Message != current.Message {
			upd.SetMessage(r.Message)
			changed = true
		}
		// Cap report-driven phase moves at the last pre-running phase. A
		// plain progress/claim/checkpoint report must never move phase to
		// "running" or later — only a `succeeded` report does that (above),
		// which also clears launch_error and ends the launch. Phases at or
		// beyond running are rejected by the < PhaseRunning.Ordinal() bound
		// below, so a broker sending phase="running" here by mistake cannot
		// bypass the running rule (§3.3 T3).
		if r.Phase != "" {
			newOrdinal := state.Phase(r.Phase).Ordinal()
			curOrdinal := state.Phase(current.Phase).Ordinal()
			if newOrdinal > curOrdinal && newOrdinal < state.PhaseRunning.Ordinal() {
				upd.SetPhase(r.Phase)
				changed = true
			}
		}
		if ownerToSet != "" {
			upd.SetLaunchOwner(ownerToSet)
		}
		updated, err := upd.Save(ctx)
		if err != nil {
			return store.LaunchReportAnswer{}, nil, mapError(err)
		}
		return store.LaunchReportAnswer{Result: store.LaunchReportResultApplied, Changed: changed}, updated, nil
	}
}

// applySucceededAgentInfo applies a succeeded broker report's agent info
// (design §3.7 steps 5/6: "apply Agent as applyBrokerResponse does" / "apply
// agent info without changing the phase"). The broker-resolved config goes
// through store.ApplyBrokerAgentConfig, the helper a synchronous dispatch
// uses, so both paths record the same template, harness config and auth,
// image, profile and runtime (ptone/scion#4818). current is the row read in
// the launch transaction. Its applied_config is read as entAgentToStore reads
// it (a sanitised config is used), and a row whose applied_config is empty
// (no AppliedConfig, as on the sync path) or not valid JSON keeps it
// untouched.
func applySucceededAgentInfo(upd *ent.AgentUpdateOne, current *ent.Agent, r store.LaunchReport) {
	a := store.Agent{
		Template:              current.Template,
		Runtime:               current.Runtime,
		PinnedRuntimeTargetID: derefString(current.PinnedRuntimeTargetID),
	}
	if current.AppliedConfig != "" {
		// A non-nil config with an error is the sanitised config, which
		// entAgentToStore also uses; nil means not valid JSON.
		a.AppliedConfig, _ = parseAppliedConfig(current.AppliedConfig)
	}
	store.ApplyBrokerAgentConfig(&a, r.AgentConfig)
	if a.Template != current.Template {
		upd.SetTemplate(a.Template)
	}
	if a.Runtime != current.Runtime {
		upd.SetRuntime(a.Runtime)
	}
	if a.AppliedConfig != nil {
		if raw := marshalAppliedConfig(a.AppliedConfig); raw != "" && raw != current.AppliedConfig {
			upd.SetAppliedConfig(raw)
			upd.SetHarnessConfig(harnessConfigOf(a.AppliedConfig))
		}
	}
	if r.RuntimeState != "" {
		upd.SetRuntimeState(r.RuntimeState)
	}
}

// withFailedReportStep adds to u, a write that applies a failed report, the
// report's step as the launch's step, so clients can show where the launch
// failed (design wave 3 E-d, ptone/scion#4670). A report without a step
// leaves the last recorded step, the furthest one the launch reached.
func withFailedReportStep(u *ent.AgentUpdateOne, r store.LaunchReport) *ent.AgentUpdateOne {
	if r.Step == "" {
		return u
	}
	return u.SetLaunchStep(r.Step)
}

// formatFailureMessage builds the "<step>: <message>" form design §3.7 uses
// for a failed report's stored message.
func formatFailureMessage(step, message string) string {
	switch {
	case step == "":
		return message
	case message == "":
		return step
	default:
		return step + ": " + message
	}
}
