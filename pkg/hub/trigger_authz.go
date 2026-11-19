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
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

const (
	// agentTriggerEditAllowed mirrors agentScheduleEditAllowed: agent callers
	// may create, pause, resume, and delete triggers they hold, but do not
	// PATCH trigger definitions.
	agentTriggerEditAllowed = false

	// maxTriggerActionMessageBytes bounds the message template text stored on a
	// trigger's message action (§5.8).
	maxTriggerActionMessageBytes = 16 * 1024

	webhookEndpointNotFoundMessage = "webhook endpoint not found"

	mutationTypeWebhookEndpointCreate = "webhook_endpoint_create"
	mutationTypeWebhookEndpointDelete = "webhook_endpoint_delete"
	mutationTypeTriggerPause          = "trigger_pause"
	mutationTypeTriggerDelete         = "trigger_delete"
)

func newWebhookEndpointAudit(ctx context.Context, mutationType, endpointID string) *store.MutationAuditRecord {
	record := &store.MutationAuditRecord{
		MutationType: mutationType,
		TargetType:   permissions.ResourceWebhookEndpoint,
		TargetID:     endpointID,
		Timestamp:    time.Now(),
	}
	auditActorFromContext(ctx).ApplyActor(record)
	applyHubActorFallback(record)
	return record
}

func newTriggerAudit(ctx context.Context, mutationType, triggerID string) *store.MutationAuditRecord {
	record := &store.MutationAuditRecord{
		MutationType: mutationType,
		TargetType:   permissions.ResourceTrigger,
		TargetID:     triggerID,
		Timestamp:    time.Now(),
	}
	auditActorFromContext(ctx).ApplyActor(record)
	applyHubActorFallback(record)
	return record
}

// triggerRevisionAuthorityCeiling resolves the caller's frozen effect ceiling
// for a trigger authoring write (create, dispatch-affecting PATCH, or resume),
// mirroring revisionAuthorityCeiling for schedules.
func (s *Server) triggerRevisionAuthorityCeiling(w http.ResponseWriter, r *http.Request, projectID string, action Action) (store.EffectCeiling, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if s.authzService == nil {
		slog.ErrorContext(ctx, "trigger authoring: no authorization service", "project_id", projectID)
		InternalError(w)
		return store.EffectCeiling{}, false
	}
	ceiling, _, err := s.authzService.sourceEffectCeiling(ctx, identity)
	if err != nil {
		cause, structural := ceilingDenyCauseForError(err)
		if !structural {
			slog.ErrorContext(ctx, "trigger authoring: effect ceiling lookup failed",
				"project_id", projectID, "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Unable to evaluate the credential's delegation ceiling; retry later", nil)
			return store.EffectCeiling{}, false
		}
		logAuthzDenial(r, identity, triggerResource(projectID), action,
			"effect ceiling denied: "+string(cause)+": "+err.Error())
		msg := ceilingSourceDenialMessage(cause)
		if cause == DenyCauseCeilingSourceNotAllowed {
			msg = "This credential kind cannot authorize trigger work"
		}
		writeForbiddenDenial(w, msg, DeniedByDelegationCeiling)
		return store.EffectCeiling{}, false
	}
	return ceiling, true
}

func writeWebhookEndpointNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, ErrCodeNotFound, webhookEndpointNotFoundMessage, nil)
}

func webhookEndpointResource(projectID string) Resource {
	return Resource{
		Type:       permissions.ResourceWebhookEndpoint,
		ParentType: permissions.ResourceProject,
		ParentID:   projectID,
	}
}

func triggerResource(projectID string) Resource {
	return Resource{
		Type:       permissions.ResourceTrigger,
		ParentType: permissions.ResourceProject,
		ParentID:   projectID,
	}
}

// webhookEndpointAccess is the outcome of webhookEndpointAccessDecision.
type webhookEndpointAccess struct {
	allowed         bool
	reason          string
	byPolicy        bool
	notOwnedByAgent bool
}

