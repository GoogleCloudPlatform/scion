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
	"fmt"
	"net/url"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

type OAuthClientType string

const (
	OAuthProviderGoogle = "google"
	OAuthProviderGitHub = "github"

	OAuthClientTypeWeb    OAuthClientType = "web"
	OAuthClientTypeCLI    OAuthClientType = "cli"
	OAuthClientTypeDevice OAuthClientType = "device"
)

func OAuthProviderOrder() []string {
	return []string{
		OAuthProviderGoogle,
		OAuthProviderGitHub,
	}
}

func IsKnownOAuthProvider(provider string) bool {
	for _, candidate := range OAuthProviderOrder() {
		if provider == candidate {
			return true
		}
	}
	return false
}

// AuthService handles authentication operations.
type AuthService interface {
	// Login performs user login.
	Login(ctx context.Context, req *LoginRequest) (*LoginResponse, error)

	// Logout invalidates the current session.
	Logout(ctx context.Context) error

	// Refresh refreshes an access token.
	Refresh(ctx context.Context, refreshToken string) (*TokenResponse, error)

	// Me returns the current authenticated user.
	Me(ctx context.Context) (*User, error)

	// GetWSTicket gets a short-lived WebSocket authentication ticket.
	GetWSTicket(ctx context.Context) (*WSTicketResponse, error)

	// GetAuthProviders returns configured OAuth providers for a client type.
	GetAuthProviders(ctx context.Context, clientType string) (*AuthProvidersResponse, error)

	// GetAuthURL returns the OAuth authorization URL for CLI login.
	GetAuthURL(ctx context.Context, callbackURL, state, provider string) (*AuthURLResponse, error)

	// ExchangeCode exchanges an authorization code for tokens.
	ExchangeCode(ctx context.Context, code, callbackURL, provider string) (*CLITokenResponse, error)

	// RequestDeviceCode initiates the device authorization flow.
	RequestDeviceCode(ctx context.Context, provider string) (*DeviceCodeResponse, error)

	// PollDeviceToken polls for the device authorization result.
	PollDeviceToken(ctx context.Context, deviceCode, provider string) (*DeviceTokenPollResponse, error)
}

// authService is the implementation of AuthService.
type authService struct {
	c *client
}

// LoginRequest is the request for user login.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// LoginResponse is the response from login.
type LoginResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    string `json:"expiresAt"`
	User         *User  `json:"user"`
}

// TokenResponse is the response from token refresh.
type TokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresAt    string `json:"expiresAt"`
}

// WSTicketResponse is the response for WebSocket ticket.
type WSTicketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresAt string `json:"expiresAt"`
}

// AuthURLResponse is the response containing the OAuth authorization URL.
type AuthURLResponse struct {
	URL string `json:"url"`
}

// AuthProvidersResponse is the response containing configured OAuth providers
// for a given client type.
type AuthProvidersResponse struct {
	ClientType string   `json:"clientType"`
	Providers  []string `json:"providers"`
}

// CLITokenResponse is the response from CLI token exchange.
type CLITokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresIn    int64  `json:"expiresIn"` // seconds
	User         *User  `json:"user,omitempty"`
}

