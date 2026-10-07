// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Mutation types written by the agent-create transaction and its
// compensation.
const (
	// mutationTypeAgentDelegation is the audit record of an agent create:
	// the creator delegated authority to the new agent.
	mutationTypeAgentDelegation = "agent_delegation"
	// mutationTypeAgentCreateDispatchFailed records that a create whose
	// rows were committed was rolled back after its dispatch failed.
	mutationTypeAgentCreateDispatchFailed = "agent_create_dispatch_failed"
)

// errAgentCreateWriteInvalid marks an agentCreateWrite that is missing a
// required part. It is a programming error and maps to 500.
var errAgentCreateWriteInvalid = errors.New("agent create write is incomplete")

// agentCreateWrite is everything an agent create persists before dispatch.
// Ceiling and Provenance are supplied by the caller and copied onto Edge;
// commitAgentCreate never derives them.
type agentCreateWrite struct {
	Ceiling    store.EffectCeiling
	Provenance store.AuthorityProvenance
	Agent      *store.Agent
	Slug       string
	// Edge is required. Its DelegateID, ceiling and provenance are set by
	// commitAgentCreate.
	Edge *store.DelegationEdge
	// Audit is required. TargetType/TargetID are set to the created agent,
	// and the request's actor is applied.
	Audit *store.MutationAuditRecord
	// Subscription is optional; its AgentID is set to the created agent.
	Subscription *store.NotificationSubscription
}

// commitAgentCreate writes the agent row, its identity keys, its delegation
// edge, the create audit record and the optional notification subscription
// in one transaction: either all of them commit or none do. Dispatch (and
// the mint it performs) runs after this returns.
//
// A slug that fails display-name validation returns an error wrapping
// errInvalidDisplayName before any write. An incomplete write (nil agent,
// edge or audit, or a zero provenance) returns errAgentCreateWriteInvalid.
//
// The transaction first takes a shared lock on the row of the user the
// user delete guard would count for the agent (its owner when that is a
// user, else its ancestry root when that is a user, else its creator when
// the agent has no owner) and re-checks that the user exists
// (lockAgentGuardUserTx, ptone/scion#2769). A create racing that
// user's delete then either commits first, so the delete sees the agent and
// is refused, or fails with errAgentOwnerUserMissing and writes nothing.
func (s *Server) commitAgentCreate(ctx context.Context, w agentCreateWrite) error {
	switch {
	case w.Agent == nil:
		return fmt.Errorf("%w: no agent", errAgentCreateWriteInvalid)
	case w.Edge == nil:
		return fmt.Errorf("%w: no delegation edge", errAgentCreateWriteInvalid)
	case w.Audit == nil:
		return fmt.Errorf("%w: no audit record", errAgentCreateWriteInvalid)
	case w.Provenance.ProvenanceVersion == 0:
		return fmt.Errorf("%w: provenance not recorded", errAgentCreateWriteInvalid)
	}
	if _, err := api.ValidateDisplayName(w.Slug); err != nil {
		return fmt.Errorf("%w: %s", errInvalidDisplayName, err)
	}

	auditActorFromContext(ctx).ApplyActor(w.Audit)
	applyHubActorFallback(w.Audit)
	if w.Audit.Timestamp.IsZero() {
		w.Audit.Timestamp = time.Now()
	}
	w.Edge.EffectCeiling = w.Ceiling
	w.Edge.AuthorityProvenance = w.Provenance

	return s.store.WithTx(ctx, func(tx store.Store) error {
		if err := lockAgentGuardUserTx(ctx, tx, w.Agent); err != nil {
			return err
		}
		agent := w.Agent
		if err := tx.CreateAgent(ctx, agent); err != nil {
			return err
		}
		// The caller sets Name to the slug, so IdentityKeysFor collapses to
		// the single {slug} row, the same function rename and restore use.
		if err := tx.ReplaceAgentIdentityKeys(ctx, agent.ID, agent.ProjectID, api.IdentityKeysFor(w.Slug, agent.Name)); err != nil {
			return err
		}
		w.Edge.DelegateID = agent.ID
		if err := tx.CreateDelegationEdge(ctx, w.Edge); err != nil {
			return err
		}
		w.Audit.TargetType = "agent"
		w.Audit.TargetID = agent.ID
		if err := tx.CreateMutationAudit(ctx, w.Audit); err != nil {
			return fmt.Errorf("agent create audit: %w", err)
		}
		if w.Subscription != nil {
			w.Subscription.AgentID = agent.ID
			if err := tx.CreateNotificationSubscription(ctx, w.Subscription); err != nil {
				return fmt.Errorf("agent create notification subscription: %w", err)
			}
		}
		return nil
	})
}

