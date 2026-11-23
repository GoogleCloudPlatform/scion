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

package hubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// TriggerListScopeCaller indicates that a trigger or webhook-endpoint list
// response is filtered to rows owned or held by the calling agent.
const TriggerListScopeCaller = "caller"

// WebhookEndpointService handles webhook endpoint operations scoped to a project.
type WebhookEndpointService interface {
	// Create creates a new webhook endpoint and returns its one-time signing secret.
	Create(ctx context.Context, req *CreateWebhookEndpointRequest) (*WebhookEndpointCreated, error)

	// Get retrieves a webhook endpoint by ID (never includes the signing secret).
	Get(ctx context.Context, id string) (*WebhookEndpoint, error)

	// List returns webhook endpoints matching the filter criteria.
	List(ctx context.Context, opts *ListWebhookEndpointsOptions) (*ListWebhookEndpointsResponse, error)

	// Update updates a webhook endpoint's description, status, or agent binding.
	Update(ctx context.Context, id string, req *UpdateWebhookEndpointRequest) (*WebhookEndpoint, error)

	// RotateSecret rotates a webhook endpoint's signing secret, returning the new secret once.
	RotateSecret(ctx context.Context, id string) (*WebhookEndpointRotated, error)

	// Delete deletes a webhook endpoint and pauses its active dependent triggers.
	Delete(ctx context.Context, id string) error
}

// TriggerService handles trigger definition operations scoped to a project.
type TriggerService interface {
	// Create creates a new trigger definition.
	Create(ctx context.Context, req *CreateTriggerRequest) (*Trigger, error)

	// Get retrieves a trigger definition by ID.
	Get(ctx context.Context, id string) (*Trigger, error)

	// List returns trigger definitions matching the filter criteria.
	List(ctx context.Context, opts *ListTriggersOptions) (*ListTriggersResponse, error)

	// Update updates a trigger definition.
	Update(ctx context.Context, id string, req *UpdateTriggerRequest) (*Trigger, error)

	// Delete deletes a trigger definition.
	Delete(ctx context.Context, id string) error

	// Pause pauses an active trigger definition.
	Pause(ctx context.Context, id string) (*Trigger, error)

	// Resume resumes a paused trigger definition.
	Resume(ctx context.Context, id string) (*Trigger, error)

	// History lists fire history records for a trigger definition.
	History(ctx context.Context, id string, opts *ListTriggerFiresOptions) (*ListTriggerFiresResponse, error)

	// Render compiles and renders all template slots of a trigger against a stored event without firing.
	Render(ctx context.Context, id, eventID string) (*TriggerRenderResponse, error)
}

// TriggerEventService handles read-only trigger event operations scoped to a project.
type TriggerEventService interface {
	// Get retrieves a trigger event by ID, including headers and JSON payload.
	Get(ctx context.Context, id string) (*TriggerEventDetail, error)

	// List returns trigger events matching the filter criteria.
	List(ctx context.Context, opts *ListTriggerEventsOptions) (*ListTriggerEventsResponse, error)
}

type webhookEndpointService struct {
	c         *client
	projectID string
}

func (s *webhookEndpointService) basePath() string {
	return fmt.Sprintf("/api/v1/projects/%s/webhook-endpoints", url.PathEscape(s.projectID))
}

type triggerService struct {
	c         *client
	projectID string
}

func (s *triggerService) basePath() string {
	return fmt.Sprintf("/api/v1/projects/%s/triggers", url.PathEscape(s.projectID))
}

type triggerEventService struct {
	c         *client
	projectID string
}

func (s *triggerEventService) basePath() string {
	return fmt.Sprintf("/api/v1/projects/%s/trigger-events", url.PathEscape(s.projectID))
}

