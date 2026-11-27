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
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	maxTriggerNameLen          = 128
	maxTriggerDescriptionLen   = 1024
	maxTriggerRequestBodyBytes = 64 * 1024
	defaultWebhookMatchType    = "webhook.received"
)

var (
	errTriggerProjectLimitExceeded     = errors.New("project trigger limit exceeded")
	errTriggerSourceLimitExceeded      = errors.New("source active trigger limit exceeded")
	errTriggerWebhookSourceNotFound    = errors.New("trigger webhook endpoint source not found")
	errTriggerSourceNotReplayProtected = errors.New("trigger source is not replay protected")
)

func triggerActions(tr store.Trigger) ([]store.TriggerAction, error) {
	return trigger.Actions(tr)
}

func actionRequiresReplayProtection(actionType string) bool {
	return actionType == store.TriggerActionDispatchAgent || actionType == store.TriggerActionExec
}

func actionsRequireReplayProtection(actions []store.TriggerAction) bool {
	for _, a := range actions {
		if actionRequiresReplayProtection(a.Type) {
			return true
		}
	}
	return false
}

func actionsHasType(actions []store.TriggerAction, actionType string) bool {
	for _, a := range actions {
		if a.Type == actionType {
			return true
		}
	}
	return false
}

func isZeroTriggerActionSpec(s TriggerActionSpec) bool {
	return s.Type == "" &&
		s.AgentID == "" &&
		s.AgentName == "" &&
		s.Message == "" &&
		!s.Interrupt &&
		!s.Plain &&
		!s.Wake &&
		s.Offline == "" &&
		s.QueueTTLSeconds == 0 &&
		s.NameConflict == "" &&
		s.Template == "" &&
		s.Task == "" &&
		s.Branch == "" &&
		len(s.Labels) == 0 &&
		!s.Ephemeral &&
		s.GithubToken == "" &&
		s.AgentRole == "" &&
		s.ReuseMessage == "" &&
		s.AckComment == nil &&
		len(s.UntrustedVars) == 0 &&
		!s.NoAuth &&
		s.Body == "" &&
		s.Target == "" &&
		!s.ReplyInThread &&
		len(s.Command) == 0 &&
		s.TimeoutSeconds == 0 &&
		len(s.ContextAttributes) == 0 &&
		!s.CommandRedacted &&
		s.Argc == 0 &&
		s.CommandSHA256 == ""
}

func actionSpecsHasType(specs []TriggerActionSpec, actionType string) bool {
	for _, s := range specs {
		if strings.TrimSpace(s.Type) == actionType {
			return true
		}
	}
	return false
}

func validateTriggerLimits(hasDispatchAgent bool, maxFiresPerMinute, maxConcurrentDispatches int) error {
	if maxFiresPerMinute < 0 || maxFiresPerMinute > store.MaxTriggerFiresPerMinute {
		return fmt.Errorf("maxFiresPerMinute must be between 0 and %d", store.MaxTriggerFiresPerMinute)
	}
	if maxConcurrentDispatches < 0 || maxConcurrentDispatches > store.MaxTriggerConcurrentDispatches {
		return fmt.Errorf("maxConcurrentDispatches must be between 0 and %d", store.MaxTriggerConcurrentDispatches)
	}
	if maxConcurrentDispatches > 0 && !hasDispatchAgent {
		return errors.New("maxConcurrentDispatches is only supported for dispatch_agent triggers")
	}
	return nil
}

func writeAmbiguousTriggerActionsError(w http.ResponseWriter) {
	writeError(
		w,
		http.StatusBadRequest,
		ErrCodeInvalidAction,
		"trigger cannot specify both action and actions",
		map[string]any{
			"field":    "actions",
			"category": "ambiguous_actions",
		},
	)
}

func writeCorruptTriggerActionsError(w http.ResponseWriter) {
	writeError(
		w,
		http.StatusBadRequest,
		ErrCodeInvalidAction,
		"stored trigger actions data is corrupt; replace actions before updating or resuming",
		map[string]any{
			"field":    "actions",
			"category": "corrupt_storage",
		},
	)
}

func validateEncodedTriggerActionsSize(actions []store.TriggerAction) error {
	type encodedElem struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	elems := make([]encodedElem, len(actions))
	for i, a := range actions {
		p := strings.TrimSpace(a.Payload)
		if p == "" {
			p = "{}"
		}
		elems[i] = encodedElem{Type: a.Type, Payload: json.RawMessage(p)}
	}
	raw, err := json.Marshal(elems)
	if err != nil {
		return fmt.Errorf("encoding actions: %w", err)
	}
	if len(raw) > store.MaxTriggerActionsBytes {
		return fmt.Errorf("encoded actions exceeds %d byte limit", store.MaxTriggerActionsBytes)
	}
	return nil
}

// resolveRequestTriggerActions normalizes a create or update request's scalar
// action and list actions fields into an ordered slice of TriggerActionSpec.
// Mixing both returns 400 invalid_action {category: "ambiguous_actions"}.
func resolveRequestTriggerActions(w http.ResponseWriter, scalar *TriggerActionSpec, list *[]TriggerActionSpec, required bool) ([]TriggerActionSpec, bool, bool) {
	hasScalar := scalar != nil
	hasList := list != nil
	if hasScalar && hasList {
		writeAmbiguousTriggerActionsError(w)
		return nil, false, false
	}
	if !hasScalar && !hasList {
		if required {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidAction, "at least one action is required", map[string]any{
				"field": "actions",
			})
			return nil, false, false
		}
		return nil, false, true
	}
	if hasScalar {
		return []TriggerActionSpec{*scalar}, true, true
	}
	specs := *list
	if len(specs) == 0 {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidAction, "actions must contain between 1 and 8 elements", map[string]any{
			"field":    "actions",
			"category": "size",
		})
		return nil, false, false
	}
	if len(specs) > store.MaxTriggerActions {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidAction,
			fmt.Sprintf("actions exceeds maximum of %d elements", store.MaxTriggerActions),
			map[string]any{
				"field":    "actions",
				"category": "size",
			})
		return nil, false, false
	}
	out := make([]TriggerActionSpec, len(specs))
	copy(out, specs)
	return out, true, true
}

// authorizeTriggerActionSpecs validates and authorizes every action element in
// specs in order under the caller's identity, ceiling, and provenance.
//
// Security inventory (action-arrays-design §4, §6):
//   - Authoring create (POST /triggers), update (PATCH /triggers/{id}), and
//     resume (POST /triggers/{id}/resume) authorize EVERY element; agent-mode
//     callers are refused if ANY element exceeds their rights.
//   - Replay-protected-source rule applies if ANY action is dispatch_agent or
//     exec (enforced here and re-verified inside WithTx via verifyActiveWebhookSourceInTx).
//   - Encoded actions list is bounded to 1..8 elements and <= 64 KiB.
//   - Per-element exec argv redaction is enforced in buildTriggerResponse.
//   - Schedules refuse actions on POST/PATCH /schedules until Stage 4.
func (s *Server) authorizeTriggerActionSpecs(
	w http.ResponseWriter,
	r *http.Request,
	projectID string,
	sourceKind string,
	ep *store.WebhookEndpoint,
	matchTypes []string,
	subjectMode string,
	specs []TriggerActionSpec,
	ceiling store.EffectCeiling,
	prov store.AuthorityProvenance,
) ([]store.TriggerAction, bool) {
	out := make([]store.TriggerAction, 0, len(specs))
	for i := range specs {
		spec := specs[i]
		actionType := strings.TrimSpace(spec.Type)
		if !validateTriggerActionCrossFields(w, actionType, &spec) {
			return nil, false
		}
		var (
			actionPayload string
			ok            bool
		)
		switch actionType {
		case store.TriggerActionMessage:
			actionPayload, ok = s.authorizeTriggerMessageAuthoring(w, r, projectID, sourceKind, &spec)
		case store.TriggerActionDispatchAgent:
			actionPayload, ok = s.authorizeTriggerDispatchAgentAuthoring(w, r, projectID, sourceKind, ep, &spec, ceiling, prov)
		case store.TriggerActionExec:
			actionPayload, _, ok = s.authorizeTriggerExecAuthoring(w, r, projectID, sourceKind, ep, subjectMode, &spec, ceiling, prov)
		case store.TriggerActionGitHubComment:
			actionPayload, ok = s.authorizeTriggerGitHubCommentAuthoring(w, r, projectID, sourceKind, matchTypes, &spec)
		default:
			ValidationError(w, fmt.Sprintf("unsupported action type %q (supported: %s, %s, %s, %s)",
				actionType, store.TriggerActionMessage, store.TriggerActionDispatchAgent, store.TriggerActionExec, store.TriggerActionGitHubComment), nil)
			return nil, false
		}
		if !ok {
			return nil, false
		}
		out = append(out, store.TriggerAction{
			Type:    actionType,
			Payload: actionPayload,
		})
	}
	if err := validateEncodedTriggerActionsSize(out); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidAction, err.Error(), map[string]any{
			"field":    "actions",
			"category": "size",
		})
		return nil, false
	}
	return out, true
}

