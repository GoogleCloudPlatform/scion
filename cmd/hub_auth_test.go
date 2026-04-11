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

package cmd

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

type mockHubAuthService struct {
	providersResp         *hubclient.AuthProvidersResponse
	providersErr          error
	requestDeviceCodeFunc func(ctx context.Context, provider string) (*hubclient.DeviceCodeResponse, error)
	pollDeviceTokenFunc   func(ctx context.Context, deviceCode, provider string) (*hubclient.DeviceTokenPollResponse, error)
	requestedProviders    []string
	polledProviders       []string
}

type mockHubAuthNotImplementedError struct{}

func (mockHubAuthNotImplementedError) Error() string {
	return "not implemented"
}

func (m *mockHubAuthService) Login(ctx context.Context, req *hubclient.LoginRequest) (*hubclient.LoginResponse, error) {
	return nil, mockHubAuthNotImplementedError{}
}
func (m *mockHubAuthService) Logout(ctx context.Context) error {
	return mockHubAuthNotImplementedError{}
}
func (m *mockHubAuthService) Refresh(ctx context.Context, refreshToken string) (*hubclient.TokenResponse, error) {
	return nil, mockHubAuthNotImplementedError{}
}
func (m *mockHubAuthService) Me(ctx context.Context) (*hubclient.User, error) {
	return nil, mockHubAuthNotImplementedError{}
}
func (m *mockHubAuthService) GetWSTicket(ctx context.Context) (*hubclient.WSTicketResponse, error) {
	return nil, mockHubAuthNotImplementedError{}
}
func (m *mockHubAuthService) GetAuthProviders(ctx context.Context, clientType string) (*hubclient.AuthProvidersResponse, error) {
	return m.providersResp, m.providersErr
}
func (m *mockHubAuthService) GetAuthURL(ctx context.Context, callbackURL, state, provider string) (*hubclient.AuthURLResponse, error) {
	return nil, mockHubAuthNotImplementedError{}
}
func (m *mockHubAuthService) ExchangeCode(ctx context.Context, code, callbackURL, provider string) (*hubclient.CLITokenResponse, error) {
	return nil, mockHubAuthNotImplementedError{}
}
func (m *mockHubAuthService) RequestDeviceCode(ctx context.Context, provider string) (*hubclient.DeviceCodeResponse, error) {
	m.requestedProviders = append(m.requestedProviders, provider)
	if m.requestDeviceCodeFunc != nil {
		return m.requestDeviceCodeFunc(ctx, provider)
	}
	return nil, mockHubAuthNotImplementedError{}
}
func (m *mockHubAuthService) PollDeviceToken(ctx context.Context, deviceCode, provider string) (*hubclient.DeviceTokenPollResponse, error) {
	m.polledProviders = append(m.polledProviders, provider)
	if m.pollDeviceTokenFunc != nil {
		return m.pollDeviceTokenFunc(ctx, deviceCode, provider)
	}
	return nil, mockHubAuthNotImplementedError{}
}

func TestResolveHubAuthProvider_ExplicitProvider(t *testing.T) {
	t.Parallel()

	authSvc := &mockHubAuthService{}
	got, err := resolveHubAuthProvider(context.Background(), authSvc, hubclient.OAuthClientTypeDevice, "GitHub")
	if err != nil {
		t.Fatalf("resolveHubAuthProvider returned error: %v", err)
	}
	if got != "github" {
		t.Fatalf("resolveHubAuthProvider = %q, want github", got)
	}
}

