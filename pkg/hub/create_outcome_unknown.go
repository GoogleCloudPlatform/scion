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
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mutationTypeAgentCreateOutcomeUnknown records a create (or env finalize)
// whose synchronous dispatch ended with an unknown outcome, so the agent
// was kept rather than rolled back.
const mutationTypeAgentCreateOutcomeUnknown = "agent_create_outcome_unknown"

// startOutcomeUnknownMessage is the agent's status message, and the
// answer's message, for a start whose dispatch outcome is unknown. It names
// what was not answered.
func startOutcomeUnknownMessage(cause error) string {
	var what string
	switch {
	case errors.Is(cause, errBrokerConnectionLost):
		what = "the connection to the runtime broker closed before it answered"
	case errors.Is(cause, ErrDispatchFailed):
		what = "the hub node connected to the runtime broker did not report the outcome in time"
	case errors.Is(cause, context.DeadlineExceeded):
		what = fmt.Sprintf("the runtime broker did not answer within %s", syncDispatchTimeout)
	default:
		what = "the runtime broker did not answer in time"
	}
	return "start outcome unknown: " + what + "; the agent may still be starting"
}

// keepUnknownOutcomeStart handles a create or env finalize whose dispatch
// ended with an unknown outcome (isUnknownDispatchOutcome): the broker may
// still be starting the agent (ptone/scion#4817). Instead of rolling the
// create back (which deletes the runtime and the row while the start may
// be running), it keeps the row, its delegation edge, its quotas and its
// credential (the dispatch ran under withKeepCredentialOnUnknownOutcome).
// The start claim is already unconfirmed (withStartClaim; the classifier
// is a subset of startOutcomeOf's unconfirmed), so the start-claim reaper
// settles the agent from the broker's inventory: it releases the claim
// once the agent reports running, and once a fresh inventory shows nothing
// running it revokes the credential and releases the claim. A user can
// delete the agent at any time; the delete supersedes the claim.
//
// When a delete has removed or holds the row, it answers as a create that
// lost to a delete and writes nothing. Otherwise it records the status
// message (only while the agent has not moved on, for example to running)
// and an audit record, publishes the created event when publishCreated
// (a create), and answers 504 start_outcome_unknown with details.agentId
// and details.phase.
func (s *Server) keepUnknownOutcomeStart(ctx context.Context, w http.ResponseWriter, agent *store.Agent, stage string, cause error, createAuditID string, publishCreated bool, warnings []string) {
	phase := agent.Phase
	fresh, err := s.store.GetAgent(ctx, agent.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeDeletedDuringCreate(w, agent.ID, warnings)
		return
	case err == nil && deletedOrDeleteHeld(fresh):
		writeDeletedDuringCreate(w, agent.ID, warnings)
		return
	case err == nil:
		phase = fresh.Phase
	default:
		// The agent is still kept; the answer uses the dispatched copy's
		// phase.
		s.agentLifecycleLog.Warn("Failed to re-read the agent after an unknown start outcome", "agent_id", agent.ID, "error", err)
	}

	msg := startOutcomeUnknownMessage(cause)
	s.agentLifecycleLog.Warn("Hub: start outcome unknown; keeping the agent for the start-claim reaper",
		"agent_id", agent.ID, "agent", agent.Name, "broker", agent.RuntimeBrokerID, "stage", stage, "error", cause)
	// IfPhase: an agent that already reported running (or anything else)
	// keeps its own message.
	if err := s.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{Message: msg, IfPhase: phase}); err != nil && !errors.Is(err, store.ErrPhaseMismatch) {
		s.agentLifecycleLog.Warn("Failed to record the unknown start outcome on the agent", "agent_id", agent.ID, "error", err)
	}

	var causeText string
	if cause != nil {
		causeText = cause.Error()
	}
	summary, _ := json.Marshal(struct {
		OriginalAuditID string `json:"original_audit_id,omitempty"`
		Stage           string `json:"stage"`
		Error           string `json:"error,omitempty"`
	}{OriginalAuditID: createAuditID, Stage: stage, Error: truncateAuditText(causeText, 512)})
	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType: mutationTypeAgentCreateOutcomeUnknown,
		TargetType:   "agent",
		TargetID:     agent.ID,
		AfterSummary: string(summary),
	})

	if publishCreated && !s.publishAgentCreatedIfLive(ctx, agent) {
		writeDeletedDuringCreate(w, agent.ID, warnings)
		return
	}
	details := map[string]interface{}{"agentId": agent.ID, "phase": phase}
	if len(warnings) > 0 {
		details["warnings"] = warnings
	}
	writeError(w, http.StatusGatewayTimeout, ErrCodeStartOutcomeUnknown, msg, details)
}