func (s *Server) authorizeStoredTriggerActions(
	w http.ResponseWriter,
	r *http.Request,
	projectID string,
	sourceKind string,
	ep *store.WebhookEndpoint,
	matchTypes []string,
	subjectMode string,
	stored []store.TriggerAction,
	ceiling store.EffectCeiling,
	prov store.AuthorityProvenance,
) ([]store.TriggerAction, bool) {
	if len(stored) == 0 || len(stored) > store.MaxTriggerActions {
		writeCorruptTriggerActionsError(w)
		return nil, false
	}
	out := make([]store.TriggerAction, 0, len(stored))
	for _, act := range stored {
		normalizedPayload, ok := s.authorizeStoredTriggerAction(
			w, r, projectID, sourceKind, ep, matchTypes, subjectMode,
			act.Type, act.Payload, ceiling, prov,
		)
		if !ok {
			return nil, false
		}
		out = append(out, store.TriggerAction{
			Type:    act.Type,
			Payload: normalizedPayload,
		})
	}
	if err := validateEncodedTriggerActionsSize(out); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidAction, err.Error(), map[string]any{
			"field":    "actions",
			"category": "size",
		})
		return nil, false
	}
	return out, true
}

// verifyActiveWebhookSourceInTx re-reads the resolved webhook endpoint inside
// WithTx (after LockTriggerQuota) so a concurrent DELETE, status disable, or
// PATCH dropping replay protection cannot leave an active trigger pointing at
// an invalid endpoint.
func verifyActiveWebhookSourceInTx(ctx context.Context, tx store.Store, projectID, endpointID string, requireReplayProtected bool) error {
	ep, err := tx.GetWebhookEndpoint(ctx, endpointID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return errTriggerWebhookSourceNotFound
		}
		return err
	}
	if ep == nil || ep.ProjectID != projectID || ep.Status != store.WebhookEndpointStatusActive {
		return errTriggerWebhookSourceNotFound
	}
	if requireReplayProtected && !trigger.SourceReplayProtected(store.TriggerSourceKindWebhook, ep) {
		return errTriggerSourceNotReplayProtected
	}
	return nil
}

// CreateTriggerRequest is the request body for POST /triggers.
type CreateTriggerRequest struct {
	Name                    string               `json:"name"`
	Description             string               `json:"description,omitempty"`
	Source                  TriggerSourceSpec    `json:"source"`
	Match                   *TriggerMatchSpec    `json:"match,omitempty"`
	Action                  TriggerActionSpec    `json:"action,omitzero"`
	Actions                 *[]TriggerActionSpec `json:"actions,omitempty"`
	Subject                 *TriggerSubjectSpec  `json:"subject,omitempty"`
	MaxFiresPerMinute       int                  `json:"maxFiresPerMinute,omitempty"`
	MaxConcurrentDispatches int                  `json:"maxConcurrentDispatches,omitempty"`
}

// TriggerMatchFormPatch tracks explicit presence of match.form in PATCH
// requests so {"form": null} is distinguished from an omitted "form" field.
type TriggerMatchFormPatch struct {
	Set bool
	Raw json.RawMessage
}

// IsZero reports whether the form patch field was omitted.
func (p TriggerMatchFormPatch) IsZero() bool {
	return !p.Set
}

// UnmarshalJSON records that match.form was explicitly present in the request,
// including when its JSON value is null.
func (p *TriggerMatchFormPatch) UnmarshalJSON(data []byte) error {
	p.Set = true
	p.Raw = append(p.Raw[:0], data...)
	return nil
}

// MarshalJSON encodes an explicitly set match.form value (defaulting to null
// when Set is true and Raw is empty).
func (p TriggerMatchFormPatch) MarshalJSON() ([]byte, error) {
	if !p.Set || len(p.Raw) == 0 {
		return []byte("null"), nil
	}
	return p.Raw, nil
}

// TriggerMatchPatchSpec holds partial updates to a trigger's match block.
type TriggerMatchPatchSpec struct {
	Types   *[]string               `json:"types,omitempty"`
	Dialect *string                 `json:"dialect,omitempty"`
	Expr    *string                 `json:"expr,omitempty"`
	Rules   *[]TriggerMatchRuleSpec `json:"rules,omitempty"`
	Form    TriggerMatchFormPatch   `json:"form,omitzero"`
}

// TriggerSubjectPatchSpec holds partial updates to a trigger's subject block.
type TriggerSubjectPatchSpec struct {
	Dialect        *string `json:"dialect,omitempty"`
	Expr           *string `json:"expr,omitempty"`
	Mode           *string `json:"mode,omitempty"`
	CoalesceWindow *string `json:"coalesce_window,omitempty"`
}

// authorizeTriggerReuseSubject enforces Stage 3 §6.2: authoring a reuse trigger
// requires wake authority (agent.lifecycle) in the author's hop ceiling,
// returning HTTP 400 "reuse requires wake authority" when absent.
func authorizeTriggerReuseSubject(w http.ResponseWriter, subjectMode string, ceiling store.EffectCeiling, prov store.AuthorityProvenance) bool {
	if subjectMode != store.TriggerSubjectModeReuse {
		return true
	}
	if !EffectCeilingAllows(
		uatHopEffectCeiling(prov.SourcePrincipalKind, prov.SourcePrincipalID, prov, ceiling),
		scheduledWakePermission,
		false,
	) {
		ValidationError(w, "reuse requires wake authority", nil)
		return false
	}
	return true
}

// UpdateTriggerRequest is the request body for PATCH /triggers/{id}.
type UpdateTriggerRequest struct {
	Name                    *string                  `json:"name,omitempty"`
	Description             *string                  `json:"description,omitempty"`
	Source                  *TriggerSourceSpec       `json:"source,omitempty"`
	Match                   *TriggerMatchPatchSpec   `json:"match,omitempty"`
	Action                  *TriggerActionSpec       `json:"action,omitempty"`
	Actions                 *[]TriggerActionSpec     `json:"actions,omitempty"`
	Subject                 *TriggerSubjectPatchSpec `json:"subject,omitempty"`
	MaxFiresPerMinute       *int                     `json:"maxFiresPerMinute,omitempty"`
	MaxConcurrentDispatches *int                     `json:"maxConcurrentDispatches,omitempty"`
	ExpectedRevision        *int                     `json:"expectedRevision,omitempty"`
}

