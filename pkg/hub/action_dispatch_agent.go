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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/gitref"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/labels"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggertemplate"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

const (
	dispatchAgentPermission = "agent.create"

	// DispatchAgentNameConflictSuffix appends "-<4 hex>" on agent name collision (default).
	DispatchAgentNameConflictSuffix = "suffix"
	// DispatchAgentNameConflictSkip skips the fire when the agent name is already in use.
	DispatchAgentNameConflictSkip = "skip"

	// DispatchAgentGitHubTokenUserPreferred keeps today's user-preferred token precedence.
	DispatchAgentGitHubTokenUserPreferred = "user_preferred"
	// DispatchAgentGitHubTokenInstallation drops user PATs in favor of the GitHub App installation token (P1c-3).
	DispatchAgentGitHubTokenInstallation = "installation"

	// Hub-applied trigger provenance labels (§5.4). Reserved at authoring; hub wins at fire.
	triggerLabelKeyTrigger       = "trigger"
	triggerLabelKeyTriggerEvent  = "trigger_event"
	triggerLabelKeyTriggerSource = "trigger_source"

	hubAppliedTriggerLabelCount = 3
	maxNameCollisionRetries     = 3
)

var reservedTriggerLabelKeys = map[string]bool{
	triggerLabelKeyTrigger:       true,
	triggerLabelKeyTriggerEvent:  true,
	triggerLabelKeyTriggerSource: true,
}

var errDispatchAgentProjectLookup = errors.New("dispatch_agent project lookup failed")

type dispatchAgentProjectNotFoundError struct {
	projectID string
}

func (e *dispatchAgentProjectNotFoundError) Error() string {
	return fmt.Sprintf("project %q no longer exists", e.projectID)
}

// dispatchAgentValidationError carries a structured authoring-time error from
// dispatchAgentAction.Validate so HTTP handlers can emit the exact §5.1 error
// code and details without echoing sensitive values.
type dispatchAgentValidationError struct {
	status  int
	code    string
	message string
	details map[string]any
}

func (e *dispatchAgentValidationError) Error() string {
	return e.message
}

// dispatchAgentAction implements FireAction and FirePayloadValidator for
// deferred "dispatch_agent" fires executed through createAgentHeadless.
type dispatchAgentAction struct {
	srv *Server
}

type dispatchAgentAuthorized struct {
	payload        DispatchAgentEventPayload
	auth           FireAuthority
	identity       Identity
	creatorName    string
	edgeCeiling    store.EffectCeiling
	edgeProvenance store.AuthorityProvenance
	resolvedRole   headlessResolvedRole
	projectName    string
	sourceName     string
	signatureMode  string
}

func (a *dispatchAgentAction) Type() string {
	return "dispatch_agent"
}

func (a *dispatchAgentAction) Permission() string {
	return dispatchAgentPermission
}

func (a *dispatchAgentAction) Detaches(_ hubfire.FireRequest) bool {
	return false
}

// Validate parses, validates, and normalizes a trigger dispatch_agent payload
// at authoring time (Stage 2 design §5.1). It is not sufficient on its own to
// authorize trigger authoring: authorizeTriggerDispatchAgentAuthoring holds
// the replay-protected-source, github-source, subject-skip, and caller
// delegation-ceiling checks. Schedule authoring handlers continue authorizing
// through authorizeScheduledDispatchAgentAuthoring.
func (a *dispatchAgentAction) Validate(ctx context.Context, caller Identity, projectID string, rawPayload string) (string, error) {
	normalized, _, _, err := a.validateWithRole(ctx, caller, projectID, rawPayload)
	return normalized, err
}

