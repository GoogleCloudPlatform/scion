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
	"net/http"
	"strconv"
	"strings"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/store"
)

// ============================================================================
// Policy Endpoints
// ============================================================================

// ListPoliciesResponse is the response for listing policies.
type ListPoliciesResponse struct {
	Policies   []store.Policy `json:"policies"`
	NextCursor string         `json:"nextCursor,omitempty"`
	TotalCount int            `json:"totalCount"`
}

// CreatePolicyRequest is the request body for creating a policy.
type CreatePolicyRequest struct {
	Name         string                  `json:"name"`
	Description  string                  `json:"description,omitempty"`
	ScopeType    string                  `json:"scopeType"`   // "hub", "grove", "resource"
	ScopeID      string                  `json:"scopeId,omitempty"`
	ResourceType string                  `json:"resourceType"` // "*" for all
	ResourceID   string                  `json:"resourceId,omitempty"`
	Actions      []string                `json:"actions"`
	Effect       string                  `json:"effect"` // "allow", "deny"
	Conditions   *store.PolicyConditions `json:"conditions,omitempty"`
	Priority     int                     `json:"priority"`
	Labels       map[string]string       `json:"labels,omitempty"`
	Annotations  map[string]string       `json:"annotations,omitempty"`
}

// UpdatePolicyRequest is the request body for updating a policy.
type UpdatePolicyRequest struct {
	Name         string                  `json:"name,omitempty"`
	Description  string                  `json:"description,omitempty"`
	ResourceType string                  `json:"resourceType,omitempty"`
	ResourceID   string                  `json:"resourceId,omitempty"`
	Actions      []string                `json:"actions,omitempty"`
	Effect       string                  `json:"effect,omitempty"`
	Conditions   *store.PolicyConditions `json:"conditions,omitempty"`
	Priority     *int                    `json:"priority,omitempty"`
	Labels       map[string]string       `json:"labels,omitempty"`
	Annotations  map[string]string       `json:"annotations,omitempty"`
}

// ListPolicyBindingsResponse is the response for listing policy bindings.
type ListPolicyBindingsResponse struct {
	Bindings []store.PolicyBinding `json:"bindings"`
}

// AddPolicyBindingRequest is the request body for adding a binding to a policy.
type AddPolicyBindingRequest struct {
	PrincipalType string `json:"principalType"` // "user" or "group"
	PrincipalID   string `json:"principalId"`
}

// handlePolicies handles GET and POST on /api/v1/policies
func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listPolicies(w, r)
	case http.MethodPost:
		s.createPolicy(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listPolicies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.PolicyFilter{
		ScopeType:    query.Get("scopeType"),
		ScopeID:      query.Get("scopeId"),
		ResourceType: query.Get("resourceType"),
		Effect:       query.Get("effect"),
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListPolicies(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListPoliciesResponse{
		Policies:   result.Items,
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
	})
}

func (s *Server) createPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreatePolicyRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	if req.ScopeType == "" {
		ValidationError(w, "scopeType is required", nil)
		return
	}
	if req.ScopeType != store.PolicyScopeHub && req.ScopeType != store.PolicyScopeGrove && req.ScopeType != store.PolicyScopeResource {
		ValidationError(w, "scopeType must be 'hub', 'grove', or 'resource'", nil)
		return
	}
	if req.ScopeType != store.PolicyScopeHub && req.ScopeID == "" {
		ValidationError(w, "scopeId is required for grove and resource scopes", nil)
		return
	}
	if len(req.Actions) == 0 {
		ValidationError(w, "actions is required", nil)
		return
	}
	if req.Effect == "" {
		ValidationError(w, "effect is required", nil)
		return
	}
	if req.Effect != store.PolicyEffectAllow && req.Effect != store.PolicyEffectDeny {
		ValidationError(w, "effect must be 'allow' or 'deny'", nil)
		return
	}

	resourceType := req.ResourceType
	if resourceType == "" {
		resourceType = "*"
	}

	policy := &store.Policy{
		ID:           api.NewUUID(),
		Name:         req.Name,
		Description:  req.Description,
		ScopeType:    req.ScopeType,
		ScopeID:      req.ScopeID,
		ResourceType: resourceType,
		ResourceID:   req.ResourceID,
		Actions:      req.Actions,
		Effect:       req.Effect,
		Conditions:   req.Conditions,
		Priority:     req.Priority,
		Labels:       req.Labels,
		Annotations:  req.Annotations,
		// CreatedBy: TODO: Get from auth context
	}

	if err := s.store.CreatePolicy(ctx, policy); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, policy)
}

