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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// AgentService handles agent operations.
type AgentService interface {
	// List returns agents matching the filter criteria.
	List(ctx context.Context, opts *ListAgentsOptions) (*ListAgentsResponse, error)

	// Get returns a single agent by ID.
	Get(ctx context.Context, agentID string) (*Agent, error)

	// Create creates a new agent.
	Create(ctx context.Context, req *CreateAgentRequest) (*CreateAgentResponse, error)

	// Update updates an agent's metadata.
	Update(ctx context.Context, agentID string, req *UpdateAgentRequest) (*Agent, error)

	// Delete removes an agent.
	Delete(ctx context.Context, agentID string, opts *DeleteAgentOptions) error

	// Start starts a stopped agent. It sends no request body, so it never
	// asks for a force-resume; see agentService.Start.
	Start(ctx context.Context, agentID string) (*LifecycleResponse, error)

	// Stop stops a running agent.
	Stop(ctx context.Context, agentID string) (*LifecycleResponse, error)

	// Suspend pauses a running agent, preserving state for later resume.
	Suspend(ctx context.Context, agentID string) (*LifecycleResponse, error)

	// Restart restarts an agent.
	Restart(ctx context.Context, agentID string) (*LifecycleResponse, error)

	// ResetAuth injects a fresh token into a running agent without restarting.
	ResetAuth(ctx context.Context, agentID string) error

	// StopAll stops all running agents in scope.
	StopAll(ctx context.Context) (*StopAllResponse, error)

	// SendMessage sends a plain text message to an agent (legacy).
	SendMessage(ctx context.Context, agentID string, message string, interrupt bool) error

	// SendStructuredMessage sends a structured message to an agent.
	// If notify is true, the sender subscribes to status notifications for the target agent.
	// If wake is true, a suspended agent will be resumed before delivering the message.
	//
	// It delegates to SendStructuredMessageWithOptions with no explicit
	// mentions; callers that need to pass mentions should call that method
	// directly.
	SendStructuredMessage(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt bool, notify bool, wake bool) (*MessageResponse, error)

	// SendStructuredMessageWithOptions sends a structured message to an
	// agent, same as SendStructuredMessage, plus an explicit list of agent
	// slugs to mention: the server unions Mentions with body-extracted
	// @mentions, deduplicates, excludes the sender and the primary
	// recipient, and fans out a TypeMention to each. Callers on these send
	// paths do not fan mentions out client-side; the server does it.
	SendStructuredMessageWithOptions(ctx context.Context, agentID string, msg *messages.StructuredMessage, opts SendMessageOptions) (*MessageResponse, error)

	// BroadcastMessage broadcasts a structured message to all running agents in the project.
	// Uses the Hub's broadcast endpoint which routes through the message broker (if available)
	// or performs direct fan-out as a fallback.
	BroadcastMessage(ctx context.Context, msg *messages.StructuredMessage, interrupt bool) (*BroadcastResponse, error)

	// SubmitEnv submits gathered environment variables for an agent after a 202 env-gather response.
	SubmitEnv(ctx context.Context, agentID string, req *SubmitEnvRequest) (*CreateAgentResponse, error)

	// Restore restores a soft-deleted agent.
	Restore(ctx context.Context, agentID string) (*Agent, error)

	// Exec executes a command in an agent container.
	Exec(ctx context.Context, agentID string, command []string, timeout int) (*ExecResponse, error)

	// GetLogs retrieves agent logs.
	GetLogs(ctx context.Context, agentID string, opts *GetLogsOptions) (string, error)

	// SendOutboundMessage sends a message from an agent via the outbound
	// endpoint and returns the server-assigned message identity and delivery
	// status. The SDK adds no retry or fallback routing; send budget, policy,
	// and capabilities are server-enforced.
	SendOutboundMessage(ctx context.Context, agentID string, msg *OutboundMessageRequest) (*OutboundMessageResult, error)

	// GetCloudLogs retrieves structured log entries from Cloud Logging.
	GetCloudLogs(ctx context.Context, agentID string, opts *GetCloudLogsOptions) (*CloudLogsResponse, error)

	// StreamCloudLogs opens an SSE connection for streaming log entries.
	// The handler is called for each log entry received. Blocks until the
	// context is cancelled or the server closes the connection.
	StreamCloudLogs(ctx context.Context, agentID string, opts *GetCloudLogsOptions, handler func(CloudLogEntry)) error

	// SetMessageMode changes the messaging mode for an agent.
	SetMessageMode(ctx context.Context, agentID string, req *SetMessageModeRequest, opts *SetMessageModeOptions) (*SetMessageModeResponse, error)

	// Reincarnate requests a `scion reincarnate` migration for an agent:
	// re-resolve its configuration against the current template/harness-config
	// catalog and start a new generation with the given handoff as its first
	// task. With req.DryRun, returns the resolved plan and changes nothing.
	Reincarnate(ctx context.Context, agentID string, req *ReincarnateAgentRequest) (*ReincarnateAgentResponse, error)

	// SendKeys delivers literal terminal input to an agent's tmux session via
	// the dedicated agent-keys operation (.design/agent-keys-contract.md),
	// POSTing to {id}/keys — never to /message, and never as a Raw
	// StructuredMessage. It never creates a conversation message, never uses
	// message-mode authorization, and is never retried: the request is sent
	// exactly once regardless of how the client was constructed (including
	// WithRetry), and an HTTP redirect is never followed and re-sent. A
	// network-level failure (no response received at all) is returned
	// unchanged — the caller cannot know whether the broker executed the
	// keys, and must not infer success or failure from this call having
	// failed to connect. A response the Hub did send is decoded and
	// returned as *apiclient.APIError on any non-2xx status, preserving the
	// outcome code (Code) and, where the contract says one exists for that
	// outcome, the operation ID (Details["operation_id"]) — see contract
	// §2.4a/§2.5. SendKeys performs no client-side retry, fallback, or
	// downgrade to messaging of any kind.
	SendKeys(ctx context.Context, agentID string, keys string) (*agentkeys.Response, error)
}

