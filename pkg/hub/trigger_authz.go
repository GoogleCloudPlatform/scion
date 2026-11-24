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
	"reflect"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
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
// and provenance for a trigger authoring write (create, dispatch-affecting
// PATCH, or resume), mirroring revisionAuthorityCeiling for schedules.
func (s *Server) triggerRevisionAuthorityCeiling(w http.ResponseWriter, r *http.Request, projectID string, action Action) (store.EffectCeiling, store.AuthorityProvenance, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if s.authzService == nil {
		slog.ErrorContext(ctx, "trigger authoring: no authorization service", "project_id", projectID)
		InternalError(w)
		return store.EffectCeiling{}, store.AuthorityProvenance{}, false
	}
	ceiling, prov, err := s.authzService.sourceEffectCeiling(ctx, identity)
	if err != nil {
		cause, structural := ceilingDenyCauseForError(err)
		if !structural {
			slog.ErrorContext(ctx, "trigger authoring: effect ceiling lookup failed",
				"project_id", projectID, "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Unable to evaluate the credential's delegation ceiling; retry later", nil)
			return store.EffectCeiling{}, store.AuthorityProvenance{}, false
		}
		logAuthzDenial(r, identity, triggerResource(projectID), action,
			"effect ceiling denied: "+string(cause)+": "+err.Error())
		msg := ceilingSourceDenialMessage(cause)
		if cause == DenyCauseCeilingSourceNotAllowed {
			msg = "This credential kind cannot authorize trigger work"
		}
		writeForbiddenDenial(w, msg, DeniedByDelegationCeiling)
		return store.EffectCeiling{}, store.AuthorityProvenance{}, false
	}
	return ceiling, prov, true
}

// resolveDispatchAgentAuthoringCeiling returns the caller's effect ceiling and
// provenance for dispatch_agent authoring, reusing pre-resolved values when
// supplied by an HTTP handler and otherwise resolving them via
// authzService.sourceEffectCeiling so dispatchAgentAction.Validate is
// self-sufficient while keeping sourceEffectCeiling out of fire-core files.
func (s *Server) resolveDispatchAgentAuthoringCeiling(
	ctx context.Context,
	caller Identity,
	ceiling *store.EffectCeiling,
	prov *store.AuthorityProvenance,
) (store.EffectCeiling, store.AuthorityProvenance, error) {
	if ceiling != nil && prov != nil {
		return *ceiling, *prov, nil
	}
	if s.authzService == nil {
		return store.EffectCeiling{}, store.AuthorityProvenance{}, errors.New("dispatch_agent authoring: no authorization service")
	}
	c, p, err := s.authzService.sourceEffectCeiling(ctx, caller)
	if err != nil {
		cause, structural := ceilingDenyCauseForError(err)
		if !structural {
			return store.EffectCeiling{}, store.AuthorityProvenance{}, err
		}
		msg := ceilingSourceDenialMessage(cause)
		if cause == DenyCauseCeilingSourceNotAllowed {
			msg = "This credential kind cannot authorize trigger work"
		}
		return store.EffectCeiling{}, store.AuthorityProvenance{}, &dispatchAgentValidationError{
			status:   http.StatusForbidden,
			code:     ErrCodeForbidden,
			message:  msg,
			reason:   "effect ceiling denied: " + string(cause) + ": " + err.Error(),
			deniedBy: DeniedByDelegationCeiling,
		}
	}
	return c, p, nil
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
		if s.agentStanding(ctx, identity.ID()) != nil {
			return webhookEndpointAccess{reason: "agent not in good standing"}
		}
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

// authorizeTriggerMessageAuthoring validates a Stage 1 message action spec
// through the shared HTTP-free target rule (messageAction.Validate), enforcing
// self-only targeting for agent callers on github triggers (Stage 3 §8.1),
// updating spec with the authoritative AgentID and display AgentName, and
// returning the canonical JSON action_payload.
func (s *Server) authorizeTriggerMessageAuthoring(w http.ResponseWriter, r *http.Request, projectID, sourceKind string, spec *TriggerActionSpec) (string, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return "", false
	}

	raw, err := json.Marshal(MessageEventPayload{
		AgentID:   spec.AgentID,
		AgentName: spec.AgentName,
		Message:   spec.Message,
		Interrupt: spec.Interrupt,
		Plain:     spec.Plain,
		Wake:      spec.Wake,
	})
	if err != nil {
		InternalError(w)
		return "", false
	}

	action := &messageAction{srv: s}
	normalized, err := action.Validate(ctx, identity, projectID, string(raw))
	if err != nil {
		writeMessageAuthoringError(w, r, identity, err)
		return "", false
	}

	var resolved MessageEventPayload
	if err := json.Unmarshal([]byte(normalized), &resolved); err != nil {
		InternalError(w)
		return "", false
	}
	if sourceKind == store.TriggerSourceKindGitHub && isAgentCaller(ctx) && resolved.AgentID != identity.ID() {
		logAuthzDenial(r, identity, Resource{
			Type:       permissions.ResourceAgent,
			ID:         resolved.AgentID,
			ParentType: permissions.ResourceProject,
			ParentID:   projectID,
		}, ActionMessage, "github trigger message authoring: agent caller may only target itself")
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"agent callers may only target themselves on github triggers", nil)
		return "", false
	}
	spec.AgentID = resolved.AgentID
	spec.AgentName = resolved.AgentName
	return normalized, true
}

