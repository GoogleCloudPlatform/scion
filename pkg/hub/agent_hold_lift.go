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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// agentHoldLiftClearReason is recorded on holds cleared by the hub-admin lift.
const agentHoldLiftClearReason = "admin_lift_owner_readmitted"

// errHoldLiftRootNotAdmitted refuses a lift while some hold's root user is not
// active and admitted to the agent's project.
var errHoldLiftRootNotAdmitted = errors.New("hold root not admitted")

// AgentHoldLiftResponse is the response of POST /api/v1/agents/{id}/hold/lift.
type AgentHoldLiftResponse struct {
	AgentID string `json:"agentId"`
	Cleared int    `json:"cleared"`
}

// handleAgentHoldLift serves POST /api/v1/agents/{id}/hold/lift
// (ptone/scion#3433): a hub admin, with a session credential, clears the
// holds of an agent, but only when every active hold's root user is active
// and admitted to the agent's project again, so the agent's authority still
// comes from a current member. It does not start the agent. Agent
// credentials are refused before anything is read.
func (s *Server) handleAgentHoldLift(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	if GetAgentIdentityFromContext(r.Context()) != nil {
		Forbidden(w)
		return
	}
	user, ok := s.requireAdminFor(w, r, authzop.ReasonGovernancePending)
	if !ok {
		return
	}
	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "Agent")
		return
	}

	cleared := 0
	var before []map[string]string
	txErr := s.store.WithTx(ctx, func(tx store.Store) error {
		cleared, before = 0, nil
		// Lock order: project, then the agent row, then dependents.
		if err := tx.LockProjectForMembership(ctx, agent.ProjectID); err != nil {
			return fmt.Errorf("lock project: %w", err)
		}
		if err := tx.LockAgentRows(ctx, []string{agent.ID}); err != nil {
			return fmt.Errorf("lock agent: %w", err)
		}
		holds, err := tx.ListActiveAgentHolds(ctx, agent.ID)
		if err != nil {
			return fmt.Errorf("list holds: %w", err)
		}
		if len(holds) == 0 {
			return nil
		}
		for _, h := range holds {
			admitted, err := s.userAdmittedByIDOn(ctx, tx, h.RootPrincipalID, agent.ProjectID)
			if err != nil {
				return fmt.Errorf("admission check: %w", err)
			}
			if !admitted {
				return errHoldLiftRootNotAdmitted
			}
			u, err := tx.GetUser(ctx, h.RootPrincipalID)
			if err != nil {
				return fmt.Errorf("root user lookup: %w", err)
			}
			if u.Status != store.UserStatusActive {
				return errHoldLiftRootNotAdmitted
			}
			before = append(before, map[string]string{
				"root":  h.RootPrincipalType + ":" + h.RootPrincipalID,
				"since": h.CreatedAt.UTC().Format(time.RFC3339),
			})
		}
		n, err := tx.ClearAgentHolds(ctx, agent.ID, store.ClearActor{Kind: store.ClearActorUser, ID: user.ID()}, agentHoldLiftClearReason)
		if err != nil {
			return fmt.Errorf("clear holds: %w", err)
		}
		cleared = n
		beforeJSON, _ := json.Marshal(map[string]interface{}{"holds": before})
		afterJSON, _ := json.Marshal(map[string]string{"clear_reason": agentHoldLiftClearReason})
		record := &store.MutationAuditRecord{
			MutationType:  mutationTypeAgentHoldCleared,
			TargetType:    "agent",
			TargetID:      agent.ID,
			BeforeSummary: string(beforeJSON),
			AfterSummary:  string(afterJSON),
			Timestamp:     time.Now(),
		}
		auditActorFromContext(ctx).ApplyActor(record)
		return tx.CreateMutationAudit(ctx, record)
	})
	if txErr != nil {
		if errors.Is(txErr, errHoldLiftRootNotAdmitted) {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"The agent's holds can be lifted only when its owners are active members of the project again.", nil)
			return
		}
		writeErrorFromErr(w, txErr, "")
		return
	}
	writeJSON(w, http.StatusOK, AgentHoldLiftResponse{AgentID: agent.ID, Cleared: cleared})
}