// handlePolicyRoutes handles /api/v1/policies/{policyId}/...
func (s *Server) handlePolicyRoutes(w http.ResponseWriter, r *http.Request) {
	// Extract policy ID and remaining path
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/policies/")
	if path == "" {
		NotFound(w, "Policy")
		return
	}

	// Parse the policy ID
	parts := strings.SplitN(path, "/", 2)
	policyID := parts[0]
	subPath := ""
	if len(parts) > 1 {
		subPath = parts[1]
	}

	// Check for nested /bindings path
	if strings.HasPrefix(subPath, "bindings") {
		bindingPath := strings.TrimPrefix(subPath, "bindings")
		bindingPath = strings.TrimPrefix(bindingPath, "/")
		if bindingPath == "" {
			s.handlePolicyBindings(w, r, policyID)
		} else {
			s.handlePolicyBindingByID(w, r, policyID, bindingPath)
		}
		return
	}

	// Otherwise handle as policy resource
	if subPath != "" {
		NotFound(w, "Policy resource")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getPolicy(w, r, policyID)
	case http.MethodPatch:
		s.updatePolicy(w, r, policyID)
	case http.MethodDelete:
		s.deletePolicy(w, r, policyID)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	policy, err := s.store.GetPolicy(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) updatePolicy(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	policy, err := s.store.GetPolicy(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var req UpdatePolicyRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name != "" {
		policy.Name = req.Name
	}
	if req.Description != "" {
		policy.Description = req.Description
	}
	if req.ResourceType != "" {
		policy.ResourceType = req.ResourceType
	}
	if req.ResourceID != "" {
		policy.ResourceID = req.ResourceID
	}
	if len(req.Actions) > 0 {
		policy.Actions = req.Actions
	}
	if req.Effect != "" {
		if req.Effect != store.PolicyEffectAllow && req.Effect != store.PolicyEffectDeny {
			ValidationError(w, "effect must be 'allow' or 'deny'", nil)
			return
		}
		policy.Effect = req.Effect
	}
	if req.Conditions != nil {
		policy.Conditions = req.Conditions
	}
	if req.Priority != nil {
		policy.Priority = *req.Priority
	}
	if req.Labels != nil {
		policy.Labels = req.Labels
	}
	if req.Annotations != nil {
		policy.Annotations = req.Annotations
	}

	if err := s.store.UpdatePolicy(ctx, policy); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) deletePolicy(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	if err := s.store.DeletePolicy(ctx, id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handlePolicyBindings handles GET and POST on /api/v1/policies/{policyId}/bindings
func (s *Server) handlePolicyBindings(w http.ResponseWriter, r *http.Request, policyID string) {
	ctx := r.Context()

	// Verify policy exists
	_, err := s.store.GetPolicy(ctx, policyID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.listPolicyBindings(w, r, policyID)
	case http.MethodPost:
		s.addPolicyBinding(w, r, policyID)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listPolicyBindings(w http.ResponseWriter, r *http.Request, policyID string) {
	ctx := r.Context()

	bindings, err := s.store.GetPolicyBindings(ctx, policyID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListPolicyBindingsResponse{
		Bindings: bindings,
	})
}

func (s *Server) addPolicyBinding(w http.ResponseWriter, r *http.Request, policyID string) {
	ctx := r.Context()

	var req AddPolicyBindingRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.PrincipalType == "" {
		ValidationError(w, "principalType is required", nil)
		return
	}
	if req.PrincipalType != store.PolicyPrincipalTypeUser && req.PrincipalType != store.PolicyPrincipalTypeGroup {
		ValidationError(w, "principalType must be 'user' or 'group'", nil)
		return
	}
	if req.PrincipalID == "" {
		ValidationError(w, "principalId is required", nil)
		return
	}

	binding := &store.PolicyBinding{
		PolicyID:      policyID,
		PrincipalType: req.PrincipalType,
		PrincipalID:   req.PrincipalID,
	}

	if err := s.store.AddPolicyBinding(ctx, binding); err != nil {
		if err == store.ErrAlreadyExists {
			Conflict(w, "Binding already exists for this policy")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, binding)
}

// handlePolicyBindingByID handles DELETE on /api/v1/policies/{policyId}/bindings/{type}/{id}
func (s *Server) handlePolicyBindingByID(w http.ResponseWriter, r *http.Request, policyID, bindingPath string) {
	ctx := r.Context()

	// Parse bindingPath as "type/id"
	parts := strings.SplitN(bindingPath, "/", 2)
	if len(parts) != 2 {
		NotFound(w, "Binding")
		return
	}
	principalType := parts[0]
	principalID := parts[1]

	if principalType != store.PolicyPrincipalTypeUser && principalType != store.PolicyPrincipalTypeGroup {
		NotFound(w, "Binding")
		return
	}

	// Verify policy exists
	_, err := s.store.GetPolicy(ctx, policyID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	switch r.Method {
	case http.MethodDelete:
		s.removePolicyBinding(w, r, policyID, principalType, principalID)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) removePolicyBinding(w http.ResponseWriter, r *http.Request, policyID, principalType, principalID string) {
	ctx := r.Context()

	if err := s.store.RemovePolicyBinding(ctx, policyID, principalType, principalID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