// Login performs user login.
func (s *authService) Login(ctx context.Context, req *LoginRequest) (*LoginResponse, error) {
	resp, err := s.c.transport.Post(ctx, "/api/v1/auth/login", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[LoginResponse](resp)
}

// Logout invalidates the current session.
func (s *authService) Logout(ctx context.Context) error {
	resp, err := s.c.transport.Post(ctx, "/api/v1/auth/logout", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Refresh refreshes an access token.
func (s *authService) Refresh(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	body := struct {
		RefreshToken string `json:"refreshToken"`
	}{
		RefreshToken: refreshToken,
	}
	resp, err := s.c.transport.Post(ctx, "/api/v1/auth/refresh", body, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[TokenResponse](resp)
}

// Me returns the current authenticated user.
func (s *authService) Me(ctx context.Context) (*User, error) {
	resp, err := s.c.transport.Get(ctx, "/api/v1/auth/me", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[User](resp)
}

// GetWSTicket gets a short-lived WebSocket authentication ticket.
func (s *authService) GetWSTicket(ctx context.Context) (*WSTicketResponse, error) {
	resp, err := s.c.transport.Post(ctx, "/api/v1/auth/ws-ticket", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[WSTicketResponse](resp)
}

// GetAuthProviders returns configured OAuth providers for a client type.
func (s *authService) GetAuthProviders(ctx context.Context, clientType string) (*AuthProvidersResponse, error) {
	query := url.Values{}
	query.Set("clientType", clientType)

	resp, err := s.c.transport.GetWithQuery(ctx, "/api/v1/auth/providers", query, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		return nil, apiclient.ParseErrorResponse(resp)
	}

	var result AuthProvidersResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode auth providers response: %w", err)
	}

	return &result, nil
}

// GetAuthURL returns the OAuth authorization URL for CLI login.
func (s *authService) GetAuthURL(ctx context.Context, callbackURL, state, provider string) (*AuthURLResponse, error) {
	body := struct {
		CallbackURL string `json:"callbackUrl"`
		State       string `json:"state"`
		Provider    string `json:"provider,omitempty"`
	}{
		CallbackURL: callbackURL,
		State:       state,
		Provider:    provider,
	}
	resp, err := s.c.transport.Post(ctx, "/api/v1/auth/cli/authorize", body, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[AuthURLResponse](resp)
}

// ExchangeCode exchanges an authorization code for tokens.
func (s *authService) ExchangeCode(ctx context.Context, code, callbackURL, provider string) (*CLITokenResponse, error) {
	body := struct {
		Code        string `json:"code"`
		CallbackURL string `json:"callbackUrl"`
		Provider    string `json:"provider,omitempty"`
	}{
		Code:        code,
		CallbackURL: callbackURL,
		Provider:    provider,
	}
	resp, err := s.c.transport.Post(ctx, "/api/v1/auth/cli/token", body, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[CLITokenResponse](resp)
}

// DeviceCodeResponse is the response from initiating a device authorization flow.
type DeviceCodeResponse struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURL         string `json:"verificationUrl"`
	VerificationURLComplete string `json:"verificationUrlComplete,omitempty"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

// DeviceTokenPollResponse is the response from polling for device authorization.
type DeviceTokenPollResponse struct {
	Status       string `json:"status,omitempty"`
	Interval     int    `json:"interval,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresIn    int64  `json:"expiresIn,omitempty"`
	User         *User  `json:"user,omitempty"`
}

// RequestDeviceCode initiates the device authorization flow.
func (s *authService) RequestDeviceCode(ctx context.Context, provider string) (*DeviceCodeResponse, error) {
	body := struct {
		Provider string `json:"provider,omitempty"`
	}{
		Provider: provider,
	}
	resp, err := s.c.transport.Post(ctx, "/api/v1/auth/cli/device", body, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[DeviceCodeResponse](resp)
}

// PollDeviceToken polls for the device authorization result.
// It handles non-200 status codes directly since the polling endpoint uses
// 202 (pending), 410 (expired), and 429 (slow down) to indicate states.
func (s *authService) PollDeviceToken(ctx context.Context, deviceCode, provider string) (*DeviceTokenPollResponse, error) {
	body := struct {
		DeviceCode string `json:"deviceCode"`
		Provider   string `json:"provider,omitempty"`
	}{
		DeviceCode: deviceCode,
		Provider:   provider,
	}
	resp, err := s.c.transport.Post(ctx, "/api/v1/auth/cli/device/token", body, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 200:
		var result DeviceTokenPollResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("failed to decode device token response: %w", err)
		}
		return &result, nil
	case 202:
		return &DeviceTokenPollResponse{Status: "authorization_pending"}, nil
	case 410:
		return &DeviceTokenPollResponse{Status: "expired_token"}, nil
	case 429:
		var result DeviceTokenPollResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return &DeviceTokenPollResponse{Status: "slow_down"}, nil
		}
		result.Status = "slow_down"
		return &result, nil
	default:
		return nil, fmt.Errorf("unexpected status code %d from device token endpoint", resp.StatusCode)
	}
}
