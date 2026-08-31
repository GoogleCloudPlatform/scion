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
	"math/rand"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// =============================================================================
// Decision Audit Emitter
// =============================================================================

// auditWriteTimeout is the maximum time an async audit INSERT may take before
// the goroutine abandons the attempt and releases its store reference.
const auditWriteTimeout = 1 * time.Second

// StoreDecisionAuditEmitter implements DecisionAuditEmitter using the store.
type StoreDecisionAuditEmitter struct {
	store  store.Store
	logger *slog.Logger
}

// NewStoreDecisionAuditEmitter creates a new store-backed decision audit emitter.
func NewStoreDecisionAuditEmitter(s store.Store, logger *slog.Logger) *StoreDecisionAuditEmitter {
	return &StoreDecisionAuditEmitter{store: s, logger: logger}
}

// EmitDecisionAudit stores a decision audit record asynchronously.
func (e *StoreDecisionAuditEmitter) EmitDecisionAudit(ctx context.Context, record *store.DecisionAuditRecord) {
	// Fire-and-forget in a goroutine to avoid blocking the authorization hot path.
	// Uses a short timeout context to prevent goroutine/memory leaks on shutdown.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				e.logger.Warn("recovered panic in decision audit emit", "panic", r)
			}
		}()
		writeCtx, cancel := context.WithTimeout(context.Background(), auditWriteTimeout)
		defer cancel()
		if err := e.store.CreateDecisionAudit(writeCtx, record); err != nil {
			e.logger.Warn("failed to emit decision audit record", "error", err)
		}
	}()
}

// emitDecisionAudit builds and emits a decision audit record from a Decide call.
func (a *AuthzService) emitDecisionAudit(ctx context.Context, request AuthzRequest, decision Decision) {
	// Sampling: always audit deny decisions; sample allow decisions.
	if decision.Allowed && a.DecisionAuditSampleRate < 1.0 {
		if rand.Float64() >= a.DecisionAuditSampleRate {
			return
		}
	}

	result := "deny"
	if decision.Allowed {
		result = "allow"
	}

	sampled := a.DecisionAuditSampleRate < 1.0

	record := &store.DecisionAuditRecord{
		Timestamp:      time.Now(),
		PrincipalKind:  string(decision.PrincipalKind),
		PrincipalID:    request.Principal.ID,
		CredentialID:   decision.CredentialID,
		CredentialType: decision.CredentialKind,
		ResourceType:   request.Resource.Type,
		ResourceID:     request.Resource.ID,
		Permission:     string(request.Action),
		Result:         result,
		Reason:         decision.Reason,
		MatchedPolicy:  decision.MatchedPolicy,
		MatchedGrant:   decision.MatchedGrant,
		PolicyID:       decision.BindingID,
		Sampled:        sampled,
	}

	// Try to extract route from context
	if route := routeFromContext(ctx); route != "" {
		record.Route = route
	}

	a.decisionAuditEmitter.EmitDecisionAudit(ctx, record)
}

// routeContextKey is the context key for the current HTTP route.
type routeContextKey struct{}

// ContextWithRoute adds the HTTP route to the context.
func ContextWithRoute(ctx context.Context, route string) context.Context {
	return context.WithValue(ctx, routeContextKey{}, route)
}

// routeFromContext retrieves the HTTP route from the context.
func routeFromContext(ctx context.Context) string {
	if route, ok := ctx.Value(routeContextKey{}).(string); ok {
		return route
	}
	return ""
}

// =============================================================================
// Mutation Audit Helper
// =============================================================================

// emitMutationAudit creates a mutation audit record from a handler context.
// It extracts actor identity from the context and stores the record.
// Errors are logged but do not fail the request (best-effort).
func (s *Server) emitMutationAudit(ctx context.Context, record *store.MutationAuditRecord) {
	// Extract actor identity from context if not already populated.
	if record.ActorPrincipalKind == "" || record.ActorPrincipalID == "" {
		identity := GetIdentityFromContext(ctx)
		if identity != nil {
			record.ActorPrincipalKind = identity.Type()
			record.ActorPrincipalID = identity.ID()

			// Extract credential info
			credential := GetCredentialContextFromContext(ctx)
			if credential.Kind != "" {
				record.ActorCredentialID = credential.ID
				record.ActorCredentialType = string(credential.Kind)
			}
		}
	}

	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now()
	}

	// Fire-and-forget: do not block the handler.
	// Uses a short timeout context to prevent goroutine/memory leaks on shutdown.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("recovered panic in mutation audit emit", "panic", r)
			}
		}()
		writeCtx, cancel := context.WithTimeout(context.Background(), auditWriteTimeout)
		defer cancel()
		if err := s.store.CreateMutationAudit(writeCtx, record); err != nil {
			slog.Warn("failed to emit mutation audit record",
				"mutation_type", record.MutationType,
				"error", err)
		}
	}()
}