// Stages of a committed create at which a failure rolls it back. Each
// cleanup site records its own stage in the agent_create_dispatch_failed
// audit record, so readers can tell a failure before dispatch from a
// dispatch failure.
const (
	createStageStorage           = "storage"
	createStageUploadURL         = "upload_url"
	createStageWorkspaceStorage  = "workspace_storage"
	createStageManaged           = "managed"
	createStageRunIntent         = "run_intent"
	createStageDispatchEnvGather = "dispatch_env_gather"
	createStageDispatch          = "dispatch"
	createStageMissingEnv        = "missing_env"
	createStageProvision         = "provision"
	// createStageWorkspaceUpload: the hub-managed workspace upload ran past
	// its own budget (hubWorkspaceUploadTimeout).
	createStageWorkspaceUpload = "workspace_upload"
	// createStageManagedRecord: the managed create's post-create write
	// (managed Runtime, interaction ID) failed (ptone/scion#3557).
	createStageManagedRecord = "managed_record"
)

// createRowCompensable is the predicate of a conditional create
// compensation: the row exists and is not soft-deleted. Whether a delete
// holds it is checked by createRowHeldCheck inside the same transaction,
// because DeletionPredicate ANDs its terms and cannot express "no live
// deleting claim": a deleting row whose lease lapsed is a live agent no
// engine owns (design ptone/scion#2483 §2.1), and must be rolled back.
func createRowCompensable() store.DeletionPredicate {
	return store.DeletionPredicate{DeletedAtNull: true}
}

// createRowHeldCheck is the in-transaction half of a conditional create
// compensation (ptone/scion#3557). It runs on the row read, locked, in
// FinalizeAgentDeletion's transaction and refuses, rolling the delete back,
// when a delete holds the row by the rule deletedOrDeleteHeld applies: a
// live deleting claim, or finalizing (expired or not). A failed delete, or a
// deleting row whose lease lapsed, does not hold it. then runs after the
// check, inside the same transaction; nil for none.
func createRowHeldCheck(then func(tx store.Store) error) store.DeletionFinalizeHook {
	return func(_ context.Context, tx store.Store, a *store.Agent, _ store.DeletionFinalizeMode) error {
		if deletedOrDeleteHeld(a) {
			return errCreateRowDeleteHeld
		}
		if then == nil {
			return nil
		}
		return then(tx)
	}
}

// createCompensation is the input of compensateAgentCreate.
type createCompensation struct {
	Agent *store.Agent
	// OriginalAuditID is the ID of the create's audit record; empty when
	// unknown.
	OriginalAuditID string
	// OpID identifies the edge deactivation and is recorded in the audit
	// record. A fresh ID is used when empty.
	OpID string
	// Stage is the create stage whose failure triggered the rollback (one
	// of the createStage* values).
	Stage string
	// Cause is the failure that triggered the rollback; may be nil.
	Cause error
	// IfNotDeleteHeld makes the row delete conditional (ptone/scion#3557):
	// the row is removed only when no delete holds it (createRowHeldCheck),
	// checked inside the compensation's own transaction. When a delete
	// holds it, or the row is already gone or soft-deleted, nothing is
	// written and errCreateRowDeleteHeld is returned: the row, its edge
	// and its quotas are left to the delete.
	IfNotDeleteHeld bool
}

// errCreateRowDeleteHeld is returned by a conditional compensation
// (createCompensation.IfNotDeleteHeld) and deleteFailedCreateRow when a
// delete holds the agent row, or the row is gone: the delete owns it.
var errCreateRowDeleteHeld = errors.New("agent row is held by a delete")

