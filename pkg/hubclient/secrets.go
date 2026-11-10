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
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// SecretService handles secret operations.
// Note: Secret values are write-only and never returned by the API.
type SecretService interface {
	// List returns secret metadata for the specified scope.
	// Values are never returned.
	List(ctx context.Context, opts *ListSecretOptions) (*ListSecretResponse, error)

	// Get returns metadata for a specific secret by key.
	// Value is never returned.
	Get(ctx context.Context, key string, opts *SecretScopeOptions) (*Secret, error)

	// Set creates or updates a secret.
	Set(ctx context.Context, key string, req *SetSecretRequest) (*SetSecretResponse, error)

	// AgentSet creates or updates a secret using the agent-scoped endpoint.
	// This endpoint requires agent JWT auth and derives the project from the
	// agent's token. Use this instead of Set when running as an agent.
	AgentSet(ctx context.Context, agentID, key string, req *AgentSetSecretRequest) (*AgentSetSecretResponse, error)

	// UpdateMeta updates only the metadata of an existing secret (PATCH).
	// The secret value is not modified.
	UpdateMeta(ctx context.Context, key string, req *UpdateSecretMetaRequest) (*Secret, error)

	// Delete removes a secret.
	Delete(ctx context.Context, key string, opts *SecretScopeOptions) error

	// ListPending returns the caller's own pending secrets: user-scope
	// secrets written by an agent that wait for the caller's approval.
	// Metadata only; values are never returned.
	ListPending(ctx context.Context) (*ListPendingSecretsResponse, error)

	// ApprovePending approves the caller's pending secret id, writing it
	// as the user-scope secret the agent asked for.
	ApprovePending(ctx context.Context, id string, req *ApprovePendingSecretRequest) (*PendingSecretDecision, error)

	// RejectPending rejects the caller's pending secret id.
	RejectPending(ctx context.Context, id string) (*PendingSecretDecision, error)
}

// secretService is the implementation of SecretService.
type secretService struct {
	c *client
}

// ListSecretOptions configures secret listing.
type ListSecretOptions struct {
	Scope   string // user, project, runtime_broker (default: user)
	ScopeID string // ID of the scoped entity (required for project/runtime_broker)
	Type    string // Optional: filter by secret type (environment, variable, file)
}

// ListSecretResponse is the response from listing secrets.
type ListSecretResponse struct {
	Secrets []Secret `json:"secrets"` // Metadata only, no values
	Scope   string   `json:"scope"`
	ScopeID string   `json:"scopeId"`
}

// SecretScopeOptions specifies the scope for get/delete operations.
type SecretScopeOptions struct {
	Scope   string // user, project, runtime_broker (default: user)
	ScopeID string // ID of the scoped entity (required for project/runtime_broker)
}

// SetSecretRequest is the request for setting a secret.
type SetSecretRequest struct {
	Value         string `json:"value"`                   // Required: secret value (write-only)
	Encoding      string `json:"encoding,omitempty"`      // Value encoding: "raw" (plaintext) or "" (default: base64)
	Scope         string `json:"scope,omitempty"`         // Scope type (default: user)
	ScopeID       string `json:"scopeId,omitempty"`       // Required for project/runtime_broker scope
	Description   string `json:"description,omitempty"`   // Optional description
	InjectionMode string `json:"injectionMode,omitempty"` // "always" or "as_needed" (default: as_needed)
	Type          string `json:"type,omitempty"`          // Secret type: environment (default), variable, file
	Target        string `json:"target,omitempty"`        // Projection target (defaults to key)
	AllowProgeny  *bool  `json:"allowProgeny,omitempty"`  // Allow creator's progeny agents to access (user scope only)
}

// SetSecretResponse is the response from setting a secret.
type SetSecretResponse struct {
	Secret  *Secret `json:"secret"`  // Metadata only, no value
	Created bool    `json:"created"` // Whether this was a new secret
}

