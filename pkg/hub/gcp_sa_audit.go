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
	"log/slog"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Audit events for the GCP service account registry: register, verify and
// delete (ptone/scion#4022, spec §3.1 and §3.7).
//
// THE EVENT NEVER CARRIES THE ACCOUNT EMAIL. It records who acted, at which
// scope, on which account ID. The ID is stable and resolves to the email for
// anyone allowed to read the registry; the audit stream is read more widely
// than that, so it stays free of account strings. For the same reason a
// failed verification records only the outcome, not the IAM error text, which
// quotes the email.

// GCPServiceAccountAuditEventType names one registry operation.
type GCPServiceAccountAuditEventType string

const (
	GCPSAAuditRegister GCPServiceAccountAuditEventType = "gcp_service_account_register"
	GCPSAAuditVerify   GCPServiceAccountAuditEventType = "gcp_service_account_verify"
	GCPSAAuditDelete   GCPServiceAccountAuditEventType = "gcp_service_account_delete"
)

// Outcomes recorded on GCPServiceAccountAuditEvent.Outcome.
const (
	gcpSAAuditOutcomeRegistered    = "registered"
	gcpSAAuditOutcomeVerified      = "verified"
	gcpSAAuditOutcomeVerifyFailed  = "verification_failed"
	gcpSAAuditOutcomePersistFailed = "persist_failed"
	gcpSAAuditOutcomeDeleted       = "deleted"
	gcpSAAuditOutcomeRefusedInUse  = "refused_in_use"
	gcpSAAuditOutcomeDeleteFailed  = "delete_failed"
)

// GCPServiceAccountAuditEvent is one audit record for a registry operation.
type GCPServiceAccountAuditEvent struct {
	EventType      GCPServiceAccountAuditEventType `json:"eventType"`
	ActorKind      string                          `json:"actorKind,omitempty"`
	ActorID        string                          `json:"actorId,omitempty"`
	CredentialKind string                          `json:"credentialKind,omitempty"`
	CredentialID   string                          `json:"credentialId,omitempty"`
	// Scope and ScopeID are the account's registry scope ("project" or
	// "hub") and owner, not the GCP project the account lives in.
	Scope            string `json:"scope"`
	ScopeID          string `json:"scopeId,omitempty"`
	ServiceAccountID string `json:"serviceAccountId"`
	Managed          bool   `json:"managed,omitempty"`
	Success          bool   `json:"success"`
	Outcome          string `json:"outcome"`
	// Details carries operation-specific facts (for example force=true, or
	// the number of defaults a forced delete cleared). Never an email.
	Details   map[string]string `json:"details,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
}

// gcpServiceAccountAuditor is declared here (same package) so the
// AuditLogger interface, and every test double implementing it, stays
// unchanged; the production logger implements it and other loggers fall
// back to slog via (*Server).logGCPServiceAccountAudit. Same pattern as
// materialSelectionAuditor.
type gcpServiceAccountAuditor interface {
	LogGCPServiceAccountEvent(ctx context.Context, e *GCPServiceAccountAuditEvent) error
}

func gcpServiceAccountAuditAttrs(e *GCPServiceAccountAuditEvent) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("event_type", string(e.EventType)),
		slog.String("actor_kind", e.ActorKind),
		slog.String("actor_id", e.ActorID),
		slog.String("scope", e.Scope),
		slog.String("scope_id", e.ScopeID),
		slog.String("sa_id", e.ServiceAccountID),
		slog.Bool("managed", e.Managed),
		slog.Bool("success", e.Success),
		slog.String("outcome", e.Outcome),
	}
	if e.CredentialKind != "" {
		attrs = append(attrs, slog.String("credential_kind", e.CredentialKind),
			slog.String("credential_id", e.CredentialID))
	}
	keys := make([]string, 0, len(e.Details))
	for k := range e.Details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		attrs = append(attrs, slog.String("detail_"+k, e.Details[k]))
	}
	return attrs
}

func gcpServiceAccountAuditLevel(e *GCPServiceAccountAuditEvent) slog.Level {
	if e.Success {
		return slog.LevelInfo
	}
	return slog.LevelWarn
}

// LogGCPServiceAccountEvent logs a registry audit event. A nil receiver is
// safe (see LogMaterialSelectionEvent for why that matters).
func (l *LogAuditLogger) LogGCPServiceAccountEvent(ctx context.Context, e *GCPServiceAccountAuditEvent) error {
	if l == nil || e == nil {
		return nil
	}
	l.logger().LogAttrs(ctx, gcpServiceAccountAuditLevel(e), "GCP service account audit event", gcpServiceAccountAuditAttrs(e)...)
	return nil
}

// logGCPServiceAccountAudit records one registry operation on sa by the
// caller in ctx. Best effort: an audit sink error is logged, never returned,
// matching the other log-based audit events.
func (s *Server) logGCPServiceAccountAudit(ctx context.Context, eventType GCPServiceAccountAuditEventType, sa *store.GCPServiceAccount, success bool, outcome string, details map[string]string) {
	if sa == nil {
		return
	}
	actor := auditActorFromContext(ctx)
	e := &GCPServiceAccountAuditEvent{
		EventType:        eventType,
		ActorKind:        actor.PrincipalKind,
		ActorID:          actor.PrincipalID,
		CredentialKind:   actor.CredentialKind,
		CredentialID:     actor.CredentialID,
		Scope:            sa.Scope,
		ScopeID:          sa.ScopeID,
		ServiceAccountID: sa.ID,
		Managed:          sa.Managed,
		Success:          success,
		Outcome:          outcome,
		Details:          details,
		Timestamp:        time.Now(),
	}
	if a, ok := s.auditLogger.(gcpServiceAccountAuditor); ok {
		if err := a.LogGCPServiceAccountEvent(ctx, e); err != nil {
			slog.Warn("failed to write GCP service account audit event", "sa_id", sa.ID, "error", err)
		}
		return
	}
	slog.Default().LogAttrs(ctx, gcpServiceAccountAuditLevel(e), "GCP service account audit event", gcpServiceAccountAuditAttrs(e)...)
}

// logGCPServiceAccountRegisterAudit records a successful registration (BYO
// or mint). The verification state the registration ended in is recorded as
// a detail, since registration auto-verifies.
func (s *Server) logGCPServiceAccountRegisterAudit(ctx context.Context, sa *store.GCPServiceAccount) {
	if sa == nil {
		return
	}
	verification := sa.VerificationStatus
	if verification == "" {
		verification = store.GCPVerificationUnverified
	}
	s.logGCPServiceAccountAudit(ctx, GCPSAAuditRegister, sa, true, gcpSAAuditOutcomeRegistered,
		map[string]string{"verification": verification})
}
