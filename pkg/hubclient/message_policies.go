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
	"errors"
	"log/slog"
	"net/url"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// MessagePolicyService handles project-scoped message policy authoring,
// dry-run testing, and content-free decision inspection
// (/api/v1/projects/{projectID}/message-policies).
type MessagePolicyService interface {
	// Create creates a new project message policy (POST .../message-policies).
	Create(ctx context.Context, req *CreateMessagePolicyRequest) (*MessagePolicyResponse, error)

	// List lists message policies in the project (GET .../message-policies).
	List(ctx context.Context, opts *ListMessagePoliciesOptions) (*ListMessagePoliciesResponse, error)

	// Get retrieves a project message policy by UUID or name (GET .../message-policies/{policyRef}).
	Get(ctx context.Context, policyRef string) (*MessagePolicyResponse, error)

	// Update edits an existing project message policy (PATCH .../message-policies/{policyRef}).
	Update(ctx context.Context, policyRef string, req *PatchMessagePolicyRequest) (*MessagePolicyResponse, error)

	// Delete removes a project message policy by UUID or name (DELETE .../message-policies/{policyRef}).
	Delete(ctx context.Context, policyRef string) error

	// Test evaluates an inline message policy spec against a synthetic sample
	// without persisting any policy, message, or decision row (POST .../message-policies/test).
	Test(ctx context.Context, req *TestMessagePolicyRequest) (*TestMessagePolicyResponse, error)

	// TestPolicy evaluates a stored message policy (optionally overlaid with
	// request fields) against a synthetic sample without persisting any message
	// or decision row (POST .../message-policies/{policyRef}/test).
	TestPolicy(ctx context.Context, policyRef string, req *TestMessagePolicyRequest) (*TestMessagePolicyResponse, error)

	// ListDecisions lists content-free evaluation decisions across policies in
	// the project (GET .../message-policies/decisions).
	ListDecisions(ctx context.Context, opts *ListMessagePolicyDecisionsOptions) (*ListMessagePolicyDecisionsResponse, error)

	// ListPolicyDecisions lists content-free evaluation decisions for one
	// policy by UUID or name (GET .../message-policies/{policyRef}/decisions).
	ListPolicyDecisions(ctx context.Context, policyRef string, opts *ListMessagePolicyDecisionsOptions) (*ListMessagePolicyDecisionsResponse, error)
}

type messagePolicyService struct {
	c         *client
	projectID string
}

// ProjectMessagePolicies returns a MessagePolicyService scoped to a project.
func (c *client) ProjectMessagePolicies(projectID string) MessagePolicyService {
	return &messagePolicyService{c: c, projectID: projectID}
}

// MessagePolicyResponse represents a persisted message policy along with the
// Hub's explicit surface enforcement coverage declaration.
type MessagePolicyResponse struct {
	store.MessagePolicy
	Coverage messagepolicy.CoverageInfo `json:"coverage"`
}

// ListMessagePoliciesOptions configures filtering and pagination on
// GET /api/v1/projects/{id}/message-policies.
type ListMessagePoliciesOptions struct {
	Mode    string
	Enabled *bool
	Limit   int
	Cursor  string
}

// ListMessagePoliciesResponse is the response body for
// GET /api/v1/projects/{id}/message-policies.
type ListMessagePoliciesResponse struct {
	Policies   []MessagePolicyResponse    `json:"policies"`
	Coverage   messagepolicy.CoverageInfo `json:"coverage"`
	NextCursor string                     `json:"next_cursor,omitempty"`
	TotalCount int                        `json:"total_count"`
}

// ListMessagePolicyDecisionsOptions configures filtering and pagination on
// GET /api/v1/projects/{id}/message-policies[/...]/decisions.
type ListMessagePolicyDecisionsOptions struct {
	PolicyID string
	Mode     string
	Surface  string
	Class    string
	Action   string
	Limit    int
	Cursor   string
}

// ListMessagePolicyDecisionsResponse is the content-free response body for
// GET /api/v1/projects/{id}/message-policies[/...]/decisions.
type ListMessagePolicyDecisionsResponse struct {
	Decisions  []store.MessagePolicyDecision `json:"decisions"`
	NextCursor string                        `json:"next_cursor,omitempty"`
	TotalCount int                           `json:"total_count"`
}