// AgentSetSecretRequest is the request for setting a secret via the agent-scoped endpoint.
// Scope defaults to "project" (derived from JWT); set to "user" for personal credentials.
type AgentSetSecretRequest struct {
	Value        string `json:"value"`                  // Required: base64-encoded secret value
	Type         string `json:"type,omitempty"`         // Secret type: environment (default), variable, file
	Target       string `json:"target,omitempty"`       // Projection target (defaults to key)
	Force        bool   `json:"force,omitempty"`        // Overwrite existing secret
	Scope        string `json:"scope,omitempty"`        // "project" (default) or "user"
	AllowProgeny *bool  `json:"allowProgeny,omitempty"` // Allow creator's progeny agents to access (user scope only)
}

// AgentSetSecretResponse is the response from the agent-scoped set endpoint.
type AgentSetSecretResponse struct {
	Key     string `json:"key"`
	Scope   string `json:"scope"`
	ScopeID string `json:"scopeId,omitempty"`
	Created bool   // Derived from HTTP status code (201 = created, 204 = updated)
}

// UpdateSecretMetaRequest is the request for metadata-only secret updates (PATCH).
// Only non-null/non-empty fields are applied. The secret value is never modified.
type UpdateSecretMetaRequest struct {
	Description   *string `json:"description,omitempty"`   // null = no change, "" = clear
	InjectionMode string  `json:"injectionMode,omitempty"` // "" = no change
	Type          string  `json:"type,omitempty"`          // "" = no change
	Target        string  `json:"target,omitempty"`        // "" = no change
	AllowProgeny  *bool   `json:"allowProgeny,omitempty"`  // null = no change
	Scope         string  `json:"scope,omitempty"`         // Scope type (default: user)
	ScopeID       string  `json:"scopeId,omitempty"`       // Required for project/runtime_broker scope
}

// List returns secret metadata for the specified scope.
func (s *secretService) List(ctx context.Context, opts *ListSecretOptions) (*ListSecretResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Scope != "" {
			query.Set("scope", opts.Scope)
		}
		if opts.ScopeID != "" {
			query.Set("scopeId", opts.ScopeID)
		}
		if opts.Type != "" {
			query.Set("type", opts.Type)
		}
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/secrets", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListSecretResponse](resp)
}

// Get returns metadata for a specific secret by key.
func (s *secretService) Get(ctx context.Context, key string, opts *SecretScopeOptions) (*Secret, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Scope != "" {
			query.Set("scope", opts.Scope)
		}
		if opts.ScopeID != "" {
			query.Set("scopeId", opts.ScopeID)
		}
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/secrets/"+url.PathEscape(key), query, nil)
	if err != nil {
		return nil, err
	}
	secret, err := apiclient.DecodeResponse[Secret](resp)
	if err != nil {
		return nil, err
	}
	if secret == nil {
		// A 204 No Content response decodes to nil, nil.
		return nil, &noContentError{key: key}
	}
	return secret, nil
}

