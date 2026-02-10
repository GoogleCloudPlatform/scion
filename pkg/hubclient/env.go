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

// EnvService handles environment variable operations.
type EnvService interface {
	// List returns environment variables for the specified scope.
	List(ctx context.Context, opts *ListEnvOptions) (*ListEnvResponse, error)

	// Get returns a specific environment variable by key.
	Get(ctx context.Context, key string, opts *EnvScopeOptions) (*EnvVar, error)

	// Set creates or updates an environment variable.
	Set(ctx context.Context, key string, req *SetEnvRequest) (*SetEnvResponse, error)

	// Delete removes an environment variable.
	Delete(ctx context.Context, key string, opts *EnvScopeOptions) error
}

// envService is the implementation of EnvService.
type envService struct {
	c *client
}

// ListEnvOptions configures environment variable listing.
type ListEnvOptions struct {
	Scope   string // user, grove, runtime_broker (default: user)
	ScopeID string // ID of the scoped entity (required for grove/runtime_broker)
	Key     string // Optional: filter by specific key
}

// ListEnvResponse is the response from listing environment variables.
type ListEnvResponse struct {
	EnvVars []EnvVar `json:"envVars"`
	Scope   string   `json:"scope"`
	ScopeID string   `json:"scopeId"`
}

// EnvScopeOptions specifies the scope for get/delete operations.
type EnvScopeOptions struct {
	Scope   string // user, grove, runtime_broker (default: user)
	ScopeID string // ID of the scoped entity (required for grove/runtime_broker)
}

// SetEnvRequest is the request for setting an environment variable.
type SetEnvRequest struct {
	Value       string `json:"value"`                 // Required: variable value
	Scope       string `json:"scope,omitempty"`       // Scope type (default: user)
	ScopeID     string `json:"scopeId,omitempty"`     // Required for grove/runtime_broker scope
	Description string `json:"description,omitempty"` // Optional description
	Sensitive   bool   `json:"sensitive,omitempty"`   // Mask value in responses
}

// SetEnvResponse is the response from setting an environment variable.
type SetEnvResponse struct {
	EnvVar  *EnvVar `json:"envVar"`
	Created bool    `json:"created"` // Whether this was a new variable
}

// List returns environment variables for the specified scope.
func (s *envService) List(ctx context.Context, opts *ListEnvOptions) (*ListEnvResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Scope != "" {
			query.Set("scope", opts.Scope)
		}
		if opts.ScopeID != "" {
			query.Set("scopeId", opts.ScopeID)
		}
		if opts.Key != "" {
			query.Set("key", opts.Key)
		}
	}

	resp, err := s.c.transport.GetWithQuery(ctx, "/api/v1/env", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListEnvResponse](resp)
}

// Get returns a specific environment variable by key.
func (s *envService) Get(ctx context.Context, key string, opts *EnvScopeOptions) (*EnvVar, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Scope != "" {
			query.Set("scope", opts.Scope)
		}
		if opts.ScopeID != "" {
			query.Set("scopeId", opts.ScopeID)
		}
	}

	resp, err := s.c.transport.GetWithQuery(ctx, "/api/v1/env/"+url.PathEscape(key), query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[EnvVar](resp)
}

// Set creates or updates an environment variable.
func (s *envService) Set(ctx context.Context, key string, req *SetEnvRequest) (*SetEnvResponse, error) {
	resp, err := s.c.transport.Put(ctx, "/api/v1/env/"+url.PathEscape(key), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[SetEnvResponse](resp)
}

// Delete removes an environment variable.
func (s *envService) Delete(ctx context.Context, key string, opts *EnvScopeOptions) error {
	query := url.Values{}
	if opts != nil {
		if opts.Scope != "" {
			query.Set("scope", opts.Scope)
		}
		if opts.ScopeID != "" {
			query.Set("scopeId", opts.ScopeID)
		}
	}

	path := "/api/v1/env/" + url.PathEscape(key)
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	resp, err := s.c.transport.Delete(ctx, path, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
