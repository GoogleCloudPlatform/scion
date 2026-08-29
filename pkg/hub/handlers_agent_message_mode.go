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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// set_message_mode endpoint types (D7, D10)
// ---------------------------------------------------------------------------

// SetMessageModeRequest is the request body for the set_message_mode action.
type SetMessageModeRequest struct {
	Mode    string `json:"mode"`              // Required: "none", "lineage", "branch", "project"
	Cascade bool   `json:"cascade,omitempty"` // Optional: apply to all descendants
}

// SetMessageModeResponse is the response for the set_message_mode action.
type SetMessageModeResponse struct {
	AgentID  string         `json:"agent_id"`
	Mode     string         `json:"mode"`
	Previous string         `json:"previous_mode"`
	Cascade  *CascadeResult `json:"cascade,omitempty"`
}

// CascadeAgentDetail describes a single agent's mode transition in a cascade operation.
type CascadeAgentDetail struct {
	AgentID     string `json:"agent_id"`
	AgentName   string `json:"agent_name"`
	CurrentMode string `json:"current_mode"`
	NewMode     string `json:"new_mode"`
}

// CascadeResult describes which descendants were updated (or would be updated) in a cascade operation.
type CascadeResult struct {
	Count    int                  `json:"count"`             // Number of descendants updated
	AgentIDs []string             `json:"agent_ids"`         // IDs of updated descendants
	Details  []CascadeAgentDetail `json:"details,omitempty"` // Per-agent transition details (populated for dryRun and cascade)
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// handleSetMessageMode handles the set_message_mode action on an agent.
//
// Authorization enforces D7: this is a human-only operation. The following
// callers are denied unconditionally:
//   - Agent callers (no agent scope exists or will ever exist)
//   - UATs / scoped tokens (no UAT scope exists)
//   - Project admins who are not also project owners or lineage owners
//
// Allowed callers: super-admin, project owner, lineage owner (user in the
// agent's ancestry chain).
//
// Mode changes are live (D10): the new mode takes effect on the next message
// delivery. Every change emits an audit record. All transitions are legal
// with no preconditions. See docs/messaging-authorization.md for the full
// API reference.
func (s *Server) handleSetMessageMode(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// 0. Check for dryRun query parameter.
	dryRun := r.URL.Query().Get("dryRun") == "true"

	// 1. Parse request body.
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024) // 64KB limit
	var req SetMessageModeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ValidationError(w, "invalid request body", nil)
		return
	}

	// When dryRun=true, treat as implicit cascade — the purpose of dry-run
	// is to preview cascade effects.
	if dryRun && !req.Cascade {
		req.Cascade = true
	}

	// 2. Validate mode value.
	if !store.IsValidMessageMode(req.Mode) {
		ValidationError(w, "invalid message mode: must be one of none, lineage, branch, project", nil)
		return
	}

	// 3. Fetch the target agent.
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// 4. D7: Human-only — DENY agent callers unconditionally.
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Authentication required", nil)
		return
	}
	// D7: Human-only — DENY all non-user callers unconditionally.
	// This catches "agent", "federated_agent", "federated_service", "broker", etc.
	switch identity.Type() {
	case "user", "dev", "federated_user":
		// Allowed identity types — continue to further checks.
	default:
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Only human users can change message mode (D7: human-only operation)", nil)
		return
	}

	// 5. D7: DENY UATs — no scope exists for set_message_mode.
	if _, ok := identity.(*ScopedUserIdentity); ok {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Scoped tokens cannot change message mode", nil)
		return
	}

	// 5.5. D7: DENY project admins (non-owners).
	// The generic authz bypass grants project admins full access, but D7
	// restricts set_message_mode to project owners, lineage owners, and
	// super-admins. Project admins who are NOT super-admins must be denied.
	if userIdent, ok := identity.(UserIdentity); ok && !IsUnscopedLocalPlatformAdmin(userIdent) {
		membership, err := s.store.GetProjectMembership(ctx, agent.ProjectID, userIdent.ID())
		if err != nil {
			// Fail closed: if we can't verify role, deny rather than skip the check.
			slog.Error("failed to check project membership for set_message_mode",
				"user_id", userIdent.ID(), "project_id", agent.ProjectID, "error", err)
			RuntimeError(w, "Failed to verify project membership")
			return
		}
		// Also check ancestry before denying admins — a user who is both admin
		// AND lineage owner should be allowed via lineage ownership (LOW-3).
		isLineageOwner := false
		for _, ancestorID := range agent.Ancestry {
			if ancestorID == userIdent.ID() {
				isLineageOwner = true
				break
			}
		}
		if !isLineageOwner && membership != nil && membership.Role == store.ProjectRoleAdmin {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"Project admins cannot change message mode (D7: owner-only operation)", nil)
			return
		}
	}

	// 6. Authorize: agent.set_message_mode permission (CapabilityResource).
	// This checks: lineage owner (ancestry), project owner, super-admin.
	if !s.authorize(w, r, agentResource(agent), ActionSetMessageMode) {
		return // authorize writes 403
	}

	// 7. Record previous mode.
	previousMode := agent.MessageMode

	// --- Dry-run path: preview cascade effects without applying changes ---
	if dryRun {
		cascadeResult, err := s.cascadeMessageMode(ctx, agent, req.Mode, true)
		if err != nil {
			slog.Error("dry-run cascade message mode failed", "agent_id", agent.ID, "error", err)
			RuntimeError(w, "Failed to preview cascade")
			return
		}
		// Include the root agent in the preview details.
		rootDetail := CascadeAgentDetail{
			AgentID:     agent.ID,
			AgentName:   agentDisplayName(agent),
			CurrentMode: previousMode,
			NewMode:     req.Mode,
		}
		cascadeResult.Details = append([]CascadeAgentDetail{rootDetail}, cascadeResult.Details...)
		// Root is counted only if its mode would actually change.
		if previousMode != req.Mode {
			cascadeResult.Count++
			cascadeResult.AgentIDs = append([]string{agent.ID}, cascadeResult.AgentIDs...)
		}

		resp := SetMessageModeResponse{
			AgentID:  agent.ID,
			Mode:     req.Mode,
			Previous: previousMode,
			Cascade:  cascadeResult,
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// 8. No-op check: same mode, no cascade.
	if previousMode == req.Mode && !req.Cascade {
		writeJSON(w, http.StatusOK, SetMessageModeResponse{
			AgentID:  agent.ID,
			Mode:     req.Mode,
			Previous: previousMode,
		})
		return
	}

	// 9. Update the agent's message mode.
	if previousMode != req.Mode {
		agent.MessageMode = req.Mode
		if err := s.store.UpdateAgent(ctx, agent); err != nil {
			slog.Error("failed to update agent message mode",
				"agent_id", agent.ID, "error", err)
			RuntimeError(w, "Failed to update agent message mode")
			return
		}

		// 10. Audit event for the primary agent.
		s.emitMutationAudit(ctx, &store.MutationAuditRecord{
			MutationType:  "agent_set_message_mode",
			TargetType:    "agent",
			TargetID:      agent.ID,
			BeforeSummary: previousMode,
			AfterSummary:  req.Mode,
		})
	}

	// 11. Cascade (if requested).
	var cascadeResult *CascadeResult
	if req.Cascade {
		cascadeResult, err = s.cascadeMessageMode(ctx, agent, req.Mode, false)
		if err != nil {
			// Primary agent was updated, but cascade failed — log and return partial.
			slog.Error("cascade message mode failed", "agent_id", agent.ID, "error", err)
			// Still return success for the primary agent.
		}
	}

	// 12. Response.
	resp := SetMessageModeResponse{
		AgentID:  agent.ID,
		Mode:     req.Mode,
		Previous: previousMode,
		Cascade:  cascadeResult,
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Cascade
// ---------------------------------------------------------------------------

// cascadeMessageMode applies a message mode to all descendants of the root agent.
// When dryRun is true, it computes which agents would be affected but does not
// modify any records — this uses the same code path so the preview cannot
// disagree with what a real apply would do.
// Best-effort per agent: a failure to update one descendant does not stop the rest.
func (s *Server) cascadeMessageMode(ctx context.Context, root *store.Agent, mode string, dryRun bool) (*CascadeResult, error) {
	descendants, err := s.store.ListAgents(ctx, store.AgentFilter{
		ProjectID:  root.ProjectID,
		AncestorID: root.ID,
	}, store.ListOptions{Limit: 1000})
	if err != nil {
		return nil, fmt.Errorf("listing descendants: %w", err)
	}

	result := &CascadeResult{
		AgentIDs: make([]string, 0, len(descendants.Items)),
		Details:  make([]CascadeAgentDetail, 0, len(descendants.Items)),
	}

	for i := range descendants.Items {
		desc := &descendants.Items[i]
		if desc.ID == root.ID {
			continue // root handled separately
		}
		oldMode := desc.MessageMode
		if oldMode == mode {
			continue // already at target mode
		}

		if dryRun {
			// Preview only — record what would change without modifying.
			result.Count++
			result.AgentIDs = append(result.AgentIDs, desc.ID)
			result.Details = append(result.Details, CascadeAgentDetail{
				AgentID:     desc.ID,
				AgentName:   agentDisplayName(desc),
				CurrentMode: oldMode,
				NewMode:     mode,
			})
			continue
		}

		// Apply the change.
		desc.MessageMode = mode
		if err := s.store.UpdateAgent(ctx, desc); err != nil {
			slog.Error("cascade message mode update failed",
				"agent_id", desc.ID, "error", err)
			continue // best-effort per agent
		}
		result.Count++
		result.AgentIDs = append(result.AgentIDs, desc.ID)
		result.Details = append(result.Details, CascadeAgentDetail{
			AgentID:     desc.ID,
			AgentName:   agentDisplayName(desc),
			CurrentMode: oldMode,
			NewMode:     mode,
		})

		// Audit each descendant update.
		s.emitMutationAudit(ctx, &store.MutationAuditRecord{
			MutationType:  "agent_set_message_mode_cascade",
			TargetType:    "agent",
			TargetID:      desc.ID,
			BeforeSummary: oldMode,
			AfterSummary:  mode,
		})
	}

	return result, nil
}

// agentDisplayName returns the best available display name for an agent.
// Prefers Name, falls back to Slug, then ID.
func agentDisplayName(a *store.Agent) string {
	if a.Name != "" {
		return a.Name
	}
	if a.Slug != "" {
		return a.Slug
	}
	return a.ID
}