// handleTriggers routes requests under
// /api/v1/projects/{projectId}/triggers[/{id}[/{action}]].
func (s *Server) handleTriggers(w http.ResponseWriter, r *http.Request, projectID, triggerPath string) {
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

		pathParts := strings.SplitN(triggerPath, "/", 2)
		var authzAction Action
		authoring := false
		if triggerPath == "" {
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
			subAction := ""
			if len(pathParts) > 1 {
				subAction = pathParts[1]
			}
			switch subAction {
			case "":
				switch r.Method {
				case http.MethodGet:
					authzAction = ActionRead
				case http.MethodPatch:
					authzAction = ActionUpdate
					authoring = true
				case http.MethodDelete:
					authzAction = ActionDelete
				default:
					MethodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
					return
				}
			case "pause", "resume":
				if r.Method != http.MethodPost {
					MethodNotAllowed(w, http.MethodPost)
					return
				}
				authzAction = ActionUpdate
				authoring = subAction == "resume"
			case "history":
				if r.Method != http.MethodGet {
					MethodNotAllowed(w, http.MethodGet)
					return
				}
				authzAction = ActionRead
			case "render":
				if r.Method != http.MethodPost {
					MethodNotAllowed(w, http.MethodPost)
					return
				}
				authzAction = ActionRead
			default:
				NotFound(w, "Trigger action")
				return
			}
		}

		// Baseline agent floor (Stage 2 §4.3, ptone/scion#8065):
		// Agent callers below baseline (e.g. readonly, which lacks
		// agentAuthoringFloorScope) are refused on routes that create or widen
		// trigger authority:
		//   - POST  /triggers              (create, incl. preset apply)
		//   - PATCH /triggers/{id}         (update)
		//   - POST  /triggers/{id}/resume  (re-enable)
		// Reads (GET list/get/history, POST render) and authority-reducing routes
		// (POST pause, DELETE) stay at project:read.
		if authoring && !checkAgentAuthoringFloor(w, r) {
			return
		}
		if authoring && !authorizeDeferredAuthoringCredential(w, r) {
			return
		}
		if !s.authorizeTriggerAccess(w, r, projectID, authzAction) {
			return
		}

		if triggerPath == "" {
			switch r.Method {
			case http.MethodGet:
				s.listTriggers(w, r, projectID)
			case http.MethodPost:
				s.createTrigger(w, r, projectID)
			}
			return
		}

		triggerID := pathParts[0]
		subAction := ""
		if len(pathParts) > 1 {
			subAction = pathParts[1]
		}
		switch subAction {
		case "":
			switch r.Method {
			case http.MethodGet:
				s.getTrigger(w, r, projectID, triggerID)
			case http.MethodPatch:
				s.updateTrigger(w, r, projectID, triggerID)
			case http.MethodDelete:
				s.deleteTrigger(w, r, projectID, triggerID)
			}
		case "pause":
			s.pauseTrigger(w, r, projectID, triggerID)
		case "resume":
			s.resumeTrigger(w, r, projectID, triggerID)
		case "history":
			s.getTriggerHistory(w, r, projectID, triggerID)
		case "render":
			s.renderTrigger(w, r, projectID, triggerID)
		}
	})(w, r)
}

func isDisallowedTriggerNameRune(r rune) bool {
	return r == '/' || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

func isDisallowedTriggerDescriptionRune(r rune) bool {
	if r == '\n' || r == '\r' || r == '\t' {
		return false
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

func validateTriggerResourceName(raw string, maxLen int) (string, error) {
	if !utf8.ValidString(raw) || strings.ContainsFunc(raw, isDisallowedTriggerNameRune) {
		return "", errors.New("name must not contain '/' or control or format characters")
	}
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("name is required")
	}
	if len(name) > maxLen {
		return "", fmt.Errorf("name exceeds maximum length of %d characters", maxLen)
	}
	return name, nil
}

func validateTriggerResourceDescription(desc string, maxLen int) error {
	if len(desc) > maxLen {
		return fmt.Errorf("description exceeds maximum length of %d characters", maxLen)
	}
	if !utf8.ValidString(desc) || strings.ContainsFunc(desc, isDisallowedTriggerDescriptionRune) {
		return errors.New("description must not contain control or format characters")
	}
	return nil
}

func validateTriggerName(raw string) (string, error) {
	return validateTriggerResourceName(raw, maxTriggerNameLen)
}

// authorizeGitHubTriggerSource validates Stage 3 §4.2 / §8.2 GitHub trigger
// authoring requirements without making any outbound GitHub HTTP call:
// hub.github_triggers must be enabled (400 experiment_disabled), source.id must
// be "" (400), and the project must have an authorized GitHub App association
// (409 github_not_associated).
func (s *Server) authorizeGitHubTriggerSource(w http.ResponseWriter, r *http.Request, project *store.Project, rawSourceID string) bool {
	if !s.githubTriggersEnabled() {
		writeError(w, http.StatusBadRequest, ErrCodeExperimentDisabled,
			"GitHub triggers require the hub.github_triggers experiment to be enabled", nil)
		return false
	}
	if rawSourceID != "" {
		ValidationError(w, "source.id must be empty for github triggers", nil)
		return false
	}
	authorized, err := s.hasAuthorizedGitHubAssociationCached(r.Context(), project)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeErrorFromErr(w, err, "")
			return false
		}
		slog.ErrorContext(r.Context(), "Failed to check GitHub App association for trigger authoring",
			"project_id", project.ID, "error", err)
		InternalError(w)
		return false
	}
	if !authorized {
		writeError(w, http.StatusConflict, ErrCodeGitHubNotAssociated,
			"project does not have an authorized GitHub App association", nil)
		return false
	}
	return true
}

func triggerSourceLimitMessage(sourceKind string) string {
	if sourceKind == store.TriggerSourceKindGitHub {
		return fmt.Sprintf("GitHub trigger source has reached the maximum of %d active triggers", store.MaxActiveTriggersPerSource)
	}
	return fmt.Sprintf("webhook endpoint has reached the maximum of %d active triggers", store.MaxActiveTriggersPerSource)
}

func setTriggerETag(w http.ResponseWriter, rev int) {
	w.Header().Set("ETag", fmt.Sprintf("%q", fmt.Sprintf("%d", rev)))
}

// validateTriggerActionCrossFields refuses action payloads carrying fields
// that do not belong to actionType rather than silently dropping them
// (P1c-1 rev-3 N2). Unknown action types return true here so the caller's
// action-type switch reports the unsupported action type error.
func validateTriggerActionCrossFields(w http.ResponseWriter, actionType string, spec *TriggerActionSpec) bool {
	if spec == nil {
		return true
	}
	var badField string
	switch actionType {
	case store.TriggerActionMessage:
		switch {
		case spec.NameConflict != "":
			badField = "nameConflict"
		case spec.Template != "":
			badField = "template"
		case spec.Task != "":
			badField = "task"
		case spec.Branch != "":
			badField = "branch"
		case len(spec.Labels) > 0:
			badField = "labels"
		case spec.Ephemeral:
			badField = "ephemeral"
		case spec.GithubToken != "":
			badField = "githubToken"
		case spec.AgentRole != "":
			badField = "agentRole"
		case spec.ReuseMessage != "":
			badField = "reuseMessage"
		case spec.AckComment != nil:
			badField = "ackComment"
		case spec.NoAuth:
			badField = "noAuth"
		case spec.Body != "":
			badField = "body"
		case spec.Target != "":
			badField = "target"
		case spec.ReplyInThread:
			badField = "replyInThread"
		}
	case store.TriggerActionDispatchAgent:
		switch {
		case spec.AgentID != "":
			badField = "agentId"
		case spec.Message != "":
			badField = "message"
		case spec.Interrupt:
			badField = "interrupt"
		case spec.Plain:
			badField = "plain"
		case spec.Wake:
			badField = "wake"
		case spec.Offline != "":
			badField = "offline"
		case spec.QueueTTLSeconds != 0:
			badField = "queueTtlSeconds"
		case spec.Body != "":
			badField = "body"
		case spec.Target != "":
			badField = "target"
		case spec.ReplyInThread:
			badField = "replyInThread"
		}
	case store.TriggerActionGitHubComment:
		switch {
		case spec.AgentID != "":
			badField = "agentId"
		case spec.AgentName != "":
			badField = "agentName"
		case spec.Message != "":
			badField = "message"
		case spec.Interrupt:
			badField = "interrupt"
		case spec.Plain:
			badField = "plain"
		case spec.Wake:
			badField = "wake"
		case spec.Offline != "":
			badField = "offline"
		case spec.QueueTTLSeconds != 0:
			badField = "queueTtlSeconds"
		case spec.NameConflict != "":
			badField = "nameConflict"
		case spec.Template != "":
			badField = "template"
		case spec.Task != "":
			badField = "task"
		case spec.Branch != "":
			badField = "branch"
		case len(spec.Labels) > 0:
			badField = "labels"
		case spec.Ephemeral:
			badField = "ephemeral"
		case spec.GithubToken != "":
			badField = "githubToken"
		case spec.AgentRole != "":
			badField = "agentRole"
		case spec.ReuseMessage != "":
			badField = "reuseMessage"
		case spec.AckComment != nil:
			badField = "ackComment"
		case spec.NoAuth:
			badField = "noAuth"
		}
	default:
		return true
	}
	if badField != "" {
		ValidationError(w, fmt.Sprintf("action.%s is not supported for action type %q", badField, actionType), map[string]any{
			"field":      "action." + badField,
			"actionType": actionType,
		})
		return false
	}
	return true
}

