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
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Agent delegation grant lifecycle (.design/agent-delegation.md §16).
//
// The four agent lifecycle transactions (soft delete, hard delete, restore
// and the reincarnation claim) revoke the agent's grants through one
// AgentTxHook each, so the revocation commits or rolls back with the
// lifecycle change (§16.1). Agent suspend, issuer suspend and issuer delete
// revoke best effort after their own change has committed, because the
// use-time and exchange checks already deny in those states (§16.3). The
// project delete revokes the grants of the project's agents inside its
// cascade transaction.

// Revoke reasons of the lifecycle revocations.
const (
	revokeReasonAgentDeleted      = "agent_deleted"
	revokeReasonAgentRestored     = "agent_restored"
	revokeReasonAgentReincarnated = "agent_reincarnated"
	revokeReasonAgentSuspended    = "agent_suspended"
	revokeReasonIssuerSuspended   = "issuer_suspended"
	revokeReasonIssuerDeleted     = "issuer_deleted"
	revokeReasonProjectDeleted    = "project_deleted"
)

// agentDelegationLifecycleHookName names G's hook in each of the four
// lifecycle hook registries.
const agentDelegationLifecycleHookName = "agent-delegation-grant-revoke"

// registerAgentDelegationLifecycleHooks registers the grant revocation hook
// in the four lifecycle registries, once per server.
func (s *Server) registerAgentDelegationLifecycleHooks() {
	s.RegisterSoftDeleteHook(agentDelegationLifecycleHookName, agentDelegationRevokeHook(revokeReasonAgentDeleted))
	s.RegisterHardDeleteHook(agentDelegationLifecycleHookName, agentDelegationRevokeHook(revokeReasonAgentDeleted))
	s.RegisterRestoreHook(agentDelegationLifecycleHookName, agentDelegationRevokeHook(revokeReasonAgentRestored))
	s.RegisterReincarnateClaimHook(agentDelegationLifecycleHookName, agentDelegationRevokeHook(revokeReasonAgentReincarnated))
}

// agentDelegationRevokeHook returns the lifecycle hook that revokes every
// unrevoked grant bound to the agent, and the credentials exchanged from
// them, with reason, on the transaction-scoped store. Any error, the audit
// write included, rolls the lifecycle operation back. For a hard delete
// the agent is the row as it was before the delete; grant rows carry no
// foreign key to the agent, so they are still there to revoke.
func agentDelegationRevokeHook(reason string) AgentTxHook {
	return func(ctx context.Context, tx store.Store, agent *store.Agent, actor AuditActor) error {
		if agent == nil || agent.ID == "" {
			return fmt.Errorf("%w: agent delegation revoke hook without an agent", store.ErrInvalidInput)
		}
		if _, err := revokeAgentDelegationGrantsTx(ctx, tx, store.AgentDelegationGrantFilter{AgentID: agent.ID}, reason, actor, time.Now().UTC()); err != nil {
			return fmt.Errorf("revoke agent delegation grants: %w", err)
		}
		return nil
	}
}

// revokeAgentDelegationGrantTx revokes, on tx, the grant with grantID and
// every unrevoked credential exchanged from it, expired or not, with
// reason. It is the one revocation body the revoke route and the lifecycle
// revocations share. The grant is locked first, so the revocation
// serializes with a concurrent exchange. It writes one
// agent_delegation_grant_revoke mutation audit that lists the credentials
// revoked with the grant; block fills the G block from the locked grant,
// actor fills the principal (the hub when actor names none), and the
// record's ID is stored as the grant's revocation_audit_id. A grant that is
// already revoked is left as it is and the result is nil. An audit write
// failure wraps errAgentDelegationAuditFailed. Any error leaves the caller
// to roll back.
func revokeAgentDelegationGrantTx(ctx context.Context, tx store.Store, grantID, revokedBy, reason string, actor AuditActor, now time.Time, block func(record *store.MutationAuditRecord, grant *store.AgentDelegationGrant)) (*store.AgentDelegationGrant, error) {
	locked, err := tx.LockAgentDelegationGrant(ctx, grantID)
	if err != nil {
		return nil, fmt.Errorf("lock grant %s: %w", grantID, err)
	}
	if locked.RevokedAt != nil {
		return nil, nil
	}
	creds, err := tx.ListActiveAgentDelegatedCredentials(ctx, []string{grantID}, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("list credentials of grant %s: %w", grantID, err)
	}
	credIDs := make([]string, 0, len(creds))
	for _, cred := range creds {
		credIDs = append(credIDs, cred.ID)
	}
	auditID := uuid.New().String()
	ok, err := tx.RevokeAgentDelegationGrant(ctx, grantID, revokedBy, reason, auditID, now)
	if err != nil {
		return nil, fmt.Errorf("revoke grant %s: %w", grantID, err)
	}
	if !ok {
		return nil, nil
	}
	before, err := json.Marshal(map[string]interface{}{
		"grant_id":               grantID,
		"revoke_reason":          reason,
		"revoked_credential_ids": credIDs,
	})
	if err != nil {
		return nil, err
	}
	record := &store.MutationAuditRecord{
		ID:            auditID,
		Timestamp:     now,
		MutationType:  mutationAgentDelegationGrantRevoke,
		TargetType:    auditTargetAgentDelegationGrant,
		TargetID:      grantID,
		BeforeSummary: string(before),
		AfterSummary:  "revoked",
	}
	if block != nil {
		block(record, locked)
	}
	actor.ApplyActor(record)
	applyHubActorFallback(record)
	if err := tx.CreateMutationAudit(ctx, record); err != nil {
		return nil, fmt.Errorf("%w: %w", errAgentDelegationAuditFailed, err)
	}
	return locked, nil
}