// CreateMessagePolicyRequest is the request body for
// POST /api/v1/projects/{id}/message-policies.
type CreateMessagePolicyRequest struct {
	Name              string                   `json:"name"`
	Description       string                   `json:"description,omitempty"`
	Enabled           *bool                    `json:"enabled,omitempty"`
	Mode              string                   `json:"mode,omitempty"`
	Scope             store.MessagePolicyScope `json:"scope"`
	ViolationRule     string                   `json:"violation_rule"`
	SystemOneRule     string                   `json:"system_one_rule,omitempty"`
	OnUncertain       string                   `json:"on_uncertain,omitempty"`
	OnFailed          string                   `json:"on_failed"`
	OnFailedReasons   map[string]string        `json:"on_failed_reasons,omitempty"`
	RejectReason      string                   `json:"reject_reason,omitempty"`
	RejectRemediation string                   `json:"reject_remediation,omitempty"`
}

// PatchMessagePolicyRequest is the request body for
// PATCH /api/v1/projects/{id}/message-policies/{policyId}.
type PatchMessagePolicyRequest struct {
	Name              *string                   `json:"name,omitempty"`
	Description       *string                   `json:"description,omitempty"`
	Enabled           *bool                     `json:"enabled,omitempty"`
	Mode              *string                   `json:"mode,omitempty"`
	Scope             *store.MessagePolicyScope `json:"scope,omitempty"`
	ViolationRule     *string                   `json:"violation_rule,omitempty"`
	SystemOneRule     *string                   `json:"system_one_rule,omitempty"`
	OnUncertain       *string                   `json:"on_uncertain,omitempty"`
	OnFailed          *string                   `json:"on_failed,omitempty"`
	OnFailedReasons   *map[string]string        `json:"on_failed_reasons,omitempty"`
	RejectReason      *string                   `json:"reject_reason,omitempty"`
	RejectRemediation *string                   `json:"reject_remediation,omitempty"`
	ExpectedRevision  int64                     `json:"expected_revision,omitempty"`
}

// MessagePolicyInlineSpec carries an inline policy definition for the dry-run
// test endpoint.
type MessagePolicyInlineSpec struct {
	Name              string                    `json:"name,omitempty"`
	Description       string                    `json:"description,omitempty"`
	Mode              string                    `json:"mode,omitempty"`
	Scope             *store.MessagePolicyScope `json:"scope,omitempty"`
	ViolationRule     string                    `json:"violation_rule,omitempty"`
	SystemOneRule     string                    `json:"system_one_rule,omitempty"`
	OnUncertain       string                    `json:"on_uncertain,omitempty"`
	OnFailed          string                    `json:"on_failed,omitempty"`
	OnFailedReasons   map[string]string         `json:"on_failed_reasons,omitempty"`
	RejectReason      string                    `json:"reject_reason,omitempty"`
	RejectRemediation string                    `json:"reject_remediation,omitempty"`
}

// MessagePolicyTestSample carries synthetic send facts for the dry-run test
// endpoint. The sample body is evaluated in-memory only and is never persisted
// or echoed in the response.
type MessagePolicyTestSample struct {
	Surface           string                           `json:"surface,omitempty"`
	Body              string                           `json:"body"`
	Type              string                           `json:"type,omitempty"`
	HasChoices        bool                             `json:"has_choices,omitempty"`
	Urgent            bool                             `json:"urgent,omitempty"`
	AttachmentCount   int                              `json:"attachment_count,omitempty"`
	MentionedAgentIDs []string                         `json:"mentioned_agent_ids,omitempty"`
	MentionedUserIDs  []string                         `json:"mentioned_user_ids,omitempty"`
	Sender            *messagepolicy.SenderFacts       `json:"sender,omitempty"`
	Recipient         *messagepolicy.RecipientFacts    `json:"recipient,omitempty"`
	Conversation      *messagepolicy.ConversationFacts `json:"conversation,omitempty"`
	Judgments         *messagepolicy.Judgments         `json:"judgments,omitempty"`
	JudgmentFlags     map[string]bool                  `json:"judgment_flags,omitempty"`
	JudgmentLabels    map[string]string                `json:"judgment_labels,omitempty"`
}