// agentService is the implementation of AgentService.
type agentService struct {
	c         *client
	projectID string
}

func (s *agentService) agentPath(agentID string) string {
	if s.projectID != "" {
		return "/api/v1/projects/" + s.projectID + "/agents/" + agentID
	}
	return "/api/v1/agents/" + agentID
}

func (s *agentService) agentsPath() string {
	if s.projectID != "" {
		return "/api/v1/projects/" + s.projectID + "/agents"
	}
	return "/api/v1/agents"
}

// ListAgentsOptions configures agent list filtering.
type ListAgentsOptions struct {
	ProjectID       string            // Filter by project
	Phase           string            // Filter by lifecycle phase (created, running, stopped, error, etc.)
	RuntimeBrokerID string            // Filter by runtime broker
	Labels          map[string]string // Label selector
	IncludeDeleted  bool              // Include soft-deleted agents

	// OwnerID, when set, restricts results to agents owned by this principal
	// ID. Always combined with every other option using AND (ptone/scion#2146).
	OwnerID string

	// AncestorID, when set, restricts results to agents whose Ancestry chain
	// contains this principal ID (transitive descendants of AncestorID).
	AncestorID string

	// HarnessConfig, when set, restricts results to agents whose resolved
	// harness-config name equals this value.
	HarnessConfig string

	// IDs, when non-empty, restricts results to agents whose ID is in this
	// set. Used for relationship queries (e.g. CLI --ancestors) that resolve
	// a specific set of candidate IDs client-side and ask the Hub to narrow
	// them to the caller's authorized, currently-existing agents.
	IDs []string

	// LineageRootID, when set, restricts results to the agent whose ID
	// equals this value OR whose Ancestry contains it — the root agent plus
	// all its descendants. Used by CLI --lineage.
	LineageRootID string

	Page apiclient.PageOptions
}

// ListAgentsResponse is the response from listing agents.
type ListAgentsResponse struct {
	Agents     []Agent
	ServerTime time.Time // Hub server timestamp for clock-skew-safe sync watermarks
	Page       apiclient.PageResult
}

// StopAllResult represents the outcome of stopping a single agent.
type StopAllResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// StopAllResponse is the response from the stop-all endpoint.
type StopAllResponse struct {
	Stopped int `json:"stopped"`
	Failed  int `json:"failed"`
	// StopRecorded counts agents whose start was in flight: the stop intent
	// is recorded but the start is not interrupted (result status
	// "stop_recorded").
	StopRecorded int             `json:"stopRecorded,omitempty"`
	Total        int             `json:"total"`
	Results      []StopAllResult `json:"results"`
}

