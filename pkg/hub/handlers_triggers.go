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
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

const (
	maxTriggerNameLen        = 128
	maxTriggerDescriptionLen = 1024
	defaultWebhookMatchType  = "webhook.received"
)

var (
	errTriggerProjectLimitExceeded = errors.New("project trigger limit exceeded")
	errTriggerSourceLimitExceeded  = errors.New("source active trigger limit exceeded")
)

// CreateTriggerRequest is the request body for POST /triggers.
type CreateTriggerRequest struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Source      TriggerSourceSpec   `json:"source"`
	Match       *TriggerMatchSpec   `json:"match,omitempty"`
	Action      TriggerActionSpec   `json:"action"`
	Subject     *TriggerSubjectSpec `json:"subject,omitempty"`
}

// TriggerMatchPatchSpec holds partial updates to a trigger's match block.
type TriggerMatchPatchSpec struct {
	Types   *[]string `json:"types,omitempty"`
	Dialect *string   `json:"dialect,omitempty"`
	Expr    *string   `json:"expr,omitempty"`
}

// TriggerSubjectPatchSpec holds partial updates to a trigger's subject block.
type TriggerSubjectPatchSpec struct {
	Dialect *string `json:"dialect,omitempty"`
	Expr    *string `json:"expr,omitempty"`
	Mode    *string `json:"mode,omitempty"`
}

// UpdateTriggerRequest is the request body for PATCH /triggers/{id}.
type UpdateTriggerRequest struct {
	Name             *string                  `json:"name,omitempty"`
	Description      *string                  `json:"description,omitempty"`
	Source           *TriggerSourceSpec       `json:"source,omitempty"`
	Match            *TriggerMatchPatchSpec   `json:"match,omitempty"`
	Action           *TriggerActionSpec       `json:"action,omitempty"`
	Subject          *TriggerSubjectPatchSpec `json:"subject,omitempty"`
	ExpectedRevision *int                     `json:"expectedRevision,omitempty"`
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
			default:
				NotFound(w, "Trigger action")
				return
			}
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
		}
	})(w, r)
}

func validateTriggerName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("name is required")
	}
	if len(name) > maxTriggerNameLen {
		return "", fmt.Errorf("name exceeds maximum length of %d characters", maxTriggerNameLen)
	}
	if strings.ContainsAny(name, "/\r\n\t") {
		return "", errors.New("name must not contain '/' or control characters")
	}
	return name, nil
}

func validateStage1b1Match(spec *TriggerMatchSpec) ([]string, string, string, error) {
	var types []string
	dialect := ruleengine.DialectCEL
	expr := ""
	if spec != nil {
		types = spec.Types
		if d := strings.TrimSpace(spec.Dialect); d != "" {
			dialect = d
		}
		expr = strings.TrimSpace(spec.Expr)
	}
	if len(types) == 0 {
		types = []string{defaultWebhookMatchType}
	}
	if dialect != ruleengine.DialectCEL {
		return nil, "", "", fmt.Errorf("unsupported match dialect %q (supported: %s)", dialect, ruleengine.DialectCEL)
	}
	if expr != "" {
		return nil, "", "", errors.New("match_rule is not yet supported")
	}
	if err := triggerrule.ValidateMatchTypes(types); err != nil {
		return nil, "", "", err
	}
	copied := make([]string, len(types))
	copy(copied, types)
	return copied, dialect, "", nil
}

func validateStage1b1Subject(spec *TriggerSubjectSpec) (string, string, string, error) {
	dialect := ruleengine.DialectCEL
	expr := ""
	mode := store.TriggerSubjectModeNew
	if spec != nil {
		if d := strings.TrimSpace(spec.Dialect); d != "" {
			dialect = d
		}
		expr = strings.TrimSpace(spec.Expr)
		if m := strings.TrimSpace(spec.Mode); m != "" {
			mode = m
		}
	}
	if dialect != ruleengine.DialectCEL {
		return "", "", "", fmt.Errorf("unsupported subject dialect %q (supported: %s)", dialect, ruleengine.DialectCEL)
	}
	if expr != "" {
		return "", "", "", errors.New("subject_rule is not yet supported")
	}
	switch mode {
	case store.TriggerSubjectModeNew:
		return dialect, "", mode, nil
	case store.TriggerSubjectModeSkip:
		return "", "", "", errors.New(`subject_mode "skip" is not yet supported`)
	default:
		return "", "", "", fmt.Errorf("unsupported subject mode %q", mode)
	}
}