// Set creates or updates a secret.
func (s *secretService) Set(ctx context.Context, key string, req *SetSecretRequest) (*SetSecretResponse, error) {
	resp, err := s.c.put(ctx, "/api/v1/secrets/"+url.PathEscape(key), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[SetSecretResponse](resp)
}

// AgentSet creates or updates a secret using the agent-scoped endpoint.
func (s *secretService) AgentSet(ctx context.Context, agentID, key string, req *AgentSetSecretRequest) (*AgentSetSecretResponse, error) {
	path := "/api/v1/agents/" + url.PathEscape(agentID) + "/secrets/" + url.PathEscape(key)
	resp, err := s.c.put(ctx, path, req, nil)
	if err != nil {
		return nil, err
	}

	// The agent endpoint returns 201 Created with a JSON body for new secrets,
	// or 204 No Content (no body) for updates. DecodeResponse handles body
	// closing, error status codes, and 204 (returns nil, nil).
	if resp.StatusCode == 204 {
		_ = resp.Body.Close()
		return &AgentSetSecretResponse{Key: key, Created: false}, nil
	}

	result, err := apiclient.DecodeResponse[AgentSetSecretResponse](resp)
	if err != nil {
		return nil, err
	}
	result.Created = true
	return result, nil
}

// UpdateMeta updates only the metadata of an existing secret (PATCH).
func (s *secretService) UpdateMeta(ctx context.Context, key string, req *UpdateSecretMetaRequest) (*Secret, error) {
	query := url.Values{}
	if req.Scope != "" {
		query.Set("scope", req.Scope)
	}
	if req.ScopeID != "" {
		query.Set("scopeId", req.ScopeID)
	}

	path := "/api/v1/secrets/" + url.PathEscape(key)
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	resp, err := s.c.patch(ctx, path, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[Secret](resp)
}

// Delete removes a secret.
func (s *secretService) Delete(ctx context.Context, key string, opts *SecretScopeOptions) error {
	query := url.Values{}
	if opts != nil {
		if opts.Scope != "" {
			query.Set("scope", opts.Scope)
		}
		if opts.ScopeID != "" {
			query.Set("scopeId", opts.ScopeID)
		}
	}

	path := "/api/v1/secrets/" + url.PathEscape(key)
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	resp, err := s.c.delete(ctx, path, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// PendingSecret is a user-scope secret written by an agent that waits for
// the owner's approval: metadata and provenance only, never the value.
type PendingSecret struct {
	ID               string    `json:"id"`
	Key              string    `json:"key"`
	Type             string    `json:"type"`
	Target           string    `json:"target"`
	Scope            string    `json:"scope"`
	AllowProgeny     bool      `json:"allowProgeny"`
	InjectionMode    string    `json:"injectionMode,omitempty"`
	Description      string    `json:"description,omitempty"`
	ValueLen         int       `json:"valueLen"`
	AgentID          string    `json:"agentId"`
	AgentSlug        string    `json:"agentSlug"`
	ProjectID        string    `json:"projectId"`
	ProjectName      string    `json:"projectName"`
	Created          time.Time `json:"created"`
	Updated          time.Time `json:"updated"`
	ExpiresAt        time.Time `json:"expiresAt"`
	ReplacesExisting bool      `json:"replacesExisting"`
	ReplacesSecretID string    `json:"replacesSecretId,omitempty"`
	ReplacesVersion  int       `json:"replacesVersion,omitempty"`
	State            string    `json:"state"`
}

// ListPendingSecretsResponse is the response of ListPending.
type ListPendingSecretsResponse struct {
	PendingSecrets []PendingSecret `json:"pendingSecrets"`
}

// ApprovePendingSecretRequest is the optional body of ApprovePending. The
// only change an approval can make is turning progeny access off.
type ApprovePendingSecretRequest struct {
	AllowProgeny *bool `json:"allowProgeny,omitempty"`
}

// PendingSecretDecision is the response of ApprovePending and RejectPending.
type PendingSecretDecision struct {
	ID    string `json:"id"`
	Key   string `json:"key"`
	Scope string `json:"scope"`
	State string `json:"state"` // "approved" or "rejected"
}

// ListPending returns the caller's own pending secrets.
func (s *secretService) ListPending(ctx context.Context) (*ListPendingSecretsResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/pending-secrets", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListPendingSecretsResponse](resp)
}

// ApprovePending approves the caller's pending secret id.
func (s *secretService) ApprovePending(ctx context.Context, id string, req *ApprovePendingSecretRequest) (*PendingSecretDecision, error) {
	if req == nil {
		req = &ApprovePendingSecretRequest{}
	}
	resp, err := s.c.postNoRetry(ctx, "/api/v1/pending-secrets/"+url.PathEscape(id)+"/approve", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[PendingSecretDecision](resp)
}

// RejectPending rejects the caller's pending secret id.
func (s *secretService) RejectPending(ctx context.Context, id string) (*PendingSecretDecision, error) {
	resp, err := s.c.postNoRetry(ctx, "/api/v1/pending-secrets/"+url.PathEscape(id)+"/reject", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[PendingSecretDecision](resp)
}
