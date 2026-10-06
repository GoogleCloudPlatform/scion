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
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Agent lifecycle transactions: soft delete, hard delete,
// restore and the reincarnation claim each commit the agent-row write, the
// delegation-edge (de)activation, the registered hooks and one mutation
// audit record in a single store transaction.
//
// Soft and hard delete run inside the delete engine's finalize transaction,
// through agentFinalizeHook. Restore and the reincarnation claim open their
// own transaction.

// Mutation types written by the lifecycle transactions.
const (
	mutationTypeAgentSoftDelete       = "agent_soft_delete"
	mutationTypeAgentHardDelete       = "agent_hard_delete"
	mutationTypeAgentRestore          = "agent_restore"
	mutationTypeAgentReincarnateClaim = "agent_reincarnate_claim"
)

// errAgentNotSoftDeleted is returned by restoreAgentTx when the agent is not
// in the soft-deleted state.
var errAgentNotSoftDeleted = errors.New("agent is not in deleted state")

// errRestoreEdgeConflict wraps the store.ErrAlreadyExists a restore gets when
// a delegation edge it would reactivate conflicts with an active edge. The
// restore handler maps only this sentinel to 409, so an ErrAlreadyExists
// from another write in the restore transaction is not reported as an edge
// conflict.
var errRestoreEdgeConflict = errors.New("restore: a delegation edge to reactivate conflicts with an active edge")

// errRestoreDelegatorNotLive is returned by restoreAgentTx when the delegator
// of an edge deactivated by the soft delete is not live: a user that is
// missing or not active, or an agent that is missing or soft-deleted. The
// restore writes nothing and the deactivation records stay intact.
var errRestoreDelegatorNotLive = errors.New("restore: a delegator of the agent's delegation edges is not live")

// errRestoreDelegatorLookup wraps a store error from the delegator check. The
// restore writes nothing.
var errRestoreDelegatorLookup = errors.New("restore: delegator lookup failed")

// AgentTxHook runs inside a lifecycle transaction, after the agent-row write
// and the delegation-edge (de)activation and before the audit record, with a
// transaction-scoped store. agent is the row as written by the transaction
// (for hard delete, the row as it was just before it was removed). Hooks run
// in registration order.
//
// A non-nil error rolls back the whole operation. For soft and hard delete
// that fails the delete with finalize_failed: the agent stays in the
// finalizing state and start is refused until a retried or forced delete
// succeeds. Return an error only for work that must commit atomically with
// the lifecycle change; log and swallow anything best-effort. Hooks must not
// perform broker or network I/O, call launch_* store methods, or call any
// store method that cannot run inside a transaction.
type AgentTxHook func(ctx context.Context, tx store.Store, agent *store.Agent, actor AuditActor) error

type namedAgentTxHook struct {
	name string
	hook AgentTxHook
}

// agentLifecycleHooks holds the four hook registries. They are separate:
// soft-delete hooks do not run on hard delete. The zero value is ready.
type agentLifecycleHooks struct {
	mu          sync.Mutex
	softDelete  []namedAgentTxHook
	hardDelete  []namedAgentTxHook
	restore     []namedAgentTxHook
	reincarnate []namedAgentTxHook
}

func (h *agentLifecycleHooks) register(list *[]namedAgentTxHook, kind, name string, hook AgentTxHook) {
	if name == "" || hook == nil {
		panic(fmt.Sprintf("agent lifecycle %s hook: empty name or nil hook", kind))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, existing := range *list {
		if existing.name == name {
			panic(fmt.Sprintf("agent lifecycle %s hook %q registered twice", kind, name))
		}
	}
	*list = append(*list, namedAgentTxHook{name: name, hook: hook})
}

func (h *agentLifecycleHooks) snapshot(list *[]namedAgentTxHook) []namedAgentTxHook {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]namedAgentTxHook(nil), (*list)...)
}