func setTriggerETag(w http.ResponseWriter, rev int) {
	w.Header().Set("ETag", fmt.Sprintf("%q", fmt.Sprintf("%d", rev)))
}

func (s *Server) createTrigger(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	var req CreateTriggerRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid JSON body: "+err.Error())
		return
	}

	name, err := validateTriggerName(req.Name)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	if len(req.Description) > maxTriggerDescriptionLen {
		ValidationError(w, fmt.Sprintf("description exceeds maximum length of %d characters", maxTriggerDescriptionLen), nil)
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

	sourceKind := strings.TrimSpace(req.Source.Kind)
	if sourceKind != store.TriggerSourceKindWebhook {
		ValidationError(w, fmt.Sprintf("unsupported source kind %q (supported: %s)", sourceKind, store.TriggerSourceKindWebhook), nil)
		return
	}
	ep, ok := s.resolveTriggerWebhookSource(w, r, projectID, req.Source.ID)
	if !ok {
		return
	}

	matchTypes, matchDialect, matchRule, err := validateStage1b1Match(req.Match)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	subjectDialect, subjectRule, subjectMode, err := validateStage1b1Subject(req.Subject)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	actionType := strings.TrimSpace(req.Action.Type)
	if actionType != store.TriggerActionMessage {
		ValidationError(w, fmt.Sprintf("unsupported action type %q (supported: %s)", actionType, store.TriggerActionMessage), nil)
		return
	}
	actionPayload, ok := s.authorizeTriggerMessageAuthoring(w, r, projectID, &req.Action)
	if !ok {
		return
	}

	ceiling, ok := s.triggerRevisionAuthorityCeiling(w, r, projectID, ActionCreate)
	if !ok {
		return
	}

	trigger := store.Trigger{
		ID:                   api.NewUUID(),
		ProjectID:            projectID,
		Name:                 name,
		Description:          req.Description,
		SourceKind:           store.TriggerSourceKindWebhook,
		SourceID:             ep.ID,
		MatchTypes:           matchTypes,
		MatchDialect:         matchDialect,
		MatchRule:            matchRule,
		ActionType:           store.TriggerActionMessage,
		ActionPayload:        actionPayload,
		SubjectDialect:       subjectDialect,
		SubjectRule:          subjectRule,
		SubjectMode:          subjectMode,
		Status:               store.TriggerStatusActive,
		CreatedBy:            identity.ID(),
		InitiatorAttribution: newInitiatorAttribution(ctx),
		AuthorityCeiling:     ceiling,
	}

	fp := fixturePrincipalFromContext(ctx)
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		if lErr := tx.LockTriggerQuota(ctx, projectID); lErr != nil {
			return lErr
		}
		if sErr := stampTrigger(ctx, tx, &trigger, fp); sErr != nil {
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
			SourceKind: trigger.SourceKind,
			SourceID:   trigger.SourceID,
			Status:     store.TriggerStatusActive,
		})
		if cErr != nil {
			return cErr
		}
		if srcCount >= store.MaxActiveTriggersPerSource {
			return errTriggerSourceLimitExceeded
		}
		return tx.CreateTrigger(ctx, &trigger)
	})
	if err != nil {
		if writeFixtureRootError(w, err) {
			return
		}
		switch {
		case errors.Is(err, errTriggerProjectLimitExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("project has reached the maximum of %d triggers", store.MaxTriggersPerProject), nil)
		case errors.Is(err, errTriggerSourceLimitExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("webhook endpoint has reached the maximum of %d active triggers", store.MaxActiveTriggersPerSource), nil)
		case errors.Is(err, store.ErrAlreadyExists):
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"a trigger with this name already exists in this project", nil)
		default:
			writeErrorFromErr(w, err, "")
		}
		return
	}

	setTriggerETag(w, trigger.AuthorizationRevision)
	writeJSON(w, http.StatusCreated, buildTriggerResponse(trigger))
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
		Triggers:   buildTriggerResponses(result.Items),
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
	writeJSON(w, http.StatusOK, buildTriggerResponse(*tr))
}

