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

	// Start starts a stopped agent.
	Start(ctx context.Context, agentID string) error

	// Stop stops a running agent.
	Stop(ctx context.Context, agentID string) error

	// Suspend pauses a running agent, preserving state for later resume.
	Suspend(ctx context.Context, agentID string) error

	// Restart restarts an agent.
	Restart(ctx context.Context, agentID string) error

	// StopAll stops all running agents in scope.
	StopAll(ctx context.Context) (*StopAllResponse, error)

	// SendMessage sends a plain text message to an agent (legacy).
	SendMessage(ctx context.Context, agentID string, message string, interrupt bool) error

	// SendStructuredMessage sends a structured message to an agent.
	// If notify is true, the sender subscribes to status notifications for the target agent.
	SendStructuredMessage(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt bool, notify bool) error

	// BroadcastMessage broadcasts a structured message to all running agents in the project.
	// Uses the Hub's broadcast endpoint which routes through the message broker (if available)
	// or performs direct fan-out as a fallback.
	BroadcastMessage(ctx context.Context, msg *messages.StructuredMessage, interrupt bool) error

	// SubmitEnv submits gathered environment variables for an agent after a 202 env-gather response.
	SubmitEnv(ctx context.Context, agentID string, req *SubmitEnvRequest) (*CreateAgentResponse, error)

	// Restore restores a soft-deleted agent.
	Restore(ctx context.Context, agentID string) (*Agent, error)

	// Exec executes a command in an agent container.
	Exec(ctx context.Context, agentID string, command []string, timeout int) (*ExecResponse, error)

	// GetLogs retrieves agent logs.
	GetLogs(ctx context.Context, agentID string, opts *GetLogsOptions) (string, error)

	// SendOutboundMessage sends a message from an agent to a human inbox.
	// Used when agents need to communicate with users (e.g., asking questions).
	SendOutboundMessage(ctx context.Context, agentID string, msg *OutboundMessageRequest) error

	// GetCloudLogs retrieves structured log entries from Cloud Logging.
	GetCloudLogs(ctx context.Context, agentID string, opts *GetCloudLogsOptions) (*CloudLogsResponse, error)

	// StreamCloudLogs opens an SSE connection for streaming log entries.
	// The handler is called for each log entry received. Blocks until the
	// context is cancelled or the server closes the connection.
	StreamCloudLogs(ctx context.Context, agentID string, opts *GetCloudLogsOptions, handler func(CloudLogEntry)) error
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
	Page            apiclient.PageOptions
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
	Stopped int             `json:"stopped"`
	Failed  int             `json:"failed"`
	Total   int             `json:"total"`
	Results []StopAllResult `json:"results"`
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
}

// MarshalJSON implements custom marshaling to support legacy groveId field.
func (r CreateAgentRequest) MarshalJSON() ([]byte, error) {
	type Alias CreateAgentRequest
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId,omitempty"`
	}{
		Alias:   Alias(r),
		GroveID: r.ProjectID,
	})
}

// UnmarshalJSON implements custom unmarshaling to support legacy groveId field.
func (r *CreateAgentRequest) UnmarshalJSON(data []byte) error {
	type Alias CreateAgentRequest
	aux := &struct {
		GroveID string `json:"groveId"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if r.ProjectID == "" && aux.GroveID != "" {
		r.ProjectID = aux.GroveID
	}
	return nil
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
			query.Set("groveId", opts.ProjectID)
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

	resp, err := s.c.delete(ctx, path, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Start starts a stopped agent.
func (s *agentService) Start(ctx context.Context, agentID string) error {
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/start", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Stop stops a running agent.
func (s *agentService) Stop(ctx context.Context, agentID string) error {
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/stop", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Suspend pauses a running agent, preserving state for later resume.
func (s *agentService) Suspend(ctx context.Context, agentID string) error {
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/suspend", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Restart restarts an agent.
func (s *agentService) Restart(ctx context.Context, agentID string) error {
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/restart", nil, nil)
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

// SendStructuredMessage sends a structured message to an agent.
// If notify is true, the sender subscribes to status notifications for the target agent.
func (s *agentService) SendStructuredMessage(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt bool, notify bool) error {
	body := struct {
		StructuredMessage *messages.StructuredMessage `json:"structured_message"`
		Interrupt         bool                        `json:"interrupt,omitempty"`
		Notify            bool                        `json:"notify,omitempty"`
	}{
		StructuredMessage: msg,
		Interrupt:         interrupt,
		Notify:            notify,
	}
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/message", body, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// OutboundMessageRequest is the request body for sending an agent-to-human outbound message.
type OutboundMessageRequest struct {
	Recipient   string `json:"recipient,omitempty"`
	RecipientID string `json:"recipient_id,omitempty"`
	Msg         string `json:"msg"`
	Type        string `json:"type,omitempty"`
	Urgent      bool   `json:"urgent,omitempty"`
}

// SendOutboundMessage sends a message from an agent to a human inbox.
func (s *agentService) SendOutboundMessage(ctx context.Context, agentID string, msg *OutboundMessageRequest) error {
	resp, err := s.c.post(ctx, s.agentPath(agentID)+"/outbound-message", msg, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// BroadcastMessage broadcasts a structured message to all running agents in the project.
func (s *agentService) BroadcastMessage(ctx context.Context, msg *messages.StructuredMessage, interrupt bool) error {
	if s.projectID == "" {
		return fmt.Errorf("broadcast requires a project-scoped agent service")
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
		return err
	}
	return apiclient.CheckResponse(resp)
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
	defer resp.Body.Close()

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
