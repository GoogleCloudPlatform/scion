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
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
)

// authorizeScheduledMessageAuthoring validates a scheduled-message event at
// authoring time (create / update). It resolves the target agent from
// convenience fields or raw payload and rejects when:
//   - the target agent cannot be resolved,
//   - the target agent is in a different project from projectID (cross-project),
//   - the caller identity is a scoped UAT whose caveats cannot be preserved
//     at fire time (the scheduler stores only the creator ID, not the full
//     credential; fire time re-resolves the user but cannot reconstruct
//     scope restrictions), or
//   - the caller is not currently authorized to message the target
//     (fail-fast preview — the definitive check runs again at fire time).
//
// Returns true when authoring is allowed; writes the HTTP error response and
// returns false when denied.
func (s *Server) authorizeScheduledMessageAuthoring(
	w http.ResponseWriter,
	r *http.Request,
	projectID string,
	rawPayload string,
	agentID string,
	agentName string,
) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}

	// Resolve the target from the raw payload first, since that is what gets
	// persisted and fired. Fall back to convenience fields only when the
	// payload is empty or does not contain a target. This matches the
	// storage priority in handlers_scheduled_events.go.
	var targetAgentID, targetAgentName string
	if rawPayload != "" {
		var p MessageEventPayload
		if err := json.Unmarshal([]byte(rawPayload), &p); err == nil {
			targetAgentID = p.AgentID
			targetAgentName = p.AgentName
		}
	}
	// If the payload didn't yield a target, use convenience fields.
	if targetAgentID == "" && targetAgentName == "" {
		targetAgentID = agentID
		targetAgentName = agentName
	}
	// Reject conflicting representations: if both the payload and
	// convenience fields specify targets that disagree, the request is
	// ambiguous and must be denied. Validating one target while storing
	// another would create a false sense of authorization.
	if rawPayload != "" && (agentID != "" || agentName != "") {
		var p MessageEventPayload
		if err := json.Unmarshal([]byte(rawPayload), &p); err == nil {
			if (p.AgentID != "" && agentID != "" && p.AgentID != agentID) ||
				(p.AgentName != "" && agentName != "" && p.AgentName != agentName) {
				writeError(w, http.StatusBadRequest, ErrCodeValidationError,
					"conflicting target: payload and convenience fields specify different agents", nil)
				return false
			}
		}
	}

	// Resolve the target agent.
	var agent *store.Agent
	var err error
	if targetAgentID != "" {
		agent, err = s.store.GetAgent(ctx, targetAgentID)
	} else if targetAgentName != "" && projectID != "" {
		agent, err = s.store.GetAgentBySlug(ctx, projectID, targetAgentName)
	}
	// If the target cannot be resolved at authoring time (not found, no
	// identifier, or slug-based lookup), allow authoring. The agent may be
	// created between authoring and fire, and fire-time authorization is
	// the definitive check. Only store errors (not ErrNotFound) are
	// surfaced — those indicate an infrastructure problem, not a
	// user-visible denial.
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Agent doesn't exist yet — allow authoring; fire-time catches it.
			return true
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to resolve scheduled message target", nil)
		return false
	}
	if agent == nil {
		// No target identifiers provided or resolvable — allow authoring.
		return true
	}

	// Cross-project check: target must belong to this project.
	if agent.ProjectID != projectID {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"scheduled message target agent is not in this project", map[string]interface{}{
				"target_project":  agent.ProjectID,
				"request_project": projectID,
			})
		return false
	}

	// Reject scoped UATs: the scheduler persists only the creator ID.
	// At fire time the user is re-resolved without scope restrictions, so
	// admitting a scoped UAT here would silently discard its caveats.
	// Credential provenance that cannot be reconstructed fails closed.
	if IsScopedUserIdentity(identity) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"scoped access tokens cannot author scheduled messages: credential caveats cannot be preserved at fire time", nil)
		return false
	}

	// Fail-fast: preview whether the caller is authorized to message this
	// agent right now. The definitive check runs at fire time.
	allowed, reason := s.authorizeAgentMessage(ctx, identity, agent, false)
	if !allowed {
		slog.Warn("scheduled message authoring denied",
			"identity", identity.ID(), "identity_type", identity.Type(),
			"agent_id", agent.ID, "agent_slug", agent.Slug,
			"project_id", projectID, "reason", reason)
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"not authorized to message this agent", map[string]interface{}{
				"reason":     mapReasonToCode(reason),
				"agent_slug": agent.Slug,
			})
		return false
	}

	return true
}