func (a *dispatchAgentAction) validateWithRole(ctx context.Context, caller Identity, projectID string, rawPayload string) (string, AgentRole, bool, error) {
	if a == nil || a.srv == nil {
		return "", "", false, errors.New("dispatch_agent action: server not configured")
	}
	if caller == nil {
		return "", "", false, &dispatchAgentValidationError{
			status:  http.StatusUnauthorized,
			code:    ErrCodeUnauthorized,
			message: "authentication required",
		}
	}

	var payload DispatchAgentEventPayload
	if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil {
		return "", "", false, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    ErrCodeValidationError,
			message: "invalid dispatch_agent payload",
		}
	}

	// Template is literal only (S1 §5.8 rule 1, S2 §5.1).
	if strings.Contains(payload.Template, "${") {
		return "", "", false, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    "invalid_template",
			message: "template must be a literal template name and cannot contain template placeholders",
			details: map[string]any{
				"slot":    "template",
				"offset":  strings.Index(payload.Template, "${"),
				"message": "template placeholders are not allowed in template",
			},
		}
	}
	if !templateRefAllowed(payload.Template) {
		return "", "", false, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    ErrCodeValidationError,
			message: templateRefRefusal,
		}
	}

	// Task <= 16 KiB before rendering (§5.1).
	if len(payload.Task) > triggertemplate.MaxTaskBytes {
		return "", "", false, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    ErrCodeValidationError,
			message: fmt.Sprintf("action.task exceeds maximum length of %d bytes", triggertemplate.MaxTaskBytes),
		}
	}

	// NameConflict enum (§5.1, §5.3).
	switch payload.NameConflict {
	case "", DispatchAgentNameConflictSuffix, DispatchAgentNameConflictSkip:
	default:
		return "", "", false, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    "invalid_value",
			message: fmt.Sprintf("unsupported nameConflict %q (supported: suffix, skip)", payload.NameConflict),
			details: map[string]any{"field": "nameConflict"},
		}
	}

	// Ephemeral capability gate (§5.5).
	if payload.Ephemeral {
		return "", "", false, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    "ephemeral_unsupported",
			message: "ephemeral agents are not yet supported",
		}
	}

	// GitHubToken enum and Stage 2 P1c-1 resolution (§5.1a).
	switch payload.GithubToken {
	case "", DispatchAgentGitHubTokenUserPreferred:
		payload.GithubToken = DispatchAgentGitHubTokenUserPreferred
	case DispatchAgentGitHubTokenInstallation:
		return "", "", false, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    "github_token_unsupported",
			message: "githubToken \"installation\" is not yet supported",
		}
	default:
		return "", "", false, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    "invalid_value",
			message: fmt.Sprintf("unsupported githubToken %q (supported: user_preferred)", payload.GithubToken),
			details: map[string]any{"field": "githubToken"},
		}
	}

	// AgentRole syntax validation (§5.1, rev 2.2).
	if payload.AgentRole != "" && !ValidAgentRole(AgentRole(payload.AgentRole)) {
		return "", "", false, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    "invalid_value",
			message: fmt.Sprintf("invalid agentRole %q", payload.AgentRole),
			details: map[string]any{"field": "agentRole"},
		}
	}
	if payload.NoAuth && payload.AgentRole != "" && AgentRole(payload.AgentRole) != AgentRoleNone {
		return "", "", false, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    "invalid_value",
			message: fmt.Sprintf("cannot request agentRole %q together with noAuth", payload.AgentRole),
			details: map[string]any{"field": "agentRole"},
		}
	}

	// Validate and deduplicate untrustedVars entries (§5.1, §5.2).
	if len(payload.UntrustedVars) > 0 {
		deduped, err := validateUntrustedVars(payload.UntrustedVars)
		if err != nil {
			return "", "", false, err
		}
		payload.UntrustedVars = deduped
	}

	// Validate labels: reserved keys, count <= 16 including 3 hub labels,
	// key syntax via pkg/labels, and values as label-slot templates (§5.1, §5.4).
	if len(payload.Labels)+hubAppliedTriggerLabelCount > labels.MaxLabels {
		return "", "", false, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    ErrCodeValidationError,
			message: fmt.Sprintf("too many labels: at most %d author labels allowed (%d reserved for hub provenance)", labels.MaxLabels-hubAppliedTriggerLabelCount, hubAppliedTriggerLabelCount),
		}
	}
	labelKeys := make([]string, 0, len(payload.Labels))
	for k := range payload.Labels {
		labelKeys = append(labelKeys, k)
	}
	slices.Sort(labelKeys)
	for _, k := range labelKeys {
		if reservedTriggerLabelKeys[k] {
			return "", "", false, &dispatchAgentValidationError{
				status:  http.StatusBadRequest,
				code:    "reserved_label_key",
				message: fmt.Sprintf("label key %q is reserved for hub trigger provenance", k),
				details: map[string]any{"key": k},
			}
		}
		if err := labels.Validate(map[string]string{k: "x"}); err != nil {
			return "", "", false, &dispatchAgentValidationError{
				status:  http.StatusBadRequest,
				code:    ErrCodeValidationError,
				message: fmt.Sprintf("invalid label key %q: %v", k, err),
			}
		}
		if _, err := validateTemplateSlot(payload.Labels[k], triggertemplate.SlotLabel, payload.UntrustedVars); err != nil {
			return "", "", false, err
		}
	}

	// Parse and check untrustedVars coverage for all template slots (§5.1, §5.2).
	if _, err := validateTemplateSlot(payload.Task, triggertemplate.SlotTask, payload.UntrustedVars); err != nil {
		return "", "", false, err
	}
	if payload.AgentName != "" {
		nameTmpl, err := validateTemplateSlot(payload.AgentName, triggertemplate.SlotName, payload.UntrustedVars)
		if err != nil {
			return "", "", false, err
		}
		// When agentName is a literal (no placeholders), run the same create-path
		// name checks at authoring so reserved or non-canonical names fail early.
		if len(nameTmpl.Placeholders()) == 0 {
			rendered, _, rErr := nameTmpl.Render(nil, nil)
			if rErr != nil {
				return "", "", false, &dispatchAgentValidationError{
					status:  http.StatusBadRequest,
					code:    "invalid_value",
					message: fmt.Sprintf("invalid literal agentName: %v", rErr),
					details: map[string]any{"field": "agentName"},
				}
			}
			if _, dErr := api.ValidateDisplayName(rendered); dErr != nil {
				return "", "", false, &dispatchAgentValidationError{
					status:  http.StatusBadRequest,
					code:    "invalid_value",
					message: fmt.Sprintf("invalid literal agentName: %v", dErr),
					details: map[string]any{"field": "agentName"},
				}
			}
			slug, aErr := api.ValidateAgentName(rendered)
			if aErr != nil || slug != rendered {
				return "", "", false, &dispatchAgentValidationError{
					status:  http.StatusBadRequest,
					code:    "invalid_value",
					message: "invalid literal agentName: must already be in canonical slug form",
					details: map[string]any{"field": "agentName"},
				}
			}
		}
	}
	if payload.Branch != "" {
		if _, err := validateTemplateSlot(payload.Branch, triggertemplate.SlotBranch, payload.UntrustedVars); err != nil {
			return "", "", false, err
		}
	}
	if payload.ReuseMessage != "" {
		if _, err := validateTemplateSlot(payload.ReuseMessage, triggertemplate.SlotMessage, payload.UntrustedVars); err != nil {
			return "", "", false, err
		}
	}

	// Caller authority & agentRole capping (§5.1).
	preCapRole, roleExplicit, err := a.srv.validateDispatchAgentAuthoringAuthority(ctx, caller, projectID, AgentRole(payload.AgentRole), payload.NoAuth)
	if err != nil {
		return "", "", false, err
	}

	normalized, err := json.Marshal(payload)
	if err != nil {
		return "", "", false, fmt.Errorf("marshal dispatch_agent payload: %w", err)
	}
	return string(normalized), preCapRole, roleExplicit, nil
}