// RegisterSoftDeleteHook registers h to run in every soft-delete
// transaction. See AgentTxHook for the contract.
func (s *Server) RegisterSoftDeleteHook(name string, h AgentTxHook) {
	s.lifecycleTxHooks.register(&s.lifecycleTxHooks.softDelete, "soft-delete", name, h)
}

// RegisterHardDeleteHook registers h to run in every hard-delete
// transaction. See AgentTxHook for the contract.
func (s *Server) RegisterHardDeleteHook(name string, h AgentTxHook) {
	s.lifecycleTxHooks.register(&s.lifecycleTxHooks.hardDelete, "hard-delete", name, h)
}

// RegisterRestoreHook registers h to run in every restore transaction. See
// AgentTxHook for the contract.
func (s *Server) RegisterRestoreHook(name string, h AgentTxHook) {
	s.lifecycleTxHooks.register(&s.lifecycleTxHooks.restore, "restore", name, h)
}

// RegisterReincarnateClaimHook registers h to run in every reincarnation
// claim transaction. See AgentTxHook for the contract.
func (s *Server) RegisterReincarnateClaimHook(name string, h AgentTxHook) {
	s.lifecycleTxHooks.register(&s.lifecycleTxHooks.reincarnate, "reincarnate-claim", name, h)
}

func runAgentTxHooks(ctx context.Context, kind string, hooks []namedAgentTxHook, tx store.Store, agent *store.Agent, actor AuditActor) error {
	for _, h := range hooks {
		if err := h.hook(ctx, tx, agent, actor); err != nil {
			return fmt.Errorf("agent %s hook %q: %w", kind, h.name, err)
		}
	}
	return nil
}

// agentFinalizeHook is the hook the delete engine's finalize transaction
// runs (finalizeAgentDeletion): the agentDeletionFinalizeSeam var first,
// then this Server's lifecycle finalize for the mode.
func (s *Server) agentFinalizeHook(ctx context.Context, tx store.Store, a *store.Agent, mode store.DeletionFinalizeMode) error {
	if err := agentDeletionFinalizeSeam(ctx, tx, a, mode); err != nil {
		return err
	}
	actor := auditActorFromContext(ctx)
	switch mode {
	case store.DeletionFinalizeSoft:
		return s.softDeleteAgentTx(ctx, tx, a, actor)
	case store.DeletionFinalizeHard:
		return s.hardDeleteAgentTx(ctx, tx, a, actor)
	}
	return fmt.Errorf("agent finalize: %w: mode %q", store.ErrInvalidInput, mode)
}

// lifecycleAudit builds a lifecycle mutation audit record for agentID with
// the JSON summary, attributed to actor (the hub when actor is empty).
func lifecycleAudit(mutationType, agentID string, actor AuditActor, now time.Time, summary any) (*store.MutationAuditRecord, error) {
	after, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}
	record := &store.MutationAuditRecord{
		MutationType: mutationType,
		TargetType:   "agent",
		TargetID:     agentID,
		AfterSummary: string(after),
		Timestamp:    now,
	}
	actor.ApplyActor(record)
	applyHubActorFallback(record)
	return record, nil
}