func (s *Server) authorizeStoredTriggerAction(w http.ResponseWriter, r *http.Request, projectID, actionType, rawPayload string) (string, bool) {
	if actionType != store.TriggerActionMessage {
		ValidationError(w, fmt.Sprintf("unsupported action type %q (supported: %s)", actionType, store.TriggerActionMessage), nil)
		return "", false
	}
	spec := buildTriggerActionSpec(actionType, rawPayload)
	return s.authorizeTriggerMessageAuthoring(w, r, projectID, &spec)
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
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid JSON body: "+err.Error())
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
	prevRevision := tr.AuthorizationRevision
	prevRevisionKnown := tr.AuthorizationRevision != 0

	var fields store.TriggerFieldMask
	changesFutureDispatch := false

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
		if len(*req.Description) > maxTriggerDescriptionLen {
			ValidationError(w, fmt.Sprintf("description exceeds maximum length of %d characters", maxTriggerDescriptionLen), nil)
			return
		}
		tr.Description = *req.Description
		fields.Description = true
	}
	if req.Source != nil {
		sourceKind := strings.TrimSpace(req.Source.Kind)
		if sourceKind != store.TriggerSourceKindWebhook {
			ValidationError(w, fmt.Sprintf("unsupported source kind %q (supported: %s)", sourceKind, store.TriggerSourceKindWebhook), nil)
			return
		}
		ep, ok := s.resolveTriggerWebhookSource(w, r, projectID, req.Source.ID)
		if !ok {
			return
		}
		tr.SourceKind = store.TriggerSourceKindWebhook
		tr.SourceID = ep.ID
		fields.SourceKind = true
		fields.SourceID = true
		changesFutureDispatch = true
	}
	if req.Match != nil {
		merged := TriggerMatchSpec{
			Types:   tr.MatchTypes,
			Dialect: tr.MatchDialect,
			Expr:    tr.MatchRule,
		}
		if req.Match.Types != nil {
			if len(*req.Match.Types) == 0 {
				ValidationError(w, "match.types must contain at least 1 entry", nil)
				return
			}
			merged.Types = *req.Match.Types
			fields.MatchTypes = true
		}
		if req.Match.Dialect != nil {
			merged.Dialect = *req.Match.Dialect
			fields.MatchDialect = true
		}
		if req.Match.Expr != nil {
			merged.Expr = *req.Match.Expr
			fields.MatchRule = true
		}
		matchTypes, matchDialect, matchRule, err := validateStage1b1Match(&merged)
		if err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
		tr.MatchTypes = matchTypes
		tr.MatchDialect = matchDialect
		tr.MatchRule = matchRule
		if fields.MatchTypes || fields.MatchDialect || fields.MatchRule {
			changesFutureDispatch = true
		}
	}
	if req.Subject != nil {
		merged := TriggerSubjectSpec{
			Dialect: tr.SubjectDialect,
			Expr:    tr.SubjectRule,
			Mode:    tr.SubjectMode,
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
		subjectDialect, subjectRule, subjectMode, err := validateStage1b1Subject(&merged)
		if err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
		tr.SubjectDialect = subjectDialect
		tr.SubjectRule = subjectRule
		tr.SubjectMode = subjectMode
		if fields.SubjectDialect || fields.SubjectRule || fields.SubjectMode {
			changesFutureDispatch = true
		}
	}
	if req.Action != nil {
		actionType := strings.TrimSpace(req.Action.Type)
		if actionType != store.TriggerActionMessage {
			ValidationError(w, fmt.Sprintf("unsupported action type %q (supported: %s)", actionType, store.TriggerActionMessage), nil)
			return
		}
		actionPayload, ok := s.authorizeTriggerMessageAuthoring(w, r, projectID, req.Action)
		if !ok {
			return
		}
		tr.ActionType = store.TriggerActionMessage
		tr.ActionPayload = actionPayload
		fields.ActionType = true
		fields.ActionPayload = true
		changesFutureDispatch = true
	}

	var newAttr *store.InitiatorAttribution
	if changesFutureDispatch {
		// Re-authorizing future dispatch requires the editor to hold access to
		// both the source and the target action of the resulting trigger.
		if req.Source == nil {
			if _, ok := s.resolveTriggerWebhookSource(w, r, projectID, tr.SourceID); !ok {
				return
			}
		}
		if req.Action == nil {
			normalizedPayload, ok := s.authorizeStoredTriggerAction(w, r, projectID, tr.ActionType, tr.ActionPayload)
			if !ok {
				return
			}
			tr.ActionPayload = normalizedPayload
			fields.ActionPayload = true
		}
		ceiling, ok := s.triggerRevisionAuthorityCeiling(w, r, projectID, ActionUpdate)
		if !ok {
			return
		}
		attr := reattributeInitiator(ctx, tr.InitiatorAttribution)
		newAttr = &attr
		tr.InitiatorAttribution = attr
		tr.AuthorityCeiling = ceiling
	}

	err := s.store.WithTx(ctx, func(tx store.Store) error {
		if tr.Status == store.TriggerStatusActive &&
			(tr.SourceID != oldSourceID || tr.SourceKind != oldSourceKind) {
			if lErr := tx.LockTriggerQuota(ctx, projectID); lErr != nil {
				return lErr
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
		}
		return tx.UpdateTrigger(ctx, tr, fields, prevRevision, prevRevisionKnown, newAttr)
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrRevisionConflict):
			writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
				"trigger was concurrently modified; refresh and retry", nil)
		case errors.Is(err, errTriggerSourceLimitExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("webhook endpoint has reached the maximum of %d active triggers", store.MaxActiveTriggersPerSource), nil)
		case errors.Is(err, store.ErrAlreadyExists):
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"a trigger with this name already exists in this project", nil)
		default:
			writeErrorFromErr(w, err, "")
		}
		return
	}

	setTriggerETag(w, tr.AuthorizationRevision)
	writeJSON(w, http.StatusOK, buildTriggerResponse(*tr))
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
	writeJSON(w, http.StatusOK, buildTriggerResponse(*tr))
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

	if tr.SourceKind != store.TriggerSourceKindWebhook {
		ValidationError(w, fmt.Sprintf("unsupported source kind %q (supported: %s)", tr.SourceKind, store.TriggerSourceKindWebhook), nil)
		return
	}
	if _, ok := s.resolveTriggerWebhookSource(w, r, projectID, tr.SourceID); !ok {
		return
	}
	normalizedPayload, ok := s.authorizeStoredTriggerAction(w, r, projectID, tr.ActionType, tr.ActionPayload)
	if !ok {
		return
	}

	ceiling, ok := s.triggerRevisionAuthorityCeiling(w, r, projectID, ActionUpdate)
	if !ok {
		return
	}

	prevRevision := tr.AuthorizationRevision
	prevRevisionKnown := tr.AuthorizationRevision != 0
	tr.Status = store.TriggerStatusActive
	tr.StatusReason = store.TriggerStatusReasonNone
	tr.ActionPayload = normalizedPayload
	newAttr := reattributeInitiator(ctx, tr.InitiatorAttribution)
	tr.InitiatorAttribution = newAttr
	tr.AuthorityCeiling = ceiling

	fields := store.TriggerFieldMask{Status: true, ActionPayload: true}
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		if lErr := tx.LockTriggerQuota(ctx, projectID); lErr != nil {
			return lErr
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
		return tx.UpdateTrigger(ctx, tr, fields, prevRevision, prevRevisionKnown, &newAttr)
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrRevisionConflict):
			writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
				"trigger was concurrently modified; refresh and retry", nil)
		case errors.Is(err, errTriggerSourceLimitExceeded):
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("webhook endpoint has reached the maximum of %d active triggers", store.MaxActiveTriggersPerSource), nil)
		default:
			writeErrorFromErr(w, err, "")
		}
		return
	}

	setTriggerETag(w, tr.AuthorizationRevision)
	writeJSON(w, http.StatusOK, buildTriggerResponse(*tr))
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

	w.WriteHeader(http.StatusNoContent)
}