// CreateAgentRequest is the request body for creating an agent.
type CreateAgentRequest struct {
	Name            string            `json:"name"`
	ProjectID       string            `json:"projectId"`
	Template        string            `json:"template,omitempty"`
	HarnessConfig   string            `json:"harnessConfig,omitempty"` // Explicit harness config name (used during sync when template may not be on Hub)
	HarnessAuth     string            `json:"harnessAuth,omitempty"`   // Late-binding override for auth_selected_type
	RuntimeBrokerID string            `json:"runtimeBrokerId,omitempty"`
	Profile         string            `json:"profile,omitempty"`
	Task            string            `json:"task,omitempty"`
	Branch          string            `json:"branch,omitempty"`
	Workspace       string            `json:"workspace,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	Config          *api.ScionConfig  `json:"config,omitempty"`
	Resume          bool              `json:"resume,omitempty"`
	ForceResume     bool              `json:"forceResume,omitempty"`
	Attach          bool              `json:"attach,omitempty"`        // If true, signals interactive attach mode to the broker/harness
	ProvisionOnly   bool              `json:"provisionOnly,omitempty"` // If true, provision only (write task to prompt.md) without starting
	// WorkspaceFiles is populated for non-git workspace bootstrap.
	WorkspaceFiles []transfer.FileInfo `json:"workspaceFiles,omitempty"`

	// GatherEnv enables the env-gather flow: the Hub/Broker will evaluate env
	// completeness and return a 202 with requirements if keys are missing,
	// allowing the CLI to gather and submit them.
	GatherEnv bool `json:"gatherEnv,omitempty"`

	// Notify subscribes the creating agent/user to status notifications
	// (COMPLETED, WAITING_FOR_INPUT, LIMITS_EXCEEDED) for the new agent.
	Notify bool `json:"notify,omitempty"`

	// AgentRole specifies the requested authorization role.
	AgentRole string `json:"agentRole,omitempty"`

	// NoAuth disables auth credential propagation into the agent container
	// (CLI --no-auth). Honoured by the Hub on the create path only; an
	// existing agent that is resumed/restarted in place does not re-read it
	// (ptone/scion#1855).
	NoAuth bool `json:"noAuth,omitempty"`

	// MessageMode specifies the initial message mode for the agent.
	// Valid values: "none", "lineage", "branch", "project", "hub".
	// When omitted, resolved from template, parent inheritance, or "project" default.
	MessageMode string `json:"messageMode,omitempty"`

	// GCPIdentity specifies the GCP identity assignment for the agent.
	// Controls metadata server behavior and optional service account binding.
	// When nil, the project default (if any) is applied by the Hub.
	GCPIdentity *GCPIdentityConfig `json:"gcp_identity,omitempty"`

	// AcceptAsyncLaunch opts in to a non-blocking launch. A Hub that has
	// async launch enabled may then answer as soon as the broker accepts the
	// create, with the agent in a pre-running phase and an active Launch;
	// the client follows the launch with GET agent. A Hub that does not
	// support or enable it ignores the field and answers synchronously.
	AcceptAsyncLaunch bool `json:"acceptAsyncLaunch,omitempty"`
}

// GCPIdentityConfig specifies GCP identity configuration for agent creation.
// Mirrors the Hub's GCPIdentityAssignment structure.
type GCPIdentityConfig struct {
	// MetadataMode controls the GCE metadata server behavior for the agent:
	//   "block" — metadata server is blocked (default)
	//   "passthrough" — metadata server passes through the broker host identity
	//   "assign" — a specific service account is bound to the agent
	MetadataMode string `json:"metadata_mode"`

	// ServiceAccountID is the Scion resource ID of the service account to assign.
	// Required when MetadataMode is "assign", must be empty otherwise.
	ServiceAccountID string `json:"service_account_id,omitempty"`
}

// CreateAgentResponse is the response from creating an agent.
type CreateAgentResponse struct {
	Agent    *Agent   `json:"agent"`
	Warnings []string `json:"warnings,omitempty"`
	// UploadURLs is populated during workspace bootstrap (non-git projects).
	UploadURLs []transfer.UploadURLInfo `json:"uploadUrls,omitempty"`
	// Expires indicates when the upload URLs expire.
	Expires *time.Time `json:"expires,omitempty"`

	// EnvGather is populated when the Hub returns HTTP 202, indicating the
	// broker needs additional env vars that only the CLI can provide.
	EnvGather *EnvGatherResponse `json:"envGather,omitempty"`
}

// EnvGatherResponse contains env requirements returned by the Hub when the
// broker cannot satisfy all required environment variables.
// SecretKeyInfo provides metadata about a required secret key.
type SecretKeyInfo struct {
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`         // "harness", "template", "settings"
	Type        string `json:"type,omitempty"` // "environment" (default), "variable", "file"
}

type EnvGatherResponse struct {
	AgentID     string                   `json:"agentId"`
	Required    []string                 `json:"required"`
	HubHas      []EnvSource              `json:"hubHas"`
	BrokerHas   []string                 `json:"brokerHas"`
	Needs       []string                 `json:"needs"`
	SecretInfo  map[string]SecretKeyInfo `json:"secretInfo,omitempty"`
	HubWarnings []string                 `json:"hubWarnings,omitempty"`
}

// EnvSource tracks which scope provided an env var key.
type EnvSource struct {
	Key   string `json:"key"`
	Scope string `json:"scope"`
}

// SubmitEnvRequest is sent by the CLI to provide gathered env vars
// after receiving a 202 env-gather response.
type SubmitEnvRequest struct {
	Env map[string]string `json:"env"`
}