// readTriggerRequestJSON caps trigger and webhook-endpoint write request bodies
// at maxTriggerRequestBodyBytes (64 KiB) via http.MaxBytesReader and maps
// *http.MaxBytesError to 413 payload_too_large.
func readTriggerRequestJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxTriggerRequestBodyBytes)
	}
	if err := readJSON(r, v); err != nil {
		if isMaxBytesError(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				fmt.Sprintf("request body exceeds %d byte limit", maxTriggerRequestBodyBytes), nil)
			return false
		}
		BadRequest(w, "Invalid JSON body: "+err.Error())
		return false
	}
	if r.Body != nil {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			if isMaxBytesError(err) {
				writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
					fmt.Sprintf("request body exceeds %d byte limit", maxTriggerRequestBodyBytes), nil)
				return false
			}
			BadRequest(w, "Invalid JSON body: "+err.Error())
			return false
		}
	}
	return true
}

func (s *Server) createTrigger(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	var req CreateTriggerRequest
	if !readTriggerRequestJSON(w, r, &req) {
		return
	}

	name, err := validateTriggerName(req.Name)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	if err := validateTriggerResourceDescription(req.Description, maxTriggerDescriptionLen); err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	sourceKind := strings.TrimSpace(req.Source.Kind)
	switch sourceKind {
	case store.TriggerSourceKindWebhook:
	case store.TriggerSourceKindGitHub:
		if !s.githubTriggersEnabled() {
			writeError(w, http.StatusBadRequest, ErrCodeExperimentDisabled,
				"GitHub triggers require the hub.github_triggers experiment to be enabled", nil)
			return
		}
		if req.Source.ID != "" {
			ValidationError(w, "source.id must be empty for github triggers", nil)
			return
		}
	default:
		ValidationError(w, fmt.Sprintf("unsupported source kind %q (supported: %s, %s)",
			sourceKind, store.TriggerSourceKindWebhook, store.TriggerSourceKindGitHub), nil)
		return
	}

	defaultTypes := sourceKind == store.TriggerSourceKindWebhook
	explicitMatchDialect := req.Match != nil && req.Match.Rules == nil && strings.TrimSpace(req.Match.Dialect) != ""
	matchTypes, matchRules, ok := s.writeAndValidateTriggerMatch(w, ctx, req.Match, defaultTypes, explicitMatchDialect)
	if !ok {
		return
	}
	var matchForm string
	if req.Match != nil && len(req.Match.Form) > 0 {
		matchForm, err = validateTriggerMatchForm(req.Match.Form)
		if err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
	}
	explicitSubjectDialect := req.Subject != nil && strings.TrimSpace(req.Subject.Dialect) != ""
	subjectDialect, subjectRule, subjectMode, coalesceSeconds, ok := s.writeAndValidateTriggerSubject(w, ctx, req.Subject, explicitSubjectDialect)
	if !ok {
		return
	}

	var resolvedSourceID string
	var ep *store.WebhookEndpoint
	switch sourceKind {
	case store.TriggerSourceKindWebhook:
		var ok bool
		ep, ok = s.resolveTriggerWebhookSource(w, r, projectID, req.Source.ID)
		if !ok {
			return
		}
		resolvedSourceID = ep.ID
	case store.TriggerSourceKindGitHub:
		if !s.authorizeGitHubTriggerSource(w, r, project, req.Source.ID) {
			return
		}
		resolvedSourceID = ""
	}

	ceiling, prov, ok := s.triggerRevisionAuthorityCeiling(w, r, projectID, ActionCreate)
	if !ok {
		return
	}
	if !authorizeTriggerReuseSubject(w, subjectMode, ceiling, prov) {
		return
	}

	var scalarAction *TriggerActionSpec
	if !isZeroTriggerActionSpec(req.Action) {
		scalarAction = &req.Action
	}
	actionSpecs, _, ok := resolveRequestTriggerActions(w, scalarAction, req.Actions, true)
	if !ok {
		return
	}
	actions, ok := s.authorizeTriggerActionSpecs(w, r, projectID, sourceKind, ep, matchTypes, subjectMode, actionSpecs, ceiling, prov)
	if !ok {
		return
	}
	if err := validateTriggerLimits(actionsHasType(actions, store.TriggerActionDispatchAgent), req.MaxFiresPerMinute, req.MaxConcurrentDispatches); err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	tr := store.Trigger{
		ID:                      api.NewUUID(),
		ProjectID:               projectID,
		Name:                    name,
		Description:             req.Description,
		SourceKind:              sourceKind,
		SourceID:                resolvedSourceID,
		MatchTypes:              matchTypes,
		MatchRules:              matchRules,
		MatchForm:               matchForm,
		Actions:                 actions,
		SubjectDialect:          subjectDialect,
		SubjectRule:             subjectRule,
		SubjectMode:             subjectMode,
		CoalesceWindowSeconds:   coalesceSeconds,
		MaxFiresPerMinute:       req.MaxFiresPerMinute,
		MaxConcurrentDispatches: req.MaxConcurrentDispatches,
		Status:                  store.TriggerStatusActive,
		CreatedBy:               identity.ID(),
		InitiatorAttribution:    newInitiatorAttribution(ctx),
		AuthorityCeiling:        ceiling,
	}

	fp := fixturePrincipalFromContext(ctx)
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		if lErr := tx.LockTriggerQuota(ctx, projectID); lErr != nil {
			return lErr
		}
		if tr.SourceKind == store.TriggerSourceKindWebhook {
			if vErr := verifyActiveWebhookSourceInTx(ctx, tx, projectID, tr.SourceID, actionsRequireReplayProtection(tr.Actions)); vErr != nil {
				return vErr
			}
		}
		if sErr := stampTrigger(ctx, tx, &tr, fp); sErr != nil {
			return sErr
		}
		projCount, cErr := tx.CountTriggers(ctx, store.TriggerFilter{ProjectID: projectID})
		if cErr != nil {
			return cErr
		}
		if projCount >= store.MaxTriggersPerProject {
			return errTriggerProjectLimitExceeded
		}
		srcCount, cErr := tx.CountTriggers(ctx, store.TriggerFilter{
			ProjectID:  projectID,
			SourceKind: tr.SourceKind,
			SourceID:   tr.SourceID,
			Status:     store.TriggerStatusActive,
		})
		if cErr != nil {
			return cErr
		}
		if srcCount >= store.MaxActiveTriggersPerSource {
			return errTriggerSourceLimitExceeded
		}
		if actionsHasType(tr.Actions, store.TriggerActionExec) {
			if capErr := checkTriggerExecCapsInTx(ctx, tx, projectID, tr.Actions, ""); capErr != nil {
				return capErr
			}
		}
		return tx.CreateTrigger(ctx, &tr)
	})
	if err != nil {
		if writeFixtureRootError(w, err) {
			return
		}
		switch {
		case errors.Is(err, errTriggerWebhookSourceNotFound):
			writeWebhookEndpointNotFound(w)
		case errors.Is(err, errTriggerSourceNotReplayProtected):
			writeError(w, http.StatusBadRequest, "source_not_replay_protected",
				"dispatch_agent triggers require a replay-protected source", nil)
		case errors.Is(err, errTriggerProjectLimitExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("project has reached the maximum of %d triggers", store.MaxTriggersPerProject), nil)
		case errors.Is(err, errTriggerSourceLimitExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				triggerSourceLimitMessage(tr.SourceKind), nil)
		case errors.Is(err, errTriggerExecProjectCapExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("project has reached the maximum of %d active exec triggers", triggerExecMaxActivePerProject), nil)
		case errors.Is(err, errTriggerExecTargetCapExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("target agent has reached the maximum of %d active exec triggers", triggerExecMaxActivePerTarget), nil)
		case errors.Is(err, store.ErrAlreadyExists):
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"a trigger with this name already exists in this project", nil)
		default:
			writeErrorFromErr(w, err, "")
		}
		return
	}

	setTriggerETag(w, tr.AuthorizationRevision)
	writeJSON(w, http.StatusCreated, buildTriggerResponse(s.newExecRevealer(ctx), tr))
}