// authorizeTriggerDispatchAgentAuthoring validates a Stage 2 dispatch_agent
// action spec for triggers (§5.1, §5.1a, §5.5, §7.3) through the single
// self-sufficient dispatchAgentAction.validateForAuthoring entry point
// (replay-protected source, github-source gate, payload/template/label checks,
// authorizeAgentCreateDecision incl. standing, role caps, CanDelegate, and the
// caller's UAT-hop effect ceiling).
func (s *Server) authorizeTriggerDispatchAgentAuthoring(
	w http.ResponseWriter,
	r *http.Request,
	projectID string,
	sourceKind string,
	ep *store.WebhookEndpoint,
	spec *TriggerActionSpec,
	ceiling store.EffectCeiling,
	prov store.AuthorityProvenance,
) (string, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return "", false
	}

	raw, err := json.Marshal(DispatchAgentEventPayload{
		AgentName:     spec.AgentName,
		NameConflict:  spec.NameConflict,
		Template:      spec.Template,
		Task:          spec.Task,
		Branch:        spec.Branch,
		Labels:        spec.Labels,
		Ephemeral:     spec.Ephemeral,
		GithubToken:   spec.GithubToken,
		AgentRole:     spec.AgentRole,
		ReuseMessage:  spec.ReuseMessage,
		UntrustedVars: spec.UntrustedVars,
		NoAuth:        spec.NoAuth,
	})
	if err != nil {
		InternalError(w)
		return "", false
	}

	action := &dispatchAgentAction{srv: s}
	authCtx := withDispatchAgentAuthoringOpts(ctx, dispatchAgentAuthoringOpts{
		sourceKind: sourceKind,
		endpoint:   ep,
		ceiling:    &ceiling,
		provenance: &prov,
	})
	normalized, err := action.Validate(authCtx, identity, projectID, string(raw))
	if err != nil {
		var ve *dispatchAgentValidationError
		if errors.As(err, &ve) {
			if ve.status == http.StatusUnauthorized {
				Unauthorized(w)
				return "", false
			}
			if ve.status == http.StatusForbidden && ve.reason != "" {
				logAuthzDenial(r, identity, agentCreateResource(projectID), ActionCreate, ve.reason)
			}
			if ve.deniedBy != "" {
				writeForbiddenDenialCause(w, ve.message, ve.deniedBy, ve.cause)
				return "", false
			}
			writeError(w, ve.status, ve.code, ve.message, ve.details)
			return "", false
		}
		slog.ErrorContext(ctx, "trigger dispatch_agent authoring failed", "project_id", projectID, "error", err)
		InternalError(w)
		return "", false
	}

	return normalized, true
}