// UpdateAgentRequest is the request body for updating an agent.
type UpdateAgentRequest struct {
	Name         string            `json:"name,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	TaskSummary  string            `json:"taskSummary,omitempty"`
	StateVersion int64             `json:"stateVersion"` // Required for optimistic locking
}

// DeleteAgentOptions configures agent deletion.
type DeleteAgentOptions struct {
	DeleteFiles  bool // Also delete agent files
	RemoveBranch bool // Remove git branch
	Force        bool // Force hard-delete even when soft-delete is configured
}

// GetLogsOptions configures log retrieval.
type GetLogsOptions struct {
	Tail  int    // Number of lines from end
	Since string // RFC3339 timestamp
}

// ExecResponse is the response from executing a command.
type ExecResponse struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
}

// List returns agents matching the filter criteria.
func (s *agentService) List(ctx context.Context, opts *ListAgentsOptions) (*ListAgentsResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.ProjectID != "" {
			query.Set("projectId", opts.ProjectID)
		}
		if opts.Phase != "" {
			query.Set("phase", opts.Phase)
		}
		if opts.RuntimeBrokerID != "" {
			query.Set("runtimeBrokerId", opts.RuntimeBrokerID)
		}
		if opts.IncludeDeleted {
			query.Set("includeDeleted", "true")
		}
		for k, v := range opts.Labels {
			query.Add("label", fmt.Sprintf("%s=%s", k, v))
		}
		if opts.OwnerID != "" {
			query.Set("ownerId", opts.OwnerID)
		}
		if opts.AncestorID != "" {
			query.Set("ancestorId", opts.AncestorID)
		}
		if opts.HarnessConfig != "" {
			query.Set("harnessConfig", opts.HarnessConfig)
		}
		for _, id := range opts.IDs {
			query.Add("id", id)
		}
		if opts.LineageRootID != "" {
			query.Set("lineageRootId", opts.LineageRootID)
		}
		opts.Page.ToQuery(query)
	}

	resp, err := s.c.getWithQuery(ctx, s.agentsPath(), query, nil)
	if err != nil {
		return nil, err
	}

	type listResponse struct {
		Agents     []Agent   `json:"agents"`
		NextCursor string    `json:"nextCursor,omitempty"`
		TotalCount int       `json:"totalCount,omitempty"`
		ServerTime time.Time `json:"serverTime"`
	}

	result, err := apiclient.DecodeResponse[listResponse](resp)
	if err != nil {
		return nil, err
	}

	return &ListAgentsResponse{
		Agents:     result.Agents,
		ServerTime: result.ServerTime,
		Page: apiclient.PageResult{
			NextCursor: result.NextCursor,
			TotalCount: result.TotalCount,
		},
	}, nil
}

// Get returns a single agent by ID.
func (s *agentService) Get(ctx context.Context, agentID string) (*Agent, error) {
	resp, err := s.c.get(ctx, s.agentPath(agentID), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Agent](resp)
}

// Create creates a new agent.
func (s *agentService) Create(ctx context.Context, req *CreateAgentRequest) (*CreateAgentResponse, error) {
	resp, err := s.c.post(ctx, s.agentsPath(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[CreateAgentResponse](resp)
}

// SubmitEnv submits gathered environment variables for an agent after a 202 env-gather response.
func (s *agentService) SubmitEnv(ctx context.Context, agentID string, req *SubmitEnvRequest) (*CreateAgentResponse, error) {
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/env", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[CreateAgentResponse](resp)
}

// Update updates an agent's metadata.
func (s *agentService) Update(ctx context.Context, agentID string, req *UpdateAgentRequest) (*Agent, error) {
	resp, err := s.c.patch(ctx, s.agentPath(agentID), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Agent](resp)
}

// Delete removes an agent.
func (s *agentService) Delete(ctx context.Context, agentID string, opts *DeleteAgentOptions) error {
	resp, err := s.c.delete(ctx, s.deletePath(agentID, opts), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// deletePath builds the DELETE URL, with the query parameters for opts.
func (s *agentService) deletePath(agentID string, opts *DeleteAgentOptions) string {
	path := s.agentPath(agentID)
	if opts != nil {
		query := url.Values{}
		// Server defaults deleteFiles/removeBranch to true, so only send
		// the parameter when the caller explicitly wants to preserve them.
		if !opts.DeleteFiles {
			query.Set("deleteFiles", "false")
		}
		if !opts.RemoveBranch {
			query.Set("removeBranch", "false")
		}
		if opts.Force {
			query.Set("force", "true")
		}
		if len(query) > 0 {
			path += "?" + query.Encode()
		}
	}
	return path
}

// Start starts a stopped agent via the hub's /start lifecycle action. It
// sends no request body.
//
// The hub's /start route also accepts an optional {"forceResume":true} body
// (hub.AgentLifecycleStartRequest), which resumes the interrupted harness
// session of an agent in phase=error. The client deliberately does not expose
// it here: the scion CLI reaches force-resume through the create path
// instead (`scion resume --force` sends CreateAgentRequest with Resume and
// ForceResume set), which also covers an agent that is not yet provisioned
// or no longer exists on the hub. No caller needs force-resume on Start
// (ptone/scion#2864).
func (s *agentService) Start(ctx context.Context, agentID string) (*LifecycleResponse, error) {
	return s.lifecycle(ctx, agentID, "start")
}

// Stop stops a running agent.
func (s *agentService) Stop(ctx context.Context, agentID string) (*LifecycleResponse, error) {
	return s.lifecycle(ctx, agentID, "stop")
}

// Suspend pauses a running agent, preserving state for later resume.
func (s *agentService) Suspend(ctx context.Context, agentID string) (*LifecycleResponse, error) {
	return s.lifecycle(ctx, agentID, "suspend")
}

// Restart restarts an agent.
func (s *agentService) Restart(ctx context.Context, agentID string) (*LifecycleResponse, error) {
	return s.lifecycle(ctx, agentID, "restart")
}

// LifecycleResponse is the result of a start, stop, suspend or restart.
type LifecycleResponse struct {
	// Agent is the agent as the hub left it, when the hub returned it.
	Agent *Agent
	// Warnings are messages the hub raised while applying the action.
	Warnings []string
	// Queued is true when the hub accepted the action but has not applied
	// it yet (HTTP 202), for example a stop for an agent whose broker is
	// offline, which runs when the broker reconnects.
	Queued bool
}

// lifecycle posts a lifecycle action and decodes the hub's response. The
// body is the agent plus an optional warnings list; a body that cannot be
// decoded is ignored, since the action itself succeeded.
func (s *agentService) lifecycle(ctx context.Context, agentID, action string) (*LifecycleResponse, error) {
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/"+action, nil, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, apiclient.ParseErrorResponse(resp)
	}
	out := &LifecycleResponse{Queued: resp.StatusCode == http.StatusAccepted}
	var body struct {
		Agent
		Warnings []string `json:"warnings,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
		out.Warnings = body.Warnings
		if body.ID != "" {
			agent := body.Agent
			out.Agent = &agent
		}
	}
	return out, nil
}

