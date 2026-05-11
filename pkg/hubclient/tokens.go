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
	"encoding/json"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// TokenService handles user access token operations.
type TokenService interface {
	// Create creates a new user access token.
	Create(ctx context.Context, req *CreateTokenRequest) (*CreateTokenResponse, error)

	// List returns all tokens for the authenticated user.
	List(ctx context.Context) (*ListTokensResponse, error)

	// Get returns details for a specific token.
	Get(ctx context.Context, id string) (*TokenInfo, error)

	// Revoke soft-revokes a token (it still exists but cannot be used).
	Revoke(ctx context.Context, id string) error

	// Delete permanently removes a token.
	Delete(ctx context.Context, id string) error
}

// tokenService is the implementation of TokenService.
type tokenService struct {
	c *client
}

// CreateTokenRequest is the request for creating a user access token.
type CreateTokenRequest struct {
	Name      string     `json:"name"`
	ProjectID string     `json:"projectId"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// MarshalJSON implements custom marshaling to support legacy groveId field.
func (r CreateTokenRequest) MarshalJSON() ([]byte, error) {
	type Alias CreateTokenRequest
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId,omitempty"`
	}{
		Alias:   Alias(r),
		GroveID: r.ProjectID,
	})
}

// UnmarshalJSON implements custom unmarshaling to support legacy groveId field.
func (r *CreateTokenRequest) UnmarshalJSON(data []byte) error {
	type Alias CreateTokenRequest
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

// CreateTokenResponse is the response from creating a user access token.
type CreateTokenResponse struct {
	Token       string     `json:"token"` // Full token value, shown only once
	AccessToken *TokenInfo `json:"accessToken"`
}

// TokenInfo represents token metadata (without the actual token value).
type TokenInfo struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	ProjectID string     `json:"projectId"`
	Scopes    []string   `json:"scopes"`
	Revoked   bool       `json:"revoked"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	LastUsed  *time.Time `json:"lastUsed,omitempty"`
	Created   time.Time  `json:"created"`
}

// MarshalJSON implements custom marshaling to support legacy groveId field.
func (i TokenInfo) MarshalJSON() ([]byte, error) {
	type Alias TokenInfo
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId,omitempty"`
	}{
		Alias:   Alias(i),
		GroveID: i.ProjectID,
	})
}

// UnmarshalJSON implements custom unmarshaling to support legacy groveId field.
func (i *TokenInfo) UnmarshalJSON(data []byte) error {
	type Alias TokenInfo
	aux := &struct {
		GroveID string `json:"groveId"`
		*Alias
	}{
		Alias: (*Alias)(i),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if i.ProjectID == "" && aux.GroveID != "" {
		i.ProjectID = aux.GroveID
	}
	return nil
}

// ListTokensResponse is the response from listing user access tokens.
type ListTokensResponse struct {
	Items []TokenInfo `json:"items"`
}

// Create creates a new user access token.
func (s *tokenService) Create(ctx context.Context, req *CreateTokenRequest) (*CreateTokenResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/auth/tokens", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[CreateTokenResponse](resp)
}

// List returns all tokens for the authenticated user.
func (s *tokenService) List(ctx context.Context) (*ListTokensResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/auth/tokens", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListTokensResponse](resp)
}

// Get returns details for a specific token.
func (s *tokenService) Get(ctx context.Context, id string) (*TokenInfo, error) {
	resp, err := s.c.get(ctx, "/api/v1/auth/tokens/"+id, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[TokenInfo](resp)
}

// Revoke soft-revokes a token.
func (s *tokenService) Revoke(ctx context.Context, id string) error {
	resp, err := s.c.post(ctx, "/api/v1/auth/tokens/"+id+"/revoke", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Delete permanently removes a token.
func (s *tokenService) Delete(ctx context.Context, id string) error {
	resp, err := s.c.delete(ctx, "/api/v1/auth/tokens/"+id, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
