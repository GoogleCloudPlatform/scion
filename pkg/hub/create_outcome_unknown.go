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
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mutationTypeAgentCreateOutcomeUnknown records a create (or env finalize)
// whose synchronous dispatch ended with an unknown outcome, so the agent
// was kept rather than rolled back.
const mutationTypeAgentCreateOutcomeUnknown = "agent_create_outcome_unknown"

// startOutcomeUnknownMessage is the agent's status message, and the
// answer's message, for a start whose dispatch outcome is unknown.
func startOutcomeUnknownMessage() string {
	return fmt.Sprintf("start outcome unknown: the runtime broker did not answer within %s; the agent may still be starting", syncDispatchTimeout)
}

// keepUnknownOutcomeStart handles a create or env finalize whose dispatch
// ended with an unknown outcome (isUnknownDispatchOutcome): the broker may
// still be starting the agent (ptone/scion#4817). Instead of rolling the
// create back (which deletes the runtime and the row while the start may
// be running), it keeps the row, its delegation edge, its quotas and its
// credential. The start claim is already unconfirmed (withStartClaim), so
// the start-claim reaper settles the agent from the broker's inventory: it
// releases the claim once the agent reports running, and once a fresh
// inventory shows nothing running it reverts the run intent and revokes
// the credential. A user can delete the agent at any time; the delete
// supersedes the claim.
//
// It records the status message and an audit record, publishes the
// created event when stage is a create, and answers 504
// start_outcome_unknown with details.agentId and details.phase. When a
// delete has removed or holds the row, it answers as a create that lost
// to a delete.
func (s *Server) keepUnknownOutcomeStart(ctx context.Context, w http.ResponseWriter, agent *store.Agent, stage string, cause error, createAuditID string, publishCreated bool, warnings []string) {
	msg := startOutcomeUnknownMessage()
	s.agentLifecycleLog.Warn("Hub: start outcome unknown; keeping the agent for the start-claim reaper",
		"agent_id", agent.ID, "agent", agent.Name, "broker", agent.RuntimeBrokerID, "stage", stage, "error", cause)
	if err := s.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{Message: msg}); err != nil {
		s.agentLifecycleLog.Warn("Failed to record the unknown start outcome on the agent", "agent_id", agent.ID, "error", err)
	}

	summary, _ := json.Marshal(struct {
		OriginalAuditID string `json:"original_audit_id,omitempty"`
		Stage           string `json:"stage"`
		Error           string `json:"error,omitempty"`
	}{OriginalAuditID: createAuditID, Stage: stage, Error: truncateAuditText(cause.Error(), 512)})
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
	phase := agent.Phase
	if fresh, err := s.store.GetAgent(ctx, agent.ID); err == nil {
		if deletedOrDeleteHeld(fresh) {
			writeDeletedDuringCreate(w, agent.ID, warnings)
			return
		}
		phase = fresh.Phase
	}
	details := map[string]interface{}{"agentId": agent.ID, "phase": phase}
	if len(warnings) > 0 {
		details["warnings"] = warnings
	}
	writeError(w, http.StatusGatewayTimeout, ErrCodeStartOutcomeUnknown, msg, details)
}
