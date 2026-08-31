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

	// Mode controls what the explain endpoint returns:
	//   - "" or "decision": explain a single permission decision (default)
	//   - "effective_permissions": return the full effective permission set
	//     with per-permission provenance
	Mode string `json:"mode,omitempty"`

	// ComparePrincipalID, when set with mode="effective_permissions",
	// returns a comparison of two principals' effective permission sets.
	ComparePrincipalID   string `json:"comparePrincipalId,omitempty"`
	ComparePrincipalKind string `json:"comparePrincipalKind,omitempty"`
}

// explainResponse is the JSON response for the explain endpoint.
type explainResponse struct {
	Allowed       bool                `json:"allowed"`
	Reason        string              `json:"reason"`
	MatchedPolicy string              `json:"matchedPolicy,omitempty"`
	MatchedGrant  string              `json:"matchedGrant,omitempty"`
	PolicyID      string              `json:"policyId,omitempty"`
	Trace         []DecisionStep      `json:"trace,omitempty"`
	Provenance    *DecisionProvenance `json:"provenance,omitempty"`

	// EffectivePermissions is populated in "effective_permissions" mode.
	// Each entry describes a permission in the effective set with its
	// source grant and any boundary that capped it.
	EffectivePermissions []PermissionProvenance `json:"effectivePermissions,omitempty"`

	// CompareResult is populated when ComparePrincipalID is set.
	CompareResult *PermissionCompareResult `json:"compareResult,omitempty"`
}

// PermissionProvenance describes which grant sourced a permission and which
// boundary (if any) capped it.
type PermissionProvenance struct {
	// PermissionID is the canonical permission identifier.
	PermissionID string `json:"permissionId"`

	// Granted is true if this permission is in the effective set.
	Granted bool `json:"granted"`

	// SourceGrant identifies the binding and role that sourced this permission.
	SourceGrant *GrantDetail `json:"sourceGrant,omitempty"`

	// CappedBy lists the restrictions that would remove this permission.
	// Empty when Granted is true.
	CappedBy []RestrictionProvenance `json:"cappedBy,omitempty"`
}

