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
	"encoding/json"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

// Fail-closed response serializers for webhook endpoints, triggers, and
// trigger fires (ptone/scion#8028).
//
// WebhookEndpointResponse never carries the signing secret; only
// WebhookEndpointCreatedResponse (written once on POST /webhook-endpoints)
// includes it. TriggerResponse never embeds store.Trigger and never exposes
// stored initiator attribution, credential snapshots, frozen authority
// ceilings, or fixture provenance stamps.

// WebhookEndpointResponse is the API representation of a webhook endpoint on
// GET and LIST routes. It never includes the signing secret.
type WebhookEndpointResponse struct {
	ID             string     `json:"id"`
	ProjectID      string     `json:"projectId"`
	Name           string     `json:"name"`
	Description    string     `json:"description,omitempty"`
	URL            string     `json:"url"`
	SignatureMode  string     `json:"signatureMode"`
	SecretVersion  int        `json:"secretVersion"`
	Status         string     `json:"status"`
	LastReceivedAt *time.Time `json:"lastReceivedAt,omitempty"`
	ReceiveCount   int        `json:"receiveCount"`
	RejectCount    int        `json:"rejectCount"`
	CreatedBy      string     `json:"createdBy,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// WebhookEndpointCreatedResponse is returned once on POST /webhook-endpoints.
// When defaultTrigger was supplied, TriggerID and Trigger describe the default
// message trigger created in the same transaction.
type WebhookEndpointCreatedResponse struct {
	WebhookEndpointResponse
	Secret    string           `json:"secret"`
	TriggerID string           `json:"triggerId,omitempty"`
	Trigger   *TriggerResponse `json:"trigger,omitempty"`
}

// ListWebhookEndpointsResponse is the paginated response of GET /webhook-endpoints.
type ListWebhookEndpointsResponse struct {
	Scope      string                    `json:"scope,omitempty"`
	Endpoints  []WebhookEndpointResponse `json:"endpoints"`
	NextCursor string                    `json:"nextCursor,omitempty"`
	TotalCount int                       `json:"totalCount"`
	ServerTime time.Time                 `json:"serverTime"`
}

// TriggerSourceSpec identifies the event source of a trigger.
type TriggerSourceSpec struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// TriggerMatchRuleSpec is one dialect-tagged rule element in a trigger's ordered match list.
type TriggerMatchRuleSpec struct {
	Dialect string `json:"dialect"`
	Expr    string `json:"expr"`
}

// TriggerMatchSpec holds the event-type filter and ordered match rule list.
// On requests, scalar Dialect/Expr is sugar for a single-element Rules list;
// on responses, Rules is always populated ([] when empty) and Dialect/Expr are
// mirrored only when len(Rules) == 1 (mixed-rule-arrays §2.2). RulesBroken is
// a content-free response-only flag set when stored match_rules is corrupt.
type TriggerMatchSpec struct {
	Types       []string               `json:"types,omitempty"`
	Dialect     string                 `json:"dialect,omitempty"`
	Expr        string                 `json:"expr,omitempty"`
	Rules       []TriggerMatchRuleSpec `json:"rules"`
	Form        json.RawMessage        `json:"form,omitempty"`
	RulesBroken bool                   `json:"rulesBroken,omitempty"`
}

// TriggerActionSpec holds the action executed when a trigger matches.
type TriggerActionSpec struct {
	Type          string                   `json:"type"`
	AgentID       string                   `json:"agentId,omitempty"`
	AgentName     string                   `json:"agentName,omitempty"`
	Message       string                   `json:"message,omitempty"`
	Interrupt     bool                     `json:"interrupt,omitempty"`
	Plain         bool                     `json:"plain,omitempty"`
	Wake          bool                     `json:"wake,omitempty"`
	NameConflict  string                   `json:"nameConflict,omitempty"`
	Template      string                   `json:"template,omitempty"`
	Task          string                   `json:"task,omitempty"`
	Branch        string                   `json:"branch,omitempty"`
	Labels        map[string]string        `json:"labels,omitempty"`
	Ephemeral     bool                     `json:"ephemeral,omitempty"`
	GithubToken   string                   `json:"githubToken,omitempty"`
	AgentRole     string                   `json:"agentRole,omitempty"`
	ReuseMessage  string                   `json:"reuseMessage,omitempty"`
	AckComment    *DispatchAgentAckComment `json:"ackComment,omitempty"`
	UntrustedVars []string                 `json:"untrustedVars,omitempty"`
	NoAuth        bool                     `json:"noAuth,omitempty"`

	// github.comment action fields (Stage 3 §7.1).
	Body          string `json:"body,omitempty"`
	Target        string `json:"target,omitempty"`
	ReplyInThread bool   `json:"replyInThread,omitempty"`
}

// TriggerSubjectSpec configures per-subject handling for a trigger.
type TriggerSubjectSpec struct {
	Dialect        string `json:"dialect,omitempty"`
	Expr           string `json:"expr,omitempty"`
	Mode           string `json:"mode,omitempty"`
	CoalesceWindow string `json:"coalesce_window,omitempty"`
}

// TriggerResponse is the API representation of a trigger definition.
type TriggerResponse struct {
	ID                    string             `json:"id"`
	ProjectID             string             `json:"projectId"`
	Name                  string             `json:"name"`
	Description           string             `json:"description,omitempty"`
	Source                TriggerSourceSpec  `json:"source"`
	Match                 TriggerMatchSpec   `json:"match"`
	Action                TriggerActionSpec  `json:"action"`
	Subject               TriggerSubjectSpec `json:"subject"`
	Status                string             `json:"status"`
	StatusReason          string             `json:"statusReason,omitempty"`
	AuthorizationRevision int                `json:"authorizationRevision"`
	LastFireAt            *time.Time         `json:"lastFireAt,omitempty"`
	LastFireStatus        string             `json:"lastFireStatus,omitempty"`
	LastFireError         string             `json:"lastFireError,omitempty"`
	FireCount             int                `json:"fireCount"`
	ErrorCount            int                `json:"errorCount"`
	CreatedBy             string             `json:"createdBy,omitempty"`
	CreatedAt             time.Time          `json:"createdAt"`
	UpdatedAt             time.Time          `json:"updatedAt"`
}

// ListTriggersResponse is the paginated response of GET /triggers.
type ListTriggersResponse struct {
	Scope      string            `json:"scope,omitempty"`
	Triggers   []TriggerResponse `json:"triggers"`
	NextCursor string            `json:"nextCursor,omitempty"`
	TotalCount int               `json:"totalCount"`
	ServerTime time.Time         `json:"serverTime"`
}

// TriggerRuleTraceElement is one executed element in a trigger fire's
// content-free rule evaluation trace (mixed-rule-arrays §5).
type TriggerRuleTraceElement struct {
	Index      int    `json:"i"`
	Dialect    string `json:"dialect"`
	Class      string `json:"class"`
	Reason     string `json:"reason,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Provenance string `json:"provenance,omitempty"`
}

