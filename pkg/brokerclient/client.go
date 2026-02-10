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

// Package brokerclient provides a Go client for the Scion Runtime Broker API.
package brokerclient

import (
	"context"
	"net/http"
	"time"

	"github.com/ptone/scion-agent/pkg/apiclient"
	"github.com/ptone/scion-agent/pkg/runtimebroker"
)

// Client is the interface for the Runtime Broker API client.
type Client interface {
	// Agents returns the agent operations interface.
	Agents() AgentService

	// Info returns broker information.
	Info(ctx context.Context) (*runtimebroker.BrokerInfoResponse, error)

	// Health checks broker availability.
	Health(ctx context.Context) (*runtimebroker.HealthResponse, error)
}

// client is the concrete implementation of Client.
type client struct {
	transport *apiclient.Transport
	agents    *agentService
}

// New creates a new Runtime Broker API client.
func New(baseURL string, opts ...Option) (Client, error) {
	c := &client{
		transport: apiclient.NewTransport(baseURL),
	}

	for _, opt := range opts {
		opt(c)
	}

	c.agents = &agentService{c: c}

	return c, nil
}

// Agents returns the agent operations interface.
func (c *client) Agents() AgentService {
	return c.agents
}

// Info returns broker information.
func (c *client) Info(ctx context.Context) (*runtimebroker.BrokerInfoResponse, error) {
	resp, err := c.transport.Get(ctx, "/api/v1/info", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[runtimebroker.BrokerInfoResponse](resp)
}

// Health checks broker availability.
func (c *client) Health(ctx context.Context) (*runtimebroker.HealthResponse, error) {
	resp, err := c.transport.Get(ctx, "/healthz", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[runtimebroker.HealthResponse](resp)
}

// Option configures a Runtime Broker client.
type Option func(*client)

// WithBearerToken sets Bearer token authentication.
func WithBearerToken(token string) Option {
	return func(c *client) {
		c.transport.Auth = &apiclient.BearerAuth{Token: token}
	}
}

// WithBrokerToken sets Runtime Broker token authentication.
func WithBrokerToken(token string) Option {
	return func(c *client) {
		c.transport.Auth = &apiclient.BrokerTokenAuth{Token: token}
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
		token := apiclient.ResolveDevToken()
		if token != "" {
			c.transport.Auth = &apiclient.BearerAuth{Token: token}
		}
	}
}
