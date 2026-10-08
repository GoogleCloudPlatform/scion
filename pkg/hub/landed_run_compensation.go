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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"go.opentelemetry.io/otel/attribute"
)

// compensatingDeleteOp is the dispatch metric op attribute for the
// compensating delete of a run that landed after its agent was deleted.
const compensatingDeleteOp = "compensating_delete"

// compensatingDeleteTimeout bounds the compensating broker delete, which
// runs inside the create, start or restart request.
const compensatingDeleteTimeout = 30 * time.Second

// settleLandedRun runs after the broker answered a synchronous create,
// start or restart with the entry it started (resp). It records the
// broker's run ID (adoptBrokerRunID), then removes that entry again if the
// agent was deleted while the broker call was in flight
// (compensateLandedRun). recorded is beginRun's: an agent whose row never
// recorded the run has no row a delete could have removed, so it is not
// checked.
func (d *HTTPAgentDispatcher) settleLandedRun(ctx context.Context, agent *store.Agent, minted string, recorded bool, resp *RemoteAgentResponse) {
	d.adoptBrokerRunID(ctx, agent, minted, resp)
	if recorded {
		d.compensateLandedRun(ctx, agent, resp)
	}
}

// compensateLandedRun removes the runtime entry a synchronous dispatch just
// started when the agent was deleted, or a delete holds it, while the broker
// call was in flight (ptone/scion#3055).
//
// A delete that claims the row after beginRun recorded the run can send its
// broker delete before the broker registers the start: the broker has no
// entry yet, answers 404 (an idempotent success), and the delete finishes.
// The start then lands and its container runs with no hub row tracking it;
// heartbeats skip slugs with no row. So once the broker answers, the row is
// re-read and, when it is gone or deletedOrDeleteHeld,
// the run this dispatch started is deleted again. If the engine's own
// broker delete is still to come, the second delete is a harmless 404.
//
// The delete is scoped to the run the broker reports it labelled
// (resp.Agent.RunID), so it can never remove a same-name successor, which
// carries its own run. A broker that reports no run ID (an older broker
// that may not label entries) gets no compensating delete: a delete by name
// alone could hit a successor. Files are left alone for the same reason
// (they are addressed by name); leftover files are for the broker sweep
// (ptone/scion#3068). The re-read and the delete are best-effort and
// detached from the request's cancellation: the delete publishes nothing,
// a failure (including a failed re-read) is logged at warn and counted
// (scion.dispatch.failed, op=compensating_delete; a success counts as
// scion.dispatch.done), and the caller's response carries a warning
// either way.
//
// Asynchronous launches do not need this: the launch reports back to the
// hub, which answers a deleted or delete-held agent with a stop or an
// unknown launch, and the broker then cleans up (runtimebroker run_launch.go
// classifyGateAnswer, gateAbortCleanup).
func (d *HTTPAgentDispatcher) compensateLandedRun(ctx context.Context, agent *store.Agent, resp *RemoteAgentResponse) {
	if d.store == nil || agent == nil || agent.ID == "" || resp == nil || resp.Agent == nil {
		return
	}
	// The re-read and the delete run detached from the request: a caller
	// that goes away right after the broker answered must not skip the
	// check (the container is running either way).
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensatingDeleteTimeout)
	defer cancel()
	fresh, err := d.store.GetAgent(delCtx, agent.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		d.log.Warn("Dispatcher: failed to re-read agent after the broker started it; cannot check for a delete",
			"agent_id", agent.ID, "agent", agent.Slug, "broker_id", agent.RuntimeBrokerID, "error", err)
		d.recordCompensation(ctx, false)
		addDispatchWarnings(ctx, "could not check whether the agent was deleted while it was starting: "+err.Error())
		return
	case !deletedOrDeleteHeld(fresh):
		return
	}

	runID := resp.Agent.RunID
	if runID == "" {
		d.log.Warn("Dispatcher: agent was deleted while the broker started it, and the broker reported no run ID; leaving its entry",
			"agent_id", agent.ID, "agent", agent.Slug, "broker_id", agent.RuntimeBrokerID)
		d.recordCompensation(ctx, false)
		addDispatchWarnings(ctx, "agent was deleted while it was starting; its container could not be removed safely (the broker reported no run ID)")
		return
	}

	target := *agent
	target.RunID = runID
	// Only the run that landed: its start replaced any previous entry
	// (ptone/scion#3097). This must stay. The row is gone or held by a
	// delete, so a same-name successor may already exist, and a run-scoped
	// delete of a run with no entry is not side-effect-free on the broker:
	// with no entry of any run it falls through to the broker's file-only
	// path, which acts by name (agent files, leftover runtime objects) and
	// could hit the successor's. The struct's list is still set here when
	// the settle swap missed the gone row.
	target.PreviousRunIDs = nil
	d.log.Info("Dispatcher: agent was deleted while the broker started it; deleting the run it started",
		"agent_id", agent.ID, "agent", agent.Slug, "broker_id", agent.RuntimeBrokerID, "run_id", runID)
	err = d.DispatchAgentDelete(delCtx, &target, false, false, false, time.Time{})
	if errors.Is(err, ErrDeleteRunMismatch) {
		// The landed run's entry is already gone and another run holds the
		// name (a same-name successor): nothing of this run is left to
		// remove, and the successor is left alone (ptone/scion#3080).
		d.log.Info("Dispatcher: the run started for a deleted agent is already gone; another run holds the name",
			"agent_id", agent.ID, "agent", agent.Slug, "broker_id", agent.RuntimeBrokerID, "run_id", runID, "error", err)
		d.recordCompensation(ctx, true)
		addDispatchWarnings(ctx, "agent was deleted while it was starting; its container was already replaced by another run")
		return
	}
	if err != nil {
		d.log.Warn("Dispatcher: compensating delete of a run started for a deleted agent failed",
			"agent_id", agent.ID, "agent", agent.Slug, "broker_id", agent.RuntimeBrokerID, "run_id", runID, "error", err)
		d.recordCompensation(ctx, false)
		addDispatchWarnings(ctx, "agent was deleted while it was starting; removing its container failed: "+err.Error())
		return
	}
	d.recordCompensation(ctx, true)
	addDispatchWarnings(ctx, "agent was deleted while it was starting; its container was removed")
}

func (d *HTTPAgentDispatcher) recordCompensation(ctx context.Context, ok bool) {
	rec := d.dispatchMetrics
	if rec == nil {
		return
	}
	if ok {
		rec.IncDone(ctx, 1, attribute.String("op", compensatingDeleteOp))
		return
	}
	rec.IncFailed(ctx, 1, attribute.String("op", compensatingDeleteOp))
}
