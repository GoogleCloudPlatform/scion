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
	"errors"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reincarnateRepairAssignmentNotRecordedWarning starts the plan warning of a
// same-role repair whose requester did not pass the assignment gate for the
// agent's account.
const reincarnateRepairAssignmentNotRecordedWarning = "the service-account assignment was not re-recorded from you; the agent keeps its existing assignment"

// resolveRepairAssignment re-records the agent's service-account assignment
// from the requester on a same-role repair (ptone/scion#3948): a user's
// reincarnate that keeps the role and re-records the delegation edge
// (auth non-nil, roleChanged false). It applies only when the agent has an
// assign-mode GCP identity and the request does not patch the account; a
// --service-account patch records its own assignment.
//
// The requester must pass the normal assignment gate (evaluateSAAssignment)
// for the agent's current account. Then auth.Assignment is the new row, with
// the requester's frozen ceiling and provenance (the pair recorded on the new
// edge), and the claim transaction replaces the active row with it. A denial
// is handled by repairAssignmentDenied. A store fault looking up the account
// answers 503. ok is false when the response has been written; nothing has
// been written to the store.
func (s *Server) resolveRepairAssignment(w http.ResponseWriter, r *http.Request, agent *store.Agent, auth *reincarnateAuthority, roleChanged bool, patch *reincarnatePatch) (ok bool) {
	if auth == nil || roleChanged || (patch != nil && patch.GCPIdentity != nil) {
		return true
	}
	saID := agentAssignedServiceAccountID(agent)
	if saID == "" {
		return true
	}
	ctx := r.Context()
	// The stored ID, not a user-typed reference: an exact lookup. The gate
	// checks that the account is reachable from the project and verified.
	sa, err := s.store.GetGCPServiceAccount(ctx, saID)
	var denial *saAssignDenial
	switch {
	case errors.Is(err, store.ErrNotFound):
		denial = &saAssignDenial{kind: saAssignDenyValidation, msg: msgSANotAvailableInProject}
	case err != nil:
		s.agentLifecycleLog.Error("handleReincarnateAgent: service account lookup failed",
			"agent_id", agent.ID, "service_account_id", saID, "error", err)
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"Unable to look up the agent's service account; retry later", nil)
		return false
	default:
		denial = s.evaluateSAAssignment(ctx, r, sa, agent.ProjectID, SurfaceAgentReincarnate)
	}
	if denial != nil {
		return repairAssignmentDenied(w, auth, denial)
	}
	auth.Assignment = newSAAssignment(agent, store.SAAssignmentOriginReincarnate, auth.Ceiling, auth.Provenance)
	return true
}

// repairAssignmentDenied handles a requester that does not pass the
// assignment gate on a same-role repair: the reincarnate proceeds with the
// edge repair, the active assignment is left as is (its source and frozen
// ceiling unchanged, so nothing is widened), and the plan carries a warning.
// Refusing the reincarnate instead would be denial.write(w) and return false.
func repairAssignmentDenied(_ http.ResponseWriter, auth *reincarnateAuthority, denial *saAssignDenial) (ok bool) {
	auth.AssignmentNotRecorded = reincarnateRepairAssignmentNotRecordedWarning + ": " + denial.Error()
	return true
}

// addAuthorityToPlan adds the authority re-record's plan warning, if any.
func addAuthorityToPlan(plan *ReincarnationPlan, auth *reincarnateAuthority) {
	if auth != nil && auth.AssignmentNotRecorded != "" {
		plan.Warnings = append(plan.Warnings, auth.AssignmentNotRecorded)
	}
}
