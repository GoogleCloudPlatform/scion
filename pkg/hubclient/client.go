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
	"net/url"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Client is the interface for the Hub API client.
// This interface enables mocking for tests.
type Client interface {
	// Agents returns the agent operations interface.
	Agents() AgentService

	// ProjectAgents returns the agent operations interface scoped to a specific project.
	ProjectAgents(projectID string) AgentService

	// Projects returns the project operations interface.
	Projects() ProjectService

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

	// ScheduledEvents returns the scheduled event operations interface scoped to a project.
	ScheduledEvents(projectID string) ScheduledEventService

	// Schedules returns the recurring schedule operations interface scoped to a project.
	Schedules(projectID string) ScheduleService

	// GCPServiceAccounts returns the GCP service account operations interface scoped to a project.
	GCPServiceAccounts(projectID string) GCPServiceAccountService

	// Messages returns the user message inbox operations interface.
	Messages() MessageService

	// AllowList returns the allow list management operations interface.
	AllowList() AllowListService

	// Invites returns the invite code management operations interface.
	Invites() InviteService

	// Health checks API availability.
	Health(ctx context.Context) (*HealthResponse, error)
}

// client is the concrete implementation of Client.
type client struct {
	transport *apiclient.Transport

	agents                *agentService
	projects              *projectService
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
	allowList             *allowListService
	invites               *inviteService
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
	c.projects = &projectService{c: c}
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
	c.allowList = &allowListService{c: c}
	c.invites = &inviteService{c: c}

	return c, nil
}

// Agents returns the agent operations interface.
func (c *client) Agents() AgentService {
	return c.agents
}

// ProjectAgents returns the agent operations interface scoped to a specific project.
func (c *client) ProjectAgents(projectID string) AgentService {
	return &agentService{c: c, projectID: projectID}
}

// Projects returns the project operations interface.
func (c *client) Projects() ProjectService {
	return c.projects
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

// ScheduledEvents returns the scheduled event operations interface scoped to a project.
func (c *client) ScheduledEvents(projectID string) ScheduledEventService {
	return &scheduledEventService{c: c, projectID: projectID}
}

// Schedules returns the recurring schedule operations interface scoped to a project.
func (c *client) Schedules(projectID string) ScheduleService {
	return &scheduleService{c: c, projectID: projectID}
}

// GCPServiceAccounts returns the GCP service account operations interface scoped to a project.
func (c *client) GCPServiceAccounts(projectID string) GCPServiceAccountService {
	return &gcpServiceAccountService{c: c, projectID: projectID}
}

// Messages returns the user message inbox operations interface.
func (c *client) Messages() MessageService {
	return c.messages
}

// AllowList returns the allow list management operations interface.
func (c *client) AllowList() AllowListService {
	return c.allowList
}

// Invites returns the invite code management operations interface.
func (c *client) Invites() InviteService {
	return c.invites
}

// get performs an HTTP GET request with fallback.
func (c *client) get(ctx context.Context, path string, headers http.Header) (*http.Response, error) {
	return c.getWithQuery(ctx, path, nil, headers)
}

// getWithQuery performs an HTTP GET request with query parameters and fallback.
func (c *client) getWithQuery(ctx context.Context, path string, query url.Values, headers http.Header) (*http.Response, error) {
	resp, err := c.transport.GetWithQuery(ctx, path, query, headers)
	if err == nil && resp.StatusCode == http.StatusNotFound && strings.Contains(path, "/projects") {
		legacyPath := strings.Replace(path, "/projects", "/groves", 1)
		resp.Body.Close()
		resp, err = c.transport.GetWithQuery(ctx, legacyPath, query, headers)
		c.checkForDeprecation(resp)
		return resp, err
	}
	return resp, err
}

// post performs an HTTP POST request with fallback.
func (c *client) post(ctx context.Context, path string, body interface{}, headers http.Header) (*http.Response, error) {
	resp, err := c.transport.Post(ctx, path, body, headers)
	if err == nil && resp.StatusCode == http.StatusNotFound && strings.Contains(path, "/projects") {
		legacyPath := strings.Replace(path, "/projects", "/groves", 1)
		resp.Body.Close()
		resp, err = c.transport.Post(ctx, legacyPath, body, headers)
		c.checkForDeprecation(resp)
		return resp, err
	}
	return resp, err
}

// put performs an HTTP PUT request with fallback.
func (c *client) put(ctx context.Context, path string, body interface{}, headers http.Header) (*http.Response, error) {
	resp, err := c.transport.Put(ctx, path, body, headers)
	if err == nil && resp.StatusCode == http.StatusNotFound && strings.Contains(path, "/projects") {
		legacyPath := strings.Replace(path, "/projects", "/groves", 1)
		resp.Body.Close()
		resp, err = c.transport.Put(ctx, legacyPath, body, headers)
		c.checkForDeprecation(resp)
		return resp, err
	}
	return resp, err
}

// patch performs an HTTP PATCH request with fallback.
func (c *client) patch(ctx context.Context, path string, body interface{}, headers http.Header) (*http.Response, error) {
	resp, err := c.transport.Patch(ctx, path, body, headers)
	if err == nil && resp.StatusCode == http.StatusNotFound && strings.Contains(path, "/projects") {
		legacyPath := strings.Replace(path, "/projects", "/groves", 1)
		resp.Body.Close()
		resp, err = c.transport.Patch(ctx, legacyPath, body, headers)
		c.checkForDeprecation(resp)
		return resp, err
	}
	return resp, err
}

// delete performs an HTTP DELETE request with fallback.
func (c *client) delete(ctx context.Context, path string, headers http.Header) (*http.Response, error) {
	resp, err := c.transport.Delete(ctx, path, headers)
	if err == nil && resp.StatusCode == http.StatusNotFound && strings.Contains(path, "/projects") {
		legacyPath := strings.Replace(path, "/projects", "/groves", 1)
		resp.Body.Close()
		resp, err = c.transport.Delete(ctx, legacyPath, headers)
		c.checkForDeprecation(resp)
		return resp, err
	}
	return resp, err
}

// checkForDeprecation logs a warning if the response contains a Deprecation header.
func (c *client) checkForDeprecation(resp *http.Response) {
	if resp != nil && resp.Header.Get("Deprecation") == "true" {
		path := ""
		if resp.Request != nil {
			path = resp.Request.URL.Path
		}
		util.Debugf("WARNING: Calling deprecated endpoint %s. Please update to /projects version.", path)
	}
}

// Health checks API availability.
func (c *client) Health(ctx context.Context) (*HealthResponse, error) {
	resp, err := c.get(ctx, "/healthz", nil)
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
