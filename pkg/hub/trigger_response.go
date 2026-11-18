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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
type WebhookEndpointCreatedResponse struct {
	WebhookEndpointResponse
	Secret string `json:"secret"`
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

// TriggerMatchSpec holds the event-type filter and optional match expression.
type TriggerMatchSpec struct {
	Types   []string `json:"types,omitempty"`
	Dialect string   `json:"dialect,omitempty"`
	Expr    string   `json:"expr,omitempty"`
}

// TriggerActionSpec holds the action executed when a trigger matches.
type TriggerActionSpec struct {
	Type      string `json:"type"`
	AgentID   string `json:"agentId,omitempty"`
	AgentName string `json:"agentName,omitempty"`
	Message   string `json:"message,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Plain     bool   `json:"plain,omitempty"`
	Wake      bool   `json:"wake,omitempty"`
}

// TriggerSubjectSpec configures per-subject handling for a trigger.
type TriggerSubjectSpec struct {
	Dialect string `json:"dialect,omitempty"`
	Expr    string `json:"expr,omitempty"`
	Mode    string `json:"mode,omitempty"`
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

// TriggerFireResponse is the API representation of a trigger fire record.
type TriggerFireResponse struct {
	ID         string     `json:"id"`
	TriggerID  string     `json:"triggerId"`
	EventID    string     `json:"eventId"`
	ProjectID  string     `json:"projectId"`
	Subject    string     `json:"subject,omitempty"`
	Status     string     `json:"status"`
	ResultRef  string     `json:"resultRef,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
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
	ID           string            `json:"id"`
	ProjectID    string            `json:"projectId"`
	Source       TriggerSourceSpec `json:"source"`
	Type         string            `json:"type"`
	Subject      string            `json:"subject"`
	DedupKey     string            `json:"dedupKey"`
	OccurredAt   time.Time         `json:"occurredAt"`
	ReceivedAt   time.Time         `json:"receivedAt"`
	Attributes   map[string]string `json:"attributes,omitempty"`
	PayloadBytes int               `json:"payloadBytes"`
	Status       string            `json:"status"`
	MatchCount   int               `json:"matchCount"`
	FireCount    int               `json:"fireCount"`
	Error        string            `json:"error,omitempty"`
	ProcessedAt  *time.Time        `json:"processedAt,omitempty"`
	CreatedAt    time.Time         `json:"createdAt"`
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
	if actionType == store.TriggerActionMessage && rawPayload != "" {
		var p MessageEventPayload
		if json.Unmarshal([]byte(rawPayload), &p) == nil {
			spec.AgentID = p.AgentID
			spec.AgentName = p.AgentName
			spec.Message = p.Message
			spec.Interrupt = p.Interrupt
			spec.Plain = p.Plain
			spec.Wake = p.Wake
		}
	}
	return spec
}

func buildTriggerResponse(tr store.Trigger) TriggerResponse {
	types := make([]string, len(tr.MatchTypes))
	copy(types, tr.MatchTypes)
	return TriggerResponse{
		ID:          tr.ID,
		ProjectID:   tr.ProjectID,
		Name:        tr.Name,
		Description: tr.Description,
		Source: TriggerSourceSpec{
			Kind: tr.SourceKind,
			ID:   tr.SourceID,
		},
		Match: TriggerMatchSpec{
			Types:   types,
			Dialect: tr.MatchDialect,
			Expr:    tr.MatchRule,
		},
		Action: buildTriggerActionSpec(tr.ActionType, tr.ActionPayload),
		Subject: TriggerSubjectSpec{
			Dialect: tr.SubjectDialect,
			Expr:    tr.SubjectRule,
			Mode:    tr.SubjectMode,
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

func buildTriggerFireResponse(tf store.TriggerFire) TriggerFireResponse {
	return TriggerFireResponse{
		ID:         tf.ID,
		TriggerID:  tf.TriggerID,
		EventID:    tf.EventID,
		ProjectID:  tf.ProjectID,
		Subject:    tf.Subject,
		Status:     tf.Status,
		ResultRef:  tf.ResultRef,
		Error:      tf.Error,
		StartedAt:  tf.StartedAt,
		FinishedAt: tf.FinishedAt,
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