// softDeleteAgentTx runs inside the finalize transaction of a soft delete,
// after the engine set DeletedAt. In order: stamp a fresh operation ID as
// SoftDeleteOpID on the row, deactivate the agent's delegation edges under
// that ID (cause agent_soft_delete), run the soft-delete hooks, and write the
// agent_soft_delete audit record. a is the post-write row.
//
// Service-account assignments have no lifecycle store methods, so none are
// deactivated here.
func (s *Server) softDeleteAgentTx(ctx context.Context, tx store.Store, a *store.Agent, actor AuditActor) error {
	if a == nil {
		return fmt.Errorf("%w: nil agent in softDeleteAgentTx", store.ErrInvalidInput)
	}
	opID := api.NewUUID()
	now := time.Now()

	row := *a
	row.SoftDeleteOpID = opID
	if err := tx.SetAgentSoftDeleteOpID(ctx, row.ID, opID); err != nil {
		return fmt.Errorf("soft delete: stamp operation ID: %w", err)
	}
	n, err := tx.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, row.ID, store.Deactivation{
		Cause: store.EdgeDeactivationAgentSoftDelete,
		At:    &now,
		OpID:  opID,
	})
	if err != nil {
		return fmt.Errorf("soft delete: deactivate delegation edges: %w", err)
	}
	if err := runAgentTxHooks(ctx, "soft-delete", s.lifecycleTxHooks.snapshot(&s.lifecycleTxHooks.softDelete), tx, &row, actor); err != nil {
		return err
	}
	record, err := lifecycleAudit(mutationTypeAgentSoftDelete, row.ID, actor, now, struct {
		OpID             string `json:"op_id"`
		EdgesDeactivated int    `json:"edges_deactivated"`
	}{opID, n})
	if err != nil {
		return err
	}
	if err := tx.CreateMutationAudit(ctx, record); err != nil {
		return fmt.Errorf("soft delete audit: %w", err)
	}
	return nil
}

// hardDeleteAgentTx runs inside the finalize transaction of a hard delete,
// after the engine removed the agent row and its dependents. In order:
// deactivate the agent's delegation edges (cause agent_hard_delete; the edge
// rows have no foreign key to the agent and survive), run the hard-delete
// hooks, and write the agent_hard_delete audit record. a is the row as it
// was just before the delete; the row itself is not readable.
//
// The hard delete of an incomplete create is a hard delete like any other;
// its audit summary carries incomplete_create=true.
func (s *Server) hardDeleteAgentTx(ctx context.Context, tx store.Store, a *store.Agent, actor AuditActor) error {
	if a == nil {
		return fmt.Errorf("%w: nil agent in hardDeleteAgentTx", store.ErrInvalidInput)
	}
	opID := api.NewUUID()
	now := time.Now()

	n, err := tx.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, a.ID, store.Deactivation{
		Cause: store.EdgeDeactivationAgentHardDelete,
		At:    &now,
		OpID:  opID,
	})
	if err != nil {
		return fmt.Errorf("hard delete: deactivate delegation edges: %w", err)
	}
	if err := runAgentTxHooks(ctx, "hard-delete", s.lifecycleTxHooks.snapshot(&s.lifecycleTxHooks.hardDelete), tx, a, actor); err != nil {
		return err
	}
	record, err := lifecycleAudit(mutationTypeAgentHardDelete, a.ID, actor, now, struct {
		OpID             string `json:"op_id"`
		EdgesDeactivated int    `json:"edges_deactivated"`
		IncompleteCreate bool   `json:"incomplete_create,omitempty"`
		SoftDeleteOpID   string `json:"soft_delete_op_id,omitempty"`
	}{opID, n, a.IsIncompleteCreate(), a.SoftDeleteOpID})
	if err != nil {
		return err
	}
	if err := tx.CreateMutationAudit(ctx, record); err != nil {
		return fmt.Errorf("hard delete audit: %w", err)
	}
	return nil
}