// WebhookEndpointDefaultTriggerSpec configures an optional default message
// trigger created in the same transaction as a webhook endpoint.
type WebhookEndpointDefaultTriggerSpec struct {
	AgentID   string `json:"agentId,omitempty"`
	AgentName string `json:"agentName,omitempty"`
	ToSelf    bool   `json:"toSelf,omitempty"`
	Message   string `json:"message"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Wake      bool   `json:"wake,omitempty"`
}

// CreateWebhookEndpointRequest is the client-side request for creating a webhook endpoint.
type CreateWebhookEndpointRequest struct {
	Name           string                             `json:"name"`
	Description    string                             `json:"description,omitempty"`
	SignatureMode  string                             `json:"signatureMode,omitempty"`
	BoundAgentID   string                             `json:"boundAgentId,omitempty"`
	BoundLifetime  string                             `json:"boundLifetime,omitempty"`
	DefaultTrigger *WebhookEndpointDefaultTriggerSpec `json:"defaultTrigger,omitempty"`
	Trigger        *WebhookEndpointDefaultTriggerSpec `json:"trigger,omitempty"`
}

// UpdateWebhookEndpointRequest is the client-side request for PATCH /webhook-endpoints/{id}.
type UpdateWebhookEndpointRequest struct {
	Description   *string `json:"description,omitempty"`
	Status        *string `json:"status,omitempty"`
	BoundAgentID  *string `json:"boundAgentId,omitempty"`
	BoundLifetime *string `json:"boundLifetime,omitempty"`
}

// WebhookEndpointBoundAgent identifies the agent a webhook endpoint is bound to.
type WebhookEndpointBoundAgent struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// WebhookEndpoint represents a webhook endpoint returned by GET and LIST routes.
type WebhookEndpoint struct {
	ID              string                     `json:"id"`
	ProjectID       string                     `json:"projectId"`
	Name            string                     `json:"name"`
	Description     string                     `json:"description,omitempty"`
	URL             string                     `json:"url"`
	SignatureMode   string                     `json:"signatureMode"`
	SecretVersion   int                        `json:"secretVersion"`
	SecretRotatedAt *time.Time                 `json:"secretRotatedAt,omitempty"`
	ConfigVersion   int                        `json:"configVersion,omitempty"`
	Status          string                     `json:"status"`
	BoundAgentID    string                     `json:"boundAgentId,omitempty"`
	BoundAgent      *WebhookEndpointBoundAgent `json:"boundAgent,omitempty"`
	BoundLifetime   string                     `json:"boundLifetime,omitempty"`
	LastReceivedAt  *time.Time                 `json:"lastReceivedAt,omitempty"`
	ReceiveCount    int                        `json:"receiveCount"`
	RejectCount     int                        `json:"rejectCount"`
	CreatedBy       string                     `json:"createdBy,omitempty"`
	CreatedAt       time.Time                  `json:"createdAt"`
	UpdatedAt       time.Time                  `json:"updatedAt"`
}

// WebhookEndpointCreated is returned once when creating a webhook endpoint.
type WebhookEndpointCreated struct {
	WebhookEndpoint
	Secret    string   `json:"secret"`
	TriggerID string   `json:"triggerId,omitempty"`
	Trigger   *Trigger `json:"trigger,omitempty"`
}

// WebhookEndpointRotated is returned once when rotating a webhook endpoint's secret.
type WebhookEndpointRotated struct {
	WebhookEndpoint
	Secret             string    `json:"secret"`
	GraceWindowExpires time.Time `json:"graceWindowExpires"`
}

// ListWebhookEndpointsOptions configures webhook endpoint listing.
type ListWebhookEndpointsOptions struct {
	Name         string
	Status       string
	BoundAgentID string
	Page         apiclient.PageOptions
}

// ListWebhookEndpointsResponse is the response from listing webhook endpoints.
type ListWebhookEndpointsResponse struct {
	Scope      string            `json:"scope,omitempty"`
	Endpoints  []WebhookEndpoint `json:"endpoints"`
	NextCursor string            `json:"nextCursor,omitempty"`
	TotalCount int               `json:"totalCount,omitempty"`
	ServerTime time.Time         `json:"serverTime"`
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
type TriggerMatchSpec struct {
	Types       []string               `json:"types,omitempty"`
	Dialect     string                 `json:"dialect,omitempty"`
	Expr        string                 `json:"expr,omitempty"`
	Rules       []TriggerMatchRuleSpec `json:"rules"`
	Form        json.RawMessage        `json:"form,omitempty"`
	RulesBroken bool                   `json:"rulesBroken,omitempty"`
}

// TriggerMatchPatchSpec holds partial updates to a trigger's match block.
type TriggerMatchPatchSpec struct {
	Types   *[]string               `json:"types,omitempty"`
	Dialect *string                 `json:"dialect,omitempty"`
	Expr    *string                 `json:"expr,omitempty"`
	Rules   *[]TriggerMatchRuleSpec `json:"rules,omitempty"`
	Form    *json.RawMessage        `json:"form,omitempty"`
}

// TriggerActionSpec holds the action executed when a trigger matches.
type TriggerActionSpec struct {
	Type          string            `json:"type"`
	AgentID       string            `json:"agentId,omitempty"`
	AgentName     string            `json:"agentName,omitempty"`
	Message       string            `json:"message,omitempty"`
	Interrupt     bool              `json:"interrupt,omitempty"`
	Plain         bool              `json:"plain,omitempty"`
	Wake          bool              `json:"wake,omitempty"`
	NameConflict  string            `json:"nameConflict,omitempty"`
	Template      string            `json:"template,omitempty"`
	Task          string            `json:"task,omitempty"`
	Branch        string            `json:"branch,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Ephemeral     bool              `json:"ephemeral,omitempty"`
	GithubToken   string            `json:"githubToken,omitempty"`
	AgentRole     string            `json:"agentRole,omitempty"`
	ReuseMessage  string            `json:"reuseMessage,omitempty"`
	UntrustedVars []string          `json:"untrustedVars,omitempty"`
	NoAuth        bool              `json:"noAuth,omitempty"`

	// exec action fields (Stage 2 P8, ptone/scion#8241).
	Command           []string `json:"command,omitempty"`
	TimeoutSeconds    int      `json:"timeoutSeconds,omitempty"`
	ContextAttributes []string `json:"contextAttributes,omitempty"`
	CommandRedacted   bool     `json:"commandRedacted,omitempty"`
	Argc              int      `json:"argc,omitempty"`
	CommandSHA256     string   `json:"commandSha256,omitempty"`
}