// webhookEndpointAccessDecision decides whether the identity in ctx may
// perform action on webhook endpoints in projectID (ep == nil for the
// project-level router gate, non-nil for a specific endpoint row).
//
// Users are decided via webhook_endpoint.<action>. Agents are admitted on
// ActionCreate (they become the endpoint owner) and on rows where
// ep.OwnerPrincipalKind == "agent" && ep.OwnerPrincipalID == agent.ID().
func (s *Server) webhookEndpointAccessDecision(ctx context.Context, projectID string, action Action, ep *store.WebhookEndpoint) webhookEndpointAccess {
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		return webhookEndpointAccess{reason: "no identity"}
	}
	switch identity.Type() {
	case "agent":
		if ep == nil || action == ActionCreate {
			return webhookEndpointAccess{allowed: true}
		}
		if ep.OwnerPrincipalKind != store.DelegationPrincipalAgent || ep.OwnerPrincipalID == "" || ep.OwnerPrincipalID != identity.ID() {
			return webhookEndpointAccess{
				reason:          "agent does not own this webhook endpoint",
				notOwnedByAgent: true,
			}
		}
		return webhookEndpointAccess{allowed: true}

	case "user", "dev", "federated_user":
		userIdent, ok := identity.(UserIdentity)
		if !ok {
			return webhookEndpointAccess{reason: "invalid user identity"}
		}
		if s.authzService == nil {
			return webhookEndpointAccess{reason: "authorization service is uninitialized"}
		}
		permissionID := "webhook_endpoint." + string(action)
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(userIdent),
			Credential: credentialContextForIdentity(userIdent),
			Resource:   webhookEndpointResource(projectID),
			Action:     action,
			Permission: permissionID,
		})
		if !decision.Allowed {
			return webhookEndpointAccess{reason: decision.Reason, byPolicy: true}
		}
		return webhookEndpointAccess{allowed: true}

	default:
		return webhookEndpointAccess{reason: "identity type may not access webhook endpoints"}
	}
}

func (s *Server) authorizeWebhookEndpointAccess(w http.ResponseWriter, r *http.Request, projectID string, action Action) bool {
	return s.authorizeWebhookEndpointRow(w, r, projectID, action, nil)
}

// authorizeWebhookEndpointRow decides action on one webhook endpoint row (nil
// for the project-level gate). When an agent addresses an endpoint owned by
// another principal, it writes 404 identical to a missing endpoint so endpoint
// IDs cannot be probed (§10.2).
func (s *Server) authorizeWebhookEndpointRow(w http.ResponseWriter, r *http.Request, projectID string, action Action, ep *store.WebhookEndpoint) bool {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return false
	}
	if ep != nil && identity.Type() != "agent" {
		return true
	}
	d := s.webhookEndpointAccessDecision(r.Context(), projectID, action, ep)
	if d.allowed {
		return true
	}
	logAuthzDenial(r, identity, webhookEndpointResource(projectID), action, d.reason)
	if d.notOwnedByAgent {
		writeWebhookEndpointNotFound(w)
	} else if d.byPolicy {
		writeForbidden(w, "You don't have permission to access webhook endpoints in this project")
	} else {
		writeForbidden(w, "")
	}
	return false
}

// triggerRow wraps a Trigger's attribution for triggerAccessDecision.
type triggerRow struct {
	attribution store.InitiatorAttribution
	edit        bool
}

func triggerRowOf(tr *store.Trigger) *triggerRow {
	return &triggerRow{attribution: tr.InitiatorAttribution}
}

func triggerEditRowOf(tr *store.Trigger) *triggerRow {
	return &triggerRow{attribution: tr.InitiatorAttribution, edit: true}
}

// triggerAccess is the outcome of triggerAccessDecision.
type triggerAccess struct {
	allowed        bool
	reason         string
	byPolicy       bool
	notHeldByAgent bool
}

