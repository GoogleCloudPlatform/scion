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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// messageAuthoringCaller identifies which hub authoring surface is invoking
// the shared HTTP-free message target rule (Stage 1 §5.3, rev 8.24 / rev 8.32
// option a). It is unexported and derived strictly from the caller kind inside
// the hub—never from request input—so a trigger caller can never opt into
// schedule-style unresolved-target acceptance.
//
// The zero value is the strictest mode (messageAuthoringCallerTrigger) so an
// unset caller kind fails closed.
type messageAuthoringCaller int

const (
	// messageAuthoringCallerTrigger is the strict trigger authoring mode used
	// by messageAction.Validate and trigger CRUD handlers: unresolved targets
	// return 404, unreadable targets are masked as 404 (byte-identical to
	// nonexistent), and cross-project targets are audited.
	messageAuthoringCallerTrigger messageAuthoringCaller = iota
	// messageAuthoringCallerScheduleNew is a schedule or scheduled-event create
	// naming a new target: unresolved targets and unreadable new targets are
	// accepted at authoring and refused at fire time.
	messageAuthoringCallerScheduleNew
	// messageAuthoringCallerScheduleExisting is a schedule resume or update
	// keeping its stored target: unresolved targets are accepted, while an
	// unauthorized existing target is refused with 403.
	messageAuthoringCallerScheduleExisting
)

// messageAuthoringError is the HTTP-free denial returned by
// validateMessageAuthoringTarget so HTTP adapters (schedule and trigger
// handlers) map it to exact status codes and response bodies without
// duplicating target-resolution or masking logic.
type messageAuthoringError struct {
	httpStatus        int
	code              string
	message           string
	details           map[string]interface{}
	notFoundResource  string
	authzDenialReason string
	authzDenialTarget *store.Agent
}

func (e *messageAuthoringError) Error() string {
	if e == nil {
		return ""
	}
	if e.notFoundResource != "" {
		return e.notFoundResource + " not found"
	}
	return e.message
}

// writeMessageAuthoringError maps an error returned by
// validateMessageAuthoringTarget (or messageAction.Validate) onto the exact
// HTTP error response expected by schedule and trigger callers.
func writeMessageAuthoringError(w http.ResponseWriter, r *http.Request, identity Identity, err error) {
	var authErr *messageAuthoringError
	if !errors.As(err, &authErr) {
		InternalError(w)
		return
	}
	if authErr.authzDenialTarget != nil && authErr.authzDenialReason != "" && r != nil && identity != nil {
		logAuthzDenial(r, identity, agentResource(authErr.authzDenialTarget), ActionMessage, authErr.authzDenialReason)
	}
	if authErr.notFoundResource != "" {
		NotFound(w, authErr.notFoundResource)
		return
	}
	switch authErr.httpStatus {
	case http.StatusUnauthorized:
		Unauthorized(w)
	case http.StatusInternalServerError:
		if authErr.message != "" {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, authErr.message, nil)
		} else {
			InternalError(w)
		}
	default:
		code := authErr.code
		if code == "" {
			code = ErrCodeValidationError
		}
		writeError(w, authErr.httpStatus, code, authErr.message, authErr.details)
	}
}

// scheduledTargetMode says whether an authoring request names a new target
// or re-authors a schedule whose target is already set.
type scheduledTargetMode int

const (
	// scheduledTargetNew: the caller names the target (event or schedule
	// create).
	scheduledTargetNew scheduledTargetMode = iota
	// scheduledTargetExisting: the caller resumes a schedule, or updates it
	// without changing its target, which it can already see in the stored
	// payload.
	scheduledTargetExisting
)

// authorizeScheduledMessageAuthoring validates a scheduled-message event at
// authoring time for a caller naming a new target (event and schedule
// create). See authorizeScheduledMessageTarget.
func (s *Server) authorizeScheduledMessageAuthoring(
	w http.ResponseWriter,
	r *http.Request,
	projectID string,
	rawPayload string,
	agentID string,
	agentName string,
) bool {
	return s.authorizeScheduledMessageTarget(w, r, scheduledTargetNew, projectID, rawPayload, agentID, agentName)
}

// authorizeScheduledMessageReauthoring validates a schedule resume, or an
// update that keeps the schedule's target: the same checks as authoring, but
// the target is the existing schedule's, so a target the caller may not
// message is refused (403) rather than treated like an unknown agent.
func (s *Server) authorizeScheduledMessageReauthoring(
	w http.ResponseWriter,
	r *http.Request,
	projectID string,
	rawPayload string,
) bool {
	return s.authorizeScheduledMessageTarget(w, r, scheduledTargetExisting, projectID, rawPayload, "", "")
}

