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
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

const (
	maxWebhookEndpointNameLen = 128
	maxWebhookEndpointDescLen = 1024
)

var (
	errWebhookEndpointLimitExceeded      = errors.New("webhook endpoint limit exceeded")
	errWebhookEndpointBoundLimitExceeded = errors.New("agent bound webhook endpoint limit exceeded")
	errWebhookEndpointNameExists         = errors.New("webhook endpoint name already exists")
	errDefaultTriggerNameExists          = errors.New("default trigger name already exists")
	errReplayProtectedTriggersExist      = errors.New("replay-protected triggers exist on webhook endpoint")
	errWebhookEndpointMissingSecret      = errors.New("webhook endpoint has no signing secret")
)

// WebhookEndpointDefaultTriggerSpec configures an optional default message
// trigger created atomically in the same transaction as the webhook endpoint
// (Stage 1 §10.3, §15 PR 5, rev 8.19; Stage 2 §4.3).
type WebhookEndpointDefaultTriggerSpec struct {
	AgentID   string `json:"agentId,omitempty"`
	AgentName string `json:"agentName,omitempty"`
	ToSelf    bool   `json:"toSelf,omitempty"`
	Message   string `json:"message"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Wake      bool   `json:"wake,omitempty"`
}

// CreateWebhookEndpointRequest is the request body for POST /webhook-endpoints.
type CreateWebhookEndpointRequest struct {
	Name                string                             `json:"name"`
	Description         string                             `json:"description,omitempty"`
	AuthMode            string                             `json:"auth,omitempty"`
	AllowProjectAgents  bool                               `json:"allowProjectAgents,omitempty"`
	SignatureMode       string                             `json:"signatureMode,omitempty"`
	MaxBodyBytes        int                                `json:"maxBodyBytes,omitempty"`
	RateLimit           *WebhookEndpointRateLimitSpec      `json:"rateLimit,omitempty"`
	ReplayWindowSeconds int                                `json:"replayWindowSeconds,omitempty"`
	BoundAgentID        string                             `json:"boundAgentId,omitempty"`
	BoundLifetime       string                             `json:"boundLifetime,omitempty"`
	DefaultTrigger      *WebhookEndpointDefaultTriggerSpec `json:"defaultTrigger,omitempty"`
	// Trigger is the Stage 2 §4.3 alias for DefaultTrigger.
	Trigger *WebhookEndpointDefaultTriggerSpec `json:"trigger,omitempty"`
}

// UpdateWebhookEndpointRequest is the request body for PATCH /webhook-endpoints/{id}
// (Stage 2 §4.4, §7.1, §7.3, §7.4).
type UpdateWebhookEndpointRequest struct {
	Description         *string                       `json:"description,omitempty"`
	Status              *string                       `json:"status,omitempty"`
	BoundAgentID        *string                       `json:"boundAgentId,omitempty"`
	BoundLifetime       *string                       `json:"boundLifetime,omitempty"`
	AuthMode            *string                       `json:"auth,omitempty"`
	AllowProjectAgents  *bool                         `json:"allowProjectAgents,omitempty"`
	SignatureMode       *string                       `json:"signatureMode,omitempty"`
	MaxBodyBytes        *int                          `json:"maxBodyBytes,omitempty"`
	RateLimit           *WebhookEndpointRateLimitSpec `json:"rateLimit,omitempty"`
	ReplayWindowSeconds *int                          `json:"replayWindowSeconds,omitempty"`
}

// handleWebhookEndpoints routes requests under
// /api/v1/projects/{projectId}/webhook-endpoints[/{id}[/rotate-secret]].
func (s *Server) handleWebhookEndpoints(w http.ResponseWriter, r *http.Request, projectID, endpointPath string) {
	s.requireExperiment(experiments.Triggers, func(w http.ResponseWriter, r *http.Request) {
		identity := GetIdentityFromContext(r.Context())
		if identity == nil {
			Unauthorized(w)
			return
		}
		if !checkAgentReadScope(w, r) {
			return
		}
		if agentIdentity := GetAgentIdentityFromContext(r.Context()); agentIdentity != nil {
			if agentIdentity.ProjectID() != projectID {
				Forbidden(w)
				return
			}
		}

		var (
			authzAction Action
			authoring   bool
			endpointID  string
			subAction   string
		)
		if endpointPath == "" {
			switch r.Method {
			case http.MethodGet:
				authzAction = ActionList
			case http.MethodPost:
				authzAction = ActionCreate
				authoring = true
			default:
				MethodNotAllowed(w, http.MethodGet, http.MethodPost)
				return
			}
		} else {
			if idPart, actionPart, hasSlash := strings.Cut(endpointPath, "/"); hasSlash {
				if actionPart != "rotate-secret" || idPart == "" || strings.Contains(actionPart, "/") {
					NotFound(w, "Webhook endpoint action")
					return
				}
				if r.Method != http.MethodPost {
					MethodNotAllowed(w, http.MethodPost)
					return
				}
				endpointID = idPart
				subAction = actionPart
				authzAction = ActionUpdate
			} else {
				endpointID = endpointPath
				switch r.Method {
				case http.MethodGet:
					authzAction = ActionRead
				case http.MethodPatch:
					authzAction = ActionUpdate
				case http.MethodDelete:
					authzAction = ActionDelete
				default:
					MethodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
					return
				}
			}
		}

		if authoring && !authorizeDeferredAuthoringCredential(w, r) {
			return
		}
		if !s.authorizeWebhookEndpointAccess(w, r, projectID, authzAction) {
			return
		}

		if endpointPath == "" {
			switch r.Method {
			case http.MethodGet:
				s.listWebhookEndpoints(w, r, projectID)
			case http.MethodPost:
				s.createWebhookEndpoint(w, r, projectID)
			}
			return
		}
		if subAction == "rotate-secret" {
			s.rotateWebhookEndpointSecret(w, r, projectID, endpointID)
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.getWebhookEndpoint(w, r, projectID, endpointID)
		case http.MethodPatch:
			s.updateWebhookEndpoint(w, r, projectID, endpointID)
		case http.MethodDelete:
			s.deleteWebhookEndpoint(w, r, projectID, endpointID)
		}
	})(w, r)
}

func validateWebhookEndpointName(raw string) (string, error) {
	return validateTriggerResourceName(raw, maxWebhookEndpointNameLen)
}

// resolveWebhookEndpointBinding validates and resolves (boundAgentRef,
// boundLifetime) for POST and PATCH webhook endpoint handlers (Stage 2 §4.1, §4.3).
//
// Invariants enforced:
//   - boundLifetime must be "agent" or "persistent" when boundAgentRef is set,
//     and empty when boundAgentRef is empty (400 invalid_binding).
//   - Agent callers may bind only themselves via boundAgentId="self" at creation
//     (403 bind_self_only for any other value; 403 on PATCH).
//   - User callers may bind any live (non-deleted) agent in projectID that they
//     hold agent.read on.
func (s *Server) resolveWebhookEndpointBinding(w http.ResponseWriter, r *http.Request, projectID, boundAgentRef, boundLifetime string, isPatch bool) (string, string, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return "", "", false
	}
	if isPatch && identity.Type() == "agent" {
		writeForbidden(w, "Only user callers may modify webhook endpoint agent bindings")
		return "", "", false
	}

	boundAgentRef = strings.TrimSpace(boundAgentRef)
	boundLifetime = strings.TrimSpace(boundLifetime)

	if boundAgentRef == "" && boundLifetime == "" {
		return "", "", true
	}
	if boundAgentRef == "" {
		writeError(w, http.StatusBadRequest, "invalid_binding", "boundLifetime must be empty when boundAgentId is not set", nil)
		return "", "", false
	}
	if boundLifetime != store.WebhookBoundLifetimeAgent && boundLifetime != store.WebhookBoundLifetimePersistent {
		writeError(w, http.StatusBadRequest, "invalid_binding", "boundLifetime must be 'agent' or 'persistent' when boundAgentId is set", nil)
		return "", "", false
	}

	if identity.Type() == "agent" {
		if boundAgentRef != "self" {
			writeError(w, http.StatusForbidden, "bind_self_only", "agent callers may only bind webhook endpoints to themselves (boundAgentId: \"self\")", nil)
			return "", "", false
		}
		agentIdentity := GetAgentIdentityFromContext(ctx)
		if agentIdentity == nil || strings.TrimSpace(agentIdentity.ID()) == "" {
			Unauthorized(w)
			return "", "", false
		}
		return agentIdentity.ID(), boundLifetime, true
	}

	if boundAgentRef == "self" {
		writeError(w, http.StatusBadRequest, "invalid_binding", "boundAgentId \"self\" requires an agent credential", nil)
		return "", "", false
	}

	ag, err := s.lookupProjectAgentByRef(ctx, projectID, boundAgentRef)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			NotFound(w, "Agent")
			return "", "", false
		}
		writeErrorFromErr(w, err, "")
		return "", "", false
	}
	if ag == nil || ag.ProjectID != projectID || !ag.DeletedAt.IsZero() {
		NotFound(w, "Agent")
		return "", "", false
	}

	userIdent, ok := identity.(UserIdentity)
	if !ok || s.authzService == nil {
		writeForbidden(w, "You don't have permission to bind this agent")
		return "", "", false
	}
	res := Resource{
		Type:       permissions.ResourceAgent,
		ID:         ag.ID,
		ParentType: permissions.ResourceProject,
		ParentID:   projectID,
	}
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(userIdent),
		Credential: credentialContextForIdentity(userIdent),
		Resource:   res,
		Action:     ActionRead,
		Permission: "agent.read",
	})
	if !decision.Allowed {
		logAuthzDenial(r, identity, res, ActionRead, "webhook endpoint bind: "+decision.Reason)
		writeForbidden(w, "You don't have permission to bind this agent")
		return "", "", false
	}
	return ag.ID, boundLifetime, true
}

func (s *Server) lookupProjectAgentByRef(ctx context.Context, projectID, ref string) (*store.Agent, error) {
	if _, err := uuid.Parse(ref); err == nil {
		ag, gErr := s.store.GetAgent(ctx, ref)
		if gErr == nil && ag != nil && ag.ProjectID == projectID {
			return ag, nil
		}
		if gErr != nil && !errors.Is(gErr, store.ErrNotFound) && !errors.Is(gErr, store.ErrInvalidInput) {
			return nil, gErr
		}
	}
	return s.store.GetAgentBySlug(ctx, projectID, ref)
}

func validateWebhookEndpointAuthMode(raw string) (string, error) {
	mode := strings.TrimSpace(raw)
	if mode == "" {
		return store.WebhookAuthModeHMAC, nil
	}
	switch mode {
	case store.WebhookAuthModeHMAC, store.WebhookAuthModeHubToken:
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported auth %q (supported: %s, %s)", mode, store.WebhookAuthModeHMAC, store.WebhookAuthModeHubToken)
	}
}

func validateWebhookEndpointSignatureMode(raw string) (string, error) {
	sigMode := strings.TrimSpace(raw)
	if sigMode == "" {
		return store.WebhookSignatureModeScionV1, nil
	}
	// P4b extends this switch with github_v1 and stripe_v1.
	switch sigMode {
	case store.WebhookSignatureModeScionV1:
		return sigMode, nil
	default:
		return "", fmt.Errorf("unsupported signatureMode %q (supported: %s)", sigMode, store.WebhookSignatureModeScionV1)
	}
}

func validateWebhookEndpointMaxBodyBytes(v int) (int, error) {
	if v == 0 {
		return store.DefaultWebhookMaxBodyBytes, nil
	}
	if v < 0 || v > trigger.MaxEndpointBodyBytes {
		return 0, fmt.Errorf("maxBodyBytes must be between 1 and %d", trigger.MaxEndpointBodyBytes)
	}
	return v, nil
}

func validateWebhookEndpointRateLimit(rl *WebhookEndpointRateLimitSpec) (int, int, error) {
	if rl == nil {
		return 0, 0, nil
	}
	maxRate := int(trigger.PerEndpointRatePerSecond)
	if rl.PerSecond < 0 || rl.PerSecond > maxRate {
		return 0, 0, fmt.Errorf("rateLimit.perSecond must be between 0 and %d", maxRate)
	}
	if rl.Burst < 0 || rl.Burst > trigger.PerEndpointBurst {
		return 0, 0, fmt.Errorf("rateLimit.burst must be between 0 and %d", trigger.PerEndpointBurst)
	}
	return rl.PerSecond, rl.Burst, nil
}

func validateWebhookEndpointReplayWindow(seconds int) (int, error) {
	if seconds == 0 {
		return store.DefaultWebhookReplayWindowSeconds, nil
	}
	if seconds < store.MinWebhookReplayWindowSeconds || seconds > store.MaxWebhookReplayWindowSeconds {
		return 0, fmt.Errorf("replayWindowSeconds must be between %d and %d",
			store.MinWebhookReplayWindowSeconds, store.MaxWebhookReplayWindowSeconds)
	}
	return seconds, nil
}

func validateWebhookEndpointStatus(raw string) (string, error) {
	st := strings.TrimSpace(raw)
	switch st {
	case store.WebhookEndpointStatusActive, store.WebhookEndpointStatusDisabled:
		return st, nil
	default:
		return "", fmt.Errorf("unsupported status %q (supported: %s, %s)",
			st, store.WebhookEndpointStatusActive, store.WebhookEndpointStatusDisabled)
	}
}

func (s *Server) createWebhookEndpoint(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	var req CreateWebhookEndpointRequest
	if !readTriggerRequestJSON(w, r, &req) {
		return
	}
	if req.DefaultTrigger != nil && req.Trigger != nil {
		ValidationError(w, "cannot specify both trigger and defaultTrigger", nil)
		return
	}
	defaultSpec := req.DefaultTrigger
	if defaultSpec == nil {
		defaultSpec = req.Trigger
	}

	// When defaultTrigger/trigger is requested, run the exact project-level
	// authority checks that POST /triggers runs (trigger.create plus
	// webhook_endpoint.read for source attachment) before looking up the project
	// or target agent.
	if defaultSpec != nil {
		if !s.authorizeTriggerAccess(w, r, projectID, ActionCreate) {
			return
		}
		if !s.authorizeWebhookEndpointAccess(w, r, projectID, ActionRead) {
			return
		}
	}

	name, err := validateWebhookEndpointName(req.Name)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	if err := validateTriggerResourceDescription(req.Description, maxWebhookEndpointDescLen); err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	authMode, err := validateWebhookEndpointAuthMode(req.AuthMode)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	sigMode, err := validateWebhookEndpointSignatureMode(req.SignatureMode)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	maxBodyBytes, err := validateWebhookEndpointMaxBodyBytes(req.MaxBodyBytes)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	ratePerSec, rateBurst, err := validateWebhookEndpointRateLimit(req.RateLimit)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	replayWindow, err := validateWebhookEndpointReplayWindow(req.ReplayWindowSeconds)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	boundAgentID, boundLifetime, ok := s.resolveWebhookEndpointBinding(w, r, projectID, req.BoundAgentID, req.BoundLifetime, false)
	if !ok {
		return
	}

	attr := newInitiatorAttribution(ctx)
	ownerKind := store.DelegationPrincipalUser
	if identity.Type() == "agent" {
		ownerKind = store.DelegationPrincipalAgent
	}

	secretVersion := 1
	if authMode == store.WebhookAuthModeHubToken {
		secretVersion = 0
	}

	ep := store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           projectID,
		Name:                name,
		Description:         req.Description,
		OwnerPrincipalKind:  ownerKind,
		OwnerPrincipalID:    identity.ID(),
		OwnerCredentialKind: attr.InitiatorCredentialKind,
		AuthMode:            authMode,
		AllowProjectAgents:  req.AllowProjectAgents,
		MaxBodyBytes:        maxBodyBytes,
		RatePerSecond:       ratePerSec,
		RateBurst:           rateBurst,
		ReplayWindowSeconds: replayWindow,
		ConfigVersion:       1,
		SignatureMode:       sigMode,
		SecretVersion:       secretVersion,
		Status:              store.WebhookEndpointStatusActive,
		BoundAgentID:        boundAgentID,
		BoundLifetime:       boundLifetime,
		CreatedBy:           identity.ID(),
	}

	var defaultTr *store.Trigger
	if defaultSpec != nil {
		dtAgentID := strings.TrimSpace(defaultSpec.AgentID)
		dtAgentName := strings.TrimSpace(defaultSpec.AgentName)
		actionSpec := TriggerActionSpec{
			Type:      store.TriggerActionMessage,
			Message:   defaultSpec.Message,
			Interrupt: defaultSpec.Interrupt,
			Wake:      defaultSpec.Wake,
		}
		if defaultSpec.ToSelf {
			if dtAgentID != "" || dtAgentName != "" {
				ValidationError(w, "defaultTrigger.toSelf cannot be combined with agentId or agentName", nil)
				return
			}
			// Resolve the calling agent strictly from the authenticated Hub
			// AgentIdentity so client environment variables cannot target another agent.
			agentIdentity := GetAgentIdentityFromContext(ctx)
			if agentIdentity == nil || strings.TrimSpace(agentIdentity.ID()) == "" {
				ValidationError(w, "defaultTrigger.toSelf requires an agent credential; specify agentId or agentName", nil)
				return
			}
			actionSpec.AgentID = agentIdentity.ID()
		} else if dtAgentID == "" && dtAgentName == "" && boundAgentID != "" {
			// Stage 2 §4.3: when trigger/defaultTrigger omits target agent on a
			// bound endpoint, default the target to the bound agent.
			actionSpec.AgentID = boundAgentID
		} else {
			actionSpec.AgentID = dtAgentID
			actionSpec.AgentName = dtAgentName
		}

		matchTypes, matchRules, ok := s.writeAndValidateTriggerMatch(w, ctx, nil, true, false)
		if !ok {
			return
		}
		subjectDialect, subjectRule, subjectMode, _, ok := s.writeAndValidateTriggerSubject(w, ctx, nil, false)
		if !ok {
			return
		}
		actionPayload, ok := s.authorizeTriggerMessageAuthoring(w, r, projectID, store.TriggerSourceKindWebhook, &actionSpec)
		if !ok {
			return
		}
		ceiling, _, ok := s.triggerRevisionAuthorityCeiling(w, r, projectID, ActionCreate)
		if !ok {
			return
		}
		defaultTr = &store.Trigger{
			ID:                   api.NewUUID(),
			ProjectID:            projectID,
			Name:                 name,
			SourceKind:           store.TriggerSourceKindWebhook,
			SourceID:             ep.ID,
			MatchTypes:           matchTypes,
			MatchRules:           matchRules,
			ActionType:           store.TriggerActionMessage,
			ActionPayload:        actionPayload,
			SubjectDialect:       subjectDialect,
			SubjectRule:          subjectRule,
			SubjectMode:          subjectMode,
			Status:               store.TriggerStatusActive,
			CreatedBy:            identity.ID(),
			InitiatorAttribution: attr,
			AuthorityCeiling:     ceiling,
		}
	}

	var (
		rawSecret string
		secretRow *store.Secret
	)
	if authMode != store.WebhookAuthModeHubToken {
		var err error
		rawSecret, err = generateWebhookEndpointSecret()
		if err != nil {
			InternalError(w)
			return
		}
		secretRow, err = s.newWebhookEndpointSecretRow(projectID, ep.ID, 1, rawSecret, identity.ID())
		if err != nil {
			InternalError(w)
			return
		}
	}

	audit := newWebhookEndpointAudit(ctx, mutationTypeWebhookEndpointCreate, ep.ID)
	afterSummary := map[string]any{
		"project_id":            projectID,
		"endpoint_id":           ep.ID,
		"name":                  ep.Name,
		"auth_mode":             ep.AuthMode,
		"allow_project_agents":  ep.AllowProjectAgents,
		"signature_mode":        ep.SignatureMode,
		"max_body_bytes":        ep.MaxBodyBytes,
		"rate_per_second":       ep.RatePerSecond,
		"rate_burst":            ep.RateBurst,
		"replay_window_seconds": ep.ReplayWindowSeconds,
		"secret_version":        ep.SecretVersion,
	}
	if ep.BoundAgentID != "" {
		afterSummary["bound_agent_id"] = ep.BoundAgentID
		afterSummary["bound_lifetime"] = ep.BoundLifetime
	}
	if defaultTr != nil {
		afterSummary["trigger_id"] = defaultTr.ID
	}
	afterBytes, _ := json.Marshal(afterSummary)
	audit.AfterSummary = string(afterBytes)

	fp := fixturePrincipalFromContext(ctx)
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		if lErr := tx.LockTriggerQuota(ctx, projectID); lErr != nil {
			return lErr
		}
		if sErr := stampWebhookEndpoint(ctx, tx, &ep, fp); sErr != nil {
			return sErr
		}
		count, cErr := tx.CountWebhookEndpoints(ctx, projectID)
		if cErr != nil {
			return cErr
		}
		if count >= store.MaxWebhookEndpointsPerProject {
			return errWebhookEndpointLimitExceeded
		}
		if ep.BoundAgentID != "" {
			boundCount, bErr := tx.CountBoundWebhookEndpoints(ctx, projectID, ep.BoundAgentID)
			if bErr != nil {
				return bErr
			}
			if boundCount >= store.MaxBoundWebhookEndpointsPerAgent {
				return errWebhookEndpointBoundLimitExceeded
			}
		}
		if defaultTr != nil {
			if sErr := stampTrigger(ctx, tx, defaultTr, fp); sErr != nil {
				return sErr
			}
			projCount, cErr := tx.CountTriggers(ctx, store.TriggerFilter{ProjectID: projectID})
			if cErr != nil {
				return cErr
			}
			if projCount >= store.MaxTriggersPerProject {
				return errTriggerProjectLimitExceeded
			}
		}
		if cErr := tx.CreateWebhookEndpoint(ctx, &ep); cErr != nil {
			if errors.Is(cErr, store.ErrAlreadyExists) {
				return errWebhookEndpointNameExists
			}
			return cErr
		}
		if secretRow != nil {
			if sErr := tx.CreateSecret(ctx, secretRow); sErr != nil {
				return sErr
			}
		}
		if defaultTr != nil {
			srcCount, cErr := tx.CountTriggers(ctx, store.TriggerFilter{
				ProjectID:  projectID,
				SourceKind: defaultTr.SourceKind,
				SourceID:   defaultTr.SourceID,
				Status:     store.TriggerStatusActive,
			})
			if cErr != nil {
				return cErr
			}
			if srcCount >= store.MaxActiveTriggersPerSource {
				return errTriggerSourceLimitExceeded
			}
			if cErr := tx.CreateTrigger(ctx, defaultTr); cErr != nil {
				if errors.Is(cErr, store.ErrAlreadyExists) {
					return errDefaultTriggerNameExists
				}
				return cErr
			}
		}
		return tx.CreateMutationAudit(ctx, audit)
	})
	if err != nil {
		if writeFixtureRootError(w, err) {
			return
		}
		switch {
		case errors.Is(err, errWebhookEndpointLimitExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("project has reached the maximum of %d webhook endpoints", store.MaxWebhookEndpointsPerProject), nil)
		case errors.Is(err, errWebhookEndpointBoundLimitExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("agent has reached the maximum of %d bound webhook endpoints", store.MaxBoundWebhookEndpointsPerAgent), nil)
		case errors.Is(err, errTriggerProjectLimitExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("project has reached the maximum of %d triggers", store.MaxTriggersPerProject), nil)
		case errors.Is(err, errTriggerSourceLimitExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("webhook endpoint has reached the maximum of %d active triggers", store.MaxActiveTriggersPerSource), nil)
		case errors.Is(err, errWebhookEndpointNameExists):
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"a webhook endpoint with this name already exists in this project", nil)
		case errors.Is(err, errDefaultTriggerNameExists), errors.Is(err, store.ErrAlreadyExists):
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"a trigger with this name already exists in this project", nil)
		default:
			writeErrorFromErr(w, err, "")
		}
		return
	}

	resp := WebhookEndpointCreatedResponse{
		WebhookEndpointResponse: s.buildWebhookEndpointResponse(ctx, ep),
		Secret:                  rawSecret,
	}
	if defaultTr != nil {
		trResp := buildTriggerResponse(s.newExecRevealer(r.Context()), *defaultTr)
		resp.TriggerID = trResp.ID
		resp.Trigger = &trResp
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) listWebhookEndpoints(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	query := r.URL.Query()
	filter := store.WebhookEndpointFilter{
		ProjectID: projectID,
		Name:      strings.TrimSpace(query.Get("name")),
		Status:    strings.TrimSpace(query.Get("status")),
	}
	if rawBound := strings.TrimSpace(query.Get("boundAgentId")); rawBound != "" {
		if _, err := uuid.Parse(rawBound); err == nil {
			filter.BoundAgentID = rawBound
		} else {
			ag, gErr := s.lookupProjectAgentByRef(ctx, projectID, rawBound)
			if gErr != nil && !errors.Is(gErr, store.ErrNotFound) && !errors.Is(gErr, store.ErrInvalidInput) {
				writeErrorFromErr(w, gErr, "")
				return
			}
			if ag == nil || ag.ProjectID != projectID {
				writeJSON(w, http.StatusOK, ListWebhookEndpointsResponse{
					Endpoints:  []WebhookEndpointResponse{},
					TotalCount: 0,
					ServerTime: time.Now().UTC(),
				})
				return
			}
			filter.BoundAgentID = ag.ID
		}
	}
	scope := ""
	if isAgentCaller(ctx) {
		identity := GetIdentityFromContext(ctx)
		if identity == nil || identity.ID() == "" {
			Unauthorized(w)
			return
		}
		filter.VisibleToAgentID = identity.ID()
		scope = "caller"
	}

	result, err := s.store.ListWebhookEndpoints(ctx, filter, listOptionsFromQuery(query))
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListWebhookEndpointsResponse{
		Scope:      scope,
		Endpoints:  s.buildWebhookEndpointResponses(ctx, result.Items),
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
		ServerTime: time.Now().UTC(),
	})
}

func (s *Server) loadWebhookEndpointForProject(w http.ResponseWriter, r *http.Request, projectID, endpointID string) (*store.WebhookEndpoint, bool) {
	ep, err := s.store.GetWebhookEndpoint(r.Context(), endpointID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			writeWebhookEndpointNotFound(w)
			return nil, false
		}
		writeErrorFromErr(w, err, "")
		return nil, false
	}
	if ep.ProjectID != projectID {
		writeWebhookEndpointNotFound(w)
		return nil, false
	}
	return ep, true
}

func (s *Server) getWebhookEndpoint(w http.ResponseWriter, r *http.Request, projectID, endpointID string) {
	ep, ok := s.loadWebhookEndpointForProject(w, r, projectID, endpointID)
	if !ok {
		return
	}
	if !s.authorizeWebhookEndpointRow(w, r, projectID, ActionRead, ep) {
		return
	}
	writeJSON(w, http.StatusOK, s.buildWebhookEndpointResponse(r.Context(), *ep))
}

func (s *Server) updateWebhookEndpoint(w http.ResponseWriter, r *http.Request, projectID, endpointID string) {
	ctx := r.Context()
	ep, ok := s.loadWebhookEndpointForProject(w, r, projectID, endpointID)
	if !ok {
		return
	}
	if !s.authorizeWebhookEndpointRow(w, r, projectID, ActionUpdate, ep) {
		return
	}

	var req UpdateWebhookEndpointRequest
	if !readTriggerRequestJSON(w, r, &req) {
		return
	}

	var (
		mask             store.WebhookEndpointFieldMask
		updated          = *ep
		resolvedAgent    string
		resolvedLifetime string
	)
	if req.Description != nil {
		if err := validateTriggerResourceDescription(*req.Description, maxWebhookEndpointDescLen); err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
		updated.Description = *req.Description
		mask.Description = true
	}
	if req.Status != nil {
		status, err := validateWebhookEndpointStatus(*req.Status)
		if err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
		updated.Status = status
		mask.Status = true
	}
	if req.AuthMode != nil {
		mode, err := validateWebhookEndpointAuthMode(*req.AuthMode)
		if err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
		updated.AuthMode = mode
		mask.AuthMode = true
	}
	if req.AllowProjectAgents != nil {
		updated.AllowProjectAgents = *req.AllowProjectAgents
		mask.AllowProjectAgents = true
	}
	if req.SignatureMode != nil {
		sigMode, err := validateWebhookEndpointSignatureMode(*req.SignatureMode)
		if err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
		updated.SignatureMode = sigMode
		mask.SignatureMode = true
	}
	if req.MaxBodyBytes != nil {
		maxBody, err := validateWebhookEndpointMaxBodyBytes(*req.MaxBodyBytes)
		if err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
		updated.MaxBodyBytes = maxBody
		mask.MaxBodyBytes = true
	}
	if req.RateLimit != nil {
		ratePerSec, rateBurst, err := validateWebhookEndpointRateLimit(req.RateLimit)
		if err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
		updated.RatePerSecond = ratePerSec
		updated.RateBurst = rateBurst
		mask.RatePerSecond = true
		mask.RateBurst = true
	}
	if req.ReplayWindowSeconds != nil {
		win, err := validateWebhookEndpointReplayWindow(*req.ReplayWindowSeconds)
		if err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
		updated.ReplayWindowSeconds = win
		mask.ReplayWindowSeconds = true
	}
	if req.BoundAgentID != nil || req.BoundLifetime != nil {
		targetAgent := ep.BoundAgentID
		targetLifetime := ep.BoundLifetime
		if req.BoundAgentID != nil {
			targetAgent = strings.TrimSpace(*req.BoundAgentID)
			if targetAgent == "" && req.BoundLifetime == nil {
				// Clearing boundAgentId without specifying boundLifetime unbinds cleanly.
				targetLifetime = ""
			}
		}
		if req.BoundLifetime != nil {
			targetLifetime = strings.TrimSpace(*req.BoundLifetime)
		}
		var valid bool
		resolvedAgent, resolvedLifetime, valid = s.resolveWebhookEndpointBinding(w, r, projectID, targetAgent, targetLifetime, true)
		if !valid {
			return
		}
		mask.Binding = true
	}

	if !mask.Description && !mask.Status && !mask.Binding &&
		!mask.AuthMode && !mask.AllowProjectAgents && !mask.SignatureMode &&
		!mask.MaxBodyBytes && !mask.RatePerSecond && !mask.RateBurst &&
		!mask.ReplayWindowSeconds {
		writeJSON(w, http.StatusOK, s.buildWebhookEndpointResponse(ctx, *ep))
		return
	}

	audit := newWebhookEndpointAudit(ctx, mutationTypeWebhookEndpointUpdate, ep.ID)

	// Precompute the row-independent delete permission decision BEFORE opening
	// WithTx so authzService.Decide never queries the root DB pool while the
	// transaction holds a connection / quota lock (prevents SQLite self-deadlock
	// and Postgres pool starvation, S-1).
	var deleteDecision webhookEndpointAccess
	if mask.Binding {
		deleteDecision = s.webhookEndpointAccessDecision(ctx, projectID, ActionDelete, nil)
	}

	var (
		errDeleteForbidden = errors.New("webhook_endpoint.delete required for agent-lifetime binding")
		errInvalidBinding  = errors.New("invalid webhook endpoint binding state")
	)
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		if lErr := tx.LockTriggerQuota(ctx, projectID); lErr != nil {
			return lErr
		}
		// Re-read under LockTriggerQuota so bindingChanged, the R1 delete check,
		// the per-agent cap, replay-protection checks, and the audit snapshot see
		// the authoritative state rather than a stale pre-transaction snapshot (O-A).
		current, gErr := tx.GetWebhookEndpoint(ctx, ep.ID)
		if gErr != nil {
			return gErr
		}
		if current.ProjectID != projectID {
			return store.ErrNotFound
		}

		next := *current
		if mask.Description {
			next.Description = updated.Description
		}
		if mask.Status {
			next.Status = updated.Status
		}
		if mask.AuthMode {
			if updated.AuthMode == store.WebhookAuthModeHMAC && current.SecretVersion <= 0 {
				return errWebhookEndpointMissingSecret
			}
			next.AuthMode = updated.AuthMode
		}
		if mask.AllowProjectAgents {
			next.AllowProjectAgents = updated.AllowProjectAgents
		}
		if mask.SignatureMode {
			next.SignatureMode = updated.SignatureMode
		}
		if mask.MaxBodyBytes {
			next.MaxBodyBytes = updated.MaxBodyBytes
		}
		if mask.RatePerSecond {
			next.RatePerSecond = updated.RatePerSecond
		}
		if mask.RateBurst {
			next.RateBurst = updated.RateBurst
		}
		if mask.ReplayWindowSeconds {
			next.ReplayWindowSeconds = updated.ReplayWindowSeconds
		}
		if mask.Binding {
			effectiveAgent := current.BoundAgentID
			effectiveLifetime := current.BoundLifetime
			if req.BoundAgentID != nil {
				effectiveAgent = resolvedAgent
				if effectiveAgent == "" && req.BoundLifetime == nil {
					effectiveLifetime = ""
				}
			}
			if req.BoundLifetime != nil {
				effectiveLifetime = resolvedLifetime
			}
			if (effectiveAgent == "") != (effectiveLifetime == "") {
				return errInvalidBinding
			}
			bindingChanged := effectiveAgent != current.BoundAgentID || effectiveLifetime != current.BoundLifetime
			if effectiveLifetime == store.WebhookBoundLifetimeAgent && bindingChanged && !deleteDecision.allowed {
				// Setting or changing an agent-lifetime binding delegates endpoint
				// deletion to whoever can delete the bound agent; require
				// webhook_endpoint.delete (the same decision as DELETE) so callers
				// with update-only cannot delete endpoints indirectly.
				return errDeleteForbidden
			}
			if effectiveAgent != "" && effectiveAgent != current.BoundAgentID {
				boundCount, bErr := tx.CountBoundWebhookEndpoints(ctx, projectID, effectiveAgent)
				if bErr != nil {
					return bErr
				}
				if boundCount >= store.MaxBoundWebhookEndpointsPerAgent {
					return errWebhookEndpointBoundLimitExceeded
				}
			}
			next.BoundAgentID = effectiveAgent
			next.BoundLifetime = effectiveLifetime
		}

		// Stage 2 §7.3: refuse any PATCH that would leave the endpoint without
		// replay protection while active agent-creating / command-executing
		// triggers depend on it.
		if !trigger.SourceReplayProtected(store.TriggerSourceKindWebhook, &next) {
			activeProtected, cErr := tx.CountTriggers(ctx, store.TriggerFilter{
				ProjectID:   projectID,
				SourceKind:  store.TriggerSourceKindWebhook,
				SourceID:    ep.ID,
				Status:      store.TriggerStatusActive,
				ActionTypes: []string{store.TriggerActionDispatchAgent, store.TriggerActionExec},
			})
			if cErr != nil {
				return cErr
			}
			if activeProtected > 0 {
				return errReplayProtectedTriggersExist
			}
		}

		beforeBytes, _ := json.Marshal(map[string]any{
			"endpoint_id":           current.ID,
			"project_id":            projectID,
			"status":                current.Status,
			"bound_agent_id":        current.BoundAgentID,
			"bound_lifetime":        current.BoundLifetime,
			"auth_mode":             current.AuthMode,
			"signature_mode":        current.SignatureMode,
			"allow_project_agents":  current.AllowProjectAgents,
			"max_body_bytes":        current.MaxBodyBytes,
			"rate_per_second":       current.RatePerSecond,
			"rate_burst":            current.RateBurst,
			"replay_window_seconds": current.ReplayWindowSeconds,
		})
		audit.BeforeSummary = string(beforeBytes)

		if uErr := tx.UpdateWebhookEndpoint(ctx, &next, mask); uErr != nil {
			return uErr
		}
		updated = next
		afterBytes, _ := json.Marshal(map[string]any{
			"endpoint_id":           updated.ID,
			"project_id":            projectID,
			"status":                updated.Status,
			"bound_agent_id":        updated.BoundAgentID,
			"bound_lifetime":        updated.BoundLifetime,
			"auth_mode":             updated.AuthMode,
			"signature_mode":        updated.SignatureMode,
			"allow_project_agents":  updated.AllowProjectAgents,
			"max_body_bytes":        updated.MaxBodyBytes,
			"rate_per_second":       updated.RatePerSecond,
			"rate_burst":            updated.RateBurst,
			"replay_window_seconds": updated.ReplayWindowSeconds,
			"config_version":        updated.ConfigVersion,
		})
		audit.AfterSummary = string(afterBytes)
		return tx.CreateMutationAudit(ctx, audit)
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeWebhookEndpointNotFound(w)
		case errors.Is(err, errDeleteForbidden):
			identity := GetIdentityFromContext(ctx)
			logAuthzDenial(r, identity, webhookEndpointResource(projectID), ActionDelete, deleteDecision.reason)
			if deleteDecision.byPolicy {
				writeForbidden(w, "You don't have permission to access webhook endpoints in this project")
			} else {
				writeForbidden(w, "")
			}
		case errors.Is(err, errInvalidBinding):
			writeError(w, http.StatusBadRequest, "invalid_binding", "boundAgentId and boundLifetime must either both be set or both be empty", nil)
		case errors.Is(err, errWebhookEndpointBoundLimitExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("agent has reached the maximum of %d bound webhook endpoints", store.MaxBoundWebhookEndpointsPerAgent), nil)
		case errors.Is(err, errWebhookEndpointMissingSecret):
			ValidationError(w, "cannot switch hub_token endpoint to hmac auth without a signing secret", nil)
		case errors.Is(err, errReplayProtectedTriggersExist):
			writeError(w, http.StatusConflict, "replay_protected_triggers_exist",
				"cannot remove replay protection while active dispatch_agent or exec triggers depend on this webhook endpoint", nil)
		default:
			writeErrorFromErr(w, err, "")
		}
		return
	}

	s.invalidateWebhookEndpointCache(ep.ID)
	writeJSON(w, http.StatusOK, s.buildWebhookEndpointResponse(ctx, updated))
}

func (s *Server) rotateWebhookEndpointSecret(w http.ResponseWriter, r *http.Request, projectID, endpointID string) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	ep, ok := s.loadWebhookEndpointForProject(w, r, projectID, endpointID)
	if !ok {
		return
	}
	if !s.authorizeWebhookEndpointRow(w, r, projectID, ActionUpdate, ep) {
		return
	}

	rawSecret, err := generateWebhookEndpointSecret()
	if err != nil {
		InternalError(w)
		return
	}

	rotatedAt := time.Now().UTC().Truncate(time.Microsecond)
	var rotated *store.WebhookEndpoint
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		if lErr := tx.LockTriggerQuota(ctx, projectID); lErr != nil {
			return lErr
		}
		cur, gErr := tx.GetWebhookEndpoint(ctx, ep.ID)
		if gErr != nil {
			return gErr
		}
		if cur.ProjectID != projectID {
			return store.ErrNotFound
		}
		nextVersion := cur.SecretVersion + 1
		secretRow, sErr := s.newWebhookEndpointSecretRow(projectID, cur.ID, nextVersion, rawSecret, identity.ID())
		if sErr != nil {
			return sErr
		}
		// S22: write new secret version n+1 BEFORE bumping secret_version.
		if cErr := tx.CreateSecret(ctx, secretRow); cErr != nil {
			return cErr
		}
		var rErr error
		rotated, rErr = tx.RotateWebhookEndpointSecret(ctx, projectID, cur.ID, nextVersion, rotatedAt)
		if rErr != nil {
			return rErr
		}
		audit := newWebhookEndpointAudit(ctx, mutationTypeWebhookEndpointRotateSecret, cur.ID)
		beforeBytes, _ := json.Marshal(map[string]any{
			"endpoint_id":    cur.ID,
			"project_id":     projectID,
			"secret_version": cur.SecretVersion,
		})
		audit.BeforeSummary = string(beforeBytes)
		afterBytes, _ := json.Marshal(map[string]any{
			"endpoint_id":    rotated.ID,
			"project_id":     projectID,
			"secret_version": rotated.SecretVersion,
		})
		audit.AfterSummary = string(afterBytes)
		return tx.CreateMutationAudit(ctx, audit)
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeWebhookEndpointNotFound(w)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	s.invalidateWebhookEndpointCache(ep.ID)

	resp := WebhookEndpointRotatedResponse{
		WebhookEndpointResponse: s.buildWebhookEndpointResponse(ctx, *rotated),
		Secret:                  rawSecret,
	}
	if rotated.SecretVersion >= 2 {
		resp.GraceWindowExpires = rotatedAt.Add(store.WebhookSecretRotationGraceWindow)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) deleteWebhookEndpoint(w http.ResponseWriter, r *http.Request, projectID, endpointID string) {
	ctx := r.Context()
	ep, ok := s.loadWebhookEndpointForProject(w, r, projectID, endpointID)
	if !ok {
		return
	}
	if !s.authorizeWebhookEndpointRow(w, r, projectID, ActionDelete, ep) {
		return
	}

	audit := newWebhookEndpointAudit(ctx, mutationTypeWebhookEndpointDelete, ep.ID)
	beforeBytes, _ := json.Marshal(map[string]any{
		"endpoint_id": ep.ID,
		"project_id":  projectID,
		"name":        ep.Name,
	})
	audit.BeforeSummary = string(beforeBytes)

	if err := s.store.WithTx(ctx, func(tx store.Store) error {
		if lErr := tx.LockTriggerQuota(ctx, projectID); lErr != nil {
			return lErr
		}
		pausedTriggerIDs, _, dErr := tx.DeleteWebhookEndpoint(ctx, projectID, ep.ID)
		if dErr != nil {
			return dErr
		}
		afterBytes, _ := json.Marshal(map[string]any{
			"project_id":           projectID,
			"paused_trigger_count": len(pausedTriggerIDs),
		})
		audit.AfterSummary = string(afterBytes)
		return tx.CreateMutationAudit(ctx, audit)
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeWebhookEndpointNotFound(w)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	s.invalidateWebhookEndpointCache(ep.ID)
	w.WriteHeader(http.StatusNoContent)
}
