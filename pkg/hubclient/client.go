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

// Package hubclient provides a Go client for the Scion Hub API.
package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Client is the interface for the Hub API client.
// This interface enables mocking for tests.
type Client interface {
	// Agents returns the agent operations interface.
	Agents() AgentService

	// GroveAgents returns the agent operations interface scoped to a specific grove.
	GroveAgents(groveID string) AgentService

	// Groves returns the grove operations interface.
	Groves() GroveService

	// RuntimeBrokers returns the runtime broker operations interface.
	RuntimeBrokers() RuntimeBrokerService

	// Templates returns the template operations interface.
	Templates() TemplateService

	// HarnessConfigs returns the harness config operations interface.
	HarnessConfigs() HarnessConfigService

	// Workspace returns the workspace sync operations interface.
	Workspace() WorkspaceService

	// Users returns the user operations interface.
	Users() UserService

	// Env returns the environment variable operations interface.
	Env() EnvService

	// Secrets returns the secret operations interface.
	Secrets() SecretService

	// Auth returns the authentication operations interface.
	Auth() AuthService

	// Notifications returns the notification operations interface.
	Notifications() NotificationService

	// Tokens returns the user access token operations interface.
	Tokens() TokenService

	// Subscriptions returns the notification subscription operations interface.
	Subscriptions() SubscriptionService

	// SubscriptionTemplates returns the subscription template operations interface.
	SubscriptionTemplates() SubscriptionTemplateService

	// ScheduledEvents returns the scheduled event operations interface scoped to a grove.
	ScheduledEvents(groveID string) ScheduledEventService

	// Schedules returns the recurring schedule operations interface scoped to a grove.
	Schedules(groveID string) ScheduleService

	// GCPServiceAccounts returns the GCP service account operations interface scoped to a grove.
	GCPServiceAccounts(groveID string) GCPServiceAccountService

	// Messages returns the user message inbox operations interface.
	Messages() MessageService

	// Health checks API availability.
	Health(ctx context.Context) (*HealthResponse, error)

	// CreateWorkflowRun creates a new workflow run.
	CreateWorkflowRun(ctx context.Context, req *CreateWorkflowRunRequest) (*WorkflowRun, error)

	// ListWorkflowRuns returns workflow runs in a grove.
	// Returns (items, nextCursor, error).
	ListWorkflowRuns(ctx context.Context, groveID string, opts *ListWorkflowRunsOptions) ([]WorkflowRun, string, error)

	// GetWorkflowRun retrieves a workflow run by ID.
	// Pass include fields to expand heavy payload (source, inputs, result).
	GetWorkflowRun(ctx context.Context, runID string, include ...string) (*WorkflowRunDetail, error)

	// CancelWorkflowRun requests cancellation of a workflow run.
	CancelWorkflowRun(ctx context.Context, runID string) (*WorkflowRun, error)

	// StreamWorkflowRunLogs opens a WebSocket to the run's log stream.
	// The returned channel is closed when the server closes the connection.
	StreamWorkflowRunLogs(ctx context.Context, runID string) (<-chan LogEvent, error)
}

// client is the concrete implementation of Client.
type client struct {
	transport *apiclient.Transport

	agents                *agentService
	groves                *groveService
	runtimeBrokers        *runtimeBrokerService
	templates             *templateService
	harnessConfigs        *harnessConfigService
	workspace             *workspaceService
	users                 *userService
	env                   *envService
	secrets               *secretService
	authService           *authService
	tokens                *tokenService
	notifications         *notificationService
	subscriptions         *subscriptionService
	subscriptionTemplates *subscriptionTemplateService
	messages              *messageService
}