func validateTemplateSlot(raw, slot string, allowed []string) (*triggertemplate.Template, error) {
	tmpl, err := triggertemplate.ValidateSlot(raw, slot, allowed)
	if err == nil {
		return tmpl, nil
	}
	var pe *triggertemplate.ParseError
	if errors.As(err, &pe) {
		return nil, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    "invalid_template",
			message: pe.Error(),
			details: map[string]any{
				"slot":    pe.Slot,
				"offset":  pe.Offset,
				"message": pe.Message,
			},
		}
	}
	var ue *triggertemplate.UntrustedVarError
	if errors.As(err, &ue) {
		return nil, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    "untrusted_var_not_allowed",
			message: ue.Error(),
			details: map[string]any{
				"slot": ue.Slot,
				"path": ue.Path,
			},
		}
	}
	return nil, &dispatchAgentValidationError{
		status:  http.StatusBadRequest,
		code:    ErrCodeValidationError,
		message: err.Error(),
	}
}

// validateUntrustedVars verifies that every entry in raw is a valid untrusted
// placeholder path or trailing ".*" prefix under an untrusted namespace, caps
// the list at triggertemplate.MaxPlaceholders (64), and removes duplicates in
// first-appearance order.
func validateUntrustedVars(raw []string) ([]string, error) {
	if len(raw) > triggertemplate.MaxPlaceholders {
		return nil, &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    "invalid_untrusted_var",
			message: fmt.Sprintf("untrustedVars exceeds maximum of %d entries", triggertemplate.MaxPlaceholders),
		}
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		if !isValidUntrustedVarPattern(entry) {
			return nil, &dispatchAgentValidationError{
				status:  http.StatusBadRequest,
				code:    "invalid_untrusted_var",
				message: fmt.Sprintf("invalid untrustedVars entry %q: must be an untrusted variable path or trailing \".*\" prefix", entry),
				details: map[string]any{"path": entry},
			}
		}
		if !seen[entry] {
			seen[entry] = true
			out = append(out, entry)
		}
	}
	return out, nil
}

func isValidUntrustedVarPattern(pattern string) bool {
	if pattern == "" {
		return false
	}
	if prefix, ok := strings.CutSuffix(pattern, ".*"); ok {
		if prefix == "" || strings.ContainsAny(prefix, "*?") {
			return false
		}
		if prefix == "event.attributes" || prefix == "event.payload" {
			return true
		}
		if strings.HasPrefix(prefix, "event.payload.") || strings.HasPrefix(prefix, "event.payload[") {
			cls, err := triggertemplate.Classify(prefix)
			return err == nil && cls == triggertemplate.ClassUntrusted
		}
		return false
	}
	if strings.ContainsAny(pattern, "*?") {
		return false
	}
	cls, err := triggertemplate.Classify(pattern)
	return err == nil && cls == triggertemplate.ClassUntrusted
}