// authorizeTriggerGitHubCommentAuthoring enforces Stage 3 §7.1 / §8.1 authoring
// checks for a github.comment trigger action: source must be github, caller must
// hold github_trigger.comment (users via authz policy + UAT scope/boundary;
// agents via AgentRoleFull + delegation ceiling), replyInThread requires
// pull_request_review_comment match types, and githubCommentAction.Validate
// compiles and bounds the template and checks the project's GitHub association.
func (s *Server) authorizeTriggerGitHubCommentAuthoring(
	w http.ResponseWriter,
	r *http.Request,
	projectID string,
	sourceKind string,
	matchTypes []string,
	spec *TriggerActionSpec,
) (string, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return "", false
	}
	if sourceKind != store.TriggerSourceKindGitHub {
		ValidationError(w, "github.comment action requires a github trigger source", nil)
		return "", false
	}

	res := Resource{
		Type:       permissions.ResourceGitHubTrigger,
		ID:         projectID,
		ParentType: permissions.ResourceProject,
		ParentID:   projectID,
	}
	switch identity.Type() {
	case "user", "dev", "federated_user":
		userIdent, ok := identity.(UserIdentity)
		if !ok || s.authzService == nil {
			writeForbidden(w, "You don't have permission to post GitHub comments through triggers in this project")
			return "", false
		}
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(userIdent),
			Credential: credentialContextForIdentity(userIdent),
			Resource:   res,
			Action:     Action(permissions.ActionComment),
			Permission: githubTriggerCommentPermission,
		})
		if !decision.Allowed {
			logAuthzDenial(r, identity, res, Action(permissions.ActionComment), decision.Reason)
			writeForbidden(w, "You don't have permission to post GitHub comments through triggers in this project")
			return "", false
		}

	case "agent":
		// Design rev 1.12 §8.1: in Stage 3, github_trigger.comment has no agent
		// scope, so agents cannot author github.comment triggers.
		logAuthzDenial(r, identity, res, Action(permissions.ActionComment), "agent callers cannot author github.comment triggers")
		writeForbidden(w, "Agent callers cannot author github.comment triggers")
		return "", false

	default:
		logAuthzDenial(r, identity, res, Action(permissions.ActionComment), "identity type may not author github.comment triggers")
		writeForbidden(w, "")
		return "", false
	}

	if err := validateGitHubCommentReplyInThreadMatchTypes(spec.ReplyInThread, matchTypes); err != nil {
		ValidationError(w, err.Error(), nil)
		return "", false
	}

	raw, err := json.Marshal(GitHubCommentActionPayload{
		Body:          spec.Body,
		Target:        spec.Target,
		ReplyInThread: spec.ReplyInThread,
		UntrustedVars: spec.UntrustedVars,
	})
	if err != nil {
		InternalError(w)
		return "", false
	}

	action := &githubCommentAction{srv: s}
	normalized, err := action.Validate(ctx, identity, projectID, string(raw))
	if err != nil {
		switch {
		case errors.Is(err, errGitHubCommentNotAssociated):
			writeError(w, http.StatusConflict, ErrCodeGitHubNotAssociated,
				"project does not have an authorized GitHub App association", nil)
		case errors.Is(err, errGitHubCommentInvalid):
			ValidationError(w, err.Error(), nil)
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrInvalidInput):
			NotFound(w, "Project")
		default:
			writeErrorFromErr(w, err, "")
		}
		return "", false
	}
	return normalized, true
}

// authorizeTriggerExecAuthoring validates a Stage 2 P8 exec trigger action
// (ptone/scion#8241):
//   - requires hub.exec_schedules and hub.conduit enabled;
//   - refuses subject mode "reuse";
//   - requires a replay-protected source (400 source_not_replay_protected);
//   - refuses any non-exec action fields and any "${" in command (400 templating_not_allowed_in_argv);
//   - checks caller's effect ceiling allows agent.attach;
//   - delegates target resolution, no-existence-oracle 404, live agent.attach,
//     managed-runtime 422, and authoring rate limit to authorizeScheduledExecAuthoringOpts
//     with checkQuota=false (trigger exec uses its own per-trigger caps, never
//     the exec-schedule quotas).
func (s *Server) authorizeTriggerExecAuthoring(
	w http.ResponseWriter,
	r *http.Request,
	projectID string,
	sourceKind string,
	ep *store.WebhookEndpoint,
	subjectMode string,
	spec *TriggerActionSpec,
	ceiling store.EffectCeiling,
	prov store.AuthorityProvenance,
) (string, string, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return "", "", false
	}
	if !s.execSchedulesEnabled() {
		writeError(w, http.StatusBadRequest, ErrCodeExperimentDisabled,
			"exec triggers require hub.exec_schedules and hub.conduit experiments to be enabled", nil)
		return "", "", false
	}
	if subjectMode == "reuse" {
		ValidationError(w, "subject mode reuse is not supported for exec triggers", nil)
		return "", "", false
	}
	if !trigger.SourceReplayProtected(sourceKind, ep) {
		writeError(w, http.StatusBadRequest, "source_not_replay_protected",
			"exec triggers require a replay-protected source", nil)
		return "", "", false
	}
	// Allow-list of TriggerActionSpec fields valid on exec triggers: zeroing
	// only the allowed fields must leave the rest of the struct empty so any
	// future field added to TriggerActionSpec is rejected by default.
	stripped := *spec
	stripped.Type = ""
	stripped.AgentID = ""
	stripped.AgentName = ""
	stripped.Command = nil
	stripped.TimeoutSeconds = 0
	stripped.ContextAttributes = nil
	if !reflect.ValueOf(stripped).IsZero() {
		ValidationError(w, "exec triggers take agentId or agentName, command, timeoutSeconds, and contextAttributes only", nil)
		return "", "", false
	}
	if idx, ok := validateTriggerExecCommandNoTemplates(spec.Command); !ok {
		writeError(w, http.StatusBadRequest, "templating_not_allowed_in_argv",
			"template placeholders are not allowed in exec command argv",
			map[string]any{"field": "action.command", "index": idx})
		return "", "", false
	}
	p := &scheduledExecPayload{
		AgentID:           spec.AgentID,
		AgentName:         spec.AgentName,
		Command:           append([]string(nil), spec.Command...),
		TimeoutSeconds:    spec.TimeoutSeconds,
		Wake:              spec.Wake,
		ContextAttributes: append([]string(nil), spec.ContextAttributes...),
	}
	if err := p.validate(); err != nil {
		ValidationError(w, err.Error(), nil)
		return "", "", false
	}
	if !EffectCeilingAllows(
		uatHopEffectCeiling(prov.SourcePrincipalKind, prov.SourcePrincipalID, prov, ceiling),
		scheduledExecPermission,
		false,
	) {
		writeForbiddenDenial(w, "caller's delegation ceiling does not allow agent.attach", DeniedByDelegationCeiling)
		return "", "", false
	}
	if _, ok := s.authorizeScheduledExecAuthoringOpts(w, r, projectID, p, "", false); !ok {
		return "", "", false
	}
	encoded, err := p.encode()
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return "", "", false
	}
	spec.AgentID = p.AgentID
	spec.AgentName = p.AgentName
	return encoded, p.AgentID, true
}