func (s *Server) listTriggers(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	query := r.URL.Query()
	sourceKind := strings.TrimSpace(query.Get("source"))
	if sourceKind == "" {
		sourceKind = strings.TrimSpace(query.Get("sourceKind"))
	}
	filter := store.TriggerFilter{
		ProjectID:  projectID,
		Name:       strings.TrimSpace(query.Get("name")),
		SourceKind: sourceKind,
		SourceID:   strings.TrimSpace(query.Get("sourceId")),
		Status:     strings.TrimSpace(query.Get("status")),
	}
	scope := ""
	if isAgentCaller(ctx) {
		identity := GetIdentityFromContext(ctx)
		if identity == nil || identity.ID() == "" {
			Unauthorized(w)
			return
		}
		filter.InitiatorAgentID = identity.ID()
		scope = "caller"
	}

	result, err := s.store.ListTriggers(ctx, filter, listOptionsFromQuery(query))
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListTriggersResponse{
		Scope:      scope,
		Triggers:   buildTriggerResponses(s.newExecRevealer(ctx), result.Items),
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
		ServerTime: time.Now().UTC(),
	})
}

func (s *Server) loadTriggerForProject(w http.ResponseWriter, r *http.Request, projectID, triggerID string) (*store.Trigger, bool) {
	tr, err := s.store.GetTrigger(r.Context(), triggerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			NotFound(w, "Trigger")
			return nil, false
		}
		writeErrorFromErr(w, err, "")
		return nil, false
	}
	if tr.ProjectID != projectID {
		NotFound(w, "Trigger")
		return nil, false
	}
	return tr, true
}

func (s *Server) getTrigger(w http.ResponseWriter, r *http.Request, projectID, triggerID string) {
	tr, ok := s.loadTriggerForProject(w, r, projectID, triggerID)
	if !ok {
		return
	}
	if !s.authorizeTriggerRow(w, r, projectID, ActionRead, triggerRowOf(tr)) {
		return
	}
	setTriggerETag(w, tr.AuthorizationRevision)
	writeJSON(w, http.StatusOK, buildTriggerResponse(s.newExecRevealer(r.Context()), *tr))
}

func (s *Server) authorizeStoredTriggerAction(
	w http.ResponseWriter,
	r *http.Request,
	projectID string,
	sourceKind string,
	ep *store.WebhookEndpoint,
	matchTypes []string,
	subjectMode string,
	actionType string,
	rawPayload string,
	ceiling store.EffectCeiling,
	prov store.AuthorityProvenance,
) (string, bool) {
	spec := buildTriggerActionSpecRaw(actionType, rawPayload)
	switch actionType {
	case store.TriggerActionMessage:
		return s.authorizeTriggerMessageAuthoring(w, r, projectID, sourceKind, &spec)
	case store.TriggerActionDispatchAgent:
		return s.authorizeTriggerDispatchAgentAuthoring(w, r, projectID, sourceKind, ep, &spec, ceiling, prov)
	case store.TriggerActionExec:
		payload, _, ok := s.authorizeTriggerExecAuthoring(w, r, projectID, sourceKind, ep, subjectMode, &spec, ceiling, prov)
		return payload, ok
	case store.TriggerActionGitHubComment:
		return s.authorizeTriggerGitHubCommentAuthoring(w, r, projectID, sourceKind, matchTypes, &spec)
	default:
		ValidationError(w, fmt.Sprintf("unsupported action type %q (supported: %s, %s, %s, %s)",
			actionType, store.TriggerActionMessage, store.TriggerActionDispatchAgent, store.TriggerActionExec, store.TriggerActionGitHubComment), nil)
		return "", false
	}
}