func (s *Server) validateDispatchAgentAuthoringAuthority(
	ctx context.Context,
	caller Identity,
	projectID string,
	requestedRole AgentRole,
	noAuth bool,
) (AgentRole, bool, error) {
	if caller.Type() == "agent" {
		agentIdent, ok := caller.(AgentIdentity)
		if !ok || !agentIdent.HasScope(ScopeAgentCreate) {
			return "", false, &dispatchAgentValidationError{
				status:  http.StatusForbidden,
				code:    ErrCodeForbidden,
				message: "agent caller lacks required scope to author dispatch_agent triggers",
			}
		}
	}
	if decision := s.agentCreateDecision(ctx, caller, projectID); !decision.Allowed {
		return "", false, &dispatchAgentValidationError{
			status:  http.StatusForbidden,
			code:    ErrCodeForbidden,
			message: "not authorized to create agents in this project",
		}
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", false, &dispatchAgentValidationError{
				status:  http.StatusNotFound,
				code:    ErrCodeNotFound,
				message: "project not found",
			}
		}
		return "", false, fmt.Errorf("resolve project %q: %w", projectID, err)
	}

	auth := FireAuthority{
		PrincipalKind: principalKindForIdentity(caller),
		PrincipalID:   caller.ID(),
	}
	preCapRole, roleExplicit, _, err := s.resolveHeadlessPreCapRole(ctx, project, auth, requestedRole, noAuth)
	if err != nil {
		return "", false, &dispatchAgentValidationError{
			status:  http.StatusForbidden,
			code:    ErrCodeForbidden,
			message: err.Error(),
		}
	}

	if s.authzService != nil {
		delegateDecision := s.authzService.CanDelegate(ctx, caller, GrantDescriptor{
			Type:      GrantTypeAgentDelegation,
			AgentRole: string(preCapRole),
			ProjectID: projectID,
			ScopeType: store.RoleScopeProject,
			ScopeID:   projectID,
		})
		if !delegateDecision.Allowed {
			return "", false, &dispatchAgentValidationError{
				status:  http.StatusForbidden,
				code:    ErrCodeForbidden,
				message: fmt.Sprintf("cannot delegate agent role %q: %s", preCapRole, delegateDecision.Reason),
			}
		}
	}
	return preCapRole, roleExplicit, nil
}

func principalKindForIdentity(identity Identity) string {
	if identity == nil {
		return ""
	}
	if identity.Type() == "agent" {
		return store.DelegationPrincipalAgent
	}
	return store.DelegationPrincipalUser
}

func unmarshalDispatchAgentFirePayload(raw, definitionKind string) (DispatchAgentEventPayload, error) {
	var payload DispatchAgentEventPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return DispatchAgentEventPayload{}, fmt.Errorf("invalid dispatch_agent payload: %w", err)
	}
	if definitionKind != hubfire.DefinitionKindTrigger && payload.AgentName == "" {
		return DispatchAgentEventPayload{}, fmt.Errorf("dispatch_agent payload: agentName is required")
	}
	if definitionKind == hubfire.DefinitionKindTrigger && strings.Contains(payload.Template, "${") {
		return DispatchAgentEventPayload{}, fmt.Errorf("dispatch_agent payload: template cannot contain placeholders")
	}
	if !templateRefAllowed(payload.Template) {
		return DispatchAgentEventPayload{}, fmt.Errorf("dispatch_agent payload: %s", templateRefRefusal)
	}
	return payload, nil
}

func (a *dispatchAgentAction) ValidatePayload(req hubfire.FireRequest) error {
	_, err := unmarshalDispatchAgentFirePayload(req.Payload, req.Authority.DefinitionKind)
	return err
}

func dispatchAgentRefusalReason(req hubfire.FireRequest, err error) *FireRefusal {
	reason := "dispatch_agent refused"
	var projNotFound *dispatchAgentProjectNotFoundError
	if req.Authority.DefinitionKind == hubfire.DefinitionKindSchedule ||
		req.Authority.DefinitionKind == hubfire.DefinitionKindScheduledEvent ||
		errors.As(err, &projNotFound) {
		reason = err.Error()
	}
	return &FireRefusal{
		Reason: reason,
		Cause:  err,
	}
}

// authorizeDispatchAgentFire verifies live agent.create authorization,
// ScopeAgentCreate (for agent principals), and CanDelegate under the resolved
// revision identity, returning the resolved pre-cap role tuple approved by
// CanDelegate so Fire never re-resolves project or parent role defaults.
func (s *Server) authorizeDispatchAgentFire(
	ctx context.Context,
	projectID string,
	fireID string,
	definitionKind string,
	auth FireAuthority,
	identity Identity,
	requestedRole AgentRole,
	noAuth bool,
) (headlessResolvedRole, string, error) {
	if auth.PrincipalKind == store.DelegationPrincipalAgent {
		agentIdent, ok := identity.(AgentIdentity)
		if !ok || !agentIdent.HasScope(ScopeAgentCreate) {
			return headlessResolvedRole{}, "", fmt.Errorf("scheduled dispatch principal agent %q missing required scope: %s", auth.PrincipalID, ScopeAgentCreate)
		}
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return headlessResolvedRole{}, "", &dispatchAgentProjectNotFoundError{projectID: projectID}
		}
		return headlessResolvedRole{}, "", fmt.Errorf("%w: failed to resolve project %q: %w", errDispatchAgentProjectLookup, projectID, err)
	}
	preCapRole, roleExplicit, parentMessageMode, err := s.resolveHeadlessPreCapRole(ctx, project, auth, requestedRole, noAuth)
	if err != nil {
		return headlessResolvedRole{}, "", err
	}

	if decision := s.agentCreateDecision(ctx, identity, projectID); !decision.Allowed {
		return headlessResolvedRole{}, "", fmt.Errorf("scheduled dispatch principal %s %q is not authorized to create agents in project %q: %s",
			auth.PrincipalKind, auth.PrincipalID, projectID, decision.Reason)
	}

	targetType := "scheduled_dispatch"
	if definitionKind == hubfire.DefinitionKindTrigger {
		targetType = "trigger_dispatch"
	}
	delegateDecision := s.authzService.CanDelegate(ctx, identity, GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(preCapRole),
		ProjectID: projectID,
		ScopeType: store.RoleScopeProject,
		ScopeID:   projectID,
	})
	if !delegateDecision.Allowed {
		s.emitMutationAudit(ctx, &store.MutationAuditRecord{
			MutationType:       mutationTypeAgentDelegation,
			ActorPrincipalKind: auth.PrincipalKind,
			ActorPrincipalID:   auth.PrincipalID,
			TargetType:         targetType,
			TargetID:           fireID,
			CanDelegateResult:  "deny",
			CanDelegateReason:  delegateDecision.Reason,
		})
		return headlessResolvedRole{}, "", fmt.Errorf("scheduled dispatch principal %s %q failed CanDelegate at fire time: %s",
			auth.PrincipalKind, auth.PrincipalID, delegateDecision.Reason)
	}
	return headlessResolvedRole{
		preCapRole:        preCapRole,
		roleExplicit:      roleExplicit,
		parentMessageMode: parentMessageMode,
	}, project.Name, nil
}