// triggerAccessDecision mirrors scheduledEventAccessDecision for triggers:
// users are decided via trigger.<action>; agents manage only rows whose
// current initiator principal is that agent, and do not PATCH definitions.
func (s *Server) triggerAccessDecision(ctx context.Context, projectID string, action Action, row *triggerRow) triggerAccess {
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		return triggerAccess{reason: "no identity"}
	}
	switch identity.Type() {
	case "agent":
		if row == nil || action == ActionCreate {
			return triggerAccess{allowed: true}
		}
		if row.edit && !agentTriggerEditAllowed {
			return triggerAccess{reason: "agent callers do not edit trigger definitions"}
		}
		if !s.agentHoldsScheduledRow(identity, row.attribution) {
			return triggerAccess{
				reason:         "agent does not hold authority for this trigger",
				notHeldByAgent: true,
			}
		}
		return triggerAccess{allowed: true}

	case "user", "dev", "federated_user":
		userIdent, ok := identity.(UserIdentity)
		if !ok {
			return triggerAccess{reason: "invalid user identity"}
		}
		if s.authzService == nil {
			return triggerAccess{reason: "authorization service is uninitialized"}
		}
		permissionID := "trigger." + string(action)
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(userIdent),
			Credential: credentialContextForIdentity(userIdent),
			Resource:   triggerResource(projectID),
			Action:     action,
			Permission: permissionID,
		})
		if !decision.Allowed {
			return triggerAccess{reason: decision.Reason, byPolicy: true}
		}
		return triggerAccess{allowed: true}

	default:
		return triggerAccess{reason: "identity type may not access triggers"}
	}
}

func (s *Server) authorizeTriggerAccess(w http.ResponseWriter, r *http.Request, projectID string, action Action) bool {
	return s.authorizeTriggerRow(w, r, projectID, action, nil)
}

func (s *Server) authorizeTriggerRow(w http.ResponseWriter, r *http.Request, projectID string, action Action, row *triggerRow) bool {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return false
	}
	if row != nil && identity.Type() != "agent" {
		return true
	}
	d := s.triggerAccessDecision(r.Context(), projectID, action, row)
	if d.allowed {
		return true
	}
	logAuthzDenial(r, identity, triggerResource(projectID), action, d.reason)
	if d.byPolicy {
		writeForbidden(w, "You don't have permission to access triggers in this project")
	} else if d.notHeldByAgent {
		writeForbidden(w, "Trigger is not visible to this agent (agent callers can only access triggers they hold)")
	} else {
		writeForbidden(w, "")
	}
	return false
}

// resolveTriggerWebhookSource resolves source.id (by UUID or endpoint name in
// projectID) and checks webhookEndpointAccessDecision(..., ActionRead, ep) for
// the caller. Missing, cross-project, disabled, and unreadable/unowned
// endpoints all return the identical 404 "webhook endpoint not found" so
// endpoint IDs and names cannot be probed (§10.2).
func (s *Server) resolveTriggerWebhookSource(w http.ResponseWriter, r *http.Request, projectID, idOrName string) (*store.WebhookEndpoint, bool) {
	ctx := r.Context()
	idOrName = strings.TrimSpace(idOrName)
	if idOrName == "" {
		BadRequest(w, "source.id is required")
		return nil, false
	}

	var ep *store.WebhookEndpoint
	var err error
	if _, parseErr := uuid.Parse(idOrName); parseErr == nil {
		ep, err = s.store.GetWebhookEndpoint(ctx, idOrName)
		if errors.Is(err, store.ErrNotFound) || (err == nil && ep != nil && ep.ProjectID != projectID) {
			// Also fall back to name lookup in case an endpoint was named like a UUID.
			ep, err = s.store.GetWebhookEndpointByName(ctx, projectID, idOrName)
		}
	} else {
		ep, err = s.store.GetWebhookEndpointByName(ctx, projectID, idOrName)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.ErrorContext(ctx, "trigger source lookup failed", "project_id", projectID, "error", err)
		InternalError(w)
		return nil, false
	}
	if err != nil || ep == nil || ep.ProjectID != projectID || ep.Status != store.WebhookEndpointStatusActive {
		writeWebhookEndpointNotFound(w)
		return nil, false
	}

	d := s.webhookEndpointAccessDecision(ctx, projectID, ActionRead, ep)
	if !d.allowed {
		if identity := GetIdentityFromContext(ctx); identity != nil {
			logAuthzDenial(r, identity, webhookEndpointResource(projectID), ActionRead, "trigger source resolve: "+d.reason)
		}
		writeWebhookEndpointNotFound(w)
		return nil, false
	}
	return ep, true
}

