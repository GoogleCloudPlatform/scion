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
		PolicyID:       decision.PolicyID,
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

// sanitizePolicySummary returns a compact JSON summary of a policy, safe for audit.
// No secrets or raw condition values are included.
func sanitizePolicySummary(p *store.Policy) string {
	if p == nil {
		return ""
	}
	summary := map[string]interface{}{
		"name":         p.Name,
		"effect":       p.Effect,
		"resourceType": p.ResourceType,
		"actions":      p.Actions,
		"scopeType":    p.ScopeType,
	}
	if p.ResourceID != "" {
		summary["resourceId"] = p.ResourceID
	}
	b, err := json.Marshal(summary)
	if err != nil {
		return fmt.Sprintf(`{"name":%q}`, p.Name)
	}
	return string(b)
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
	Allowed       bool           `json:"allowed"`
	Reason        string         `json:"reason"`
	MatchedPolicy string         `json:"matchedPolicy,omitempty"`
	MatchedGrant  string         `json:"matchedGrant,omitempty"`
	PolicyID      string         `json:"policyId,omitempty"`
	Trace         []DecisionStep `json:"trace"`
}

// handleAuthzExplain handles POST /api/v1/authz/explain.
func (s *Server) handleAuthzExplain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
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
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Resource.Type == "" || req.Action == "" {
		http.Error(w, "resource.type and action are required", http.StatusBadRequest)
		return
	}

	// Determine the principal for the explain request.
	explainIdentity := identity
	// Explaining for a different principal reveals authorization internals and
	// is restricted to users with hub.audit.read (super-admin only). The Decide
	// check routes through checkAccessForUser, so the step-1 super-admin bypass
	// grants this automatically without a seed policy.
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
	if req.PrincipalID != "" && req.PrincipalID != identity.ID() {
		if !isSuperAdmin {
			http.Error(w, "Forbidden: cannot explain for another principal", http.StatusForbidden)
			return
		}
		// Super-admin: resolve the target principal.
		if req.PrincipalKind == "agent" || req.PrincipalKind == string(PrincipalKindAgent) {
			agent, err := s.store.GetAgent(ctx, req.PrincipalID)
			if err != nil {
				http.Error(w, "Principal not found", http.StatusNotFound)
				return
			}
			explainIdentity = newAgentIdentityFromStore(agent)
		} else {
			user, err := s.store.GetUser(ctx, req.PrincipalID)
			if err != nil {
				http.Error(w, "Principal not found", http.StatusNotFound)
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

	resp := explainResponse{
		Allowed:       decision.Allowed,
		Reason:        decision.Reason,
		MatchedPolicy: decision.MatchedPolicy,
		MatchedGrant:  decision.MatchedGrant,
		PolicyID:      decision.PolicyID,
		Trace:         decision.ExplainTrace,
	}
	if resp.Trace == nil {
		resp.Trace = []DecisionStep{}
	}

	writeJSON(w, http.StatusOK, resp)
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
