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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

type mockHubAuthService struct {
	providersResp *hubclient.AuthProvidersResponse
	providersErr  error
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
	return nil, mockHubAuthNotImplementedError{}
}
func (m *mockHubAuthService) PollDeviceToken(ctx context.Context, deviceCode, provider string) (*hubclient.DeviceTokenPollResponse, error) {
	return nil, mockHubAuthNotImplementedError{}
}

func TestResolveHubAuthProvider_ExplicitProvider(t *testing.T) {
	t.Parallel()

	authSvc := &mockHubAuthService{}
	got, err := resolveHubAuthProvider(context.Background(), authSvc, "device", "GitHub")
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
	_, err := resolveHubAuthProvider(context.Background(), authSvc, "cli", "gitlab")
	if err == nil {
		t.Fatal("expected error for invalid provider")
	}
	if !strings.Contains(err.Error(), "must be google or github") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveHubAuthProvider_AutoSelectSingleProvider(t *testing.T) {
	t.Parallel()

	authSvc := &mockHubAuthService{
		providersResp: &hubclient.AuthProvidersResponse{
			ClientType: "device",
			Providers:  []string{"github"},
		},
	}

	got, err := resolveHubAuthProvider(context.Background(), authSvc, "device", "")
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
			ClientType: "cli",
			Providers:  []string{"google", "github"},
		},
	}

	_, err := resolveHubAuthProvider(context.Background(), authSvc, "cli", "")
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
			ClientType: "device",
			Providers:  []string{},
		},
	}

	_, err := resolveHubAuthProvider(context.Background(), authSvc, "device", "")
	if err == nil {
		t.Fatal("expected error when no providers are configured")
	}
	if !strings.Contains(err.Error(), "no OAuth providers configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}