func (s *Server) updateTrigger(w http.ResponseWriter, r *http.Request, projectID, triggerID string) {
	ctx := r.Context()
	tr, ok := s.loadTriggerForProject(w, r, projectID, triggerID)
	if !ok {
		return
	}
	if !s.authorizeTriggerRow(w, r, projectID, ActionUpdate, triggerEditRowOf(tr)) {
		return
	}

	var req UpdateTriggerRequest
	if !readTriggerRequestJSON(w, r, &req) {
		return
	}

	if rawIfMatch := strings.TrimSpace(r.Header.Get("If-Match")); rawIfMatch != "" {
		expected, pErr := parseIfMatchRevision(rawIfMatch)
		if pErr != nil {
			BadRequest(w, "invalid If-Match header; expected integer revision")
			return
		}
		if int(expected) != tr.AuthorizationRevision {
			writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
				"trigger revision mismatch; refresh and retry", nil)
			return
		}
	}
	if req.ExpectedRevision != nil && *req.ExpectedRevision != tr.AuthorizationRevision {
		writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
			"trigger revision mismatch; refresh and retry", nil)
		return
	}

	oldSourceID := tr.SourceID
	oldSourceKind := tr.SourceKind
	oldActions, oldActionsErr := triggerActions(*tr)
	prevRevision := tr.AuthorizationRevision
	prevRevisionKnown := tr.AuthorizationRevision != 0

	var fields store.TriggerFieldMask
	changesFutureDispatch := false
	var resolvedEP *store.WebhookEndpoint

	if req.Name != nil {
		name, err := validateTriggerName(*req.Name)
		if err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
		tr.Name = name
		fields.Name = true
	}
	if req.Description != nil {
		if err := validateTriggerResourceDescription(*req.Description, maxTriggerDescriptionLen); err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
		tr.Description = *req.Description
		fields.Description = true
	}
	if req.Source != nil {
		sourceKind := strings.TrimSpace(req.Source.Kind)
		switch sourceKind {
		case store.TriggerSourceKindWebhook:
			ep, ok := s.resolveTriggerWebhookSource(w, r, projectID, req.Source.ID)
			if !ok {
				return
			}
			resolvedEP = ep
			tr.SourceKind = store.TriggerSourceKindWebhook
			tr.SourceID = ep.ID
		case store.TriggerSourceKindGitHub:
			if !s.githubTriggersEnabled() {
				writeError(w, http.StatusBadRequest, ErrCodeExperimentDisabled,
					"GitHub triggers require the hub.github_triggers experiment to be enabled", nil)
				return
			}
			if req.Source.ID != "" {
				ValidationError(w, "source.id must be empty for github triggers", nil)
				return
			}
			if oldSourceKind != store.TriggerSourceKindGitHub &&
				(req.Match == nil || req.Match.Types == nil || len(*req.Match.Types) == 0) {
				ValidationError(w, "match.types is required when changing source.kind to github", nil)
				return
			}
			project, pErr := s.store.GetProject(ctx, projectID)
			if pErr != nil {
				if errors.Is(pErr, store.ErrNotFound) || errors.Is(pErr, store.ErrInvalidInput) {
					NotFound(w, "Project")
					return
				}
				writeErrorFromErr(w, pErr, "")
				return
			}
			if !s.authorizeGitHubTriggerSource(w, r, project, req.Source.ID) {
				return
			}
			tr.SourceKind = store.TriggerSourceKindGitHub
			tr.SourceID = ""
		default:
			ValidationError(w, fmt.Sprintf("unsupported source kind %q (supported: %s, %s)",
				sourceKind, store.TriggerSourceKindWebhook, store.TriggerSourceKindGitHub), nil)
			return
		}
		fields.SourceKind = true
		fields.SourceID = true
		changesFutureDispatch = true
	}
	if req.Match != nil {
		var patchDialect, patchExpr string
		if req.Match.Dialect != nil {
			patchDialect = *req.Match.Dialect
		}
		if req.Match.Expr != nil {
			patchExpr = *req.Match.Expr
		}
		if req.Match.Rules != nil && hasAmbiguousScalarWithRules(patchDialect, patchExpr) {
			writeAmbiguousTriggerMatchError(w)
			return
		}
		existingRules, existingRulesErr := triggerMatchRules(*tr)
		merged := TriggerMatchSpec{
			Types: tr.MatchTypes,
		}
		if req.Match.Types != nil {
			if len(*req.Match.Types) == 0 {
				ValidationError(w, "match.types must contain at least 1 entry", nil)
				return
			}
			merged.Types = *req.Match.Types
			fields.MatchTypes = true
		}
		explicitMatchDialect := false
		updatingRules := false
		switch {
		case req.Match.Rules != nil:
			merged.Rules = *req.Match.Rules
			updatingRules = true
		case req.Match.Expr != nil || strings.TrimSpace(patchDialect) != "":
			updatingRules = true
			if existingRulesErr != nil {
				if req.Match.Expr == nil {
					writeCorruptTriggerMatchRulesError(w)
					return
				}
			} else {
				if req.Match.Expr == nil && len(existingRules) > 1 {
					writeAmbiguousTriggerMatchError(w)
					return
				}
				if len(existingRules) == 1 {
					merged.Dialect = existingRules[0].Dialect
					merged.Expr = existingRules[0].Expr
				}
			}
			if req.Match.Dialect != nil {
				merged.Dialect = *req.Match.Dialect
				explicitMatchDialect = strings.TrimSpace(*req.Match.Dialect) != ""
			}
			if req.Match.Expr != nil {
				merged.Expr = *req.Match.Expr
			}
		default:
			if existingRulesErr != nil {
				writeCorruptTriggerMatchRulesError(w)
				return
			}
			merged.Rules = triggerRulesToSpecs(existingRules)
		}
		matchTypes, matchRules, ok := s.writeAndValidateTriggerMatch(w, ctx, &merged, false, explicitMatchDialect)
		if !ok {
			return
		}
		tr.MatchTypes = matchTypes
		if updatingRules {
			tr.MatchRules = matchRules
			tr.MatchRulesCorrupt = false
			tr.MatchDialect = ""
			tr.MatchRule = ""
			fields.MatchRules = true
			fields.MatchDialect = true
			fields.MatchRule = true
		}
		if req.Match.Form.Set {
			validatedForm, fErr := validateTriggerMatchForm(req.Match.Form.Raw)
			if fErr != nil {
				ValidationError(w, fErr.Error(), nil)
				return
			}
			tr.MatchForm = validatedForm
			fields.MatchForm = true
		} else if updatingRules || fields.MatchTypes {
			// Any hand edit to match rules or types clears the stored preset form (§9.2).
			tr.MatchForm = ""
			fields.MatchForm = true
		}
		if fields.MatchTypes || fields.MatchRules {
			changesFutureDispatch = true
		}
	}
	if req.Subject != nil {
		merged := TriggerSubjectSpec{
			Dialect:        tr.SubjectDialect,
			Expr:           tr.SubjectRule,
			Mode:           tr.SubjectMode,
			CoalesceWindow: formatTriggerCoalesceWindow(tr.CoalesceWindowSeconds),
		}
		if req.Subject.Dialect != nil {
			merged.Dialect = *req.Subject.Dialect
			fields.SubjectDialect = true
		}
		if req.Subject.Expr != nil {
			merged.Expr = *req.Subject.Expr
			fields.SubjectRule = true
		}
		if req.Subject.Mode != nil {
			merged.Mode = *req.Subject.Mode
			fields.SubjectMode = true
		}
		if req.Subject.CoalesceWindow != nil {
			merged.CoalesceWindow = *req.Subject.CoalesceWindow
			fields.CoalesceWindowSeconds = true
		}
		explicitSubjectDialect := req.Subject.Dialect != nil && strings.TrimSpace(*req.Subject.Dialect) != ""
		subjectDialect, subjectRule, subjectMode, coalesceSeconds, ok := s.writeAndValidateTriggerSubject(w, ctx, &merged, explicitSubjectDialect)
		if !ok {
			return
		}
		tr.SubjectDialect = subjectDialect
		tr.SubjectRule = subjectRule
		tr.SubjectMode = subjectMode
		tr.CoalesceWindowSeconds = coalesceSeconds
		if fields.SubjectDialect || fields.SubjectRule || fields.SubjectMode || fields.CoalesceWindowSeconds {
			changesFutureDispatch = true
		}
	}
	reqActionSpecs, updatingActions, ok := resolveRequestTriggerActions(w, req.Action, req.Actions, false)
	if !ok {
		return
	}
	if updatingActions {
		for _, spec := range reqActionSpecs {
			actionType := strings.TrimSpace(spec.Type)
			if actionType != store.TriggerActionMessage &&
				actionType != store.TriggerActionDispatchAgent &&
				actionType != store.TriggerActionExec &&
				actionType != store.TriggerActionGitHubComment {
				ValidationError(w, fmt.Sprintf("unsupported action type %q (supported: %s, %s, %s, %s)",
					actionType, store.TriggerActionMessage, store.TriggerActionDispatchAgent, store.TriggerActionExec, store.TriggerActionGitHubComment), nil)
				return
			}
		}
		fields.Actions = true
		fields.ActionType = true
		fields.ActionPayload = true
		changesFutureDispatch = true
	}

	if req.MaxFiresPerMinute != nil {
		tr.MaxFiresPerMinute = *req.MaxFiresPerMinute
		fields.MaxFiresPerMinute = true
	}
	if req.MaxConcurrentDispatches != nil {
		tr.MaxConcurrentDispatches = *req.MaxConcurrentDispatches
		fields.MaxConcurrentDispatches = true
	}
	if fields.MaxFiresPerMinute || fields.MaxConcurrentDispatches || updatingActions {
		hasDispatch := false
		if updatingActions {
			hasDispatch = actionSpecsHasType(reqActionSpecs, store.TriggerActionDispatchAgent)
		} else if oldActionsErr == nil {
			hasDispatch = actionsHasType(oldActions, store.TriggerActionDispatchAgent)
		}
		if err := validateTriggerLimits(hasDispatch, tr.MaxFiresPerMinute, tr.MaxConcurrentDispatches); err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
	}

	var newAttr *store.InitiatorAttribution
	if changesFutureDispatch {
		// Re-authorizing future dispatch requires the editor to hold access to
		// both the source and EVERY target action of the resulting trigger, and
		// re-validates stored match/subject rules not already compiled above.
		if req.Source == nil {
			switch tr.SourceKind {
			case store.TriggerSourceKindWebhook:
				ep, ok := s.resolveTriggerWebhookSource(w, r, projectID, tr.SourceID)
				if !ok {
					return
				}
				resolvedEP = ep
			case store.TriggerSourceKindGitHub:
				project, pErr := s.store.GetProject(ctx, projectID)
				if pErr != nil {
					if errors.Is(pErr, store.ErrNotFound) || errors.Is(pErr, store.ErrInvalidInput) {
						NotFound(w, "Project")
						return
					}
					writeErrorFromErr(w, pErr, "")
					return
				}
				if !s.authorizeGitHubTriggerSource(w, r, project, tr.SourceID) {
					return
				}
			default:
				ValidationError(w, fmt.Sprintf("unsupported source kind %q (supported: %s, %s)",
					tr.SourceKind, store.TriggerSourceKindWebhook, store.TriggerSourceKindGitHub), nil)
				return
			}
		}
		if req.Match == nil {
			existingRules, rErr := triggerMatchRules(*tr)
			if rErr != nil {
				writeCorruptTriggerMatchRulesError(w)
				return
			}
			if _, _, ok := s.writeAndValidateTriggerMatch(w, ctx, &TriggerMatchSpec{
				Types: tr.MatchTypes,
				Rules: triggerRulesToSpecs(existingRules),
			}, false, false); !ok {
				return
			}
		}
		if req.Subject == nil {
			if _, _, _, _, ok := s.writeAndValidateTriggerSubject(w, ctx, &TriggerSubjectSpec{
				Dialect:        tr.SubjectDialect,
				Expr:           tr.SubjectRule,
				Mode:           tr.SubjectMode,
				CoalesceWindow: formatTriggerCoalesceWindow(tr.CoalesceWindowSeconds),
			}, false); !ok {
				return
			}
		}
		ceiling, prov, ok := s.triggerRevisionAuthorityCeiling(w, r, projectID, ActionUpdate)
		if !ok {
			return
		}
		if !authorizeTriggerReuseSubject(w, tr.SubjectMode, ceiling, prov) {
			return
		}
		if oldSourceKind != store.TriggerSourceKindGitHub &&
			tr.SourceKind == store.TriggerSourceKindGitHub &&
			!updatingActions &&
			oldActionsErr == nil &&
			actionsHasType(oldActions, store.TriggerActionDispatchAgent) {
			writeError(w, http.StatusBadRequest, "github_token_required",
				"set action.githubToken explicitly to installation or user_preferred when changing a dispatch_agent trigger source to github without an action payload",
				map[string]any{"field": "action.githubToken"})
			return
		}
		if oldActionsErr == nil {
			for _, oldAct := range oldActions {
				if oldAct.Type != store.TriggerActionExec {
					continue
				}
				prevLive, ok := s.authorizeStoredExecTargetForUpdate(w, r, projectID, execPayloadTarget(oldAct.Payload))
				if !ok {
					return
				}
				if !prevLive {
					if !updatingActions {
						ValidationError(w, "retargeting an exec trigger whose previous target was deleted requires command", nil)
						return
					}
					for _, ns := range reqActionSpecs {
						if strings.TrimSpace(ns.Type) == store.TriggerActionExec && len(ns.Command) == 0 {
							ValidationError(w, "retargeting an exec trigger whose previous target was deleted requires command", nil)
							return
						}
					}
				}
			}
		} else if updatingActions {
			// L-1: fail closed when replacing corrupt stored actions. Re-authorize
			// agent.attach on the legacy ActionPayload exec target as a floor, and
			// refuse with 403 when no target can be determined.
			legacyTarget := execPayloadTarget(tr.ActionPayload)
			if legacyTarget == "" {
				Forbidden(w)
				return
			}
			prevLive, ok := s.authorizeStoredExecTargetForUpdate(w, r, projectID, legacyTarget)
			if !ok {
				return
			}
			if !prevLive {
				for _, ns := range reqActionSpecs {
					if strings.TrimSpace(ns.Type) == store.TriggerActionExec && len(ns.Command) == 0 {
						ValidationError(w, "retargeting an exec trigger whose previous target was deleted requires command", nil)
						return
					}
				}
			}
		}
		if updatingActions {
			newActions, ok := s.authorizeTriggerActionSpecs(w, r, projectID, tr.SourceKind, resolvedEP, tr.MatchTypes, tr.SubjectMode, reqActionSpecs, ceiling, prov)
			if !ok {
				return
			}
			tr.Actions = newActions
			tr.ActionsCorrupt = false
			tr.ActionType = ""
			tr.ActionPayload = ""
		} else {
			if oldActionsErr != nil {
				writeCorruptTriggerActionsError(w)
				return
			}
			normalizedActions, ok := s.authorizeStoredTriggerActions(w, r, projectID, tr.SourceKind, resolvedEP, tr.MatchTypes, tr.SubjectMode, oldActions, ceiling, prov)
			if !ok {
				return
			}
			tr.Actions = normalizedActions
			tr.ActionsCorrupt = false
			tr.ActionType = ""
			tr.ActionPayload = ""
			fields.Actions = true
			fields.ActionType = true
			fields.ActionPayload = true
		}
		attr := reattributeInitiator(ctx, tr.InitiatorAttribution)
		newAttr = &attr
		tr.InitiatorAttribution = attr
		tr.AuthorityCeiling = ceiling
	}

	err := s.store.WithTx(ctx, func(tx store.Store) error {
		if changesFutureDispatch {
			if lErr := tx.LockTriggerQuota(ctx, projectID); lErr != nil {
				return lErr
			}
			if tr.SourceKind == store.TriggerSourceKindWebhook {
				if vErr := verifyActiveWebhookSourceInTx(ctx, tx, projectID, tr.SourceID, actionsRequireReplayProtection(tr.Actions)); vErr != nil {
					return vErr
				}
			}
			if tr.Status == store.TriggerStatusActive &&
				(tr.SourceID != oldSourceID || tr.SourceKind != oldSourceKind) {
				srcCount, cErr := tx.CountTriggers(ctx, store.TriggerFilter{
					ProjectID:  projectID,
					SourceKind: tr.SourceKind,
					SourceID:   tr.SourceID,
					Status:     store.TriggerStatusActive,
				})
				if cErr != nil {
					return cErr
				}
				if srcCount >= store.MaxActiveTriggersPerSource {
					return errTriggerSourceLimitExceeded
				}
			}
			if tr.Status == store.TriggerStatusActive && actionsHasType(tr.Actions, store.TriggerActionExec) {
				if capErr := checkTriggerExecCapsInTx(ctx, tx, projectID, tr.Actions, tr.ID); capErr != nil {
					return capErr
				}
			}
		}
		return tx.UpdateTrigger(ctx, tr, fields, prevRevision, prevRevisionKnown, newAttr)
	})
	if err != nil {
		switch {
		case errors.Is(err, errTriggerWebhookSourceNotFound):
			writeWebhookEndpointNotFound(w)
		case errors.Is(err, errTriggerSourceNotReplayProtected):
			writeError(w, http.StatusBadRequest, "source_not_replay_protected",
				"dispatch_agent triggers require a replay-protected source", nil)
		case errors.Is(err, store.ErrRevisionConflict):
			writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
				"trigger was concurrently modified; refresh and retry", nil)
		case errors.Is(err, errTriggerSourceLimitExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				triggerSourceLimitMessage(tr.SourceKind), nil)
		case errors.Is(err, errTriggerExecProjectCapExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("project has reached the maximum of %d active exec triggers", triggerExecMaxActivePerProject), nil)
		case errors.Is(err, errTriggerExecTargetCapExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("target agent has reached the maximum of %d active exec triggers", triggerExecMaxActivePerTarget), nil)
		case errors.Is(err, store.ErrAlreadyExists):
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"a trigger with this name already exists in this project", nil)
		default:
			writeErrorFromErr(w, err, "")
		}
		return
	}

	if changesFutureDispatch {
		s.invalidateTriggerRuleCache(projectID, triggerID, int64(prevRevision))
	}

	setTriggerETag(w, tr.AuthorizationRevision)
	writeJSON(w, http.StatusOK, buildTriggerResponse(s.newExecRevealer(ctx), *tr))
}