func TestResolveHubAuthProvider_InvalidExplicitProvider(t *testing.T) {
	t.Parallel()

	authSvc := &mockHubAuthService{}
	_, err := resolveHubAuthProvider(context.Background(), authSvc, hubclient.OAuthClientTypeCLI, "gitlab")
	if err == nil {
		t.Fatal("expected error for invalid provider")
	}
	if !strings.Contains(err.Error(), "must be one of google, github") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveHubAuthProvider_AutoSelectSingleProvider(t *testing.T) {
	t.Parallel()

	authSvc := &mockHubAuthService{
		providersResp: &hubclient.AuthProvidersResponse{
			ClientType: string(hubclient.OAuthClientTypeDevice),
			Providers:  []string{"github"},
		},
	}

	got, err := resolveHubAuthProvider(context.Background(), authSvc, hubclient.OAuthClientTypeDevice, "")
	if err != nil {
		t.Fatalf("resolveHubAuthProvider returned error: %v", err)
	}
	if got != "github" {
		t.Fatalf("resolveHubAuthProvider = %q, want github", got)
	}
}

func TestResolveHubAuthProvider_MultipleProviders(t *testing.T) {
	t.Parallel()

	authSvc := &mockHubAuthService{
		providersResp: &hubclient.AuthProvidersResponse{
			ClientType: string(hubclient.OAuthClientTypeCLI),
			Providers:  hubclient.OAuthProviderOrder(),
		},
	}

	_, err := resolveHubAuthProvider(context.Background(), authSvc, hubclient.OAuthClientTypeCLI, "")
	if err == nil {
		t.Fatal("expected error for multiple providers")
	}
	if !strings.Contains(err.Error(), "--provider") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveHubAuthProvider_NoProviders(t *testing.T) {
	t.Parallel()

	authSvc := &mockHubAuthService{
		providersResp: &hubclient.AuthProvidersResponse{
			ClientType: string(hubclient.OAuthClientTypeDevice),
			Providers:  []string{},
		},
	}

	_, err := resolveHubAuthProvider(context.Background(), authSvc, hubclient.OAuthClientTypeDevice, "")
	if err == nil {
		t.Fatal("expected error when no providers are configured")
	}
	if !strings.Contains(err.Error(), "no OAuth providers configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveExplicitDeviceFlowProvider_ImplicitProviderSkipsResolution(t *testing.T) {
	t.Parallel()

	authSvc := &mockHubAuthService{
		providersResp: &hubclient.AuthProvidersResponse{
			ClientType: string(hubclient.OAuthClientTypeDevice),
			Providers:  hubclient.OAuthProviderOrder(),
		},
	}

	got, err := resolveExplicitDeviceFlowProvider(context.Background(), authSvc, "")
	if err != nil {
		t.Fatalf("resolveExplicitDeviceFlowProvider returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("resolveExplicitDeviceFlowProvider = %q, want empty string", got)
	}
}

func TestResolveExplicitDeviceFlowProvider_ExplicitProviderUsesValidation(t *testing.T) {
	t.Parallel()

	authSvc := &mockHubAuthService{}

	got, err := resolveExplicitDeviceFlowProvider(context.Background(), authSvc, "GitHub")
	if err != nil {
		t.Fatalf("resolveExplicitDeviceFlowProvider returned error: %v", err)
	}
	if got != "github" {
		t.Fatalf("resolveExplicitDeviceFlowProvider = %q, want github", got)
	}
}

func TestNewDeviceFlowAuth_ImplicitProviderUsesFallbackPath(t *testing.T) {
	t.Parallel()

	authSvc := &mockHubAuthService{
		requestDeviceCodeFunc: func(ctx context.Context, provider string) (*hubclient.DeviceCodeResponse, error) {
			if provider == "google" {
				return nil, &apiclient.APIError{
					StatusCode: 400,
					Code:       apiclient.ErrCodeValidationError,
					Message:    "OAuth provider not configured for device flow: google",
				}
			}
			if provider == "github" {
				return &hubclient.DeviceCodeResponse{
					DeviceCode:      "github-device-code",
					UserCode:        "GH-1234",
					VerificationURL: "https://github.com/login/device",
					ExpiresIn:       300,
					Interval:        1,
				}, nil
			}
			return nil, fmt.Errorf("unexpected provider: %s", provider)
		},
		pollDeviceTokenFunc: func(ctx context.Context, deviceCode, provider string) (*hubclient.DeviceTokenPollResponse, error) {
			if provider != "github" {
				return nil, fmt.Errorf("expected github provider during polling, got %s", provider)
			}
			return &hubclient.DeviceTokenPollResponse{
				AccessToken: "github-access-token",
				ExpiresIn:   3600,
			}, nil
		},
	}

	deviceAuth := newDeviceFlowAuth(authSvc, "")

	resp, err := deviceAuth.Authenticate(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.AccessToken != "github-access-token" {
		t.Fatalf("expected github access token, got %q", resp.AccessToken)
	}
	if got, want := len(authSvc.requestedProviders), 2; got != want {
		t.Fatalf("expected %d provider attempts, got %d (%v)", want, got, authSvc.requestedProviders)
	}
	if authSvc.requestedProviders[0] != "google" || authSvc.requestedProviders[1] != "github" {
		t.Fatalf("expected provider fallback google -> github, got %v", authSvc.requestedProviders)
	}
	if got, want := len(authSvc.polledProviders), 1; got != want {
		t.Fatalf("expected %d poll provider, got %d (%v)", want, got, authSvc.polledProviders)
	}
	if authSvc.polledProviders[0] != "github" {
		t.Fatalf("expected github polling provider, got %v", authSvc.polledProviders)
	}
}