// TriggerRuleTrace is the fail-closed API projection of trigger_fires.rule_trace
// (mixed-rule-arrays §5). Decoding into explicit typed fields prevents unknown
// JSON keys from passing through the serializer.
type TriggerRuleTrace struct {
	Elements          []TriggerRuleTraceElement `json:"elements"`
	Unexecuted        []int                     `json:"unexecuted,omitempty"`
	BudgetRemainingMs *int64                    `json:"budget_remaining_ms,omitempty"`
}

// TriggerFireResponse is the API representation of a trigger fire record.
type TriggerFireResponse struct {
	ID             string            `json:"id"`
	TriggerID      string            `json:"triggerId"`
	EventID        string            `json:"eventId"`
	ProjectID      string            `json:"projectId"`
	Subject        string            `json:"subject,omitempty"`
	Status         string            `json:"status"`
	ResultRef      string            `json:"resultRef,omitempty"`
	TargetAgentID  string            `json:"targetAgentId,omitempty"`
	Error          string            `json:"error,omitempty"`
	MatchedThrough string            `json:"matchedThrough,omitempty"`
	DecidedDialect string            `json:"decidedDialect,omitempty"`
	RuleTrace      *TriggerRuleTrace `json:"ruleTrace,omitempty"`
	NotBefore      *time.Time        `json:"notBefore,omitempty"`
	StartedAt      time.Time         `json:"startedAt"`
	FinishedAt     *time.Time        `json:"finishedAt,omitempty"`
}