// scheduledPayloadTargetChanged reports whether a replacement message
// payload names another target (agentId or agentName) than the stored one.
// A payload that does not decode names no target.
func scheduledPayloadTargetChanged(stored, replacement string) bool {
	target := func(raw string) MessageEventPayload {
		var p MessageEventPayload
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), &p)
		}
		return MessageEventPayload{AgentID: p.AgentID, AgentName: p.AgentName}
	}
	return target(stored) != target(replacement)
}

// authorizeScheduledMessageTarget validates a scheduled-message event at
// authoring time (create / update / resume). It resolves the target agent from
// convenience fields or raw payload and rejects when:
//   - the request's credential may not author scheduled work
//     (authorizeScheduleAuthoringCredential), checked before the target is
//     resolved,
//   - the target agent is in a different project from projectID and
//     cross-project messaging is disabled, or
//   - the caller is not currently authorized to message the target
//     (fail-fast preview — the definitive check runs again at fire time).
//
// When the caller names a new target (scheduledTargetNew), a target the
// caller cannot read and may not message is treated like an unknown agent
// ID: authoring is accepted, and the fire-time check refuses. An update or
// resume (scheduledTargetExisting) keeps the refusal below.
//
// An admitted revision records its author's ceiling, and each fire
// re-checks the recorded authority under resolveRecordedAuthority
// (authorizeMessageFire).
//
// Returns true when authoring is allowed; writes the HTTP error response and
// returns false when denied.
func (s *Server) authorizeScheduledMessageTarget(
	w http.ResponseWriter,
	r *http.Request,
	mode scheduledTargetMode,
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

	var callerKind messageAuthoringCaller
	switch mode {
	case scheduledTargetNew:
		callerKind = messageAuthoringCallerScheduleNew
	case scheduledTargetExisting:
		callerKind = messageAuthoringCallerScheduleExisting
	default:
		// Fail closed to the strictest mode if an unknown mode is ever passed.
		callerKind = messageAuthoringCallerTrigger
	}
	if _, err := s.validateMessageAuthoringTarget(ctx, identity, projectID, callerKind, rawPayload, agentID, agentName); err != nil {
		writeMessageAuthoringError(w, r, identity, err)
		return false
	}
	return true
}

