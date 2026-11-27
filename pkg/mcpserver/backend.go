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

package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/google/uuid"
)

// HubBackend is the narrow interface over the Hub REST API used by MCP tools
// and front door. One implementation serves both embedded and standalone modes;
// only the underlying http.RoundTripper differs.
type HubBackend interface {
	Me(ctx context.Context, c Credential) (Principal, error)
	SendToAgent(ctx context.Context, c Credential, project, agent, text string) (SendResult, error)
	Watch(ctx context.Context, c Credential, convID string, o hubclient.WatchOptions) (*hubclient.WatchResult, error)
	ExperimentEnabled(ctx context.Context, c Credential, name string) (bool, error)
}

// ExperimentChecker resolves whether a named server experiment is enabled.
// In embedded mode this wraps hubSrv.ExperimentEnabled directly; in standalone
// mode (P4) it queries GET /api/v1/experiments with caching.
type ExperimentChecker func(ctx context.Context, c Credential, name string) (bool, error)

// BackendOptions configures NewBackend.
type BackendOptions struct {
	// HubBaseURL is the base URL passed to hubclient.New (e.g., "http://in-process"
	// in embedded mode or the remote hub URL in standalone mode).
	HubBaseURL string
	// Transport is the base http.RoundTripper (InProcessTransport for embedded
	// mode, network RoundTripper for standalone mode).
	Transport http.RoundTripper
	// Version is the server version stamped into User-Agent: scion-mcp/<ver>.
	Version string
	// ExperimentChecker resolves hub experiments per request.
	ExperimentChecker ExperimentChecker
	// DisableTransportAuth suppresses hubclient's automatic SCION_TRANSPORT_MODE
	// wrapping. Set true for embedded mode; P4 leaves it false for standalone.
	DisableTransportAuth bool
}

type hubBackend struct {
	baseURL              string
	transport            http.RoundTripper
	expCheck             ExperimentChecker
	disableTransportAuth bool
}

// NewBackend creates a HubBackend backed by hubclient and the given transport.
// The transport is always wrapped in AllowlistTransport so only allowlisted
// headers can ever reach the Hub.
func NewBackend(opts BackendOptions) HubBackend {
	baseURL := opts.HubBaseURL
	if baseURL == "" {
		baseURL = "http://in-process"
	}
	rt := &AllowlistTransport{
		Base:    opts.Transport,
		Version: opts.Version,
	}
	return &hubBackend{
		baseURL:              baseURL,
		transport:            rt,
		expCheck:             opts.ExperimentChecker,
		disableTransportAuth: opts.DisableTransportAuth,
	}
}

func (b *hubBackend) clientFor(c Credential) (hubclient.Client, error) {
	hc := &http.Client{
		Transport: b.transport,
	}
	opts := []hubclient.Option{
		hubclient.WithHTTPClient(hc),
		hubclient.WithAuthenticator(&credentialAuthenticator{cred: c}),
	}
	if b.disableTransportAuth {
		opts = append(opts, hubclient.WithTransportAuth(nil, 0))
	}
	return hubclient.New(b.baseURL, opts...)
}

func (b *hubBackend) Me(ctx context.Context, c Credential) (Principal, error) {
	cl, err := b.clientFor(c)
	if err != nil {
		return Principal{}, err
	}
	u, err := cl.Auth().Me(ctx)
	if err != nil {
		return Principal{}, err
	}
	// /api/v1/auth/me does not expose an identity kind field, so leave Kind
	// empty rather than hard-coding "user" (which would mislabel visitors).
	return Principal{
		ID:      u.ID,
		Email:   u.Email,
		Display: u.DisplayName,
	}, nil
}

