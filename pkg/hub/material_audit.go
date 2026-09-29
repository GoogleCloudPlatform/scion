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
	"time"

	"github.com/google/uuid"
)

// MaterialSelectionEventItem is one evaluated Candidate in a
// MaterialSelectionEvent. It never contains a secret value.
type MaterialSelectionEventItem struct {
	Kind          MaterialKind
	Key           string
	Scope         string
	ScopeID       string
	Grant         GrantKind
	SharingSource *SourceRef
	Allowed       bool
	Selected      bool
	Reason        string // audit only, never sent to the caller
	Permission    string // "project.secret_read" (project) or "" (user; no permission named)
	Detail        string // check 7 only, Decision.Reason verbatim; audit-only, never parsed
}

// MaterialSelectionEvent is emitted once per request by the F.2a runtime
// check sequence. It never contains values.
type MaterialSelectionEvent struct {
	EventType      string // "material_selection"
	CorrelationID  string // uuid.NewString() per request (no request-ID helper exists on main)
	Purpose        string // "runtime_read"
	Endpoint       string // "fetch" | "get" | "list"
	ActorKind      string
	ActorID        string
	CredentialKind string // "agent_jwt"
	CredentialID   string // agentCredentialIDContextKey value when present, else ""
	TargetAgent    struct{ AgentID, ProjectID, BrokerID string }
	ProvenanceRoot struct{ Kind, ID string } // Target.Root.Kind/ID (F.2a: {"user", Ancestry[0]}); not the ancestry
	RequestReason  string                    // set when checks 1-6 deny; items then empty
	Items          []MaterialSelectionEventItem
	Timestamp      time.Time
}

// materialSelectionAuditor is declared here (same package) so audit.go is
// not edited to add it. The production logger (NewLogAuditLogger)
// implements it; other AuditLogger implementations, such as test mocks,
// compile unchanged and fall back to slog via (*Server).logMaterialSelection.
type materialSelectionAuditor interface {
	LogMaterialSelectionEvent(ctx context.Context, e *MaterialSelectionEvent) error
}

// LogMaterialSelectionEvent logs a material selection audit event to the
// standard logger. It never logs a secret value.
func (l *LogAuditLogger) LogMaterialSelectionEvent(ctx context.Context, e *MaterialSelectionEvent) error {
	if e == nil {
		return nil
	}

	attrs := []slog.Attr{
		slog.String("event_type", e.EventType),
		slog.String("correlation_id", e.CorrelationID),
		slog.String("purpose", e.Purpose),
		slog.String("endpoint", e.Endpoint),
		slog.String("actor_kind", e.ActorKind),
		slog.String("actor_id", e.ActorID),
		slog.String("credential_kind", e.CredentialKind),
		slog.String("credential_id", e.CredentialID),
		slog.String("target_agent_id", e.TargetAgent.AgentID),
		slog.String("target_project_id", e.TargetAgent.ProjectID),
		slog.String("provenance_root_kind", e.ProvenanceRoot.Kind),
		slog.String("provenance_root_id", e.ProvenanceRoot.ID),
		slog.Int("item_count", len(e.Items)),
	}
	if e.TargetAgent.BrokerID != "" {
		attrs = append(attrs, slog.String("target_broker_id", e.TargetAgent.BrokerID))
	}
	if e.RequestReason != "" {
		attrs = append(attrs, slog.String("request_reason", e.RequestReason))
	}

	l.logger().LogAttrs(ctx, slog.LevelInfo, "material selection audit event", attrs...)

	return nil
}

// logMaterialSelection emits e through the audit logger when it implements
// materialSelectionAuditor, and falls back to slog otherwise. A nil
// s.auditLogger is safe: the type assertion on a nil interface yields
// ok == false, so this never dereferences the logger
// (TestMaterialAudit_NilAuditLoggerSafe).
func (s *Server) logMaterialSelection(ctx context.Context, e *MaterialSelectionEvent) {
	if e == nil {
		return
	}
	if a, ok := s.auditLogger.(materialSelectionAuditor); ok {
		_ = a.LogMaterialSelectionEvent(ctx, e)
		return
	}
	slog.Default().LogAttrs(ctx, slog.LevelInfo, "material selection audit event",
		slog.String("event_type", e.EventType),
		slog.String("correlation_id", e.CorrelationID),
		slog.String("endpoint", e.Endpoint),
	)
}