// validateMessageAuthoringTarget is the single HTTP-free message authoring
// target rule shared by schedule handlers (authorizeScheduledMessageTarget)
// and trigger authoring (messageAction.Validate). callerKind is derived inside
// the hub and never from request input (Stage 1 §5.3, rev 8.24 / rev 8.32).
func (s *Server) validateMessageAuthoringTarget(
	ctx context.Context,
	identity Identity,
	projectID string,
	callerKind messageAuthoringCaller,
	rawPayload string,
	agentID string,
	agentName string,
) (string, error) {
	if identity == nil {
		return "", &messageAuthoringError{httpStatus: http.StatusUnauthorized, message: "unauthorized"}
	}
	if s == nil || s.store == nil {
		return "", &messageAuthoringError{httpStatus: http.StatusInternalServerError, message: "server not configured"}
	}

	var isTrigger bool
	switch callerKind {
	case messageAuthoringCallerScheduleNew, messageAuthoringCallerScheduleExisting:
		isTrigger = false
	default:
		// Fail closed to the strictest trigger mode for messageAuthoringCallerTrigger
		// and any out-of-range value (S2C-I1).
		isTrigger = true
	}

	var (
		parsed          MessageEventPayload
		targetAgentID   string
		targetAgentName string
	)
	if isTrigger {
		if err := json.Unmarshal([]byte(rawPayload), &parsed); err != nil {
			return "", &messageAuthoringError{
				httpStatus: http.StatusBadRequest,
				code:       ErrCodeValidationError,
				message:    "invalid message action payload",
			}
		}
		if strings.TrimSpace(parsed.Message) == "" {
			return "", &messageAuthoringError{
				httpStatus: http.StatusBadRequest,
				code:       ErrCodeValidationError,
				message:    "action.message is required for message actions",
			}
		}
		if len(parsed.Message) > maxTriggerActionMessageBytes {
			return "", &messageAuthoringError{
				httpStatus: http.StatusBadRequest,
				code:       ErrCodeValidationError,
				message:    "action.message exceeds maximum length of 16384 bytes",
			}
		}
		targetAgentID = strings.TrimSpace(parsed.AgentID)
		targetAgentName = strings.TrimSpace(parsed.AgentName)
		if targetAgentID == "" && targetAgentName == "" {
			return "", &messageAuthoringError{
				httpStatus: http.StatusBadRequest,
				code:       ErrCodeValidationError,
				message:    "action.agentId or action.agentName is required for message actions",
			}
		}
	} else {
		// Resolve the target from the raw payload first, since that is what gets
		// persisted and fired. Fall back to convenience fields only when the
		// payload is empty or does not contain a target. This matches the
		// storage priority in handlers_scheduled_events.go.
		if rawPayload != "" {
			var p MessageEventPayload
			if err := json.Unmarshal([]byte(rawPayload), &p); err == nil {
				targetAgentID = p.AgentID
				targetAgentName = p.AgentName
			}
		}
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
					return "", &messageAuthoringError{
						httpStatus: http.StatusBadRequest,
						code:       ErrCodeValidationError,
						message:    "conflicting target: payload and convenience fields specify different agents",
					}
				}
			}
		}
	}

	// Resolve the target agent.
	var (
		agent *store.Agent
		err   error
	)
	if targetAgentID != "" {
		agent, err = s.store.GetAgent(ctx, targetAgentID)
	} else if targetAgentName != "" && projectID != "" {
		agent, err = s.store.GetAgentBySlug(ctx, projectID, targetAgentName)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		if isTrigger {
			slog.ErrorContext(ctx, "trigger message authoring: target lookup failed", "project_id", projectID, "error", err)
			return "", &messageAuthoringError{httpStatus: http.StatusInternalServerError}
		}
		return "", &messageAuthoringError{
			httpStatus: http.StatusInternalServerError,
			code:       ErrCodeInternalError,
			message:    "failed to resolve scheduled message target",
		}
	}

	if isTrigger {
		if err != nil || agent == nil || !agent.DeletedAt.IsZero() ||
			(targetAgentID != "" && targetAgentName != "" && targetAgentName != agent.Slug) {
			return "", &messageAuthoringError{httpStatus: http.StatusNotFound, notFoundResource: "Agent"}
		}
	} else {
		// If the target cannot be resolved at schedule authoring time (not
		// found, no identifier, or slug-based lookup), allow authoring. The
		// agent may be created between authoring and fire, and fire-time
		// authorization is the definitive check.
		if errors.Is(err, store.ErrNotFound) || agent == nil {
			return rawPayload, nil
		}
	}

	// Schedule new-target masking: a target the caller cannot read, and may
	// not message, is treated exactly like an unknown agent ID: authoring is
	// accepted and the fire-time check refuses it. This runs before any answer
	// that would describe the target.
	if callerKind == messageAuthoringCallerScheduleNew && !s.scheduledTargetReadable(ctx, identity, agent) {
		if allowed, _, _ := s.authorizeAgentMessage(ctx, identity, agent, false); !allowed {
			slog.InfoContext(ctx, "scheduled target not readable; treated as unknown",
				"identity", identity.ID(), "identity_type", identity.Type(),
				"agent_id", agent.ID, "project_id", projectID)
			return rawPayload, nil
		}
	}

	// Cross-project messaging check and audit.
	isCrossProject := agent.ProjectID != projectID
	if isCrossProject {
		ops := s.GetOperationalSettings()
		if ops == nil || !ops.CrossProjectMessagingEnabled() {
			if isTrigger && !s.scheduledTargetReadable(ctx, identity, agent) {
				return "", &messageAuthoringError{httpStatus: http.StatusNotFound, notFoundResource: "Agent"}
			}
			msg := "cross-project scheduled messages require cross-project messaging to be enabled"
			if isTrigger {
				msg = "cross-project trigger messages require cross-project messaging to be enabled"
			}
			return "", &messageAuthoringError{
				httpStatus: http.StatusForbidden,
				code:       ErrCodeForbidden,
				message:    msg,
				details: map[string]interface{}{
					"code": string(MessageDenialCrossProjectScheduledDisabled),
				},
			}
		}
		auditAction := "scheduled_author"
		auditSurface := "scheduled_message"
		if isTrigger {
			auditAction = "trigger_author"
			auditSurface = "trigger_message"
		}
		LogCrossProjectDecision(CrossProjectAuditEntry{
			Timestamp:        time.Now(),
			Action:           auditAction,
			SenderID:         identity.ID(),
			SenderProjectID:  projectID,
			RecipientID:      agent.ID,
			RecipientProject: agent.ProjectID,
			CrossProject:     true,
			Surface:          auditSurface,
		})
	}

	// Fail-fast: preview whether the caller is authorized to message this
	// agent right now. The definitive check runs at fire time.
	allowed, reason, _ := s.authorizeAgentMessage(ctx, identity, agent, false)
	if !allowed {
		if isTrigger && !s.scheduledTargetReadable(ctx, identity, agent) {
			return "", &messageAuthoringError{
				httpStatus:        http.StatusNotFound,
				notFoundResource:  "Agent",
				authzDenialReason: "trigger message authoring: " + reason,
				authzDenialTarget: agent,
			}
		}
		if isTrigger {
			slog.WarnContext(ctx, "trigger message authoring denied",
				"identity", identity.ID(), "identity_type", identity.Type(),
				"agent_id", agent.ID, "agent_slug", agent.Slug,
				"project_id", projectID, "reason", reason,
				"cross_project", isCrossProject)
		} else {
			slog.Warn("scheduled message authoring denied",
				"identity", identity.ID(), "identity_type", identity.Type(),
				"agent_id", agent.ID, "agent_slug", agent.Slug,
				"project_id", projectID, "reason", reason,
				"cross_project", isCrossProject)
		}
		return "", &messageAuthoringError{
			httpStatus: http.StatusForbidden,
			code:       ErrCodeForbidden,
			message:    "not authorized to message this agent",
			details: map[string]interface{}{
				"reason":     mapReasonToCode(reason),
				"agent_slug": agent.Slug,
			},
		}
	}

	if isTrigger {
		encoded, err := json.Marshal(MessageEventPayload{
			AgentID:   agent.ID,
			AgentName: agent.Slug,
			Message:   parsed.Message,
			Interrupt: parsed.Interrupt,
			Plain:     parsed.Plain,
			Wake:      parsed.Wake,
		})
		if err != nil {
			return "", &messageAuthoringError{httpStatus: http.StatusInternalServerError}
		}
		return string(encoded), nil
	}
	return rawPayload, nil
}