// =============================================================================
// Explain API Handler
// =============================================================================

// explainRequest is the JSON body for the explain endpoint.
type explainRequest struct {
	Resource struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		ProjectID string `json:"projectId"`
	} `json:"resource"`
	Action        string `json:"action"`
	PrincipalID   string `json:"principalId,omitempty"`
	PrincipalKind string `json:"principalKind,omitempty"`
}

// explainResponse is the JSON response for the explain endpoint.
type explainResponse struct {
	Allowed       bool                `json:"allowed"`
	Reason        string              `json:"reason"`
	MatchedPolicy string              `json:"matchedPolicy,omitempty"`
	MatchedGrant  string              `json:"matchedGrant,omitempty"`
	PolicyID      string              `json:"policyId,omitempty"`
	Trace         []DecisionStep      `json:"trace"`
	Provenance    *DecisionProvenance `json:"provenance,omitempty"`
}

// handleAuthzExplain handles POST /api/v1/authz/explain.
func (s *Server) handleAuthzExplain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, ErrCodeInvalidRequest, "Method not allowed", nil)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	var req explainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	if req.Resource.Type == "" || req.Action == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "resource.type and action are required", nil)
		return
	}

	// Determine the principal for the explain request.
	explainIdentity := identity
	isCrossPrincipal := req.PrincipalID != "" && req.PrincipalID != identity.ID()

	// Explaining for a different principal reveals authorization internals and
	// is restricted to users with hub.audit.read (super-admin only). The Decide
	// check evaluates via the AK1 kernel, so the super-admin role binding
	// grants this automatically.
	isSuperAdmin := false
	if user, ok := identity.(UserIdentity); ok {
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(user),
			Credential: credentialContextForIdentity(user),
			Resource:   Resource{Type: "hub", ID: "hub"},
			Action:     Action("manage"),
			Permission: "hub.audit.read",
		})
		isSuperAdmin = decision.Allowed
	}

	// Non-admin cannot explain for a different principal.
	if isCrossPrincipal {
		if !isSuperAdmin {
			writeForbidden(w, "cannot explain for another principal without hub.audit.read")
			return
		}
		// Super-admin: resolve the target principal.
		if req.PrincipalKind == "agent" || req.PrincipalKind == string(PrincipalKindAgent) {
			agent, err := s.store.GetAgent(ctx, req.PrincipalID)
			if err != nil {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "Principal not found", nil)
				return
			}
			explainIdentity = newAgentIdentityFromStore(agent)
		} else {
			user, err := s.store.GetUser(ctx, req.PrincipalID)
			if err != nil {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "Principal not found", nil)
				return
			}
			explainIdentity = NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "api")
		}
	}

	// Build the resource.
	resource := Resource{
		Type: req.Resource.Type,
		ID:   req.Resource.ID,
	}
	if req.Resource.ProjectID != "" {
		resource.ParentType = "project"
		resource.ParentID = req.Resource.ProjectID
	}

	// Build the authz request with Explain enabled.
	authzReq := AuthzRequest{
		Principal:  principalContextForIdentity(explainIdentity),
		Credential: credentialContextForIdentity(explainIdentity),
		Resource:   resource,
		Action:     Action(req.Action),
		Explain:    true,
	}

	decision := s.authzService.Decide(ctx, authzReq)

	// Apply field-level redaction for cross-principal explain.
	// When the requesting user is explaining another principal's access,
	// redact sensitive fields but preserve causal structure.
	provenance := decision.Provenance
	if isCrossPrincipal && provenance != nil {
		provenance = redactCrossPrincipalProvenance(provenance)
	}

	resp := explainResponse{
		Allowed:       decision.Allowed,
		Reason:        decision.Reason,
		MatchedPolicy: decision.MatchedPolicy,
		MatchedGrant:  decision.MatchedGrant,
		PolicyID:      decision.BindingID,
		Trace:         decision.ExplainTrace,
		Provenance:    provenance,
	}
	if resp.Trace == nil {
		resp.Trace = []DecisionStep{}
	}

	writeJSON(w, http.StatusOK, resp)
}

