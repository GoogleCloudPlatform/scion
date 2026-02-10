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

package brokerclient

import (
	"context"
	"fmt"
	"net/url"

	"github.com/ptone/scion-agent/pkg/apiclient"
	"github.com/ptone/scion-agent/pkg/runtimebroker"
)

// AgentService handles agent operations on a runtime broker.
type AgentService interface {
	// List returns agents on this broker.
	List(ctx context.Context, opts *ListAgentsOptions) (*runtimebroker.ListAgentsResponse, error)

	// Get returns a single agent by ID.
	Get(ctx context.Context, agentID string) (*runtimebroker.AgentResponse, error)

	// Create creates and starts a new agent.
	Create(ctx context.Context, req *runtimebroker.CreateAgentRequest) (*runtimebroker.CreateAgentResponse, error)

	// Start starts a stopped agent.
	Start(ctx context.Context, agentID string) error

	// Stop stops a running agent.
	Stop(ctx context.Context, agentID string, timeout int) error

	// Restart restarts an agent.
	Restart(ctx context.Context, agentID string) error

	// Delete removes an agent.
	Delete(ctx context.Context, agentID string, opts *DeleteAgentOptions) error

	// SendMessage sends a message to an agent.
	SendMessage(ctx context.Context, agentID string, message string, interrupt bool) error

	// Exec executes a command in an agent container.
	Exec(ctx context.Context, agentID string, command []string, timeout int) (*runtimebroker.ExecResponse, error)

	// GetLogs retrieves agent logs.
	GetLogs(ctx context.Context, agentID string, opts *GetLogsOptions) (string, error)

	// GetStats retrieves agent resource statistics.
	GetStats(ctx context.Context, agentID string) (*runtimebroker.StatsResponse, error)
}

// agentService is the implementation of AgentService.
type agentService struct {
	c *client
}

// ListAgentsOptions configures agent list filtering.
type ListAgentsOptions struct {
	GroveID string
	Status  string
	Page    apiclient.PageOptions
}

// DeleteAgentOptions configures agent deletion.
type DeleteAgentOptions struct {
	DeleteFiles  bool
	RemoveBranch bool
}

// GetLogsOptions configures log retrieval.
type GetLogsOptions struct {
	Follow     bool   // Stream logs
	Tail       int    // Lines from end
	Since      string // RFC3339 timestamp
	Timestamps bool   // Include timestamps
}

// List returns agents on this broker.
func (s *agentService) List(ctx context.Context, opts *ListAgentsOptions) (*runtimebroker.ListAgentsResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.GroveID != "" {
			query.Set("groveId", opts.GroveID)
		}
		if opts.Status != "" {
			query.Set("status", opts.Status)
		}
		opts.Page.ToQuery(query)
	}

	resp, err := s.c.transport.GetWithQuery(ctx, "/api/v1/agents", query, nil)
	if err != nil {
		return nil, err
	}

	return apiclient.DecodeResponse[runtimebroker.ListAgentsResponse](resp)
}

// Get returns a single agent by ID.
func (s *agentService) Get(ctx context.Context, agentID string) (*runtimebroker.AgentResponse, error) {
	resp, err := s.c.transport.Get(ctx, "/api/v1/agents/"+agentID, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[runtimebroker.AgentResponse](resp)
}

// Create creates and starts a new agent.
func (s *agentService) Create(ctx context.Context, req *runtimebroker.CreateAgentRequest) (*runtimebroker.CreateAgentResponse, error) {
	resp, err := s.c.transport.Post(ctx, "/api/v1/agents", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[runtimebroker.CreateAgentResponse](resp)
}

// Start starts a stopped agent.
func (s *agentService) Start(ctx context.Context, agentID string) error {
	resp, err := s.c.transport.Post(ctx, "/api/v1/agents/"+agentID+"/start", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Stop stops a running agent.
func (s *agentService) Stop(ctx context.Context, agentID string, timeout int) error {
	path := "/api/v1/agents/" + agentID + "/stop"
	if timeout > 0 {
		path += fmt.Sprintf("?timeout=%d", timeout)
	}
	resp, err := s.c.transport.Post(ctx, path, nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Restart restarts an agent.
func (s *agentService) Restart(ctx context.Context, agentID string) error {
	resp, err := s.c.transport.Post(ctx, "/api/v1/agents/"+agentID+"/restart", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Delete removes an agent.
func (s *agentService) Delete(ctx context.Context, agentID string, opts *DeleteAgentOptions) error {
	path := "/api/v1/agents/" + agentID
	if opts != nil {
		query := url.Values{}
		if opts.DeleteFiles {
			query.Set("deleteFiles", "true")
		}
		if opts.RemoveBranch {
			query.Set("removeBranch", "true")
		}
		if len(query) > 0 {
			path += "?" + query.Encode()
		}
	}

	resp, err := s.c.transport.Delete(ctx, path, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// SendMessage sends a message to an agent.
func (s *agentService) SendMessage(ctx context.Context, agentID string, message string, interrupt bool) error {
	body := &runtimebroker.MessageRequest{
		Message:   message,
		Interrupt: interrupt,
	}
	resp, err := s.c.transport.Post(ctx, "/api/v1/agents/"+agentID+"/message", body, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Exec executes a command in an agent container.
func (s *agentService) Exec(ctx context.Context, agentID string, command []string, timeout int) (*runtimebroker.ExecResponse, error) {
	body := &runtimebroker.ExecRequest{
		Command: command,
		Timeout: timeout,
	}
	resp, err := s.c.transport.Post(ctx, "/api/v1/agents/"+agentID+"/exec", body, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[runtimebroker.ExecResponse](resp)
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
		if opts.Timestamps {
			query.Set("timestamps", "true")
		}
	}

	resp, err := s.c.transport.GetWithQuery(ctx, "/api/v1/agents/"+agentID+"/logs", query, nil)
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

// GetStats retrieves agent resource statistics.
func (s *agentService) GetStats(ctx context.Context, agentID string) (*runtimebroker.StatsResponse, error) {
	resp, err := s.c.transport.Get(ctx, "/api/v1/agents/"+agentID+"/stats", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[runtimebroker.StatsResponse](resp)
}
