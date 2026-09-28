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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Project messaging-policy API types
// ---------------------------------------------------------------------------

// ProjectMessagingPolicyResponse is the response for GET/PUT messaging-policy.
type ProjectMessagingPolicyResponse struct {
	// CrossProjectInbound is the configured inbound policy: "none", "members", "any".
	CrossProjectInbound string `json:"crossProjectInbound"`
	// Revision is the optimistic concurrency revision counter.
	Revision int64 `json:"revision"`
	// EffectiveCrossProjectInbound reflects the actual effective policy,
	// taking into account whether the Hub-level cross-project messaging is enabled.
	// When the Hub switch is off, this is always "none" regardless of config.
	EffectiveCrossProjectInbound string `json:"effectiveCrossProjectInbound"`
	// HubCrossProjectEnabled indicates whether the Hub-level switch is on.
	HubCrossProjectEnabled bool `json:"hubCrossProjectEnabled"`
	// Capabilities lists supported cross-project conversation kinds.
	Capabilities *MessagingPolicyCapabilities `json:"capabilities,omitempty"`
}

// MessagingPolicyCapabilities describes what cross-project messaging features
// are supported in the current release.
type MessagingPolicyCapabilities struct {
	CrossProjectConversationKinds []string `json:"crossProjectConversationKinds"`
}

// ProjectMessagingPolicyUpdateRequest is the PUT request body.
type ProjectMessagingPolicyUpdateRequest struct {
	CrossProjectInbound string `json:"crossProjectInbound"`
	ExpectedRevision    int64  `json:"expectedRevision"`
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// handleProjectMessagingPolicy handles GET/PUT /api/v1/projects/{id}/messaging-policy.
//
// GET: Returns current policy under existing project-read authorization.
// PUT: Requires active direct project owner or local unscoped Hub admin.
func (s *Server) handleProjectMessagingPolicy(w http.ResponseWriter, r *http.Request, projectID string) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetProjectMessagingPolicy(w, r, projectID)
	case http.MethodPut:
		s.handlePutProjectMessagingPolicy(w, r, projectID)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

// handleGetProjectMessagingPolicy returns the project's cross-project inbound
// policy and its effective state. Read authorization uses existing project-read rules.
func (s *Server) handleGetProjectMessagingPolicy(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	// Authorization: require project read access.
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return
	}

	resp := s.buildMessagingPolicyResponse(project)
	writeJSON(w, http.StatusOK, resp)
}

// handlePutProjectMessagingPolicy updates the cross-project inbound policy.
// Only active direct project owners or local unscoped Hub admins may change it.
func (s *Server) handlePutProjectMessagingPolicy(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	// 1. Fetch the project.
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// 2. Authorization: active direct owner or local unscoped Hub admin only.
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Authentication required", nil)
		return
	}

	allowed := false
	switch identity.Type() {
	case "user", "dev", "federated_user":
		userIdent, ok := identity.(UserIdentity)
		if !ok {
			break
		}
		// Local unscoped Hub admin
		if IsUnscopedLocalPlatformAdmin(userIdent) {
			allowed = true
			break
		}
		// Active direct project owner
		if s.isProjectOwner(ctx, userIdent.ID(), project.ID) {
			allowed = true
		}
	}

	if !allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Only active direct project owners or Hub administrators can change the messaging policy", nil)
		return
	}

	// 3. Parse request body.
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req ProjectMessagingPolicyUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ValidationError(w, "invalid request body", nil)
		return
	}

	// 4. Validate the inbound value.
	if !store.IsValidCrossProjectInbound(req.CrossProjectInbound) {
		ValidationError(w, "invalid crossProjectInbound: must be one of none, members, any", nil)
		return
	}

	// 5. Update with optimistic concurrency.
	updated, err := s.store.UpdateProjectMessagingPolicy(ctx, project.ID, req.CrossProjectInbound, req.ExpectedRevision)
	if err != nil {
		if errors.Is(err, store.ErrRevisionConflict) {
			writeError(w, http.StatusConflict, "revision_conflict",
				"The messaging policy was modified concurrently. Please refresh and retry.", nil)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		slog.Error("failed to update project messaging policy",
			"project_id", project.ID, "error", err)
		RuntimeError(w, "Failed to update project messaging policy")
		return
	}

	// 6. Audit.
	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType:  "project_messaging_policy_update",
		TargetType:    "project",
		TargetID:      project.ID,
		BeforeSummary: project.CrossProjectInbound,
		AfterSummary:  req.CrossProjectInbound,
	})

	// 7. Response.
	resp := s.buildMessagingPolicyResponse(updated)
	writeJSON(w, http.StatusOK, resp)
}

// buildMessagingPolicyResponse constructs the policy response from a project.
func (s *Server) buildMessagingPolicyResponse(p *store.Project) ProjectMessagingPolicyResponse {
	configured := p.CrossProjectInbound
	if configured == "" {
		configured = store.CrossProjectInboundNone
	}

	// Effective policy: when the Hub switch is off, effective is always "none".
	hubEnabled := s.crossProjectMessagingEnabled()
	effective := configured
	if !hubEnabled {
		effective = store.CrossProjectInboundNone
	}

	return ProjectMessagingPolicyResponse{
		CrossProjectInbound:          configured,
		Revision:                     p.CrossProjectInboundRevision,
		EffectiveCrossProjectInbound: effective,
		HubCrossProjectEnabled:       hubEnabled,
		Capabilities: &MessagingPolicyCapabilities{
			CrossProjectConversationKinds: []string{"direct"},
		},
	}
}

// crossProjectMessagingEnabled returns whether the Hub-level cross-project
// messaging switch is enabled. Returns false when operational settings are
// unavailable (fail-closed).
func (s *Server) crossProjectMessagingEnabled() bool {
	ops := s.GetOperationalSettings()
	if ops == nil {
		return false
	}
	return ops.CrossProjectMessagingEnabled()
}