// ListTriggerFiresResponse is the paginated response of GET /triggers/{id}/history.
type ListTriggerFiresResponse struct {
	Fires      []TriggerFireResponse `json:"fires"`
	NextCursor string                `json:"nextCursor,omitempty"`
	TotalCount int                   `json:"totalCount"`
	ServerTime time.Time             `json:"serverTime"`
}

// TriggerEventSummaryResponse is the API representation of a trigger event row
// on GET /trigger-events. It intentionally omits payload and headers (§10.1).
type TriggerEventSummaryResponse struct {
	ID           string              `json:"id"`
	ProjectID    string              `json:"projectId"`
	Source       TriggerSourceSpec   `json:"source"`
	Type         string              `json:"type"`
	Subject      string              `json:"subject"`
	DedupKey     string              `json:"dedupKey"`
	OccurredAt   time.Time           `json:"occurredAt"`
	ReceivedAt   time.Time           `json:"receivedAt"`
	Attributes   map[string]string   `json:"attributes,omitempty"`
	Lists        map[string][]string `json:"lists,omitempty"`
	PayloadBytes int                 `json:"payloadBytes"`
	Status       string              `json:"status"`
	MatchCount   int                 `json:"matchCount"`
	FireCount    int                 `json:"fireCount"`
	Error        string              `json:"error,omitempty"`
	ProcessedAt  *time.Time          `json:"processedAt,omitempty"`
	CreatedAt    time.Time           `json:"createdAt"`
}

// ListTriggerEventsResponse is the paginated response of GET /trigger-events.
type ListTriggerEventsResponse struct {
	Scope      string                        `json:"scope,omitempty"`
	Events     []TriggerEventSummaryResponse `json:"events"`
	NextCursor string                        `json:"nextCursor,omitempty"`
	TotalCount int                           `json:"totalCount"`
	ServerTime time.Time                     `json:"serverTime"`
}

// TriggerEventDetailResponse is the API representation of GET /trigger-events/{id},
// including allow-listed headers and the raw JSON payload (§4.3, §10.1).
type TriggerEventDetailResponse struct {
	TriggerEventSummaryResponse
	Headers map[string]string `json:"headers,omitempty"`
	Payload json.RawMessage   `json:"payload,omitempty"`
}

// webhookEndpointIngressURL returns the full public ingress URL for endpointID.
func (s *Server) webhookEndpointIngressURL(endpointID string) string {
	base := strings.TrimRight(s.HubEndpointFor(HubEndpointForWebhooks), "/")
	return base + "/api/v1/hooks/" + endpointID
}

func (s *Server) buildWebhookEndpointResponse(ep store.WebhookEndpoint) WebhookEndpointResponse {
	return WebhookEndpointResponse{
		ID:             ep.ID,
		ProjectID:      ep.ProjectID,
		Name:           ep.Name,
		Description:    ep.Description,
		URL:            s.webhookEndpointIngressURL(ep.ID),
		SignatureMode:  ep.SignatureMode,
		SecretVersion:  ep.SecretVersion,
		Status:         ep.Status,
		LastReceivedAt: ep.LastReceivedAt,
		ReceiveCount:   ep.ReceiveCount,
		RejectCount:    ep.RejectCount,
		CreatedBy:      ep.CreatedBy,
		CreatedAt:      ep.CreatedAt,
		UpdatedAt:      ep.UpdatedAt,
	}
}

func (s *Server) buildWebhookEndpointResponses(endpoints []store.WebhookEndpoint) []WebhookEndpointResponse {
	out := make([]WebhookEndpointResponse, len(endpoints))
	for i := range endpoints {
		out[i] = s.buildWebhookEndpointResponse(endpoints[i])
	}
	return out
}