func (a *dispatchAgentAction) Authorize(
	ctx context.Context,
	req hubfire.FireRequest,
	auth FireAuthority,
	identity Identity,
) (FireAuthorized, *FireRefusal, error) {
	if a == nil || a.srv == nil {
		return nil, nil, errors.New("dispatch_agent action: server not configured")
	}
	s := a.srv
	payload, err := unmarshalDispatchAgentFirePayload(req.Payload, req.Authority.DefinitionKind)
	if err != nil {
		return nil, nil, err
	}

	var (
		sourceName    string
		signatureMode string
	)
	if req.Authority.DefinitionKind == hubfire.DefinitionKindTrigger {
		if payload.Ephemeral {
			return nil, &FireRefusal{Reason: "ephemeral agents are not yet supported"}, nil
		}
		if payload.GithubToken != "" && payload.GithubToken != DispatchAgentGitHubTokenUserPreferred {
			return nil, &FireRefusal{Reason: "githubToken mode is not yet supported"}, nil
		}
		if req.Event != nil {
			var ep *store.WebhookEndpoint
			if req.Event.SourceKind == store.TriggerSourceKindWebhook {
				loaded, epErr := s.store.GetWebhookEndpoint(ctx, req.Event.SourceID)
				if epErr != nil {
					if errors.Is(epErr, store.ErrNotFound) || errors.Is(epErr, store.ErrInvalidInput) {
						return nil, &FireRefusal{Reason: "source not replay protected"}, nil
					}
					return nil, nil, fmt.Errorf("dispatch_agent trigger fire: load webhook endpoint: %w", epErr)
				}
				ep = loaded
				sourceName = ep.Name
				signatureMode = ep.SignatureMode
			}
			if !trigger.SourceReplayProtected(req.Event.SourceKind, ep) {
				return nil, &FireRefusal{Reason: "source not replay protected"}, nil
			}
		}
	}

	fireID := req.Authority.FireID
	var requestedRole AgentRole
	if req.Authority.DefinitionKind == hubfire.DefinitionKindTrigger {
		requestedRole = AgentRole(payload.AgentRole)
	}
	resolvedRole, projectName, err := s.authorizeDispatchAgentFire(
		ctx,
		req.ProjectID,
		fireID,
		req.Authority.DefinitionKind,
		auth,
		identity,
		requestedRole,
		payload.NoAuth,
	)
	if err != nil {
		if errors.Is(err, errDispatchAgentProjectLookup) {
			return nil, nil, err
		}
		slog.Warn("Scheduler: dispatch_agent refused at fire time",
			"eventID", fireID,
			"definitionKind", req.Authority.DefinitionKind,
			"definitionID", req.Authority.DefinitionID,
			"projectID", req.ProjectID,
			"principal_kind", auth.PrincipalKind,
			"principal_id", auth.PrincipalID,
			"authorization_revision", auth.Revision,
			"error", err)
		return nil, dispatchAgentRefusalReason(req, err), nil
	}

	edgeCeiling, edgeProvenance, err := s.fireEffectCeiling(ctx, auth)
	if err != nil {
		if errors.Is(err, errScheduledAuthorityDenied) {
			slog.Warn("Scheduler: dispatch_agent ceiling denied at fire time",
				"eventID", fireID,
				"definitionKind", req.Authority.DefinitionKind,
				"definitionID", req.Authority.DefinitionID,
				"projectID", req.ProjectID,
				"error", err)
			return nil, dispatchAgentRefusalReason(req, err), nil
		}
		return nil, nil, err
	}

	if err := s.fireStanding(ctx, auth, nil); err != nil {
		return nil, dispatchAgentRefusalReason(req, err), nil
	}

	return &dispatchAgentAuthorized{
		payload:        payload,
		auth:           auth,
		identity:       identity,
		creatorName:    fireCreatorName(auth),
		edgeCeiling:    edgeCeiling,
		edgeProvenance: edgeProvenance,
		resolvedRole:   resolvedRole,
		projectName:    projectName,
		sourceName:     sourceName,
		signatureMode:  signatureMode,
	}, nil, nil
}