// ResetAuth injects a fresh token into a running agent without restarting.
func (s *agentService) ResetAuth(ctx context.Context, agentID string) error {
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/reset-auth", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// StopAll stops all running agents in scope.
func (s *agentService) StopAll(ctx context.Context) (*StopAllResponse, error) {
	resp, err := s.c.post(ctx, s.agentsPath()+"/stop-all", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[StopAllResponse](resp)
}

// Restore restores a soft-deleted agent.
func (s *agentService) Restore(ctx context.Context, agentID string) (*Agent, error) {
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/restore", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Agent](resp)
}

// SendMessage sends a message to an agent.
func (s *agentService) SendMessage(ctx context.Context, agentID string, message string, interrupt bool) error {
	body := struct {
		Message   string `json:"message"`
		Interrupt bool   `json:"interrupt,omitempty"`
	}{
		Message:   message,
		Interrupt: interrupt,
	}
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/message", body, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// MessageResponse is the parsed response from a successful agent message delivery.
type MessageResponse struct {
	MessageID  string `json:"message_id"`
	Status     string `json:"status"`
	Agent      string `json:"agent"`
	AgentPhase string `json:"agent_phase"`
	// Deferred is set when Status is "deferred": the recipient is
	// mid-`scion reincarnate` (design agent-reincarnate §3.7). The message
	// was saved to conversation history but not dispatched.
	Deferred string `json:"deferred,omitempty"`
	// MentionResults reports the outcome of server-side @mention fan-out,
	// one entry per resolved mention name. Empty when the message had no
	// mentions, or on hubs that predate this field.
	MentionResults []messages.MentionResult `json:"mention_results,omitempty"`
	// ArtifactWarning is set when artifact references the message named
	// were not attached. Empty on hubs that predate this field.
	ArtifactWarning string `json:"artifact_warning,omitempty"`

	// AttachmentWarnings lists attachments the hub could not record on the
	// message, which was still sent without them. Empty when every
	// attachment was recorded, or on hubs that predate this field.
	AttachmentWarnings []AttachmentWarning `json:"attachment_warnings,omitempty"`
}

// AttachmentWarning names one attachment the hub could not record on a sent
// message, for example because the file was staged on a different broker
// than the hub and so does not exist on the hub host.
type AttachmentWarning struct {
	// Path is the attachment path as sent.
	Path string `json:"path"`
	// Reason is the hub's short explanation.
	Reason string `json:"reason"`
}

// SendMessageOptions holds the optional parameters for
// SendStructuredMessageWithOptions.
type SendMessageOptions struct {
	// Interrupt the harness before sending.
	Interrupt bool
	// Notify subscribes the sender to status notifications for the target agent.
	Notify bool
	// Wake resumes a suspended target agent before delivering the message.
	Wake bool
	// Mentions lists agent slugs to receive mention notifications, in
	// addition to any @mentions the server extracts from the body. The
	// primary recipient and the sender are excluded automatically.
	Mentions []string
}

// SendStructuredMessage sends a structured message to an agent.
// If notify is true, the sender subscribes to status notifications for the target agent.
// If wake is true, a suspended agent will be resumed before delivering the message.
func (s *agentService) SendStructuredMessage(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt bool, notify bool, wake bool) (*MessageResponse, error) {
	return s.SendStructuredMessageWithOptions(ctx, agentID, msg, SendMessageOptions{
		Interrupt: interrupt,
		Notify:    notify,
		Wake:      wake,
	})
}

// SendStructuredMessageWithOptions sends a structured message to an agent
// with an explicit mentions list. See AgentService.SendStructuredMessageWithOptions.
func (s *agentService) SendStructuredMessageWithOptions(ctx context.Context, agentID string, msg *messages.StructuredMessage, opts SendMessageOptions) (*MessageResponse, error) {
	body := struct {
		StructuredMessage *messages.StructuredMessage `json:"structured_message"`
		Interrupt         bool                        `json:"interrupt,omitempty"`
		Notify            bool                        `json:"notify,omitempty"`
		Wake              bool                        `json:"wake,omitempty"`
		Mentions          []string                    `json:"mentions,omitempty"`
	}{
		StructuredMessage: msg,
		Interrupt:         opts.Interrupt,
		Notify:            opts.Notify,
		Wake:              opts.Wake,
		Mentions:          opts.Mentions,
	}
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/message", body, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[MessageResponse](resp)
}

// SendKeys implements AgentService.SendKeys. See that method's doc comment
// for the no-replay and error-fidelity guarantees; agentPath already honors
// project scoping, so the same call reaches either public route shape
// (.design/agent-keys-contract.md §2.1) depending on whether this service was
// obtained from Client.Agents() or Client.ProjectAgents(projectID).
func (s *agentService) SendKeys(ctx context.Context, agentID string, keys string) (*agentkeys.Response, error) {
	resp, err := s.c.postNoRetry(ctx, s.agentPath(agentID)+"/keys", agentkeys.Request{Keys: keys}, nil)
	if err != nil {
		// No response was received at all (connection refused/reset, DNS
		// failure, context deadline, ...): honest uncertainty, not a 503/502
		// to reclassify. The caller must not infer either outcome from this.
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return nil, apiclient.ParseErrorResponse(resp)
	}

	// Contract §2.4: the ONLY success shape is HTTP 200 with
	// Response.Status == agentkeys.StatusDispatched. Nothing else — a 204,
	// a 2xx the server never defines, or a 3xx apiclient.DecodeResponse's
	// own <400 check would otherwise treat as decodable — may be reported
	// as success: each is either a response this contract never promises
	// (so treating it as success would be a guess, not a decision) or a
	// response DoNoRetry's "do not follow the redirect" contract leaves
	// unresolved. Either way the caller must receive "unknown", never
	// "dispatched" and never a nil *Response with a nil error.
	if resp.StatusCode != http.StatusOK {
		return nil, &apiclient.APIError{
			StatusCode: resp.StatusCode,
			Code:       string(agentkeys.OutcomeKeysOutcomeUnknown),
			Message:    fmt.Sprintf("unexpected keys response status %d (only 200 means dispatched)", resp.StatusCode),
		}
	}

	var result agentkeys.Response
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, &apiclient.APIError{
			StatusCode: resp.StatusCode,
			Code:       string(agentkeys.OutcomeKeysOutcomeUnknown),
			Message:    "could not decode keys response body",
		}
	}
	if result.Status != agentkeys.StatusDispatched {
		return nil, &apiclient.APIError{
			StatusCode: resp.StatusCode,
			Code:       string(agentkeys.OutcomeKeysOutcomeUnknown),
			Message:    fmt.Sprintf("unexpected keys response status %q (not %q)", result.Status, agentkeys.StatusDispatched),
		}
	}
	return &result, nil
}

// OutboundMessageRequest is the request body for sending an outbound message
// from an agent. The recipient may be a human user or another agent; the hub
// determines the delivery path from the addressing fields.
type OutboundMessageRequest struct {
	Recipient       string            `json:"recipient,omitempty"`
	RecipientID     string            `json:"recipient_id,omitempty"`
	Msg             string            `json:"msg"`
	Type            string            `json:"type,omitempty"`
	Urgent          bool              `json:"urgent,omitempty"`
	Attachments     []string          `json:"attachments,omitempty"`
	Channel         string            `json:"channel,omitempty"`
	ThreadID        string            `json:"thread_id,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	ConversationID  string            `json:"conversation_id,omitempty"`
	ConversationRef string            `json:"conversation_ref,omitempty"`
	// Wake requests that a suspended target agent be resumed before
	// delivering the message. Ignored for non-agent recipients.
	Wake bool `json:"wake,omitempty"`
}

// OutboundMessageResult is the parsed response from a successful outbound
// message send. It carries the server-assigned message identity and delivery
// status so callers can correlate the message or detect ambiguous delivery.
//
// All fields are populated by the server on every 2xx response; omitempty is
// intentionally absent because the contract guarantees non-empty values on
// success. A nil result (with a non-nil error) indicates a non-2xx response.
type OutboundMessageResult struct {
	// MessageID is the server-assigned UUID for the persisted message.
	MessageID string `json:"message_id"`
	// Status is the delivery status reported by the hub (e.g. "sent").
	Status string `json:"status"`
	// Recipient is the wire-format recipient (e.g. "user:alice" or "agent:builder").
	Recipient string `json:"recipient"`
	// RecipientID is the recipient's UUID.
	RecipientID string `json:"recipient_id"`
	// Deferred is set only when Status == "deferred".
	Deferred string `json:"deferred,omitempty"`
	// ConversationID is the conversation the message was recorded in. It is
	// empty on hubs that predate this field, and on paths that do not report
	// it (for example, a send to another agent).
	ConversationID string `json:"conversation_id,omitempty"`
	// MentionResults reports the outcome of server-side @mention fan-out,
	// one entry per resolved mention name. Empty when the message had no
	// mentions, or on hubs that predate this field.
	MentionResults []messages.MentionResult `json:"mention_results,omitempty"`
	// ArtifactWarning is set when artifact references the message named
	// were not attached. Empty on hubs that predate this field.
	ArtifactWarning string `json:"artifact_warning,omitempty"`

	// AttachmentWarnings lists attachments the hub could not record on the
	// message, which was still sent without them. Empty when every
	// attachment was recorded, or on hubs that predate this field.
	AttachmentWarnings []AttachmentWarning `json:"attachment_warnings,omitempty"`
}

// SendOutboundMessage sends a message from an agent via the outbound endpoint.
// Returns the server-assigned message identity and delivery status, or a
// structured error on failure. The SDK adds no retry or fallback routing;
// send budget, policy, and capabilities are server-enforced.
func (s *agentService) SendOutboundMessage(ctx context.Context, agentID string, msg *OutboundMessageRequest) (*OutboundMessageResult, error) {
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/outbound-message", msg, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[OutboundMessageResult](resp)
}

// BroadcastResponse is the parsed response from a broadcast message delivery.
type BroadcastResponse struct {
	Status           string         `json:"status"`
	Total            int            `json:"total"`
	Targeted         int            `json:"targeted"`
	Skipped          int            `json:"skipped"`
	SkippedBreakdown map[string]int `json:"skipped_breakdown,omitempty"`
}

// BroadcastMessage broadcasts a structured message to all running agents in the project.
func (s *agentService) BroadcastMessage(ctx context.Context, msg *messages.StructuredMessage, interrupt bool) (*BroadcastResponse, error) {
	if s.projectID == "" {
		return nil, fmt.Errorf("broadcast requires a project-scoped agent service")
	}
	body := struct {
		StructuredMessage *messages.StructuredMessage `json:"structured_message"`
		Interrupt         bool                        `json:"interrupt,omitempty"`
	}{
		StructuredMessage: msg,
		Interrupt:         interrupt,
	}
	resp, err := s.c.post(ctx, "/api/v1/projects/"+s.projectID+"/broadcast", body, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[BroadcastResponse](resp)
}

// Exec executes a command in an agent container.
func (s *agentService) Exec(ctx context.Context, agentID string, command []string, timeout int) (*ExecResponse, error) {
	body := struct {
		Command []string `json:"command"`
		Timeout int      `json:"timeout,omitempty"`
	}{
		Command: command,
		Timeout: timeout,
	}
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/exec", body, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ExecResponse](resp)
}

// GetLogs retrieves agent logs.
func (s *agentService) GetLogs(ctx context.Context, agentID string, opts *GetLogsOptions) (string, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Tail > 0 {
			query.Set("tail", fmt.Sprintf("%d", opts.Tail))
		}
		if opts.Since != "" {
			query.Set("since", opts.Since)
		}
	}

	resp, err := s.c.getWithQuery(ctx, s.agentPath(agentID)+"/logs", query, nil)
	if err != nil {
		return "", err
	}

	type logsResponse struct {
		Logs string `json:"logs"`
	}

	result, err := apiclient.DecodeResponse[logsResponse](resp)
	if err != nil {
		return "", err
	}
	return result.Logs, nil
}

// GetCloudLogsOptions configures cloud log retrieval.
type GetCloudLogsOptions struct {
	Tail     int
	Since    string
	Until    string
	Severity string
	BrokerID string
}

// CloudLogsResponse is the response from querying cloud logs.
type CloudLogsResponse struct {
	Entries       []CloudLogEntry `json:"entries"`
	NextPageToken string          `json:"nextPageToken,omitempty"`
	HasMore       bool            `json:"hasMore"`
}

// CloudLogEntry represents a structured log entry from Cloud Logging.
type CloudLogEntry struct {
	Timestamp      time.Time              `json:"timestamp"`
	Severity       string                 `json:"severity"`
	Message        string                 `json:"message"`
	Labels         map[string]string      `json:"labels,omitempty"`
	Resource       map[string]interface{} `json:"resource,omitempty"`
	JSONPayload    map[string]interface{} `json:"jsonPayload,omitempty"`
	InsertID       string                 `json:"insertId"`
	SourceLocation *SourceLocation        `json:"sourceLocation,omitempty"`
}

// SourceLocation identifies the source code location of a log entry.
type SourceLocation struct {
	File     string `json:"file,omitempty"`
	Line     string `json:"line,omitempty"`
	Function string `json:"function,omitempty"`
}

// GetCloudLogs retrieves structured log entries from Cloud Logging.
func (s *agentService) GetCloudLogs(ctx context.Context, agentID string, opts *GetCloudLogsOptions) (*CloudLogsResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Tail > 0 {
			query.Set("tail", fmt.Sprintf("%d", opts.Tail))
		}
		if opts.Since != "" {
			query.Set("since", opts.Since)
		}
		if opts.Until != "" {
			query.Set("until", opts.Until)
		}
		if opts.Severity != "" {
			query.Set("severity", opts.Severity)
		}
		if opts.BrokerID != "" {
			query.Set("broker_id", opts.BrokerID)
		}
	}

	resp, err := s.c.getWithQuery(ctx, s.agentPath(agentID)+"/cloud-logs", query, nil)
	if err != nil {
		return nil, err
	}

	return apiclient.DecodeResponse[CloudLogsResponse](resp)
}

// StreamCloudLogs opens an SSE connection for streaming cloud log entries.
func (s *agentService) StreamCloudLogs(ctx context.Context, agentID string, opts *GetCloudLogsOptions, handler func(CloudLogEntry)) error {
	query := url.Values{}
	if opts != nil {
		if opts.Severity != "" {
			query.Set("severity", opts.Severity)
		}
		if opts.BrokerID != "" {
			query.Set("broker_id", opts.BrokerID)
		}
	}

	headers := http.Header{}
	headers.Set("Accept", "text/event-stream")

	resp, err := s.c.getWithQuery(ctx, s.agentPath(agentID)+"/cloud-logs/stream", query, headers)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return apiclient.CheckResponse(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()

		// Skip empty lines, heartbeats, and event type lines
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}

		// Parse data lines
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var entry CloudLogEntry
			if err := json.Unmarshal([]byte(data), &entry); err != nil {
				continue
			}
			handler(entry)
		}
	}

	return scanner.Err()
}

// SetMessageModeRequest is the request body for changing an agent's message mode.
type SetMessageModeRequest struct {
	Mode    string `json:"mode"`
	Cascade bool   `json:"cascade,omitempty"`
}

// SetMessageModeResponse is the response from changing an agent's message mode.
type SetMessageModeResponse struct {
	AgentID  string          `json:"agent_id"`
	Mode     string          `json:"mode"`
	Previous string          `json:"previous_mode"`
	Cascade  json.RawMessage `json:"cascade,omitempty"`
}

// SetMessageModeOptions configures the set-message-mode call.
type SetMessageModeOptions struct {
	DryRun bool
}

// SetMessageMode changes the messaging mode for an agent.
func (s *agentService) SetMessageMode(ctx context.Context, agentID string, req *SetMessageModeRequest, opts *SetMessageModeOptions) (*SetMessageModeResponse, error) {
	path := s.agentPath(agentID) + "/set_message_mode"
	if opts != nil && opts.DryRun {
		path += "?dryRun=true"
	}
	resp, err := s.c.post(ctx, path, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[SetMessageModeResponse](resp)
}

// Reincarnate requests a `scion reincarnate` migration for an agent (design
// /scion-volumes/scratchpad/projects/agent-migrate/design.md §3.2).
func (s *agentService) Reincarnate(ctx context.Context, agentID string, req *ReincarnateAgentRequest) (*ReincarnateAgentResponse, error) {
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/reincarnate", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ReincarnateAgentResponse](resp)
}

// ReincarnateAgentRequest is the request body for Reincarnate. Besides
// Handoff and DryRun it carries the patch fields of ptone/scion#3302. A hub
// that predates them ignores ServiceAccount, Role and ThinkingLevel and
// rejects the others with a 400; ReincarnationPlan.Patched tells a client
// whether the hub applied them.
type ReincarnateAgentRequest struct {
	Handoff string `json:"handoff,omitempty"`
	DryRun  bool   `json:"dryRun,omitempty"`
	// TargetBroker (a broker ID, name or slug) asks to move the agent to
	// that broker, which must mount the same NFS export as its current one.
	TargetBroker string `json:"targetBroker,omitempty"`
	// SharedDirBackends changes the recorded shared-dir storage backend of
	// the named shared dirs (dir name to "nfs"). Only the agent's record
	// changes; the data is copied by the operator.
	SharedDirBackends map[string]string `json:"sharedDirBackends,omitempty"`
	// AllowEmptySharedDir skips the start check that refuses an empty nfs
	// directory while the previous local directory is not empty.
	AllowEmptySharedDir bool `json:"allowEmptySharedDir,omitempty"`

	// Patch fields: each changes the next generation's setting, and later
	// reincarnations keep it. Empty (nil for ThinkingLevel) is unchanged.
	ServiceAccount string `json:"serviceAccount,omitempty"`
	Role           string `json:"role,omitempty"`
	Image          string `json:"image,omitempty"`
	Model          string `json:"model,omitempty"`
	ThinkingLevel  *int   `json:"thinkingLevel,omitempty"`
	HarnessAuth    string `json:"harnessAuth,omitempty"`

	// Overrides not yet supported by the hub.
	HarnessConfig  string            `json:"harnessConfig,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TemplateHash   string            `json:"templateHash,omitempty"`
	ResetOverrides bool              `json:"resetOverrides,omitempty"`
	Rollback       bool              `json:"rollback,omitempty"`
}

// ReincarnateAgentResponse is the response body for Reincarnate: 202 for a
// persisted (pending) reincarnation, or 200 for a dry run.
type ReincarnateAgentResponse struct {
	AgentID    string            `json:"agentId"`
	Generation int               `json:"generation"`
	State      string            `json:"state"`
	Plan       ReincarnationPlan `json:"plan"`
	// SourceBrokerID and TargetBrokerID are set when the request named a
	// target broker; they are equal for a plain reincarnation.
	SourceBrokerID string `json:"sourceBrokerId,omitempty"`
	TargetBrokerID string `json:"targetBrokerId,omitempty"`
	// MoveVerdict is the move eligibility verdict of a dry-run move.
	MoveVerdict *MoveVerdict `json:"moveVerdict,omitempty"`
}

// MoveVerdict is the hub's eligibility verdict for moving an agent to
// another broker. A refused request carries it in the error details under
// "verdict". Checks lists every check in evaluation order, each passed,
// failed or not_evaluated.
type MoveVerdict struct {
	Eligible     bool          `json:"eligible"`
	SourceBroker MoveBrokerRef `json:"sourceBroker"`
	TargetBroker MoveBrokerRef `json:"targetBroker"`
	Profile      string        `json:"profile,omitempty"`
	RuntimeType  string        `json:"runtimeType,omitempty"`
	Checks       []MoveCheck   `json:"checks"`
}

// MoveBrokerRef identifies a broker in a MoveVerdict.
type MoveBrokerRef struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// MoveCheck is one move eligibility check result.
type MoveCheck struct {
	Name    string `json:"name"`
	Result  string `json:"result"`
	Message string `json:"message,omitempty"`
}

// FieldChange describes an old→new change to a single scalar field on the
// reincarnation plan.
type FieldChange struct {
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
}

// KeyDiff describes an old→new change to a set of map keys (e.g. env var
// names), by name only — never by value.
type KeyDiff struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Changed []string `json:"changed,omitempty"`
}

// ReincarnationPlan is the old→new diff returned by both a dry run and a real
// reincarnate request.
type ReincarnationPlan struct {
	Template   FieldChange `json:"template"`
	Image      FieldChange `json:"image"`
	HarnessCfg FieldChange `json:"harnessConfig"`
	Model      FieldChange `json:"model"`
	EnvKeys    KeyDiff     `json:"envKeys"`
	Branch     string      `json:"branch"`
	Warnings   []string    `json:"warnings,omitempty"`

	// Patched lists the patch fields the hub applied, in display order.
	Patched []string `json:"patched,omitempty"`
	// Old and new values of patch fields not otherwise on the plan, set
	// only when patched.
	Role           *FieldChange `json:"role,omitempty"`
	ServiceAccount *FieldChange `json:"serviceAccount,omitempty"`
	ThinkingLevel  *FieldChange `json:"thinkingLevel,omitempty"`
	HarnessAuth    *FieldChange `json:"harnessAuth,omitempty"`
	// SharedDirBackends and AllowEmptySharedDir echo the request's shared
	// dir backend change.
	SharedDirBackends   map[string]string `json:"sharedDirBackends,omitempty"`
	AllowEmptySharedDir bool              `json:"allowEmptySharedDir,omitempty"`
}
