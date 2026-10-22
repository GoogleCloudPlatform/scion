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
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mutationTypeAgentReincarnateAssignmentRestore is the audit record of a
// failed reincarnation's service-account assignment restore.
const mutationTypeAgentReincarnateAssignmentRestore = "agent_reincarnate_assignment_restore"

// restoreReincarnationAssignment returns the agent's service-account
// assignment to the row it held before the failed reincarnation
// reincarnationID. It runs in the transaction of the agent write that
// restores the previous configuration (writeFailedAgent), so both commit or
// neither does. agent is the row as written in that transaction.
//
// A reincarnation claim that changed the account left a restorable
// replacement: the active row's ID is the reincarnation ID, and the replaced
// row is inactive with cause reincarnate_replaced under the reincarnation ID
// (reincarnateClaimTx). Here:
//
//   - When the replacement is the agent's one active row, it is deactivated
//     with cause under the reincarnation ID and the replaced row is
//     reactivated, with its own source and frozen ceiling. When the agent had
//     no assignment before the claim, none is active afterwards, which
//     matches the restored configuration.
//   - When the agent is soft-deleted and its soft delete deactivated the
//     replacement, nothing becomes active: the replacement is re-marked with
//     cause under the reincarnation ID and the replaced row is re-marked as
//     deactivated by the soft delete, so a later restore of the agent returns
//     the replaced row.
//   - Otherwise (no restorable replacement, or a later write replaced or
//     ended it) the rows are left as they are: a restore never acts against
//     a later write. The restored configuration may then name another account
//     than the active row, and that account is withheld until assigned again.
//
// Each restore writes an agent_reincarnate_assignment_restore audit record in
// the same transaction.
func restoreReincarnationAssignment(ctx context.Context, tx store.Store, agent *store.Agent, reincarnationID string, cause store.EdgeDeactivationCause, now time.Time) error {
	if agent == nil || reincarnationID == "" {
		return nil
	}
	rows, err := tx.GetActiveAgentServiceAccountAssignments(ctx, agent.ID)
	if err != nil {
		return fmt.Errorf("reincarnation assignment restore: read active assignment: %w", err)
	}
	failed := store.Deactivation{Cause: cause, At: &now, OpID: reincarnationID}
	var deactivated, reactivated, remarked int
	switch {
	case len(rows) == 1 && rows[0].ID == reincarnationID:
		if deactivated, err = tx.DeactivateAgentServiceAccountAssignments(ctx, agent.ID, failed); err != nil {
			return fmt.Errorf("reincarnation assignment restore: deactivate replacement: %w", err)
		}
		reactivated, err = tx.ReactivateAgentServiceAccountAssignments(ctx, agent.ID, store.EdgeDeactivationReincarnateReplaced, reincarnationID)
		if err != nil {
			return fmt.Errorf("reincarnation assignment restore: reactivate replaced assignment: %w", err)
		}
	case len(rows) == 0 && !agent.DeletedAt.IsZero() && agent.SoftDeleteOpID != "":
		n, err := tx.RemarkDeactivatedAgentServiceAccountAssignments(ctx, agent.ID, reincarnationID,
			store.EdgeDeactivationAgentSoftDelete, agent.SoftDeleteOpID, failed)
		if err != nil {
			return fmt.Errorf("reincarnation assignment restore: re-mark replacement: %w", err)
		}
		if n == 0 {
			return nil
		}
		remarked = n
		m, err := tx.RemarkDeactivatedAgentServiceAccountAssignments(ctx, agent.ID, "",
			store.EdgeDeactivationReincarnateReplaced, reincarnationID,
			store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, At: &now, OpID: agent.SoftDeleteOpID})
		if err != nil {
			return fmt.Errorf("reincarnation assignment restore: re-mark replaced assignment: %w", err)
		}
		remarked += m
	default:
		return nil
	}
	record, err := lifecycleAudit(mutationTypeAgentReincarnateAssignmentRestore, agent.ID, AuditActor{}, now, struct {
		ReincarnationID string `json:"reincarnation_id"`
		Cause           string `json:"cause"`
		Deactivated     int    `json:"deactivated"`
		Reactivated     int    `json:"reactivated"`
		Remarked        int    `json:"remarked,omitempty"`
	}{reincarnationID, string(cause), deactivated, reactivated, remarked})
	if err != nil {
		return err
	}
	if err := tx.CreateMutationAudit(ctx, record); err != nil {
		return fmt.Errorf("reincarnation assignment restore audit: %w", err)
	}
	return nil
}