func (a *dispatchAgentAction) Fire(ctx context.Context, req hubfire.FireRequest, authorized FireAuthorized) (hubfire.FireOutcome, error) {
	if a == nil || a.srv == nil {
		return hubfire.FireOutcome{}, errors.New("dispatch_agent action: server not configured")
	}
	da, ok := authorized.(*dispatchAgentAuthorized)
	if !ok || da == nil {
		return hubfire.FireOutcome{}, errors.New("dispatch_agent action: missing authorized state")
	}

	if req.Authority.DefinitionKind == hubfire.DefinitionKindTrigger {
		return a.fireTriggerDispatchAgent(ctx, req, da)
	}

	staleness := time.Since(req.FireAt)
	if !req.FireAt.IsZero() && staleness > 1*time.Minute {
		slog.Warn("Scheduler: firing stale dispatch_agent event",
			"eventID", req.Authority.FireID,
			"agentName", da.payload.AgentName,
			"scheduledFor", req.FireAt.UTC().Format(time.RFC3339),
			"staleness", staleness.Truncate(time.Second).String())
	}

	resolvedRole := da.resolvedRole
	agent, err := a.srv.createAgentHeadless(ctx, HeadlessCreate{
		ProjectID:      req.ProjectID,
		Authority:      da.auth,
		Identity:       da.identity,
		CreatorName:    da.creatorName,
		EdgeCeiling:    da.edgeCeiling,
		EdgeProvenance: da.edgeProvenance,
		NoAuth:         da.payload.NoAuth,
		resolvedRole:   &resolvedRole,
		Name:           da.payload.AgentName,
		Template:       da.payload.Template,
		Task:           da.payload.Task,
		Branch:         da.payload.Branch,
		Detached:       true,
		NotifyBlocked: func(ctx context.Context, existing *store.Agent, placementRefused bool) {
			a.srv.notifyFireBlocked(ctx, req, existing, placementRefused)
		},
	})
	if err != nil {
		return hubfire.FireOutcome{}, err
	}
	var resultRef string
	if agent != nil {
		resultRef = agent.ID
	}
	return hubfire.FireOutcome{
		Status:    hubfire.FireStatusFired,
		ResultRef: resultRef,
	}, nil
}

// payloadErrorVars returns a PayloadValueError only when a template actually
// attempts to look up an event.payload.* path, so non-JSON event bodies do not
// fail triggers that never reference event.payload.*.
type payloadErrorVars struct {
	err error
}

func (p payloadErrorVars) Lookup(path string) (string, bool, error) {
	if strings.HasPrefix(path, "event.payload.") || strings.HasPrefix(path, "event.payload[") {
		return "", false, p.err
	}
	return "", false, nil
}

func buildDispatchTemplateVars(req hubfire.FireRequest, da *dispatchAgentAuthorized) triggertemplate.Vars {
	ev := req.Event
	triggerID := req.Authority.DefinitionID
	triggerName := ""
	if ev.Trigger != nil {
		if ev.Trigger.ID != "" {
			triggerID = ev.Trigger.ID
		}
		triggerName = ev.Trigger.Name
	}

	// Hub-assigned trusted values are placed FIRST in ChainVars so event-derived
	// data can never answer or shadow a trusted path (§5.2, §5.3).
	trusted := triggertemplate.MapVars{
		"event.id":          ev.ID,
		"event.source.kind": ev.SourceKind,
		"event.source.id":   ev.SourceID,
		"event.received_at": ev.ReceivedAt.UTC().Format(time.RFC3339),
		"trigger.id":        triggerID,
		"trigger.name":      triggerName,
		"project.id":        req.ProjectID,
		"project.name":      da.projectName,
	}
	if da.sourceName != "" {
		trusted["event.source.name"] = da.sourceName
	}

	untrusted := triggertemplate.MapVars{
		"event.type":    ev.Type,
		"event.subject": ev.Subject,
	}
	if !ev.OccurredAt.IsZero() {
		untrusted["event.occurred_at"] = ev.OccurredAt.UTC().Format(time.RFC3339)
	}
	for k, v := range ev.Attributes {
		untrusted["event.attributes."+k] = v
	}

	var payloadVars triggertemplate.Vars
	if len(ev.Payload) > 0 && len(ev.Payload) <= trigger.MaxPayloadBytes {
		pv, err := triggertemplate.NewPayloadVars(ev.Payload)
		if err != nil {
			payloadVars = payloadErrorVars{err: err}
		} else {
			payloadVars = pv
		}
	}

	return triggertemplate.ChainVars(trusted, untrusted, payloadVars)
}

func renderTemplateSlot(raw, slot string, vars triggertemplate.Vars, allowed []string, opts ...triggertemplate.RenderOption) (string, error) {
	tmpl, err := triggertemplate.Parse(raw, slot)
	if err != nil {
		return "", err
	}
	rendered, _, err := tmpl.Render(vars, allowed, opts...)
	if err != nil {
		return "", err
	}
	return rendered, nil
}

