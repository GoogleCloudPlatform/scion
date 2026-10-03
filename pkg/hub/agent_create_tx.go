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

// compensateAgentCreate rolls back a committed create after a later step
// failed, in one transaction: it deletes the agent row (identity keys
// cascade), deactivates the agent's delegation edges with cause
// create_compensation, and writes an agent_create_dispatch_failed audit
// record naming the create's audit record. originalAuditID may be empty
// when it is unknown.
//
// The caller passes a context detached from the request (see
// cleanupFailedCreate). Credential revocation, the broker-side delete and
// the quota release stay with the caller.
func (s *Server) compensateAgentCreate(ctx context.Context, agent *store.Agent, originalAuditID string, cause error) error {
	opID := api.NewUUID()
	now := time.Now()
	summary := struct {
		OriginalAuditID string `json:"original_audit_id,omitempty"`
		OpID            string `json:"op_id"`
		Error           string `json:"error,omitempty"`
	}{OriginalAuditID: originalAuditID, OpID: opID}
	if cause != nil {
		summary.Error = truncateAuditText(cause.Error(), 512)
	}
	after, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	record := &store.MutationAuditRecord{
		MutationType: mutationTypeAgentCreateDispatchFailed,
		TargetType:   "agent",
		TargetID:     agent.ID,
		AfterSummary: string(after),
		Timestamp:    now,
	}
	auditActorFromContext(ctx).ApplyActor(record)
	applyHubActorFallback(record)

	return s.store.WithTx(ctx, func(tx store.Store) error {
		if err := tx.DeleteAgent(ctx, agent.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("delete agent: %w", err)
		}
		if _, err := tx.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID, store.Deactivation{
			Cause: store.EdgeDeactivationCreateCompensation,
			At:    &now,
			OpID:  opID,
		}); err != nil {
			return fmt.Errorf("deactivate delegation edges: %w", err)
		}
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return fmt.Errorf("compensation audit: %w", err)
		}
		return nil
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

// truncateAuditText bounds free text copied into an audit summary.
func truncateAuditText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// logCompensationFailure logs a failed compensation at ERROR with its
// correlation ID.
func logCompensationFailure(ctx context.Context, agentID, correlationID string, err error) {
	slog.ErrorContext(ctx, "agent create compensation failed",
		"agent_id", agentID, "correlation_id", correlationID, "error", err)
}
