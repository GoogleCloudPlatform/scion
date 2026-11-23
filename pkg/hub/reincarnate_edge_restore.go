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

// mutationTypeAgentReincarnateEdgeRestore is the audit record of a failed
// reincarnation's delegation edge restore.
const mutationTypeAgentReincarnateEdgeRestore = "agent_reincarnate_edge_restore"

// restoreReincarnationDelegationEdge returns the agent's delegation edge to
// the edge it held before the failed reincarnation reincarnationID. It runs
// in the transaction of the agent write that restores the previous
// configuration (writeFailedAgent), so both commit or neither does. agent is
// the row as written in that transaction.
//
// A reincarnation claim that changed the role left a restorable replacement:
// the active edge's ID is the reincarnation ID, and any replaced edge is
// inactive with cause reincarnate_replaced under the reincarnation ID
// (reincarnateClaimTx). Here:
//
//   - When the replacement is the agent's one active edge, it is deactivated
//     by edge ID with cause under the reincarnation ID and any replaced edge
//     is reactivated, with its own delegator, role, provenance and frozen
//     ceiling. When the agent had no active edge before the claim, none is
//     active afterwards, matching the restored configuration's history.
//   - When the agent is soft-deleted and its soft delete deactivated the
//     replacement, nothing becomes active: the replacement is re-marked with
//     cause under the reincarnation ID and any replaced edge is re-marked as
//     deactivated by the soft delete, so a later restore of the agent returns
//     the replaced edge.
//   - Otherwise (no restorable replacement, such as a same-role repair or
//     owner reclaim, or a later write replaced or ended it) the edges are
//     left as they are: a restore never acts against a later write.
//
// Each restore writes an agent_reincarnate_edge_restore audit record in the
// same transaction. Errors name the step only; the caller adds the
// "delegation edge restore" context.
func restoreReincarnationDelegationEdge(ctx context.Context, tx store.Store, agent *store.Agent, reincarnationID string, cause store.EdgeDeactivationCause, now time.Time) error {
	if agent == nil || reincarnationID == "" {
		return nil
	}
	edges, err := tx.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID)
	if err != nil {
		return fmt.Errorf("read active delegation edges: %w", err)
	}
	failed := store.Deactivation{Cause: cause, At: &now, OpID: reincarnationID}
	var deactivated, reactivated, remarked int
	switch {
	case len(edges) == 1 && edges[0].ID == reincarnationID:
		if deactivated, err = tx.DeactivateDelegationEdge(ctx, store.DelegationPrincipalAgent, agent.ID, reincarnationID, failed); err != nil {
			return fmt.Errorf("deactivate replacement: %w", err)
		}
		if deactivated != 1 {
			// A write between the read above and this one ended the
			// replacement. A version conflict re-runs the failure write,
			// whose fresh read then sees that write.
			return fmt.Errorf("%w: the replacement edge changed", store.ErrVersionConflict)
		}
		reactivated, err = tx.ReactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID, store.EdgeDeactivationReincarnateReplaced, reincarnationID)
		if err != nil {
			return fmt.Errorf("reactivate replaced edge: %w", err)
		}
	case len(edges) == 0 && !agent.DeletedAt.IsZero() && agent.SoftDeleteOpID != "":
		n, err := tx.RemarkDeactivatedDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID, reincarnationID,
			store.EdgeDeactivationAgentSoftDelete, agent.SoftDeleteOpID, failed)
		if err != nil {
			return fmt.Errorf("re-mark replacement: %w", err)
		}
		if n == 0 {
			return nil
		}
		remarked = n
		m, err := tx.RemarkDeactivatedDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID, "",
			store.EdgeDeactivationReincarnateReplaced, reincarnationID,
			store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, At: &now, OpID: agent.SoftDeleteOpID})
		if err != nil {
			return fmt.Errorf("re-mark replaced edge: %w", err)
		}
		remarked += m
	default:
		return nil
	}
	record, err := lifecycleAudit(mutationTypeAgentReincarnateEdgeRestore, agent.ID, AuditActor{}, now, struct {
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
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}