func (b *hubBackend) SendToAgent(ctx context.Context, c Credential, project, agent, text string) (SendResult, error) {
	cl, err := b.clientFor(c)
	if err != nil {
		return SendResult{}, err
	}

	projectID, err := resolveProjectID(ctx, cl, project)
	if err != nil {
		return SendResult{}, err
	}

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Msg:     text,
		Type:    messages.TypeInstruction,
	}
	resp, err := cl.ProjectAgents(projectID).SendStructuredMessageWithOptions(ctx, agent, msg, hubclient.SendMessageOptions{})
	if err != nil {
		return SendResult{}, err
	}

	res := SendResult{
		MessageID:      resp.MessageID,
		ConversationID: resp.ConversationID,
		Status:         resp.Status,
		Deferred:       resp.Deferred,
		Cursor:         resp.Cursor,
	}
	if resp.Cursor == "" {
		// Hub already accepted/delivered the message (2xx), so never return a
		// tool error here (which would prompt an LLM caller to resend and
		// duplicate the side effect).
		res.Warning = "delivered but not recorded in conversation history; replies cannot be read via this conversation; do not resend"
	}

	return res, nil
}

// resolveProjectID resolves a project slug to its UUID via the Hub API using
// the caller's own credential. If project is already a valid UUID it is
// returned directly. If slug lookup fails or finds no match (for example when
// a project-scoped UAT lacks project:list), a clear error is returned.
func resolveProjectID(ctx context.Context, cl hubclient.Client, project string) (string, error) {
	if _, err := uuid.Parse(project); err == nil {
		return project, nil
	}
	list, err := cl.Projects().List(ctx, &hubclient.ListProjectsOptions{Slug: project})
	if err == nil && list != nil && len(list.Projects) == 1 && list.Projects[0].ID != "" {
		return list.Projects[0].ID, nil
	}
	return "", fmt.Errorf("cannot resolve project slug %q: pass the project ID (UUID) instead (slug lookup requires the project:list scope)", project)
}

func (b *hubBackend) Watch(ctx context.Context, c Credential, convID string, o hubclient.WatchOptions) (*hubclient.WatchResult, error) {
	cl, err := b.clientFor(c)
	if err != nil {
		return nil, err
	}
	return cl.Conversations().Watch(ctx, convID, o)
}

func (b *hubBackend) ExperimentEnabled(ctx context.Context, c Credential, name string) (bool, error) {
	if b.expCheck == nil {
		return false, nil
	}
	return b.expCheck(ctx, c, name)
}

// credentialAuthenticator applies a Credential to outbound Hub requests.
type credentialAuthenticator struct {
	cred Credential
}

func (a *credentialAuthenticator) ApplyAuth(req *http.Request) error {
	if req == nil {
		return errors.New("mcpserver: nil request")
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if a.cred.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+a.cred.Bearer)
		return nil
	}
	if a.cred.Federation != "" {
		req.Header.Set(HeaderFederationToken, a.cred.Federation)
		return nil
	}
	return nil
}

func (a *credentialAuthenticator) Refresh() (bool, error) { return false, nil }

// principalCache caches GET /api/v1/auth/me results keyed by sha256(credential)
// for at most ttl (default 60s) to feed audit logs. It never authorizes a Hub
// call: every tool/event request re-presents the credential to the Hub.
type principalCache struct {
	mu      sync.RWMutex
	ttl     time.Duration
	entries map[string]principalCacheEntry
}

type principalCacheEntry struct {
	principal Principal
	expiresAt time.Time
}

func newPrincipalCache(ttl time.Duration) *principalCache {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &principalCache{
		ttl:     ttl,
		entries: make(map[string]principalCacheEntry),
	}
}

func (c *principalCache) get(key string, now time.Time) (Principal, bool) {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || now.After(e.expiresAt) {
		return Principal{}, false
	}
	return e.principal, true
}

func (c *principalCache) set(key string, p Principal, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Bound memory for long-lived servers by pruning expired entries opportunistically.
	if len(c.entries) >= 1024 {
		for k, v := range c.entries {
			if now.After(v.expiresAt) {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = principalCacheEntry{
		principal: p,
		expiresAt: now.Add(c.ttl),
	}
}

// hubStatusCode extracts the HTTP status code from a Hub error if present.
func hubStatusCode(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// formatHubToolError formats a Hub error for an MCP tool error result,
// preserving the Hub's error code and message verbatim (e.g. missing UAT
// selector or 403 reason).
func formatHubToolError(err error) string {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Code != "" {
			return fmt.Sprintf("hub error (%d %s): %s", apiErr.StatusCode, apiErr.Code, apiErr.Message)
		}
		return fmt.Sprintf("hub error (%d): %s", apiErr.StatusCode, apiErr.Message)
	}
	return err.Error()
}
