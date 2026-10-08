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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// authorizeScheduledMessageAuthoring validates a scheduled-message event at
// authoring time (create / update). It resolves the target agent from
// convenience fields or raw payload and rejects when:
//   - the request's credential may not author scheduled work
//     (authorizeScheduleAuthoringCredential: every user access token is
//     refused), checked before the target is resolved,
//   - the target agent is in a different project from projectID and
//     cross-project messaging is disabled, or
//   - the caller is not currently authorized to message the target
//     (fail-fast preview — the definitive check runs again at fire time).
//
// An admitted revision records its author's ceiling, and each fire
// re-checks the recorded authority under resolveScheduledAuthority
// (authorizeScheduledMessageFire).
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
	// The credential gate runs before the target is resolved, so a refused
	// credential is refused whether or not the target exists.
	if !authorizeScheduleAuthoringCredential(w, r) {
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

	// Phase 5 D3: Cross-project scheduled message support.
	// The event remains owned/administered in the sender's project; its target
	// agent/project are separate immutable fields.
	isCrossProject := agent.ProjectID != projectID
	if isCrossProject {
		// Cross-project scheduled message: validate the Hub feature is enabled
		// and the sender has cross-project messaging authorization.
		ops := s.GetOperationalSettings()
		if ops == nil || !ops.CrossProjectMessagingEnabled() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"cross-project scheduled messages require cross-project messaging to be enabled", map[string]interface{}{
					"code": string(MessageDenialCrossProjectScheduledDisabled),
				})
			return false
		}
		// Persist cross-project context with the scheduled event for
		// fire-time reauthorization.
		LogCrossProjectDecision(CrossProjectAuditEntry{
			Timestamp:        time.Now(),
			Action:           "scheduled_author",
			SenderID:         identity.ID(),
			SenderProjectID:  projectID,
			RecipientID:      agent.ID,
			RecipientProject: agent.ProjectID,
			CrossProject:     true,
			Surface:          "scheduled_message",
		})
	}

	// Fail-fast: preview whether the caller is authorized to message this
	// agent right now. The definitive check runs at fire time.
	allowed, reason, _ := s.authorizeAgentMessage(ctx, identity, agent, false)
	if !allowed {
		slog.Warn("scheduled message authoring denied",
			"identity", identity.ID(), "identity_type", identity.Type(),
			"agent_id", agent.ID, "agent_slug", agent.Slug,
			"project_id", projectID, "reason", reason,
			"cross_project", isCrossProject)
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"not authorized to message this agent", map[string]interface{}{
				"reason":     mapReasonToCode(reason),
				"agent_slug": agent.Slug,
			})
		return false
	}

	return true
}

// scheduledMessagePermission is the permission a scheduled message fire
// exercises; scheduledEventPermissions maps the message event type to it.
const scheduledMessagePermission = "agent.message"

// authorizeScheduledMessageFire authorizes a scheduled message event at fire
// time, immediately before dispatch, under auth and identity: the authority
// and identity resolveScheduledAuthority returned for evt. CreatedBy is
// history only and is never read for authority.
//
// It refuses when the target agent is nil or deleted, when the target is in
// another project and cross-project messaging is disabled, when a user
// principal's revision ceiling does not allow agent.message, or when
// authorizeAgentMessage (isSystemPlane=false) denies identity the send. On a
// refusal the caller must fail the event with no dispatch.
//
// The revision ceiling is read for user principals only. agent.message is
// not covered by any agent scope, so an agent's bounded ceiling never lists
// it; an agent principal's send is decided by the agent message rule
// (authorizeAgentToAgent), as a live send by that agent is.
func (s *Server) authorizeScheduledMessageFire(
	ctx context.Context,
	evt store.ScheduledEvent,
	auth ScheduledAuthority,
	identity Identity,
	agent *store.Agent,
) error {
	// Fail closed if the target agent was deleted between scheduling and fire.
	if agent == nil {
		return fmt.Errorf("%s: target agent is nil; may have been deleted since scheduling",
			MessageDenialScheduledTargetDeleted)
	}

	// Phase 5 D3: Cross-project scheduled messages are allowed when the
	// Hub feature is enabled. The event's project is the sender's project;
	// the target agent may be in a different project.
	if agent.ProjectID != evt.ProjectID {
		ops := s.GetOperationalSettings()
		if ops == nil || !ops.CrossProjectMessagingEnabled() {
			return fmt.Errorf("%s: cross-project messaging is disabled; target agent %q is in project %q, event is in project %q",
				MessageDenialCrossProjectScheduledDisabled, agent.ID, agent.ProjectID, evt.ProjectID)
		}
		// Check if target has been deleted since scheduling.
		if !agent.DeletedAt.IsZero() {
			return fmt.Errorf("%s: target agent %q has been deleted since scheduling",
				MessageDenialScheduledTargetDeleted, agent.ID)
		}
	}

	if identity == nil {
		return fmt.Errorf("%w: no resolved identity", errScheduledAuthorityDenied)
	}
	switch auth.PrincipalKind {
	case store.DelegationPrincipalUser:
		if !EffectCeilingAllows(auth.Ceiling, scheduledMessagePermission, false) {
			return fmt.Errorf("%w: revision ceiling does not allow %s", errScheduledAuthorityDenied, scheduledMessagePermission)
		}
	case store.DelegationPrincipalAgent:
	default:
		return fmt.Errorf("%w: unsupported principal kind %q", errScheduledAuthorityDenied, auth.PrincipalKind)
	}

	// The production messaging authorization choke point.
	// isSystemPlane=false: scheduled messages are request-derived.
	allowed, reason, _ := s.authorizeAgentMessage(ctx, identity, agent, false)
	if !allowed {
		return fmt.Errorf("scheduled_message_denied: %s", reason)
	}
	return nil
}

// NOTE: markScheduledEventFailed was removed. Authorization denials now return
// errors directly from the handler, and the enclosing scheduler wrapper
// (fireEvent / executeSchedule) owns status recording — setting
// ScheduledEventFailed when the handler returns an error (R3 O-R3-1).
// This eliminates the dual-status-update race where markScheduledEventFailed
// set "failed" and the wrapper subsequently overwrote it (R2 O-R2-1).