func (s *Server) pauseTrigger(w http.ResponseWriter, r *http.Request, projectID, triggerID string) {
	ctx := r.Context()
	tr, ok := s.loadTriggerForProject(w, r, projectID, triggerID)
	if !ok {
		return
	}
	if !s.authorizeTriggerRow(w, r, projectID, ActionUpdate, triggerRowOf(tr)) {
		return
	}
	if tr.Status != store.TriggerStatusActive {
		ValidationError(w, "only active triggers can be paused", nil)
		return
	}

	audit := newTriggerAudit(ctx, mutationTypeTriggerPause, triggerID)
	afterBytes, _ := json.Marshal(map[string]any{
		"project_id": projectID,
		"trigger_id": triggerID,
		"name":       tr.Name,
	})
	audit.AfterSummary = string(afterBytes)

	if err := s.store.WithTx(ctx, func(tx store.Store) error {
		if uErr := tx.UpdateTriggerStatus(ctx, triggerID, store.TriggerStatusPaused, store.TriggerStatusReasonNone); uErr != nil {
			return uErr
		}
		return tx.CreateMutationAudit(ctx, audit)
	}); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	tr.Status = store.TriggerStatusPaused
	tr.StatusReason = store.TriggerStatusReasonNone
	setTriggerETag(w, tr.AuthorizationRevision)
	writeJSON(w, http.StatusOK, buildTriggerResponse(s.newExecRevealer(ctx), *tr))
}

