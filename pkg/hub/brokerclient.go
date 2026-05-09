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

// Package hub provides the Scion Hub API server.
package hub

import (
	"context"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AuthenticatedBrokerClient is an HTTP-based RuntimeBrokerClient that signs
// outgoing requests with HMAC authentication. This allows the Hub to make
// authenticated requests to Runtime Brokers.
type AuthenticatedBrokerClient struct {
	transport *brokerHTTPTransport
}

// NewAuthenticatedBrokerClient creates a new authenticated broker client.
func NewAuthenticatedBrokerClient(s store.Store, debug bool) *AuthenticatedBrokerClient {
	return &AuthenticatedBrokerClient{
		transport: newBrokerHTTPTransport(debug, &hmacBrokerSigner{store: s}),
	}
}

// CreateAgent creates an agent on a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) CreateAgent(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	return c.transport.CreateAgent(ctx, brokerID, brokerEndpoint, req)
}

// StartAgent starts an agent on a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace bool) (*RemoteAgentResponse, error) {
	return c.transport.StartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, resolvedEnv, resolvedSecrets, inlineConfig, sharedDirs, sharedWorkspace)
}

// StopAgent stops an agent on a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) error {
	return c.transport.StopAgent(ctx, brokerID, brokerEndpoint, agentID, projectID)
}

// RestartAgent restarts an agent on a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string) error {
	return c.transport.RestartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, resolvedEnv)
}

// DeleteAgent deletes an agent from a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	return c.transport.DeleteAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, deleteFiles, removeBranch, softDelete, deletedAt)
}

// MessageAgent sends a message to an agent on a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	return c.transport.MessageAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, message, interrupt, structuredMsg)
}

// CheckAgentPrompt checks if an agent has a non-empty prompt.md file on a remote runtime broker.
func (c *AuthenticatedBrokerClient) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) (bool, error) {
	return c.transport.CheckAgentPrompt(ctx, brokerID, brokerEndpoint, agentID, projectID)
}

// CreateAgentWithGather creates an agent and handles 202 env-gather responses.
func (c *AuthenticatedBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	return c.transport.CreateAgentWithGather(ctx, brokerID, brokerEndpoint, req)
}

// GetAgentLogs retrieves agent.log content from a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) GetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, tail int) (string, error) {
	return c.transport.GetAgentLogs(ctx, brokerID, brokerEndpoint, agentID, projectID, tail)
}

// ExecAgent executes a command in an agent on a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) ExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, command []string, timeout int) (string, int, error) {
	return c.transport.ExecAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, command, timeout)
}

// CleanupProject asks a broker to remove its local hub-native project directory with HMAC authentication.
func (c *AuthenticatedBrokerClient) CleanupProject(ctx context.Context, brokerID, brokerEndpoint, projectSlug string) error {
	return c.transport.CleanupProject(ctx, brokerID, brokerEndpoint, projectSlug)
}

// FinalizeEnv sends gathered env vars to a broker to complete agent creation.
func (c *AuthenticatedBrokerClient) FinalizeEnv(ctx context.Context, brokerID, brokerEndpoint, agentID string, env map[string]string) (*RemoteAgentResponse, error) {
	return c.transport.FinalizeEnv(ctx, brokerID, brokerEndpoint, agentID, env)
}