// buildMaterialSelectionEvent assembles the one MaterialSelectionEvent
// emitted per request. facts is nil for a whole-request denial (checks
// 1-6); items is then empty and requestReason is set instead.
func (s *Server) buildMaterialSelectionEvent(ctx context.Context, endpoint, correlationID string, facts *TargetFacts, requestReason string, items []MaterialSelectionEventItem) *MaterialSelectionEvent {
	actorKind, actorID, credentialKind, credentialID := materialAuditActorFromContext(ctx)
	e := &MaterialSelectionEvent{
		EventType:      "material_selection",
		CorrelationID:  correlationID,
		Purpose:        string(PurposeRuntimeRead),
		Endpoint:       endpoint,
		ActorKind:      actorKind,
		ActorID:        actorID,
		CredentialKind: credentialKind,
		CredentialID:   credentialID,
		RequestReason:  requestReason,
		Items:          items,
		Timestamp:      time.Now(),
	}
	if facts != nil {
		e.TargetAgent.AgentID = facts.Agent.ID
		e.TargetAgent.ProjectID = facts.ProjectID
		e.TargetAgent.BrokerID = facts.Agent.RuntimeBrokerID
		e.ProvenanceRoot.Kind = facts.Root.Kind
		e.ProvenanceRoot.ID = facts.Root.ID
	}
	return e
}

// materialSelectionItem converts an evaluated Candidate into the audit item
// shape, attaching the permission name (project scope only) and the check-7
// Detail string (Decision.Reason verbatim; audit-only, never parsed).
func materialSelectionItem(item ItemResult, permission, detail string) MaterialSelectionEventItem {
	return MaterialSelectionEventItem{
		Kind:          item.Kind,
		Key:           item.Key,
		Scope:         item.Scope,
		ScopeID:       item.ScopeID,
		Grant:         item.Grant,
		SharingSource: item.SharingSource,
		Allowed:       item.Allowed,
		Selected:      item.Selected,
		Reason:        item.Reason,
		Permission:    permission,
		Detail:        detail,
	}
}

// materialAuditActorFromContext fills the actor fields from the presented
// agent identity. It is unexported, one function, and is deleted in favour
// of E.2a's helper when E.2a merges.
func materialAuditActorFromContext(ctx context.Context) (actorKind, actorID, credentialKind, credentialID string) {
	ident := GetAgentIdentityFromContext(ctx)
	if ident == nil {
		return "", "", "", ""
	}
	return ident.Type(), ident.ID(), string(CredentialKindAgentJWT), GetAgentCredentialIDFromContext(ctx)
}

// newMaterialCorrelationID returns a fresh correlation ID for one request's
// MaterialSelectionEvent and its derived compatibility events.
func newMaterialCorrelationID() string {
	return uuid.NewString()
}

// logAgentSecretReadCompat writes one AgentSecretReadEvent per item through
// the existing logger.LogAgentSecretReadEvent, in the corrected shape that
// replaces the deleted package-level LogAgentSecretRead: Scope and ScopeID
// are recorded separately instead of folding the scope ID into ProjectID.
// Derived means "has a partner MaterialSelectionEvent with the same
// CorrelationID" (v6, N-2). Nil-safe like LogAgentSecretRead was
// (TestMaterialAudit_NilAuditLoggerSafe).
func (s *Server) logAgentSecretReadCompat(ctx context.Context, agentID, projectID, scope, scopeID, key string, success bool, failReason string, derived bool, correlationID string) {
	if s.auditLogger == nil {
		return
	}

	event := &AgentSecretReadEvent{
		AgentID:       agentID,
		ProjectID:     projectID,
		Scope:         scope,
		ScopeID:       scopeID,
		SecretKey:     key,
		Success:       success,
		FailReason:    failReason,
		Derived:       derived,
		CorrelationID: correlationID,
		Timestamp:     time.Now(),
	}

	_ = s.auditLogger.LogAgentSecretReadEvent(ctx, event)
}