// PermissionCompareResult compares two principals' effective permission sets.
type PermissionCompareResult struct {
	// PrincipalAID is the first principal.
	PrincipalAID string `json:"principalAId"`

	// PrincipalBID is the second principal.
	PrincipalBID string `json:"principalBId"`

	// OnlyA lists permissions held only by principal A.
	OnlyA []string `json:"onlyA"`

	// OnlyB lists permissions held only by principal B.
	OnlyB []string `json:"onlyB"`

	// Both lists permissions held by both principals.
	Both []string `json:"both"`
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

	// Handle effective_permissions mode: return full effective permission set
	// with per-permission provenance.
	if req.Mode == "effective_permissions" {
		s.handleExplainEffectivePermissions(w, ctx, req, explainIdentity, resource, isCrossPrincipal, isSuperAdmin)
		return
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

	writeJSON(w, http.StatusOK, resp)
}

// handleExplainEffectivePermissions handles the effective_permissions mode
// of the explain endpoint. It returns the full effective permission set
// with per-permission provenance showing which grant sourced each permission
// and which boundary (if any) capped it.
//
// NOTE: Performance — this runs a full Decide() call for each permission in
// the effective set. For principals with many permissions (e.g., super-admin
// with 50+ permissions), this results in N full authz pipeline evaluations.
// The shared work (principal closure, bindings, roles, restrictions) is
// identical across all N calls and could be cached. This is acceptable for
// the explain API (low-volume diagnostic endpoint) but should be optimized
// if usage patterns change. TODO: cache shared authz context across the
// per-permission Decide loop.
func (s *Server) handleExplainEffectivePermissions(
	w http.ResponseWriter,
	ctx context.Context,
	req explainRequest,
	explainIdentity Identity,
	resource Resource,
	isCrossPrincipal bool,
	isSuperAdmin bool,
) {
	// C1 fix: ComparePrincipalID reveals another principal's effective
	// permissions. Require the same hub.audit.read gate used for PrincipalID.
	if req.ComparePrincipalID != "" {
		if !isSuperAdmin {
			writeForbidden(w, "comparison requires hub.audit.read")
			return
		}
	}
	scopeType := ""
	scopeID := ""
	if resource.ParentType == "project" && resource.ParentID != "" {
		scopeType = ScopeTypeProject
		scopeID = resource.ParentID
	} else if resource.Type == "project" && resource.ID != "" {
		scopeType = ScopeTypeProject
		scopeID = resource.ID
	} else {
		scopeType = ScopeTypeSystem
	}

	// Normalize the principal type for effective permission lookup.
	// Identity types like "dev", "federated_user" need to map to the
	// store principal type ("user") for binding queries.
	principalType := normalizePrincipalType(explainIdentity.Type())

	// Get effective permissions for the principal.
	effectivePerms, err := s.authzService.getEffectivePermissions(
		ctx,
		principalType,
		explainIdentity.ID(),
		scopeType, scopeID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to compute effective permissions", nil)
		return
	}

	// Build per-permission provenance by running a Decide for each
	// permission in the effective set.
	var permProvenance []PermissionProvenance
	effectiveSet := make(map[string]bool, len(effectivePerms))
	for _, permID := range effectivePerms {
		effectiveSet[permID] = true

		authzReq := AuthzRequest{
			Principal:  principalContextForIdentity(explainIdentity),
			Credential: credentialContextForIdentity(explainIdentity),
			Resource:   resource,
			Permission: permID,
			Action:     Action(req.Action),
			Explain:    true,
		}
		decision := s.authzService.Decide(ctx, authzReq)

		pp := PermissionProvenance{
			PermissionID: permID,
			Granted:      decision.Allowed,
		}

		// Extract source grant from provenance.
		if decision.Provenance != nil && len(decision.Provenance.Grants) > 0 {
			g := decision.Provenance.Grants[0]
			// Prefer a grant that contains the requested permission.
			for _, candidate := range decision.Provenance.Grants {
				if candidate.ContainsRequested {
					g = candidate
					break
				}
			}
			pp.SourceGrant = &g
		}

		// Extract capping restrictions.
		if decision.Provenance != nil {
			for _, r := range decision.Provenance.Restrictions {
				if r.Applied {
					pp.CappedBy = append(pp.CappedBy, r)
				}
			}
		}

		if isCrossPrincipal && pp.SourceGrant != nil {
			redacted := redactGrantDetail(*pp.SourceGrant)
			pp.SourceGrant = &redacted
		}

		permProvenance = append(permProvenance, pp)
	}

	resp := explainResponse{
		Allowed:              len(effectivePerms) > 0,
		Reason:               fmt.Sprintf("%d effective permissions", len(effectivePerms)),
		EffectivePermissions: permProvenance,
	}

	// Handle comparison with another principal.
	if req.ComparePrincipalID != "" {
		compareIdentity, err := s.resolveExplainPrincipal(ctx, req.ComparePrincipalID, req.ComparePrincipalKind)
		if err != nil {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "Compare principal not found", nil)
			return
		}

		comparePerms, err := s.authzService.getEffectivePermissions(
			ctx,
			normalizePrincipalType(compareIdentity.Type()),
			compareIdentity.ID(),
			scopeType, scopeID,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"failed to compute compare principal's effective permissions", nil)
			return
		}

		compareSet := make(map[string]bool, len(comparePerms))
		for _, p := range comparePerms {
			compareSet[p] = true
		}

		principalBID := req.ComparePrincipalID
		if isCrossPrincipal {
			// N3 fix: redact the comparison principal ID in cross-principal
			// requests for defense in depth.
			principalBID = "[redacted]"
		}
		result := &PermissionCompareResult{
			PrincipalAID: explainIdentity.ID(),
			PrincipalBID: principalBID,
		}

		for _, p := range effectivePerms {
			if compareSet[p] {
				result.Both = append(result.Both, p)
			} else {
				result.OnlyA = append(result.OnlyA, p)
			}
		}
		for _, p := range comparePerms {
			if !effectiveSet[p] {
				result.OnlyB = append(result.OnlyB, p)
			}
		}

		resp.CompareResult = result
	}

	writeJSON(w, http.StatusOK, resp)
}

// resolveExplainPrincipal resolves a principal ID and kind to an Identity.
func (s *Server) resolveExplainPrincipal(ctx context.Context, principalID, principalKind string) (Identity, error) {
	if principalKind == "agent" || principalKind == string(PrincipalKindAgent) {
		agent, err := s.store.GetAgent(ctx, principalID)
		if err != nil {
			return nil, err
		}
		return newAgentIdentityFromStore(agent), nil
	}
	user, err := s.store.GetUser(ctx, principalID)
	if err != nil {
		return nil, err
	}
	return NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "api"), nil
}

// normalizePrincipalType maps identity types to store principal types for
// binding queries. Identity types "dev" and "federated_user" are treated
// as "user" by the authorization system.
func normalizePrincipalType(identityType string) string {
	switch identityType {
	case "user", "dev", "federated_user":
		return "user"
	case "agent", "federated_agent":
		return "agent"
	default:
		return identityType
	}
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