func buildTriggerActionSpec(actionType, rawPayload string) TriggerActionSpec {
	spec := TriggerActionSpec{Type: actionType}
	if rawPayload == "" {
		return spec
	}
	switch actionType {
	case store.TriggerActionMessage:
		var p MessageEventPayload
		if json.Unmarshal([]byte(rawPayload), &p) == nil {
			spec.AgentID = p.AgentID
			spec.AgentName = p.AgentName
			spec.Message = p.Message
			spec.Interrupt = p.Interrupt
			spec.Plain = p.Plain
			spec.Wake = p.Wake
		}
	case store.TriggerActionDispatchAgent:
		var p DispatchAgentEventPayload
		if json.Unmarshal([]byte(rawPayload), &p) == nil {
			spec.AgentName = p.AgentName
			spec.NameConflict = p.NameConflict
			spec.Template = p.Template
			spec.Task = p.Task
			spec.Branch = p.Branch
			spec.Labels = copyTriggerStringMap(p.Labels)
			spec.Ephemeral = p.Ephemeral
			spec.GithubToken = p.GithubToken
			spec.AgentRole = p.AgentRole
			spec.ReuseMessage = p.ReuseMessage
			if p.AckComment != nil {
				cp := *p.AckComment
				spec.AckComment = &cp
			}
			if len(p.UntrustedVars) > 0 {
				spec.UntrustedVars = append([]string(nil), p.UntrustedVars...)
			}
			spec.NoAuth = p.NoAuth
		}
	case store.TriggerActionGitHubComment:
		var p GitHubCommentActionPayload
		if json.Unmarshal([]byte(rawPayload), &p) == nil {
			spec.Body = p.Body
			spec.Target = p.Target
			spec.ReplyInThread = p.ReplyInThread
			if len(p.UntrustedVars) > 0 {
				spec.UntrustedVars = append([]string(nil), p.UntrustedVars...)
			}
		}
	}
	return spec
}

func buildTriggerResponse(tr store.Trigger) TriggerResponse {
	types := make([]string, len(tr.MatchTypes))
	copy(types, tr.MatchTypes)
	normRules, rulesErr := triggerMatchRules(tr)
	respRules := make([]TriggerMatchRuleSpec, len(normRules))
	for i, r := range normRules {
		respRules[i] = TriggerMatchRuleSpec{
			Dialect: r.Dialect,
			Expr:    r.Expr,
		}
	}
	matchSpec := TriggerMatchSpec{
		Types:       types,
		Rules:       respRules,
		RulesBroken: rulesErr != nil,
	}
	if trimmedForm := bytes.TrimSpace([]byte(tr.MatchForm)); len(trimmedForm) > 0 && trimmedForm[0] == '{' && json.Valid(trimmedForm) {
		matchSpec.Form = json.RawMessage(trimmedForm)
	}
	if rulesErr == nil && len(respRules) == 1 {
		matchSpec.Dialect = respRules[0].Dialect
		matchSpec.Expr = respRules[0].Expr
	}
	var coalesceWindow string
	if tr.CoalesceWindowSeconds > 0 {
		coalesceWindow = (time.Duration(tr.CoalesceWindowSeconds) * time.Second).String()
	}
	return TriggerResponse{
		ID:          tr.ID,
		ProjectID:   tr.ProjectID,
		Name:        tr.Name,
		Description: tr.Description,
		Source: TriggerSourceSpec{
			Kind: tr.SourceKind,
			ID:   tr.SourceID,
		},
		Match:  matchSpec,
		Action: buildTriggerActionSpec(tr.ActionType, tr.ActionPayload),
		Subject: TriggerSubjectSpec{
			Dialect:        tr.SubjectDialect,
			Expr:           tr.SubjectRule,
			Mode:           tr.SubjectMode,
			CoalesceWindow: coalesceWindow,
		},
		Status:                tr.Status,
		StatusReason:          tr.StatusReason,
		AuthorizationRevision: tr.AuthorizationRevision,
		LastFireAt:            tr.LastFireAt,
		LastFireStatus:        tr.LastFireStatus,
		LastFireError:         tr.LastFireError,
		FireCount:             tr.FireCount,
		ErrorCount:            tr.ErrorCount,
		CreatedBy:             tr.CreatedBy,
		CreatedAt:             tr.CreatedAt,
		UpdatedAt:             tr.UpdatedAt,
	}
}