// New creates a new Hub API client.
func New(baseURL string, opts ...Option) (Client, error) {
	c := &client{
		transport: apiclient.NewTransport(baseURL),
	}

	for _, opt := range opts {
		opt(c)
	}

	// Initialize service implementations
	c.agents = &agentService{c: c}
	c.groves = &groveService{c: c}
	c.runtimeBrokers = &runtimeBrokerService{c: c}
	c.templates = &templateService{c: c}
	c.harnessConfigs = &harnessConfigService{c: c}
	c.workspace = &workspaceService{c: c}
	c.users = &userService{c: c}
	c.env = &envService{c: c}
	c.secrets = &secretService{c: c}
	c.authService = &authService{c: c}
	c.tokens = &tokenService{c: c}
	c.notifications = &notificationService{c: c}
	c.subscriptions = &subscriptionService{c: c}
	c.subscriptionTemplates = &subscriptionTemplateService{c: c}
	c.messages = &messageService{c: c}

	return c, nil
}

// Agents returns the agent operations interface.
func (c *client) Agents() AgentService {
	return c.agents
}

// GroveAgents returns the agent operations interface scoped to a specific grove.
func (c *client) GroveAgents(groveID string) AgentService {
	return &agentService{c: c, groveID: groveID}
}

// Groves returns the grove operations interface.
func (c *client) Groves() GroveService {
	return c.groves
}

// RuntimeBrokers returns the runtime broker operations interface.
func (c *client) RuntimeBrokers() RuntimeBrokerService {
	return c.runtimeBrokers
}

// Templates returns the template operations interface.
func (c *client) Templates() TemplateService {
	return c.templates
}

// HarnessConfigs returns the harness config operations interface.
func (c *client) HarnessConfigs() HarnessConfigService {
	return c.harnessConfigs
}

// Workspace returns the workspace sync operations interface.
func (c *client) Workspace() WorkspaceService {
	return c.workspace
}

// Users returns the user operations interface.
func (c *client) Users() UserService {
	return c.users
}

// Env returns the environment variable operations interface.
func (c *client) Env() EnvService {
	return c.env
}

// Secrets returns the secret operations interface.
func (c *client) Secrets() SecretService {
	return c.secrets
}

// Auth returns the authentication operations interface.
func (c *client) Auth() AuthService {
	return c.authService
}

// Tokens returns the user access token operations interface.
func (c *client) Tokens() TokenService {
	return c.tokens
}

// Notifications returns the notification operations interface.
func (c *client) Notifications() NotificationService {
	return c.notifications
}

// Subscriptions returns the notification subscription operations interface.
func (c *client) Subscriptions() SubscriptionService {
	return c.subscriptions
}

// SubscriptionTemplates returns the subscription template operations interface.
func (c *client) SubscriptionTemplates() SubscriptionTemplateService {
	return c.subscriptionTemplates
}

// ScheduledEvents returns the scheduled event operations interface scoped to a grove.
func (c *client) ScheduledEvents(groveID string) ScheduledEventService {
	return &scheduledEventService{c: c, groveID: groveID}
}

// Schedules returns the recurring schedule operations interface scoped to a grove.
func (c *client) Schedules(groveID string) ScheduleService {
	return &scheduleService{c: c, groveID: groveID}
}

// GCPServiceAccounts returns the GCP service account operations interface scoped to a grove.
func (c *client) GCPServiceAccounts(groveID string) GCPServiceAccountService {
	return &gcpServiceAccountService{c: c, groveID: groveID}
}

// Messages returns the user message inbox operations interface.
func (c *client) Messages() MessageService {
	return c.messages
}