// authorizeScheduledMessageFire authorizes a scheduled message event at fire
// time, immediately before dispatch. It re-resolves the creator identity from
// the store and calls authorizeAgentMessage with isSystemPlane=false.
//
// Returns (creatorIdentity, nil) on success. On denial or resolution failure
// it returns (nil, error) and the caller must fail the event with no dispatch.
//
// Denial codes use lower_snake_case for stable programmatic consumption.
func (s *Server) authorizeScheduledMessageFire(
	ctx context.Context,
	evt store.ScheduledEvent,
	agent *store.Agent,
) (Identity, error) {
	// Fail closed on empty creator — legacy or corrupted rows.
	if evt.CreatedBy == "" {
		return nil, fmt.Errorf("scheduled_message_no_creator: message event has no creator; cannot authorize at fire time")
	}

	// Target must be in the event's project.
	if agent.ProjectID != evt.ProjectID {
		return nil, fmt.Errorf("scheduled_message_cross_project: target agent %q is in project %q, event is in project %q",
			agent.ID, agent.ProjectID, evt.ProjectID)
	}

	// Resolve the creator. Try agent first (mirrors authorizeScheduledAgentCreate),
	// then user. The creator kind is not stored; we try both.
	var creatorIdentity Identity

	if creator, err := s.store.GetAgent(ctx, evt.CreatedBy); err == nil {
		// Creator is an agent.
		if creator.ProjectID != evt.ProjectID {
			return nil, fmt.Errorf("scheduled_message_creator_cross_project: creator agent %q is not in project %q",
				evt.CreatedBy, evt.ProjectID)
		}
		// Check creator agent is not soft-deleted. Agent soft-deletion sets
		// DeletedAt; hard deletion removes the record entirely (caught by
		// ErrNotFound above).
		if !creator.DeletedAt.IsZero() {
			return nil, fmt.Errorf("scheduled_message_creator_deleted: creator agent %q has been deleted",
				evt.CreatedBy)
		}

		role, additionalScopes := agentRoleAndScopes(creator)
		scopes := append(ScopesForRole(role), additionalScopes...)
		creatorIdentity = &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: creator.ID},
			ProjectID: creator.ProjectID,
			Scopes:    scopes,
		}}
	} else if !errors.Is(err, store.ErrNotFound) {
		// Store error — fail closed.
		return nil, fmt.Errorf("scheduled_message_store_error: failed to resolve creator agent %q: %w",
			evt.CreatedBy, err)
	} else {
		// Not an agent — try user.
		user, userErr := s.store.GetUser(ctx, evt.CreatedBy)
		if userErr != nil {
			if errors.Is(userErr, store.ErrNotFound) {
				return nil, fmt.Errorf("scheduled_message_creator_not_found: creator %q not found",
					evt.CreatedBy)
			}
			return nil, fmt.Errorf("scheduled_message_store_error: failed to resolve creator user %q: %w",
				evt.CreatedBy, userErr)
		}
		if user.Status != store.UserStatusActive {
			return nil, fmt.Errorf("scheduled_message_creator_inactive: creator user %q has status %s",
				evt.CreatedBy, user.Status)
		}
		creatorIdentity = NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "scheduler")
	}

	// Call the production messaging authorization choke point.
	// isSystemPlane=false: scheduled messages are request-derived.
	allowed, reason := s.authorizeAgentMessage(ctx, creatorIdentity, agent, false)
	if !allowed {
		return nil, fmt.Errorf("scheduled_message_denied: %s", reason)
	}

	return creatorIdentity, nil
}

// NOTE: markScheduledEventFailed was removed. Authorization denials now return
// errors directly from the handler, and the enclosing scheduler wrapper
// (fireEvent / executeSchedule) owns status recording — setting
// ScheduledEventFailed when the handler returns an error (R3 O-R3-1).
// This eliminates the dual-status-update race where markScheduledEventFailed
// set "failed" and the wrapper subsequently overwrote it (R2 O-R2-1).