// revokeAgentDelegationGrantsTx revokes, on tx, every unrevoked grant that
// matches filter, expired or not, through revokeAgentDelegationGrantTx,
// with reason, attributed to actor. The G block names the grant's agent,
// issuer and grant. A grant revoked concurrently is skipped. It returns the
// grants it revoked. Any error leaves the caller to roll back.
func revokeAgentDelegationGrantsTx(ctx context.Context, tx store.Store, filter store.AgentDelegationGrantFilter, reason string, actor AuditActor, now time.Time) ([]*store.AgentDelegationGrant, error) {
	grants, err := tx.ListAgentDelegationGrants(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	revokedBy := agentDelegationCascadeRevokedBy(actor)
	var revoked []*store.AgentDelegationGrant
	for _, g := range grants {
		if g.RevokedAt != nil {
			continue
		}
		r, err := revokeAgentDelegationGrantTx(ctx, tx, g.ID, revokedBy, reason, actor, now,
			func(record *store.MutationAuditRecord, grant *store.AgentDelegationGrant) {
				agentDelegationAuditBlock(record, grant, nil)
			})
		if err != nil {
			return nil, err
		}
		if r != nil {
			revoked = append(revoked, r)
		}
	}
	return revoked, nil
}

// agentDelegationCascadeRevokedBy is the revoked_by value of a lifecycle
// revocation: the principal whose action caused it, in the
// "<identity type>:<id>" form the revoke route records, or the hub itself
// when the action names no principal (a background job).
func agentDelegationCascadeRevokedBy(actor AuditActor) string {
	if actor.PrincipalKind != "" && actor.PrincipalID != "" {
		return actor.PrincipalKind + ":" + actor.PrincipalID
	}
	return agentDelegationSystemActor
}

// revokeAgentDelegationGrantsBestEffort revokes the grants that match
// filter in a transaction of its own, after the change that calls for it
// has committed (§16.3). A failure is logged and never fails the caller:
// exchange and use deny in the states these paths create. The conduit
// streams of every agent whose grant was revoked are re-checked.
func (s *Server) revokeAgentDelegationGrantsBestEffort(ctx context.Context, filter store.AgentDelegationGrantFilter, reason string) {
	if s.store == nil {
		return
	}
	actor := auditActorFromContext(ctx)
	var revoked []*store.AgentDelegationGrant
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		var err error
		revoked, err = revokeAgentDelegationGrantsTx(ctx, tx, filter, reason, actor, time.Now().UTC())
		return err
	})
	if err != nil {
		slog.Warn("agent delegation grants not revoked", "agent_id", filter.AgentID,
			"issuer_user_id", filter.IssuerUserID, "reason", reason, "error", err)
		return
	}
	seen := make(map[string]bool, len(revoked))
	for _, g := range revoked {
		if seen[g.AgentID] {
			continue
		}
		seen[g.AgentID] = true
		s.publishConduitAuthzChanged(conduitAuthzMatch{AgentID: g.AgentID})
	}
}

// revokeProjectAgentDelegationGrantsTx revokes, inside the project delete
// transaction, the grants of every agent of projectID, soft-deleted ones
// included, and their credentials (§16.3). It returns the number of grants
// revoked.
func revokeProjectAgentDelegationGrantsTx(ctx context.Context, tx store.Store, projectID string, actor AuditActor, now time.Time) (int, error) {
	total := 0
	cursor := ""
	for {
		page, err := tx.ListAgents(ctx, store.AgentFilter{ProjectID: projectID, IncludeDeleted: true},
			store.ListOptions{Limit: 100, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return total, fmt.Errorf("list project agents for grant revocation: %w", err)
		}
		if page == nil {
			return total, fmt.Errorf("%w: nil agent page in revokeProjectAgentDelegationGrantsTx", store.ErrInvalidInput)
		}
		for _, a := range page.Items {
			revoked, err := revokeAgentDelegationGrantsTx(ctx, tx, store.AgentDelegationGrantFilter{AgentID: a.ID}, revokeReasonProjectDeleted, actor, now)
			if err != nil {
				return total, fmt.Errorf("revoke agent delegation grants of agent %s: %w", a.ID, err)
			}
			total += len(revoked)
		}
		if page.NextCursor == "" {
			return total, nil
		}
		cursor = page.NextCursor
	}
}