func (s *Server) resumeTrigger(w http.ResponseWriter, r *http.Request, projectID, triggerID string) {
	ctx := r.Context()
	tr, ok := s.loadTriggerForProject(w, r, projectID, triggerID)
	if !ok {
		return
	}
	if !s.authorizeTriggerRow(w, r, projectID, ActionUpdate, triggerRowOf(tr)) {
		return
	}
	if tr.Status != store.TriggerStatusPaused {
		ValidationError(w, "only paused triggers can be resumed", nil)
		return
	}

	var ep *store.WebhookEndpoint
	switch tr.SourceKind {
	case store.TriggerSourceKindWebhook:
		var ok bool
		ep, ok = s.resolveTriggerWebhookSource(w, r, projectID, tr.SourceID)
		if !ok {
			return
		}
	case store.TriggerSourceKindGitHub:
		project, pErr := s.store.GetProject(ctx, projectID)
		if pErr != nil {
			if errors.Is(pErr, store.ErrNotFound) || errors.Is(pErr, store.ErrInvalidInput) {
				NotFound(w, "Project")
				return
			}
			writeErrorFromErr(w, pErr, "")
			return
		}
		if !s.authorizeGitHubTriggerSource(w, r, project, tr.SourceID) {
			return
		}
		if tr.StatusReason == store.TriggerStatusReasonSourceDeleted && !s.requireGitHubInstallationHasRepo(w, r, project) {
			return
		}
	default:
		ValidationError(w, fmt.Sprintf("unsupported source kind %q (supported: %s, %s)",
			tr.SourceKind, store.TriggerSourceKindWebhook, store.TriggerSourceKindGitHub), nil)
		return
	}
	existingRules, rErr := triggerMatchRules(*tr)
	if rErr != nil {
		writeCorruptTriggerMatchRulesError(w)
		return
	}
	if _, _, ok := s.writeAndValidateTriggerMatch(w, ctx, &TriggerMatchSpec{
		Types: tr.MatchTypes,
		Rules: triggerRulesToSpecs(existingRules),
	}, false, false); !ok {
		return
	}
	if _, _, _, _, ok := s.writeAndValidateTriggerSubject(w, ctx, &TriggerSubjectSpec{
		Dialect:        tr.SubjectDialect,
		Expr:           tr.SubjectRule,
		Mode:           tr.SubjectMode,
		CoalesceWindow: formatTriggerCoalesceWindow(tr.CoalesceWindowSeconds),
	}, false); !ok {
		return
	}
	ceiling, prov, ok := s.triggerRevisionAuthorityCeiling(w, r, projectID, ActionUpdate)
	if !ok {
		return
	}
	if !authorizeTriggerReuseSubject(w, tr.SubjectMode, ceiling, prov) {
		return
	}
	storedActions, actErr := triggerActions(*tr)
	if actErr != nil {
		writeCorruptTriggerActionsError(w)
		return
	}
	normalizedActions, ok := s.authorizeStoredTriggerActions(w, r, projectID, tr.SourceKind, ep, tr.MatchTypes, tr.SubjectMode, storedActions, ceiling, prov)
	if !ok {
		return
	}

	prevRevision := tr.AuthorizationRevision
	prevRevisionKnown := tr.AuthorizationRevision != 0
	tr.Status = store.TriggerStatusActive
	tr.StatusReason = store.TriggerStatusReasonNone
	tr.Actions = normalizedActions
	tr.ActionsCorrupt = false
	tr.ActionType = ""
	tr.ActionPayload = ""
	newAttr := reattributeInitiator(ctx, tr.InitiatorAttribution)
	tr.InitiatorAttribution = newAttr
	tr.AuthorityCeiling = ceiling

	fields := store.TriggerFieldMask{Status: true, Actions: true, ActionType: true, ActionPayload: true}
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		if lErr := tx.LockTriggerQuota(ctx, projectID); lErr != nil {
			return lErr
		}
		if tr.SourceKind == store.TriggerSourceKindWebhook {
			if vErr := verifyActiveWebhookSourceInTx(ctx, tx, projectID, tr.SourceID, actionsRequireReplayProtection(tr.Actions)); vErr != nil {
				return vErr
			}
		}
		srcCount, cErr := tx.CountTriggers(ctx, store.TriggerFilter{
			ProjectID:  projectID,
			SourceKind: tr.SourceKind,
			SourceID:   tr.SourceID,
			Status:     store.TriggerStatusActive,
		})
		if cErr != nil {
			return cErr
		}
		if srcCount >= store.MaxActiveTriggersPerSource {
			return errTriggerSourceLimitExceeded
		}
		if actionsHasType(tr.Actions, store.TriggerActionExec) {
			if capErr := checkTriggerExecCapsInTx(ctx, tx, projectID, tr.Actions, tr.ID); capErr != nil {
				return capErr
			}
		}
		return tx.UpdateTrigger(ctx, tr, fields, prevRevision, prevRevisionKnown, &newAttr)
	})
	if err != nil {
		switch {
		case errors.Is(err, errTriggerWebhookSourceNotFound):
			writeWebhookEndpointNotFound(w)
		case errors.Is(err, errTriggerSourceNotReplayProtected):
			writeError(w, http.StatusBadRequest, "source_not_replay_protected",
				"dispatch_agent triggers require a replay-protected source", nil)
		case errors.Is(err, store.ErrRevisionConflict):
			writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
				"trigger was concurrently modified; refresh and retry", nil)
		case errors.Is(err, errTriggerSourceLimitExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				triggerSourceLimitMessage(tr.SourceKind), nil)
		case errors.Is(err, errTriggerExecProjectCapExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("project has reached the maximum of %d active exec triggers", triggerExecMaxActivePerProject), nil)
		case errors.Is(err, errTriggerExecTargetCapExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("target agent has reached the maximum of %d active exec triggers", triggerExecMaxActivePerTarget), nil)
		default:
			writeErrorFromErr(w, err, "")
		}
		return
	}

	s.invalidateTriggerRuleCache(projectID, triggerID, int64(prevRevision))

	setTriggerETag(w, tr.AuthorizationRevision)
	writeJSON(w, http.StatusOK, buildTriggerResponse(s.newExecRevealer(ctx), *tr))
}

func (s *Server) deleteTrigger(w http.ResponseWriter, r *http.Request, projectID, triggerID string) {
	ctx := r.Context()
	tr, ok := s.loadTriggerForProject(w, r, projectID, triggerID)
	if !ok {
		return
	}
	if !s.authorizeTriggerRow(w, r, projectID, ActionDelete, triggerRowOf(tr)) {
		return
	}

	audit := newTriggerAudit(ctx, mutationTypeTriggerDelete, triggerID)
	beforeBytes, _ := json.Marshal(map[string]any{
		"trigger_id": triggerID,
		"project_id": projectID,
		"name":       tr.Name,
	})
	audit.BeforeSummary = string(beforeBytes)

	if err := s.store.WithTx(ctx, func(tx store.Store) error {
		if dErr := tx.DeleteTrigger(ctx, triggerID); dErr != nil {
			return dErr
		}
		return tx.CreateMutationAudit(ctx, audit)
	}); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	s.invalidateTriggerRuleCache(projectID, triggerID, 0)

	w.WriteHeader(http.StatusNoContent)
}

// getTriggerHistory handles GET /api/v1/projects/{projectId}/triggers/{id}/history.
func (s *Server) getTriggerHistory(w http.ResponseWriter, r *http.Request, projectID, triggerID string) {
	ctx := r.Context()
	tr, ok := s.loadTriggerForProject(w, r, projectID, triggerID)
	if !ok {
		return
	}
	if !s.authorizeTriggerRow(w, r, projectID, ActionRead, triggerRowOf(tr)) {
		return
	}

	result, err := s.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		ProjectID: projectID,
		TriggerID: tr.ID,
	}, listOptionsFromQuery(r.URL.Query()))
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListTriggerFiresResponse{
		Fires:      s.buildTriggerFireResponses(ctx, result.Items),
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
		ServerTime: time.Now().UTC(),
	})
}
