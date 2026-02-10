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

	"github.com/ptone/scion-agent/pkg/apiclient"
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

	// Delete removes a secret.
	Delete(ctx context.Context, key string, opts *SecretScopeOptions) error
}

// secretService is the implementation of SecretService.
type secretService struct {
	c *client
}

// ListSecretOptions configures secret listing.
type ListSecretOptions struct {
	Scope   string // user, grove, runtime_broker (default: user)
	ScopeID string // ID of the scoped entity (required for grove/runtime_broker)
}

// ListSecretResponse is the response from listing secrets.
type ListSecretResponse struct {
	Secrets []Secret `json:"secrets"` // Metadata only, no values
	Scope   string   `json:"scope"`
	ScopeID string   `json:"scopeId"`
}

// SecretScopeOptions specifies the scope for get/delete operations.
type SecretScopeOptions struct {
	Scope   string // user, grove, runtime_broker (default: user)
	ScopeID string // ID of the scoped entity (required for grove/runtime_broker)
}

// SetSecretRequest is the request for setting a secret.
type SetSecretRequest struct {
	Value       string `json:"value"`                 // Required: secret value (write-only)
	Scope       string `json:"scope,omitempty"`       // Scope type (default: user)
	ScopeID     string `json:"scopeId,omitempty"`     // Required for grove/runtime_broker scope
	Description string `json:"description,omitempty"` // Optional description
}

// SetSecretResponse is the response from setting a secret.
type SetSecretResponse struct {
	Secret  *Secret `json:"secret"` // Metadata only, no value
	Created bool    `json:"created"` // Whether this was a new secret
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
	}

	resp, err := s.c.transport.GetWithQuery(ctx, "/api/v1/secrets", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListSecretResponse](resp)
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

	resp, err := s.c.transport.GetWithQuery(ctx, "/api/v1/secrets/"+url.PathEscape(key), query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Secret](resp)
}

// Set creates or updates a secret.
func (s *secretService) Set(ctx context.Context, key string, req *SetSecretRequest) (*SetSecretResponse, error) {
	resp, err := s.c.transport.Put(ctx, "/api/v1/secrets/"+url.PathEscape(key), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[SetSecretResponse](resp)
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

	resp, err := s.c.transport.Delete(ctx, path, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