// scheduledTargetReadable reports whether identity may read the target
// agent: an agent caller only within its own project, any other caller when
// the authorization service allows agent read. A denied decision, for any
// reason, counts as not readable.
func (s *Server) scheduledTargetReadable(ctx context.Context, identity Identity, agent *store.Agent) bool {
	if agentIdent, ok := identity.(AgentIdentity); ok {
		return agentIdent.ProjectID() == agent.ProjectID
	}
	return s.authzService.CheckAccess(ctx, identity, agentResource(agent), ActionRead).Allowed
}

// scheduledMessagePermission is the permission a deferred message fire
// exercises.
const scheduledMessagePermission = "agent.message"

// authorizeMessageFire authorizes a deferred message fire immediately before
// dispatch under auth and identity: the authority and identity
// resolveRecordedAuthority returned for the fire. CreatedBy is history only and
// is never read for authority.
//
// It refuses when the target agent is nil or deleted, when the target is in
// another project and cross-project messaging is disabled, when a user
// principal's revision ceiling does not allow agent.message, or when
// authorizeAgentMessage (isSystemPlane=false) denies identity the send. On a
// refusal the caller must refuse the fire with no dispatch.
//
// The revision ceiling is read for user principals only. agent.message is
// not covered by any agent scope, so an agent's bounded ceiling never lists
// it; an agent principal's send is decided by the agent message rule
// (authorizeAgentToAgent), as a live send by that agent is.
func (s *Server) authorizeMessageFire(
	ctx context.Context,
	projectID string,
	auth FireAuthority,
	identity Identity,
	agent *store.Agent,
) error {
	// Fail closed if the target agent was deleted between authoring and fire.
	if agent == nil {
		return fmt.Errorf("%s: target agent is nil; may have been deleted since scheduling",
			MessageDenialScheduledTargetDeleted)
	}

	// Phase 5 D3: Cross-project scheduled messages are allowed when the
	// Hub feature is enabled. projectID is the sender's project;
	// the target agent may be in a different project.
	if agent.ProjectID != projectID {
		ops := s.GetOperationalSettings()
		if ops == nil || !ops.CrossProjectMessagingEnabled() {
			return fmt.Errorf("%s: cross-project messaging is disabled; target agent %q is in project %q, event is in project %q",
				MessageDenialCrossProjectScheduledDisabled, agent.ID, agent.ProjectID, projectID)
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
	// isSystemPlane=false: deferred messages are request-derived.
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