// authorizeTriggerMessageAuthoring validates a Stage 1 message action spec,
// resolves the target agent (storing authoritative AgentID and display
// AgentName per §4.2), checks live messaging authorization for the caller, and
// returns the canonical JSON action_payload.
func (s *Server) authorizeTriggerMessageAuthoring(w http.ResponseWriter, r *http.Request, projectID string, spec *TriggerActionSpec) (string, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return "", false
	}

	if strings.TrimSpace(spec.Message) == "" {
		ValidationError(w, "action.message is required for message actions", nil)
		return "", false
	}
	if len(spec.Message) > maxTriggerActionMessageBytes {
		ValidationError(w, "action.message exceeds maximum length of 16384 bytes", nil)
		return "", false
	}

	agentID := strings.TrimSpace(spec.AgentID)
	agentName := strings.TrimSpace(spec.AgentName)
	var target *store.Agent
	var err error
	switch {
	case agentID != "":
		target, err = s.store.GetAgent(ctx, agentID)
	case agentName != "":
		target, err = s.store.GetAgentBySlug(ctx, projectID, agentName)
	default:
		ValidationError(w, "action.agentId or action.agentName is required for message actions", nil)
		return "", false
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.ErrorContext(ctx, "trigger message authoring: target lookup failed", "project_id", projectID, "error", err)
		InternalError(w)
		return "", false
	}
	if err != nil || target == nil || !target.DeletedAt.IsZero() ||
		(agentID != "" && agentName != "" && agentName != target.Slug) {
		NotFound(w, "Agent")
		return "", false
	}

	isCrossProject := target.ProjectID != projectID
	if isCrossProject {
		ops := s.GetOperationalSettings()
		if ops == nil || !ops.CrossProjectMessagingEnabled() {
			if !s.scheduledTargetReadable(ctx, identity, target) {
				NotFound(w, "Agent")
				return "", false
			}
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"cross-project trigger messages require cross-project messaging to be enabled", map[string]interface{}{
					"code": string(MessageDenialCrossProjectScheduledDisabled),
				})
			return "", false
		}
		LogCrossProjectDecision(CrossProjectAuditEntry{
			Timestamp:        time.Now(),
			Action:           "trigger_author",
			SenderID:         identity.ID(),
			SenderProjectID:  projectID,
			RecipientID:      target.ID,
			RecipientProject: target.ProjectID,
			CrossProject:     true,
			Surface:          "trigger_message",
		})
	}

	allowed, reason, _ := s.authorizeAgentMessage(ctx, identity, target, false)
	if !allowed {
		if !s.scheduledTargetReadable(ctx, identity, target) {
			logAuthzDenial(r, identity, agentResource(target), ActionMessage, "trigger message authoring: "+reason)
			NotFound(w, "Agent")
			return "", false
		}
		slog.WarnContext(ctx, "trigger message authoring denied",
			"identity", identity.ID(), "identity_type", identity.Type(),
			"agent_id", target.ID, "agent_slug", target.Slug,
			"project_id", projectID, "reason", reason,
			"cross_project", isCrossProject)
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"not authorized to message this agent", map[string]interface{}{
				"reason":     mapReasonToCode(reason),
				"agent_slug": target.Slug,
			})
		return "", false
	}

	spec.AgentID = target.ID
	spec.AgentName = target.Slug
	encoded, err := json.Marshal(MessageEventPayload{
		AgentID:   target.ID,
		AgentName: target.Slug,
		Message:   spec.Message,
		Interrupt: spec.Interrupt,
		Plain:     spec.Plain,
		Wake:      spec.Wake,
	})
	if err != nil {
		InternalError(w)
		return "", false
	}
	return string(encoded), true
}

// triggerEventAccess is the outcome of triggerEventAccessDecision.
type triggerEventAccess struct {
	allowed           bool
	reason            string
	byPolicy          bool
	notVisibleToAgent bool
	storeErr          error
}