// restoreAgentTx restores the soft-deleted agent a in one transaction. a is
// the row the caller loaded and gated; the restore write is guarded by its
// state_version. SetAgentSoftDeleteOpID does not bump state_version, so the
// operation ID is re-read with GetAgent inside the transaction and that
// value, not the one on a, selects the edges. In order: re-read the
// operation ID, check that the delegator of every edge deactivated under it
// is live, clear DeletedAt and SoftDeleteOpID, re-assert the identity keys,
// reactivate exactly those edges (none when the operation ID is empty), run
// the restore hooks, and write the agent_restore audit record.
//
// A delegator that is not live returns errRestoreDelegatorNotLive, a store
// error during that check errRestoreDelegatorLookup, a conflicting active
// edge errRestoreEdgeConflict and a concurrent change
// store.ErrVersionConflict; each rolls everything back. On success a
// carries the restored row.
func (s *Server) restoreAgentTx(ctx context.Context, a *store.Agent, actor AuditActor) error {
	if a == nil {
		return fmt.Errorf("%w: nil agent in restoreAgentTx", store.ErrInvalidInput)
	}
	if a.DeletedAt.IsZero() {
		return errAgentNotSoftDeleted
	}
	now := time.Now()

	row := *a
	row.DeletedAt = time.Time{}
	row.SoftDeleteOpID = ""
	row.Updated = now
	hooks := s.lifecycleTxHooks.snapshot(&s.lifecycleTxHooks.restore)
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		cur, err := tx.GetAgent(ctx, row.ID)
		if err != nil {
			return err
		}
		if cur.DeletedAt.IsZero() {
			return errAgentNotSoftDeleted
		}
		opID := cur.SoftDeleteOpID
		if opID != "" {
			if err := checkRestoreDelegatorsLive(ctx, tx, row.ID, opID); err != nil {
				return err
			}
		}
		if err := tx.UpdateAgent(ctx, &row); err != nil {
			return err
		}
		if err := tx.SetAgentSoftDeleteOpID(ctx, row.ID, ""); err != nil {
			return err
		}
		if err := tx.ReplaceAgentIdentityKeys(ctx, row.ID, row.ProjectID, api.IdentityKeysFor(row.Slug, row.Name)); err != nil {
			return err
		}
		reactivated := 0
		if opID != "" {
			n, err := tx.ReactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, row.ID, store.EdgeDeactivationAgentSoftDelete, opID)
			if errors.Is(err, store.ErrAlreadyExists) {
				return fmt.Errorf("%w: %w", errRestoreEdgeConflict, err)
			}
			if err != nil {
				return fmt.Errorf("restore: reactivate delegation edges: %w", err)
			}
			reactivated = n
		}
		if err := runAgentTxHooks(ctx, "restore", hooks, tx, &row, actor); err != nil {
			return err
		}
		record, err := lifecycleAudit(mutationTypeAgentRestore, row.ID, actor, now, struct {
			OpID             string `json:"op_id,omitempty"`
			EdgesReactivated int    `json:"edges_reactivated"`
		}{opID, reactivated})
		if err != nil {
			return err
		}
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return fmt.Errorf("restore audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	*a = row
	return nil
}

// checkRestoreDelegatorsLive checks the delegator of every edge deactivated
// for agentID under the soft delete opID. A user delegator is live when it
// exists and is active; an agent delegator when it exists and is not
// soft-deleted. It reads only, so the restore writes nothing when it fails.
func checkRestoreDelegatorsLive(ctx context.Context, tx store.Store, agentID, opID string) error {
	edges, err := tx.GetDeactivatedDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentID, store.EdgeDeactivationAgentSoftDelete, opID)
	if err != nil {
		return fmt.Errorf("%w: list deactivated edges: %w", errRestoreDelegatorLookup, err)
	}
	for _, e := range edges {
		live, err := delegatorLive(ctx, tx, e.DelegatorType, e.DelegatorID)
		if err != nil {
			return fmt.Errorf("%w: %s %s: %w", errRestoreDelegatorLookup, e.DelegatorType, e.DelegatorID, err)
		}
		if !live {
			return fmt.Errorf("%w: %s %s", errRestoreDelegatorNotLive, e.DelegatorType, e.DelegatorID)
		}
	}
	return nil
}