// compensateAgentCreate rolls back a committed create after a later step
// failed, in one transaction: it deletes the agent row (identity keys
// cascade), deactivates the agent's delegation edges with cause
// create_compensation, and writes an agent_create_dispatch_failed audit
// record naming the create's audit record and the failed stage.
//
// It is idempotent: when the agent row is already gone and no active edge
// is left, it changes nothing and writes no audit record.
//
// The caller passes a context detached from the request (see
// cleanupFailedCreate). Credential revocation, the broker-side delete and
// the quota release stay with the caller.
func (s *Server) compensateAgentCreate(ctx context.Context, c createCompensation) error {
	if c.Agent == nil {
		return fmt.Errorf("%w: no agent to compensate", errAgentCreateWriteInvalid)
	}
	opID := c.OpID
	if opID == "" {
		opID = api.NewUUID()
	}
	now := time.Now()
	summary := struct {
		OriginalAuditID string `json:"original_audit_id,omitempty"`
		OpID            string `json:"op_id"`
		Stage           string `json:"stage,omitempty"`
		Error           string `json:"error,omitempty"`
	}{OriginalAuditID: c.OriginalAuditID, OpID: opID, Stage: c.Stage}
	if c.Cause != nil {
		summary.Error = truncateAuditText(c.Cause.Error(), 512)
	}
	after, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	agentID := c.Agent.ID
	record := &store.MutationAuditRecord{
		MutationType: mutationTypeAgentCreateDispatchFailed,
		TargetType:   "agent",
		TargetID:     agentID,
		AfterSummary: string(after),
		Timestamp:    now,
	}
	auditActorFromContext(ctx).ApplyActor(record)
	applyHubActorFallback(record)

	// edgesAndAudit runs in the compensation's transaction, after the row
	// delete.
	edgesAndAudit := func(tx store.Store, rowDeleted bool) error {
		deactivated, err := tx.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentID, store.Deactivation{
			Cause: store.EdgeDeactivationCreateCompensation,
			At:    &now,
			OpID:  opID,
		})
		if err != nil {
			return fmt.Errorf("deactivate delegation edges: %w", err)
		}
		if !rowDeleted && deactivated == 0 {
			// Already compensated: nothing changed, so nothing to record.
			return nil
		}
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return fmt.Errorf("compensation audit: %w", err)
		}
		return nil
	}

	if c.IfNotDeleteHeld {
		// One transaction that reads the row (locked where supported),
		// removes it, checks it is not delete-held and runs edgesAndAudit
		// (the same agent_create_dispatch_failed record as below); a
		// refused check rolls it all back. A delete claim that lands after
		// the read bumps state_version, so the row delete misses and the
		// attempt is retried from a fresh read, which then refuses.
		n, err := s.store.FinalizeAgentDeletion(ctx, agentID, createRowCompensable(), store.DeletionFinalizeHard, store.DeletionFields{},
			createRowHeldCheck(func(tx store.Store) error { return edgesAndAudit(tx, true) }))
		if errors.Is(err, errCreateRowDeleteHeld) {
			return errCreateRowDeleteHeld
		}
		if err != nil {
			return fmt.Errorf("delete agent: %w", err)
		}
		if n == 0 {
			// The row is gone or soft-deleted: a delete removed it.
			return errCreateRowDeleteHeld
		}
		return nil
	}

	return s.store.WithTx(ctx, func(tx store.Store) error {
		rowDeleted := true
		if err := tx.DeleteAgent(ctx, agentID); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("delete agent: %w", err)
			}
			rowDeleted = false
		}
		return edgesAndAudit(tx, rowDeleted)
	})
}

// compensationFailureID returns the correlation ID reported to the caller
// and logged when a compensation fails: the request ID when there is one,
// otherwise a fresh ID.
func compensationFailureID(ctx context.Context) string {
	if id := logging.RequestIDFromContext(ctx); id != "" {
		return id
	}
	return api.NewUUID()
}

// applyHubActorFallback records the hub as the actor of a record written
// with no request principal; the audit store rejects an empty actor kind.
func applyHubActorFallback(record *store.MutationAuditRecord) {
	if record.ActorPrincipalKind == "" {
		record.ActorPrincipalKind = mintAuditSystemActorKind
		record.ActorPrincipalID = mintAuditSystemActorID
	}
}

// truncateAuditText bounds free text copied into an audit summary to at
// most max bytes, cutting at a rune boundary so a multi-byte character is
// never split.
func truncateAuditText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// logCompensationFailure logs a failed compensation at ERROR with its
// correlation ID and the op ID its edge deactivation used.
func logCompensationFailure(ctx context.Context, agentID, correlationID, opID string, err error) {
	slog.ErrorContext(ctx, "agent create compensation failed",
		"agent_id", agentID, "correlation_id", correlationID, "op_id", opID, "error", err)
}