func (a *dispatchAgentAction) refuseTemplateRender(req hubfire.FireRequest, fallbackSlot string, err error) hubfire.FireOutcome {
	slot := fallbackSlot
	path := ""
	reason := string(triggertemplate.KindValueRejected)

	var re *triggertemplate.RenderError
	var ue *triggertemplate.UntrustedVarError
	var pe *triggertemplate.ParseError
	switch {
	case errors.As(err, &re):
		if re.Slot != "" {
			slot = re.Slot
		}
		path = re.Path
		if re.Kind != "" {
			reason = string(re.Kind)
		}
	case errors.As(err, &ue):
		if ue.Slot != "" {
			slot = ue.Slot
		}
		path = ue.Path
	case errors.As(err, &pe):
		if pe.Slot != "" {
			slot = pe.Slot
		}
	}

	// Never log expanded values or rendered output (§5.2, §5.3).
	logging.Subsystem("hub.audit").Warn("trigger.fire.template_refused",
		"event_type", "trigger.fire.template_refused",
		"fire_id", req.Authority.FireID,
		"trigger_id", req.Authority.DefinitionID,
		"project_id", req.ProjectID,
		"slot", slot,
		"path", path,
		"reason", reason)
	return hubfire.FireOutcome{
		Status: hubfire.FireStatusRefused,
		Error:  reason,
	}
}