// TriggerRenderUsedVar describes one placeholder path expanded during trigger render preview.
type TriggerRenderUsedVar struct {
	Path  string `json:"path"`
	Class string `json:"class"`
}

// TriggerRenderSlotResult holds the rendered text and used variables for one slot.
type TriggerRenderSlotResult struct {
	Rendered   string                 `json:"rendered"`
	Used       []TriggerRenderUsedVar `json:"used"`
	Templating string                 `json:"templating,omitempty"`
}

// TriggerRenderSlots holds rendered slot outputs keyed by slot name.
type TriggerRenderSlots struct {
	Task         *TriggerRenderSlotResult           `json:"task,omitempty"`
	Name         *TriggerRenderSlotResult           `json:"name,omitempty"`
	Branch       *TriggerRenderSlotResult           `json:"branch,omitempty"`
	Labels       map[string]TriggerRenderSlotResult `json:"labels,omitempty"`
	ReuseMessage *TriggerRenderSlotResult           `json:"reuseMessage,omitempty"`
	Message      *TriggerRenderSlotResult           `json:"message,omitempty"`
	Body         *TriggerRenderSlotResult           `json:"body,omitempty"`
}

// TriggerRenderError describes one compile or render failure for a slot.
type TriggerRenderError struct {
	Slot    string `json:"slot"`
	Path    string `json:"path,omitempty"`
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
}

// TriggerRenderResponse is the response from POST /triggers/{id}/render?eventId=<id>.
type TriggerRenderResponse struct {
	Slots  TriggerRenderSlots   `json:"slots"`
	Errors []TriggerRenderError `json:"errors"`
	Notes  []string             `json:"notes,omitempty"`
}

// TriggerSubjectSpec configures per-subject handling for a trigger.
type TriggerSubjectSpec struct {
	Dialect        string `json:"dialect,omitempty"`
	Expr           string `json:"expr,omitempty"`
	Mode           string `json:"mode,omitempty"`
	CoalesceWindow string `json:"coalesce_window,omitempty"`
}

// TriggerSubjectPatchSpec holds partial updates to a trigger's subject block.
type TriggerSubjectPatchSpec struct {
	Dialect        *string `json:"dialect,omitempty"`
	Expr           *string `json:"expr,omitempty"`
	Mode           *string `json:"mode,omitempty"`
	CoalesceWindow *string `json:"coalesce_window,omitempty"`
}

// Trigger represents a trigger definition returned by the Hub API.
type Trigger struct {
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

// CreateTriggerRequest is the client-side request for creating a trigger.
type CreateTriggerRequest struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Source      TriggerSourceSpec   `json:"source"`
	Match       *TriggerMatchSpec   `json:"match,omitempty"`
	Action      TriggerActionSpec   `json:"action"`
	Subject     *TriggerSubjectSpec `json:"subject,omitempty"`
}

// UpdateTriggerRequest is the client-side request for updating a trigger.
type UpdateTriggerRequest struct {
	Name             *string                  `json:"name,omitempty"`
	Description      *string                  `json:"description,omitempty"`
	Source           *TriggerSourceSpec       `json:"source,omitempty"`
	Match            *TriggerMatchPatchSpec   `json:"match,omitempty"`
	Action           *TriggerActionSpec       `json:"action,omitempty"`
	Subject          *TriggerSubjectPatchSpec `json:"subject,omitempty"`
	ExpectedRevision *int                     `json:"expectedRevision,omitempty"`
	IfMatch          string                   `json:"-"`
}