func buildTriggerResponses(triggers []store.Trigger) []TriggerResponse {
	out := make([]TriggerResponse, len(triggers))
	for i := range triggers {
		out[i] = buildTriggerResponse(triggers[i])
	}
	return out
}

func decodeTriggerFireRuleTrace(raw string) *TriggerRuleTrace {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var parsed TriggerRuleTrace
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil
	}
	if parsed.Elements == nil {
		parsed.Elements = []TriggerRuleTraceElement{}
	}
	for i := range parsed.Elements {
		parsed.Elements[i].Dialect = triggerrule.SafeDialectName(parsed.Elements[i].Dialect)
	}
	return &parsed
}

func buildTriggerFireResponse(tf store.TriggerFire) TriggerFireResponse {
	decidedDialect := ""
	if tf.DecidedDialect != "" {
		decidedDialect = triggerrule.SafeDialectName(tf.DecidedDialect)
	}
	return TriggerFireResponse{
		ID:             tf.ID,
		TriggerID:      tf.TriggerID,
		EventID:        tf.EventID,
		ProjectID:      tf.ProjectID,
		Subject:        tf.Subject,
		Status:         tf.Status,
		ResultRef:      tf.ResultRef,
		TargetAgentID:  tf.TargetAgentID,
		Error:          tf.Error,
		MatchedThrough: tf.MatchedThrough,
		DecidedDialect: decidedDialect,
		RuleTrace:      decodeTriggerFireRuleTrace(tf.RuleTrace),
		NotBefore:      tf.NotBefore,
		StartedAt:      tf.StartedAt,
		FinishedAt:     tf.FinishedAt,
	}
}

func buildTriggerFireResponses(fires []store.TriggerFire) []TriggerFireResponse {
	out := make([]TriggerFireResponse, len(fires))
	for i := range fires {
		out[i] = buildTriggerFireResponse(fires[i])
	}
	return out
}

func copyTriggerStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyTriggerStringListMap(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, v := range in {
		if v == nil {
			out[k] = []string{}
			continue
		}
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

func buildTriggerEventSummaryResponse(ev store.TriggerEvent) TriggerEventSummaryResponse {
	attrs := copyTriggerStringMap(ev.Attributes)
	if rawCT, hasCT := attrs["content_type"]; hasCT {
		attrs["content_type"] = sanitizeEventContentType(rawCT)
	}
	return TriggerEventSummaryResponse{
		ID:        ev.ID,
		ProjectID: ev.ProjectID,
		Source: TriggerSourceSpec{
			Kind: ev.SourceKind,
			ID:   ev.SourceID,
		},
		Type:         ev.Type,
		Subject:      ev.Subject,
		DedupKey:     ev.DedupKey,
		OccurredAt:   ev.OccurredAt,
		ReceivedAt:   ev.ReceivedAt,
		Attributes:   attrs,
		Lists:        copyTriggerStringListMap(ev.Lists),
		PayloadBytes: ev.PayloadBytes,
		Status:       ev.Status,
		MatchCount:   ev.MatchCount,
		FireCount:    ev.FireCount,
		Error:        ev.Error,
		ProcessedAt:  ev.ProcessedAt,
		CreatedAt:    ev.CreatedAt,
	}
}

func buildTriggerEventSummaryResponses(events []store.TriggerEvent) []TriggerEventSummaryResponse {
	out := make([]TriggerEventSummaryResponse, len(events))
	for i := range events {
		out[i] = buildTriggerEventSummaryResponse(events[i])
	}
	return out
}

func buildTriggerEventDetailResponse(ev store.TriggerEvent) TriggerEventDetailResponse {
	var rawPayload json.RawMessage
	if ev.Payload != "" {
		if json.Valid([]byte(ev.Payload)) {
			rawPayload = json.RawMessage(ev.Payload)
		} else if encoded, err := json.Marshal(ev.Payload); err == nil {
			rawPayload = json.RawMessage(encoded)
		}
	}
	return TriggerEventDetailResponse{
		TriggerEventSummaryResponse: buildTriggerEventSummaryResponse(ev),
		Headers:                     copyTriggerStringMap(ev.Headers),
		Payload:                     rawPayload,
	}
}