// delegatorLive reports whether the delegator of an edge is live. A missing
// principal is not live; any other store error is returned.
func delegatorLive(ctx context.Context, tx store.Store, delegatorType, delegatorID string) (bool, error) {
	switch delegatorType {
	case store.DelegationPrincipalUser:
		u, err := tx.GetUser(ctx, delegatorID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return u.Status == store.UserStatusActive, nil
	case store.DelegationPrincipalAgent:
		a, err := tx.GetAgent(ctx, delegatorID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return a.DeletedAt.IsZero(), nil
	default:
		return false, nil
	}
}

// reincarnateAuthority is the authority a reincarnation by another principal
// re-records: a new delegation edge from the requester, with the requester's
// frozen provenance and ceiling. nil for a self-reincarnation, which keeps
// the existing edge unchanged.
type reincarnateAuthority struct {
	DelegatorType string
	DelegatorID   string
	Role          string
	Ceiling       store.EffectCeiling
	Provenance    store.AuthorityProvenance
}

// reincarnateClaimTx claims agent for a reincarnation in one transaction.
// agent carries the claimed fields (ReincarnationState pending) and the
// state_version the caller read. In order: the claim UpdateAgent (its
// state_version guard turns a concurrent change into
// store.ErrVersionConflict), the reincarnation record, the authority
// re-record when auth is non-nil (deactivate the agent's active edges with
// cause reincarnate_replaced, create the requester's edge), the
// reincarnate-claim hooks, and the agent_reincarnate_claim audit record.
// Any error rolls everything back, so nothing is claimed.
func (s *Server) reincarnateClaimTx(ctx context.Context, agent *store.Agent, rec *store.AgentReincarnation, auth *reincarnateAuthority, actor AuditActor) error {
	if agent == nil {
		return fmt.Errorf("%w: nil agent in reincarnateClaimTx", store.ErrInvalidInput)
	}
	if auth != nil && auth.Provenance.ProvenanceVersion == 0 {
		return fmt.Errorf("%w: reincarnation provenance not recorded", errAgentCreateWriteInvalid)
	}
	opID := api.NewUUID()
	now := time.Now()
	row := *agent
	hooks := s.lifecycleTxHooks.snapshot(&s.lifecycleTxHooks.reincarnate)
	claimedAt := now
	if row.ReincarnationUpdatedAt != nil {
		claimedAt = *row.ReincarnationUpdatedAt
	}
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		// The claim: the state_version compare-and-set, refused while a
		// start claim is held or a reincarnation is already in flight.
		newVersion, err := tx.ClaimAgentReincarnation(ctx, row.ID, row.StateVersion, claimedAt)
		if err != nil {
			return err
		}
		row.StateVersion = newVersion
		row.ReincarnationState = store.ReincarnationStatePending
		if err := tx.CreateAgentReincarnation(ctx, rec); err != nil {
			return err
		}
		replaced := 0
		if auth != nil {
			n, err := tx.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, row.ID, store.Deactivation{
				Cause: store.EdgeDeactivationReincarnateReplaced,
				At:    &now,
				OpID:  opID,
			})
			if err != nil {
				return fmt.Errorf("reincarnate: deactivate delegation edges: %w", err)
			}
			replaced = n
			edge := &store.DelegationEdge{
				DelegatorType:       auth.DelegatorType,
				DelegatorID:         auth.DelegatorID,
				DelegateType:        store.DelegationPrincipalAgent,
				DelegateID:          row.ID,
				ScopeType:           store.RoleScopeProject,
				ScopeID:             row.ProjectID,
				Role:                auth.Role,
				Active:              true,
				AuthorityProvenance: auth.Provenance,
				EffectCeiling:       auth.Ceiling,
			}
			if err := tx.CreateDelegationEdge(ctx, edge); err != nil {
				return fmt.Errorf("reincarnate: record delegation edge: %w", err)
			}
		}
		if err := runAgentTxHooks(ctx, "reincarnate-claim", hooks, tx, &row, actor); err != nil {
			return err
		}
		record, err := lifecycleAudit(mutationTypeAgentReincarnateClaim, row.ID, actor, now, struct {
			OpID            string `json:"op_id"`
			ReincarnationID string `json:"reincarnation_id"`
			ReRecorded      bool   `json:"re_recorded"`
			EdgesReplaced   int    `json:"edges_replaced"`
		}{opID, rec.ID, auth != nil, replaced})
		if err != nil {
			return err
		}
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return fmt.Errorf("reincarnate claim audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	*agent = row
	return nil
}