// ListTriggersOptions configures trigger listing.
type ListTriggersOptions struct {
	Name     string
	Source   string
	SourceID string
	Status   string
	Page     apiclient.PageOptions
}

// ListTriggersResponse is the response from listing triggers.
type ListTriggersResponse struct {
	Scope      string    `json:"scope,omitempty"`
	Triggers   []Trigger `json:"triggers"`
	NextCursor string    `json:"nextCursor,omitempty"`
	TotalCount int       `json:"totalCount,omitempty"`
	ServerTime time.Time `json:"serverTime"`
}

// TriggerRuleTraceElement describes one executed element in a trigger fire's
// content-free rule evaluation trace (mixed-rule-arrays §5).
type TriggerRuleTraceElement struct {
	Index      int    `json:"i"`
	Dialect    string `json:"dialect"`
	Class      string `json:"class"`
	Reason     string `json:"reason,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Provenance string `json:"provenance,omitempty"`
}

// TriggerRuleTrace describes the content-free rule evaluation trace recorded on
// a trigger fire row (mixed-rule-arrays §5).
type TriggerRuleTrace struct {
	Elements          []TriggerRuleTraceElement `json:"elements"`
	Unexecuted        []int                     `json:"unexecuted,omitempty"`
	BudgetRemainingMs *int64                    `json:"budget_remaining_ms,omitempty"`
}

// TriggerFire represents a trigger fire history record returned by GET /triggers/{id}/history.
type TriggerFire struct {
	ID             string            `json:"id"`
	TriggerID      string            `json:"triggerId"`
	EventID        string            `json:"eventId"`
	ProjectID      string            `json:"projectId"`
	Subject        string            `json:"subject,omitempty"`
	Status         string            `json:"status"`
	ResultRef      string            `json:"resultRef,omitempty"`
	Error          string            `json:"error,omitempty"`
	MatchedThrough string            `json:"matchedThrough,omitempty"`
	DecidedDialect string            `json:"decidedDialect,omitempty"`
	RuleTrace      *TriggerRuleTrace `json:"ruleTrace,omitempty"`
	NotBefore      *time.Time        `json:"notBefore,omitempty"`
	ExecSummary    *ExecSummary      `json:"execSummary,omitempty"`
	StartedAt      time.Time         `json:"startedAt"`
	FinishedAt     *time.Time        `json:"finishedAt,omitempty"`
}

// ListTriggerFiresOptions configures trigger fire history listing.
type ListTriggerFiresOptions struct {
	Page apiclient.PageOptions
}

// ListTriggerFiresResponse is the response from GET /triggers/{id}/history.
type ListTriggerFiresResponse struct {
	Fires      []TriggerFire `json:"fires"`
	NextCursor string        `json:"nextCursor,omitempty"`
	TotalCount int           `json:"totalCount,omitempty"`
	ServerTime time.Time     `json:"serverTime"`
}

// TriggerEventSummary represents a trigger event summary row returned by GET /trigger-events.
type TriggerEventSummary struct {
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

// TriggerEventDetail represents a trigger event detail returned by GET /trigger-events/{id}.
type TriggerEventDetail struct {
	TriggerEventSummary
	Headers map[string]string `json:"headers,omitempty"`
	Payload json.RawMessage   `json:"payload,omitempty"`
}

// ListTriggerEventsOptions configures trigger event listing.
type ListTriggerEventsOptions struct {
	Source   string
	SourceID string
	Status   string
	Since    *time.Time
	Page     apiclient.PageOptions
}

// ListTriggerEventsResponse is the response from GET /trigger-events.
type ListTriggerEventsResponse struct {
	Scope      string                `json:"scope,omitempty"`
	Events     []TriggerEventSummary `json:"events"`
	NextCursor string                `json:"nextCursor,omitempty"`
	TotalCount int                   `json:"totalCount,omitempty"`
	ServerTime time.Time             `json:"serverTime"`
}

func (s *webhookEndpointService) Create(ctx context.Context, req *CreateWebhookEndpointRequest) (*WebhookEndpointCreated, error) {
	resp, err := s.c.post(ctx, s.basePath(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[WebhookEndpointCreated](resp)
}

func (s *webhookEndpointService) Get(ctx context.Context, id string) (*WebhookEndpoint, error) {
	resp, err := s.c.get(ctx, s.basePath()+"/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[WebhookEndpoint](resp)
}

func (s *webhookEndpointService) List(ctx context.Context, opts *ListWebhookEndpointsOptions) (*ListWebhookEndpointsResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Name != "" {
			query.Set("name", opts.Name)
		}
		if opts.Status != "" {
			query.Set("status", opts.Status)
		}
		if opts.BoundAgentID != "" {
			query.Set("boundAgentId", opts.BoundAgentID)
		}
		opts.Page.ToQuery(query)
	}
	resp, err := s.c.getWithQuery(ctx, s.basePath(), query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListWebhookEndpointsResponse](resp)
}

func (s *webhookEndpointService) Update(ctx context.Context, id string, req *UpdateWebhookEndpointRequest) (*WebhookEndpoint, error) {
	resp, err := s.c.patch(ctx, s.basePath()+"/"+url.PathEscape(id), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[WebhookEndpoint](resp)
}

func (s *webhookEndpointService) RotateSecret(ctx context.Context, id string) (*WebhookEndpointRotated, error) {
	resp, err := s.c.post(ctx, s.basePath()+"/"+url.PathEscape(id)+"/rotate-secret", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[WebhookEndpointRotated](resp)
}

func (s *webhookEndpointService) Delete(ctx context.Context, id string) error {
	resp, err := s.c.delete(ctx, s.basePath()+"/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

func (s *triggerService) Create(ctx context.Context, req *CreateTriggerRequest) (*Trigger, error) {
	resp, err := s.c.post(ctx, s.basePath(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[Trigger](resp)
}

func (s *triggerService) Get(ctx context.Context, id string) (*Trigger, error) {
	resp, err := s.c.get(ctx, s.basePath()+"/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[Trigger](resp)
}

func (s *triggerService) List(ctx context.Context, opts *ListTriggersOptions) (*ListTriggersResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Name != "" {
			query.Set("name", opts.Name)
		}
		if opts.Source != "" {
			query.Set("source", opts.Source)
		}
		if opts.SourceID != "" {
			query.Set("sourceId", opts.SourceID)
		}
		if opts.Status != "" {
			query.Set("status", opts.Status)
		}
		opts.Page.ToQuery(query)
	}
	resp, err := s.c.getWithQuery(ctx, s.basePath(), query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListTriggersResponse](resp)
}

func (s *triggerService) Update(ctx context.Context, id string, req *UpdateTriggerRequest) (*Trigger, error) {
	var headers http.Header
	if req != nil && req.IfMatch != "" {
		headers = http.Header{}
		headers.Set("If-Match", req.IfMatch)
	}
	resp, err := s.c.patch(ctx, s.basePath()+"/"+url.PathEscape(id), req, headers)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[Trigger](resp)
}

func (s *triggerService) Delete(ctx context.Context, id string) error {
	resp, err := s.c.delete(ctx, s.basePath()+"/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

func (s *triggerService) Pause(ctx context.Context, id string) (*Trigger, error) {
	resp, err := s.c.post(ctx, s.basePath()+"/"+url.PathEscape(id)+"/pause", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[Trigger](resp)
}

func (s *triggerService) Resume(ctx context.Context, id string) (*Trigger, error) {
	resp, err := s.c.post(ctx, s.basePath()+"/"+url.PathEscape(id)+"/resume", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[Trigger](resp)
}

func (s *triggerService) History(ctx context.Context, id string, opts *ListTriggerFiresOptions) (*ListTriggerFiresResponse, error) {
	query := url.Values{}
	if opts != nil {
		opts.Page.ToQuery(query)
	}
	resp, err := s.c.getWithQuery(ctx, s.basePath()+"/"+url.PathEscape(id)+"/history", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListTriggerFiresResponse](resp)
}

func (s *triggerService) Render(ctx context.Context, id, eventID string) (*TriggerRenderResponse, error) {
	query := url.Values{}
	if eventID != "" {
		query.Set("eventId", eventID)
	}
	path := s.basePath() + "/" + url.PathEscape(id) + "/render"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	resp, err := s.c.post(ctx, path, nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[TriggerRenderResponse](resp)
}

func (s *triggerEventService) Get(ctx context.Context, id string) (*TriggerEventDetail, error) {
	resp, err := s.c.get(ctx, s.basePath()+"/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[TriggerEventDetail](resp)
}

func (s *triggerEventService) List(ctx context.Context, opts *ListTriggerEventsOptions) (*ListTriggerEventsResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Source != "" {
			query.Set("source", opts.Source)
		}
		if opts.SourceID != "" {
			query.Set("sourceId", opts.SourceID)
		}
		if opts.Status != "" {
			query.Set("status", opts.Status)
		}
		if opts.Since != nil {
			query.Set("since", opts.Since.UTC().Format(time.RFC3339Nano))
		}
		opts.Page.ToQuery(query)
	}
	resp, err := s.c.getWithQuery(ctx, s.basePath(), query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListTriggerEventsResponse](resp)
}