func (a *dispatchAgentAction) fireTriggerDispatchAgent(
	ctx context.Context,
	req hubfire.FireRequest,
	da *dispatchAgentAuthorized,
) (hubfire.FireOutcome, error) {
	ev := req.Event
	if ev == nil {
		return hubfire.FireOutcome{}, errors.New("dispatch_agent trigger fire: missing event")
	}

	vars := buildDispatchTemplateVars(req, da)
	frameOpt := triggertemplate.WithFrame(triggertemplate.FrameContext{
		EndpointName:  da.sourceName,
		SignatureMode: da.signatureMode,
		EventID:       ev.ID,
	})

	renderedTask, err := renderTemplateSlot(da.payload.Task, triggertemplate.SlotTask, vars, da.payload.UntrustedVars, frameOpt)
	if err != nil {
		return a.refuseTemplateRender(req, triggertemplate.SlotTask, err), nil
	}

	var renderedName string
	if da.payload.AgentName != "" {
		renderedName, err = renderTemplateSlot(da.payload.AgentName, triggertemplate.SlotName, vars, da.payload.UntrustedVars)
		if err != nil {
			return a.refuseTemplateRender(req, triggertemplate.SlotName, err), nil
		}
	}

	var renderedBranch string
	if da.payload.Branch != "" {
		renderedBranch, err = renderTemplateSlot(da.payload.Branch, triggertemplate.SlotBranch, vars, da.payload.UntrustedVars)
		if err != nil {
			return a.refuseTemplateRender(req, triggertemplate.SlotBranch, err), nil
		}
		// Strip an author's literal refs/heads/ prefix so downstream git
		// operations never double-prefix refs/heads/refs/heads/... (§5.3).
		if trimmed, ok := strings.CutPrefix(renderedBranch, "refs/heads/"); ok {
			if err := gitref.ValidBranch(trimmed); err != nil {
				return a.refuseTemplateRender(req, triggertemplate.SlotBranch, &triggertemplate.RenderError{
					Kind: triggertemplate.KindValueRejected,
					Slot: triggertemplate.SlotBranch,
				}), nil
			}
			renderedBranch = trimmed
		}
	}

	// Render author labels in deterministic key order, then merge hub provenance labels (hub wins).
	mergedLabels := make(map[string]string, len(da.payload.Labels)+hubAppliedTriggerLabelCount)
	if len(da.payload.Labels) > 0 {
		keys := make([]string, 0, len(da.payload.Labels))
		for k := range da.payload.Labels {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			val, rErr := renderTemplateSlot(da.payload.Labels[k], triggertemplate.SlotLabel, vars, da.payload.UntrustedVars)
			if rErr != nil {
				return a.refuseTemplateRender(req, triggertemplate.SlotLabel, rErr), nil
			}
			mergedLabels[k] = val
		}
	}
	triggerID := req.Authority.DefinitionID
	triggerName := ""
	if ev.Trigger != nil {
		if ev.Trigger.ID != "" {
			triggerID = ev.Trigger.ID
		}
		triggerName = ev.Trigger.Name
	}
	mergedLabels[triggerLabelKeyTrigger] = triggerID
	mergedLabels[triggerLabelKeyTriggerEvent] = ev.ID
	mergedLabels[triggerLabelKeyTriggerSource] = ev.SourceKind
	if err := labels.Validate(mergedLabels); err != nil {
		return a.refuseTemplateRender(req, triggertemplate.SlotLabel, &triggertemplate.RenderError{
			Kind: triggertemplate.KindValueRejected,
			Slot: triggertemplate.SlotLabel,
		}), nil
	}

	// Name resolution and normal create-path name checks (§5.3).
	baseName := renderedName
	if baseName == "" {
		var genErr error
		baseName, genErr = defaultTriggerAgentName(triggerName)
		if genErr != nil {
			return hubfire.FireOutcome{}, fmt.Errorf("generate default agent name: %w", genErr)
		}
	}
	if _, err := api.ValidateDisplayName(baseName); err != nil {
		return a.refuseTemplateRender(req, triggertemplate.SlotName, &triggertemplate.RenderError{
			Kind: triggertemplate.KindValueRejected,
			Slot: triggertemplate.SlotName,
		}), nil
	}
	baseSlug, err := api.ValidateAgentName(baseName)
	if err != nil {
		return a.refuseTemplateRender(req, triggertemplate.SlotName, &triggertemplate.RenderError{
			Kind: triggertemplate.KindValueRejected,
			Slot: triggertemplate.SlotName,
		}), nil
	}

	// Event-context injection (§5.4): 3 allow-listed env vars + home file.
	eventEnv := map[string]string{
		EventEnvKeyTriggerEventID:   ev.ID,
		EventEnvKeyTriggerID:        triggerID,
		EventEnvKeyTriggerEventFile: HeadlessEventFileContainerPath,
	}
	eventBody := buildDispatchedEventBody(ev)
	eventBody.FireID = req.Authority.FireID
	var eventFileBuf bytes.Buffer
	enc := json.NewEncoder(&eventFileBuf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(eventBody); err != nil {
		return hubfire.FireOutcome{}, fmt.Errorf("marshal trigger event file: %w", err)
	}
	eventFileBytes := bytes.TrimSuffix(eventFileBuf.Bytes(), []byte("\n"))
	if len(eventFileBytes) > maxHeadlessEventFileBytes {
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusFailed,
			Error:  "event file too large",
		}, nil
	}

	nameConflict := da.payload.NameConflict
	if nameConflict == "" {
		nameConflict = DispatchAgentNameConflictSuffix
	}

	// Pass "" to HeadlessCreate.GitHubToken for user_preferred (= today's
	// precedence; createAgentHeadless refuses non-empty values until P1c-3).
	headlessGitHubToken := ""
	if da.payload.GithubToken != "" && da.payload.GithubToken != DispatchAgentGitHubTokenUserPreferred {
		headlessGitHubToken = da.payload.GithubToken
	}

	resolvedRole := da.resolvedRole
	candidate := baseSlug
	for attempt := 0; ; attempt++ {
		agent, createErr := a.srv.createAgentHeadless(ctx, HeadlessCreate{
			ProjectID:      req.ProjectID,
			Authority:      da.auth,
			Identity:       da.identity,
			CreatorName:    da.creatorName,
			EdgeCeiling:    da.edgeCeiling,
			EdgeProvenance: da.edgeProvenance,
			AgentRole:      AgentRole(da.payload.AgentRole),
			NoAuth:         da.payload.NoAuth,
			resolvedRole:   &resolvedRole,
			Name:           candidate,
			Template:       da.payload.Template,
			Task:           renderedTask,
			Branch:         renderedBranch,
			Labels:         maps.Clone(mergedLabels),
			GitHubToken:    headlessGitHubToken,
			EventEnv:       eventEnv,
			EventFile:      eventFileBytes,
			Detached:       true,
		})
		if createErr == nil {
			var agentID string
			if agent != nil {
				agentID = agent.ID
			}
			return hubfire.FireOutcome{
				Status:        hubfire.FireStatusFired,
				ResultRef:     agentID,
				TargetAgentID: agentID,
			}, nil
		}
		if errors.Is(createErr, errHeadlessAgentExists) || errors.Is(createErr, store.ErrAlreadyExists) {
			if nameConflict == DispatchAgentNameConflictSkip {
				return hubfire.FireOutcome{
					Status: hubfire.FireStatusSkipped,
					Error:  "agent name in use",
				}, nil
			}
			if attempt >= maxNameCollisionRetries {
				return hubfire.FireOutcome{
					Status: hubfire.FireStatusFailed,
					Error:  "name exhausted",
				}, nil
			}
			suffixed, sErr := appendRandomHexSuffix(baseSlug)
			if sErr != nil {
				return hubfire.FireOutcome{}, fmt.Errorf("generate name collision suffix: %w", sErr)
			}
			candidate = suffixed
			continue
		}
		return hubfire.FireOutcome{}, createErr
	}
}

var triggerBase32Encoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func defaultTriggerAgentName(triggerName string) (string, error) {
	prefix := api.Slugify(triggerName)
	if prefix == "" {
		prefix = "trigger"
	}
	const suffixLen = 6
	const maxPrefix = api.MaxSlugLength - 1 - suffixLen // 63 - 7 = 56
	if len(prefix) > maxPrefix {
		prefix = strings.TrimRight(prefix[:maxPrefix], "-")
		if prefix == "" {
			prefix = "trigger"
		}
	}
	var raw [4]byte // 32 bits -> 7 base32 chars; take first 6
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	suffix := triggerBase32Encoding.EncodeToString(raw[:])[:suffixLen]
	return prefix + "-" + suffix, nil
}

func appendRandomHexSuffix(baseSlug string) (string, error) {
	const suffixLen = 4
	const maxPrefix = api.MaxSlugLength - 1 - suffixLen // 63 - 5 = 58
	prefix := baseSlug
	if len(prefix) > maxPrefix {
		prefix = strings.TrimRight(prefix[:maxPrefix], "-")
		if prefix == "" {
			prefix = "agent"
		}
	}
	var raw [2]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(raw[:]), nil
}