// Health checks API availability.
func (c *client) Health(ctx context.Context) (*HealthResponse, error) {
	resp, err := c.transport.Get(ctx, "/healthz", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[HealthResponse](resp)
}

// HealthResponse is the response from health check.
type HealthResponse struct {
	Status       string            `json:"status"`
	Version      string            `json:"version"`
	ScionVersion string            `json:"scionVersion"`
	Uptime       string            `json:"uptime"`
	Checks       map[string]string `json:"checks,omitempty"`

	// Composite sub-component health (present in combo mode).
	Web    json.RawMessage `json:"web,omitempty"`
	Hub    json.RawMessage `json:"hub,omitempty"`
	Broker json.RawMessage `json:"broker,omitempty"`
}

// Option configures a Hub client.
type Option func(*client)

// WithBearerToken sets Bearer token authentication.
func WithBearerToken(token string) Option {
	return func(c *client) {
		c.transport.Auth = &apiclient.BearerAuth{Token: token}
	}
}

// WithAuthenticator sets a custom authenticator.
func WithAuthenticator(auth apiclient.Authenticator) Option {
	return func(c *client) {
		c.transport.Auth = auth
	}
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *client) {
		c.transport.HTTPClient = hc
	}
}

// WithTimeout sets the request timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *client) {
		c.transport.HTTPClient.Timeout = d
	}
}

// WithRetry configures retry behavior.
func WithRetry(maxRetries int, wait time.Duration) Option {
	return func(c *client) {
		c.transport.MaxRetries = maxRetries
		c.transport.RetryWait = wait
	}
}

// WithDevToken sets a development token for authentication.
// This is equivalent to WithBearerToken but makes the intent clearer.
func WithDevToken(token string) Option {
	return func(c *client) {
		c.transport.Auth = &apiclient.BearerAuth{Token: token}
	}
}

// WithAutoDevAuth attempts to load a development token automatically.
// It checks in order:
// 1. SCION_DEV_TOKEN environment variable
// 2. SCION_DEV_TOKEN_FILE environment variable (path to token file)
// 3. Default token file (~/.scion/dev-token)
// If no token is found, authentication is not configured.
func WithAutoDevAuth() Option {
	return func(c *client) {
		token, source := apiclient.ResolveDevTokenWithSource()
		if token != "" {
			c.transport.Auth = &apiclient.BearerAuth{Token: token}
			if util.DebugEnabled() {
				// Truncate token for display
				displayToken := token
				if len(displayToken) > 20 {
					displayToken = displayToken[:20] + "..."
				}
				util.Debugf("Dev auth token: %s (source: %s)", displayToken, source)
			}
		} else {
			util.Debugf("No dev auth token found")
		}
	}
}

// WithAgentToken sets agent token authentication using the X-Scion-Agent-Token header.
// Use this when authenticating as an agent (not a user) to the Hub API.
func WithAgentToken(token string) Option {
	return func(c *client) {
		c.transport.Auth = &apiclient.AgentTokenAuth{Token: token}
	}
}

// WithHMACAuth sets HMAC-based broker authentication.
// This is used by Runtime Brokers to authenticate with the Hub using
// the shared secret established during the join process.
func WithHMACAuth(brokerID string, secretKey []byte) Option {
	return func(c *client) {
		c.transport.Auth = &apiclient.HMACAuth{
			BrokerID:  brokerID,
			SecretKey: secretKey,
		}
	}
}

// AgentTokenEnvVar is the canonical environment variable that agent containers
// use to carry their Hub JWT. Set by the runtime broker when provisioning an
// agent container (alongside SCION_AUTH_TOKEN for backwards compatibility).
const AgentTokenEnvVar = "SCION_AGENT_TOKEN"

// WithAutoAgentAuth attempts to load an agent JWT automatically from the
// SCION_AGENT_TOKEN environment variable. When set, the client sends the
// JWT via the X-Scion-Agent-Token header (agent auth path). If the variable
// is unset the option is a no-op, allowing the caller to chain additional
// auth options as fallbacks.
func WithAutoAgentAuth() Option {
	return func(c *client) {
		token := os.Getenv(AgentTokenEnvVar)
		if token != "" {
			c.transport.Auth = &apiclient.AgentTokenAuth{Token: token}
			if util.DebugEnabled() {
				util.Debugf("Agent auth token loaded from %s", AgentTokenEnvVar)
			}
		}
	}
}