// redactCrossPrincipalProvenance redacts sensitive fields from provenance
// for cross-principal explain requests. It preserves causal structure
// (the reader learns THAT something is hidden and WHY) but removes
// sensitive names and display names.
func redactCrossPrincipalProvenance(dp *DecisionProvenance) *DecisionProvenance {
	if dp == nil {
		return nil
	}

	redacted := &DecisionProvenance{
		Permission:           dp.Permission,
		EffectivePermissions: dp.EffectivePermissions,
		DenyReasons:          dp.DenyReasons,
		Errors:               dp.Errors,
	}

	// Redact grant details: preserve binding/role IDs, redact principal names.
	for _, g := range dp.Grants {
		redacted.Grants = append(redacted.Grants, redactGrantDetail(g))
	}
	for _, g := range dp.InactiveGrants {
		redacted.InactiveGrants = append(redacted.InactiveGrants, redactGrantDetail(g))
	}

	// Copy restrictions: boundary IDs are stable identifiers the reader
	// can follow, but names may be sensitive.
	for _, r := range dp.Restrictions {
		rr := r
		rr.BoundaryName = "[redacted]"
		redacted.Restrictions = append(redacted.Restrictions, rr)
	}

	// Copy status restrictions.
	redacted.StatusRestrictions = dp.StatusRestrictions

	// Redact membership paths: preserve structure (path length) and typed
	// target IDs but redact group names within paths.
	for _, mp := range dp.MembershipPaths {
		rmp := MembershipPathDetail{
			TargetID: mp.TargetID,
			Kind:     mp.Kind,
		}
		for _, p := range mp.Path {
			rmp.Path = append(rmp.Path, redactPathElement(p))
		}
		redacted.MembershipPaths = append(redacted.MembershipPaths, rmp)
	}

	// Ensure non-nil slices.
	if redacted.Grants == nil {
		redacted.Grants = []GrantDetail{}
	}
	if redacted.InactiveGrants == nil {
		redacted.InactiveGrants = []GrantDetail{}
	}
	if redacted.Restrictions == nil {
		redacted.Restrictions = []RestrictionProvenance{}
	}
	if redacted.MembershipPaths == nil {
		redacted.MembershipPaths = []MembershipPathDetail{}
	}

	return redacted
}

// redactGrantDetail redacts sensitive fields from a GrantDetail while
// preserving the causal structure (binding IDs, role IDs, scope info).
func redactGrantDetail(g GrantDetail) GrantDetail {
	return GrantDetail{
		BindingID:         g.BindingID,
		RoleID:            g.RoleID,
		RoleName:          g.RoleName, // Role names are not sensitive (they are system-defined).
		ScopeType:         g.ScopeType,
		ScopeID:           g.ScopeID,
		PrincipalType:     g.PrincipalType,
		PrincipalID:       "[redacted]",
		ContainsRequested: g.ContainsRequested,
		MembershipPath:    nil, // Redact path details in cross-principal.
		Permissions:       g.Permissions,
		InactiveReason:    g.InactiveReason,
		RejectReasons:     g.RejectReasons,
	}
}

// redactPathElement redacts the ID part of a typed path element (e.g.,
// "group:engineers" → "group:[redacted]") while preserving the type prefix.
func redactPathElement(element string) string {
	for _, prefix := range []string{"user:", "agent:", "group:", "dev:", "federated_user:", "federated_agent:"} {
		if len(element) > len(prefix) && element[:len(prefix)] == prefix {
			return prefix + "[redacted]"
		}
	}
	return "[redacted]"
}

// explainAgentIdentity is a minimal AgentIdentity for the explain endpoint.
type explainAgentIdentity struct {
	id        string
	projectID string
	ancestry  []string
}

func (a *explainAgentIdentity) ID() string                    { return a.id }
func (a *explainAgentIdentity) Type() string                  { return "agent" }
func (a *explainAgentIdentity) ProjectID() string             { return a.projectID }
func (a *explainAgentIdentity) Scopes() []AgentTokenScope     { return nil }
func (a *explainAgentIdentity) HasScope(AgentTokenScope) bool { return false }
func (a *explainAgentIdentity) Ancestry() []string            { return a.ancestry }
func (a *explainAgentIdentity) TokenID() string               { return "" }
func (a *explainAgentIdentity) OriginUserID() string {
	if len(a.ancestry) > 0 {
		return a.ancestry[0]
	}
	return ""
}

// newAgentIdentityFromStore creates an AgentIdentity from a store Agent record.
// Used by the explain endpoint to resolve agent principals.
func newAgentIdentityFromStore(agent *store.Agent) AgentIdentity {
	return &explainAgentIdentity{
		id:        agent.ID,
		projectID: agent.ProjectID,
		ancestry:  agent.Ancestry,
	}
}

// =============================================================================
// Audit Retention Controls
// =============================================================================

// CleanupAuditRecords removes audit records older than the specified retention period.
func (s *Server) CleanupAuditRecords(ctx context.Context, retentionDays int) error {
	cutoff := time.Now().AddDate(0, 0, -retentionDays)

	decisionCount, err := s.store.DeleteDecisionAuditsBefore(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("failed to cleanup decision audit records: %w", err)
	}

	mutationCount, err := s.store.DeleteMutationAuditsBefore(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("failed to cleanup mutation audit records: %w", err)
	}

	slog.Info("audit records cleaned up",
		"decision_records_deleted", decisionCount,
		"mutation_records_deleted", mutationCount,
		"retention_days", retentionDays,
		"cutoff", cutoff)

	return nil
}