// LogValue omits the synthetic sample body if the struct is ever passed to slog.
func (s MessagePolicyTestSample) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("surface", s.Surface),
		slog.Int("body_bytes", len(s.Body)),
		slog.Bool("has_choices", s.HasChoices),
		slog.Bool("urgent", s.Urgent),
	)
}

// TestMessagePolicyRequest is the request body for
// POST /api/v1/projects/{id}/message-policies/test and
// POST /api/v1/projects/{id}/message-policies/{policyId}/test.
type TestMessagePolicyRequest struct {
	Policy            *MessagePolicyInlineSpec  `json:"policy,omitempty"`
	Name              string                    `json:"name,omitempty"`
	Mode              string                    `json:"mode,omitempty"`
	Scope             *store.MessagePolicyScope `json:"scope,omitempty"`
	ViolationRule     string                    `json:"violation_rule,omitempty"`
	SystemOneRule     string                    `json:"system_one_rule,omitempty"`
	OnUncertain       string                    `json:"on_uncertain,omitempty"`
	OnFailed          string                    `json:"on_failed,omitempty"`
	OnFailedReasons   map[string]string         `json:"on_failed_reasons,omitempty"`
	RejectReason      string                    `json:"reject_reason,omitempty"`
	RejectRemediation string                    `json:"reject_remediation,omitempty"`

	Sample  *MessagePolicyTestSample `json:"sample,omitempty"`
	Surface string                   `json:"surface,omitempty"`
	Body    string                   `json:"body,omitempty"`
}

// LogValue omits synthetic sample body content if the struct is ever passed to slog.
func (r TestMessagePolicyRequest) LogValue() slog.Value {
	bodyBytes := len(r.Body)
	surface := r.Surface
	if r.Sample != nil {
		bodyBytes = len(r.Sample.Body)
		if r.Sample.Surface != "" {
			surface = r.Sample.Surface
		}
	}
	return slog.GroupValue(
		slog.String("name", r.Name),
		slog.String("mode", r.Mode),
		slog.String("surface", surface),
		slog.Int("body_bytes", bodyBytes),
	)
}

// TestMessagePolicyResponse is the content-free dry-run evaluation response.
// It never echoes the sample message body.
type TestMessagePolicyResponse struct {
	PolicyID       string `json:"policy_id,omitempty"`
	PolicyName     string `json:"policy_name"`
	PolicyRevision int64  `json:"policy_revision,omitempty"`
	Mode           string `json:"mode"`
	Surface        string `json:"surface"`
	ScopeMatched   bool   `json:"scope_matched"`
	Class          string `json:"class"`
	ReasonCode     string `json:"reason_code,omitempty"`
	Outcome        string `json:"outcome"`
	Action         string `json:"action"`
	Reason         string `json:"reason,omitempty"`
	Remediation    string `json:"remediation,omitempty"`
	ElapsedMs      int64  `json:"elapsed_ms"`
	BodyBytes      int    `json:"body_bytes"`
	BodyTruncated  bool   `json:"body_truncated"`
}

func (s *messagePolicyService) basePath() string {
	return "/api/v1/projects/" + url.PathEscape(s.projectID) + "/message-policies"
}

func (s *messagePolicyService) policyPath(policyRef string) string {
	return s.basePath() + "/" + url.PathEscape(policyRef)
}

func (s *messagePolicyService) validateProjectID() error {
	if s.projectID == "" {
		return errors.New("project ID is required")
	}
	return nil
}