// triggerEventAccessDecision decides whether the identity in ctx may list or
// read trigger events in projectID (§10.2, F1):
//   - Users are decided via trigger.list / trigger.read.
//   - Agents are admitted at the project-level gate (ev == nil) for list/read,
//     and on a specific event row (ev != nil) only when either:
//     1. ev's webhook endpoint passes webhookEndpointAccessDecision(read) for
//     the agent, OR
//     2. a trigger_fires row joins ev to a trigger currently held by the agent
//     (AgentHasFiredTriggerForEvent).
func (s *Server) triggerEventAccessDecision(ctx context.Context, projectID string, action Action, ev *store.TriggerEvent) triggerEventAccess {
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		return triggerEventAccess{reason: "no identity"}
	}
	switch identity.Type() {
	case "agent":
		if action != ActionList && action != ActionRead {
			return triggerEventAccess{reason: "agent callers may only list or read trigger events"}
		}
		if ev == nil {
			return triggerEventAccess{allowed: true}
		}
		if ev.ProjectID != projectID {
			return triggerEventAccess{
				reason:            "trigger event does not belong to project",
				notVisibleToAgent: true,
			}
		}
		if ev.SourceKind == store.TriggerSourceKindWebhook && ev.SourceID != "" {
			ep, err := s.store.GetWebhookEndpoint(ctx, ev.SourceID)
			if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrInvalidInput) {
				return triggerEventAccess{
					reason:   "webhook endpoint lookup failed",
					storeErr: err,
				}
			}
			if err == nil && ep != nil && ep.ProjectID == projectID {
				if s.webhookEndpointAccessDecision(ctx, projectID, ActionRead, ep).allowed {
					return triggerEventAccess{allowed: true}
				}
			}
		}
		fired, err := s.store.AgentHasFiredTriggerForEvent(ctx, ev.ID, identity.ID())
		if err != nil && !errors.Is(err, store.ErrInvalidInput) {
			return triggerEventAccess{
				reason:   "trigger fire visibility lookup failed",
				storeErr: err,
			}
		}
		if fired {
			return triggerEventAccess{allowed: true}
		}
		return triggerEventAccess{
			reason:            "trigger event is not visible to agent",
			notVisibleToAgent: true,
		}

	case "user", "dev", "federated_user":
		userIdent, ok := identity.(UserIdentity)
		if !ok {
			return triggerEventAccess{reason: "invalid user identity"}
		}
		if s.authzService == nil {
			return triggerEventAccess{reason: "authorization service is uninitialized"}
		}
		permissionID := "trigger." + string(action)
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(userIdent),
			Credential: credentialContextForIdentity(userIdent),
			Resource:   triggerResource(projectID),
			Action:     action,
			Permission: permissionID,
		})
		if !decision.Allowed {
			return triggerEventAccess{reason: decision.Reason, byPolicy: true}
		}
		return triggerEventAccess{allowed: true}

	default:
		return triggerEventAccess{reason: "identity type may not access trigger events"}
	}
}

func (s *Server) authorizeTriggerEventAccess(w http.ResponseWriter, r *http.Request, projectID string, action Action) bool {
	return s.authorizeTriggerEventRow(w, r, projectID, action, nil)
}

// authorizeTriggerEventRow decides action on one trigger event row (nil for the
// project-level gate). When an agent addresses an event it neither received on
// its own endpoint nor fired through a trigger it holds, it writes 404
// identical to a missing event so event IDs cannot be probed (§10.2, §16.7).
func (s *Server) authorizeTriggerEventRow(w http.ResponseWriter, r *http.Request, projectID string, action Action, ev *store.TriggerEvent) bool {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return false
	}
	if ev != nil && identity.Type() != "agent" {
		return true
	}
	d := s.triggerEventAccessDecision(r.Context(), projectID, action, ev)
	if d.allowed {
		return true
	}
	if d.storeErr != nil {
		slog.ErrorContext(r.Context(), "trigger event authorization lookup failed",
			"project_id", projectID, "error", d.storeErr)
		InternalError(w)
		return false
	}
	logAuthzDenial(r, identity, triggerResource(projectID), action, d.reason)
	if d.notVisibleToAgent {
		NotFound(w, "Trigger event")
	} else if d.byPolicy {
		writeForbidden(w, "You don't have permission to access triggers in this project")
	} else {
		writeForbidden(w, "")
	}
	return false
}