var (
	errTriggerExecProjectCapExceeded = errors.New("project active exec trigger limit exceeded")
	errTriggerExecTargetCapExceeded  = errors.New("target agent active exec trigger limit exceeded")
)

// checkTriggerExecCapsInTx enforces P8's own per-trigger-exec authoring caps
// inside WithTx under LockTriggerQuota, independent of exec-schedule quotas.
func checkTriggerExecCapsInTx(
	ctx context.Context,
	tx store.Store,
	projectID string,
	targetAgentID string,
	alreadyCountedInProject bool,
	alreadyCountedOnTarget bool,
) error {
	projExecCount, err := tx.CountTriggers(ctx, store.TriggerFilter{
		ProjectID:   projectID,
		ActionTypes: []string{store.TriggerActionExec},
		Status:      store.TriggerStatusActive,
	})
	if err != nil {
		return err
	}
	if alreadyCountedInProject && projExecCount > 0 {
		projExecCount--
	}
	if projExecCount >= triggerExecMaxActivePerProject {
		return errTriggerExecProjectCapExceeded
	}
	if targetAgentID != "" {
		targetExecCount, err := tx.CountTriggers(ctx, store.TriggerFilter{
			ProjectID:     projectID,
			ActionTypes:   []string{store.TriggerActionExec},
			TargetAgentID: targetAgentID,
			Status:        store.TriggerStatusActive,
		})
		if err != nil {
			return err
		}
		if alreadyCountedOnTarget && targetExecCount > 0 {
			targetExecCount--
		}
		if targetExecCount >= triggerExecMaxActivePerTarget {
			return errTriggerExecTargetCapExceeded
		}
	}
	return nil
}

// triggerEventAccess is the outcome of triggerEventAccessDecision.
type triggerEventAccess struct {
	allowed           bool
	reason            string
	byPolicy          bool
	notVisibleToAgent bool
	wrongProject      bool
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
		dispatched, err := s.store.EventHasTriggerFireForResultRef(ctx, ev.ID, identity.ID())
		if err != nil && !errors.Is(err, store.ErrInvalidInput) {
			return triggerEventAccess{
				reason:   "trigger fire result_ref visibility lookup failed",
				storeErr: err,
			}
		}
		if dispatched {
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
		if ev != nil && ev.ProjectID != projectID {
			return triggerEventAccess{
				reason:       "trigger event does not belong to project",
				wrongProject: true,
			}
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
// project-level gate). It always enforces ev.ProjectID == projectID for every
// caller kind (writing 404 on mismatch) so callers cannot accidentally leak
// cross-project events. When an agent addresses an event it neither received on
// its own endpoint nor fired through a trigger it holds, it also writes 404
// identical to a missing event so event IDs cannot be probed (§10.2, §16.7).
func (s *Server) authorizeTriggerEventRow(w http.ResponseWriter, r *http.Request, projectID string, action Action, ev *store.TriggerEvent) bool {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return false
	}
	if ev != nil && ev.ProjectID != projectID {
		logAuthzDenial(r, identity, triggerResource(projectID), action, "trigger event does not belong to project")
		NotFound(w, "Trigger event")
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
	if d.notVisibleToAgent || d.wrongProject {
		NotFound(w, "Trigger event")
	} else if d.byPolicy {
		writeForbidden(w, "You don't have permission to access triggers in this project")
	} else {
		writeForbidden(w, "")
	}
	return false
}