func (s *messagePolicyService) Create(ctx context.Context, req *CreateMessagePolicyRequest) (*MessagePolicyResponse, error) {
	if err := s.validateProjectID(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, errors.New("create message policy request is required")
	}
	resp, err := s.c.postNoRetry(ctx, s.basePath(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[MessagePolicyResponse](resp)
}

func (s *messagePolicyService) List(ctx context.Context, opts *ListMessagePoliciesOptions) (*ListMessagePoliciesResponse, error) {
	if err := s.validateProjectID(); err != nil {
		return nil, err
	}
	query := url.Values{}
	if opts != nil {
		if opts.Mode != "" {
			query.Set("mode", opts.Mode)
		}
		if opts.Enabled != nil {
			query.Set("enabled", strconv.FormatBool(*opts.Enabled))
		}
		if opts.Limit > 0 {
			query.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.Cursor != "" {
			query.Set("cursor", opts.Cursor)
		}
	}
	resp, err := s.c.getWithQuery(ctx, s.basePath(), query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListMessagePoliciesResponse](resp)
}

func (s *messagePolicyService) Get(ctx context.Context, policyRef string) (*MessagePolicyResponse, error) {
	if err := s.validateProjectID(); err != nil {
		return nil, err
	}
	if policyRef == "" {
		return nil, errors.New("policy ID or name is required")
	}
	resp, err := s.c.get(ctx, s.policyPath(policyRef), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[MessagePolicyResponse](resp)
}

func (s *messagePolicyService) Update(ctx context.Context, policyRef string, req *PatchMessagePolicyRequest) (*MessagePolicyResponse, error) {
	if err := s.validateProjectID(); err != nil {
		return nil, err
	}
	if policyRef == "" {
		return nil, errors.New("policy ID or name is required")
	}
	if req == nil {
		return nil, errors.New("patch message policy request is required")
	}
	resp, err := s.c.patchNoRetry(ctx, s.policyPath(policyRef), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[MessagePolicyResponse](resp)
}

func (s *messagePolicyService) Delete(ctx context.Context, policyRef string) error {
	if err := s.validateProjectID(); err != nil {
		return err
	}
	if policyRef == "" {
		return errors.New("policy ID or name is required")
	}
	resp, err := s.c.deleteNoRetry(ctx, s.policyPath(policyRef), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

func (s *messagePolicyService) Test(ctx context.Context, req *TestMessagePolicyRequest) (*TestMessagePolicyResponse, error) {
	if err := s.validateProjectID(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, errors.New("test message policy request is required")
	}
	resp, err := s.c.postNoRetry(ctx, s.basePath()+"/test", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[TestMessagePolicyResponse](resp)
}

func (s *messagePolicyService) TestPolicy(ctx context.Context, policyRef string, req *TestMessagePolicyRequest) (*TestMessagePolicyResponse, error) {
	if err := s.validateProjectID(); err != nil {
		return nil, err
	}
	if policyRef == "" {
		return nil, errors.New("policy ID or name is required")
	}
	if req == nil {
		return nil, errors.New("test message policy request is required")
	}
	resp, err := s.c.postNoRetry(ctx, s.policyPath(policyRef)+"/test", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[TestMessagePolicyResponse](resp)
}

func buildMessagePolicyDecisionsQuery(opts *ListMessagePolicyDecisionsOptions, allowPolicyID bool) url.Values {
	query := url.Values{}
	if opts == nil {
		return query
	}
	if allowPolicyID && opts.PolicyID != "" {
		query.Set("policy_id", opts.PolicyID)
	}
	if opts.Mode != "" {
		query.Set("mode", opts.Mode)
	}
	if opts.Surface != "" {
		query.Set("surface", opts.Surface)
	}
	if opts.Class != "" {
		query.Set("class", opts.Class)
	}
	if opts.Action != "" {
		query.Set("action", opts.Action)
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		query.Set("cursor", opts.Cursor)
	}
	return query
}

func (s *messagePolicyService) ListDecisions(ctx context.Context, opts *ListMessagePolicyDecisionsOptions) (*ListMessagePolicyDecisionsResponse, error) {
	if err := s.validateProjectID(); err != nil {
		return nil, err
	}
	query := buildMessagePolicyDecisionsQuery(opts, true)
	resp, err := s.c.getWithQuery(ctx, s.basePath()+"/decisions", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListMessagePolicyDecisionsResponse](resp)
}

func (s *messagePolicyService) ListPolicyDecisions(ctx context.Context, policyRef string, opts *ListMessagePolicyDecisionsOptions) (*ListMessagePolicyDecisionsResponse, error) {
	if err := s.validateProjectID(); err != nil {
		return nil, err
	}
	if policyRef == "" {
		return nil, errors.New("policy ID or name is required")
	}
	query := buildMessagePolicyDecisionsQuery(opts, false)
	resp, err := s.c.getWithQuery(ctx, s.policyPath(policyRef)+"/decisions", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListMessagePolicyDecisionsResponse](resp)
}
