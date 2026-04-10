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

package auth

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// mockAuthService implements hubclient.AuthService for testing.
type mockAuthService struct {
	deviceCodeResp        *hubclient.DeviceCodeResponse
	deviceCodeErr         error
	requestDeviceCodeFunc func(ctx context.Context, provider string) (*hubclient.DeviceCodeResponse, error)
	pollResponses         []*hubclient.DeviceTokenPollResponse
	pollErrors            []error
	pollIndex             int
	pollDeviceTokenFunc   func(ctx context.Context, deviceCode, provider string) (*hubclient.DeviceTokenPollResponse, error)
}

func (m *mockAuthService) Login(ctx context.Context, req *hubclient.LoginRequest) (*hubclient.LoginResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAuthService) Logout(ctx context.Context) error {
	return fmt.Errorf("not implemented")
}
func (m *mockAuthService) Refresh(ctx context.Context, refreshToken string) (*hubclient.TokenResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAuthService) Me(ctx context.Context) (*hubclient.User, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAuthService) GetWSTicket(ctx context.Context) (*hubclient.WSTicketResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAuthService) GetAuthURL(ctx context.Context, callbackURL, state string) (*hubclient.AuthURLResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockAuthService) ExchangeCode(ctx context.Context, code, callbackURL string) (*hubclient.CLITokenResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockAuthService) RequestDeviceCode(ctx context.Context, provider string) (*hubclient.DeviceCodeResponse, error) {
	if m.requestDeviceCodeFunc != nil {
		return m.requestDeviceCodeFunc(ctx, provider)
	}
	return m.deviceCodeResp, m.deviceCodeErr
}

func (m *mockAuthService) PollDeviceToken(ctx context.Context, deviceCode, provider string) (*hubclient.DeviceTokenPollResponse, error) {
	if m.pollDeviceTokenFunc != nil {
		return m.pollDeviceTokenFunc(ctx, deviceCode, provider)
	}
	if m.pollIndex >= len(m.pollResponses) {
		return nil, fmt.Errorf("no more poll responses")
	}
	resp := m.pollResponses[m.pollIndex]
	var err error
	if m.pollIndex < len(m.pollErrors) {
		err = m.pollErrors[m.pollIndex]
	}
	m.pollIndex++
	return resp, err
}

func TestDeviceFlowAuth_Success(t *testing.T) {
	mock := &mockAuthService{
		deviceCodeResp: &hubclient.DeviceCodeResponse{
			DeviceCode:      "test-device-code",
			UserCode:        "ABCD-1234",
			VerificationURL: "https://example.com/device",
			ExpiresIn:       300,
			Interval:        1, // 1 second for fast test
		},
		pollResponses: []*hubclient.DeviceTokenPollResponse{
			{Status: "authorization_pending"},
			{Status: "authorization_pending"},
			{
				AccessToken:  "test-access-token",
				RefreshToken: "test-refresh-token",
				ExpiresIn:    3600,
				User: &hubclient.User{
					ID:    "user-1",
					Email: "test@example.com",
				},
			},
		},
	}

	d := NewDeviceFlowAuth(mock)
	var buf bytes.Buffer
	d.output = &buf

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := d.Authenticate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.AccessToken != "test-access-token" {
		t.Errorf("expected access token 'test-access-token', got %q", resp.AccessToken)
	}
	if resp.RefreshToken != "test-refresh-token" {
		t.Errorf("expected refresh token 'test-refresh-token', got %q", resp.RefreshToken)
	}
	if resp.User == nil || resp.User.Email != "test@example.com" {
		t.Error("expected user email 'test@example.com'")
	}

	// Check output contains the user code
	output := buf.String()
	if !bytes.Contains([]byte(output), []byte("ABCD-1234")) {
		t.Error("expected output to contain user code")
	}
}

func TestDeviceFlowAuth_SlowDown(t *testing.T) {
	mock := &mockAuthService{
		deviceCodeResp: &hubclient.DeviceCodeResponse{
			DeviceCode:      "test-device-code",
			UserCode:        "ABCD-1234",
			VerificationURL: "https://example.com/device",
			ExpiresIn:       300,
			Interval:        1,
		},
		pollResponses: []*hubclient.DeviceTokenPollResponse{
			{Status: "slow_down"},
			{
				AccessToken: "token",
				ExpiresIn:   3600,
			},
		},
	}

	d := NewDeviceFlowAuth(mock)
	d.output = &bytes.Buffer{}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := d.Authenticate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.AccessToken != "token" {
		t.Errorf("expected access token 'token', got %q", resp.AccessToken)
	}
}

func TestDeviceFlowAuth_ExpiredToken(t *testing.T) {
	mock := &mockAuthService{
		deviceCodeResp: &hubclient.DeviceCodeResponse{
			DeviceCode:      "test-device-code",
			UserCode:        "ABCD-1234",
			VerificationURL: "https://example.com/device",
			ExpiresIn:       300,
			Interval:        1,
		},
		pollResponses: []*hubclient.DeviceTokenPollResponse{
			{Status: "expired_token"},
		},
	}

	d := NewDeviceFlowAuth(mock)
	d.output = &bytes.Buffer{}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := d.Authenticate(ctx)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	if err.Error() != "device authorization expired" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestDeviceFlowAuth_ContextCancellation(t *testing.T) {
	mock := &mockAuthService{
		deviceCodeResp: &hubclient.DeviceCodeResponse{
			DeviceCode:      "test-device-code",
			UserCode:        "ABCD-1234",
			VerificationURL: "https://example.com/device",
			ExpiresIn:       300,
			Interval:        1,
		},
		// No poll responses — context will cancel before we poll
		pollResponses: []*hubclient.DeviceTokenPollResponse{},
	}

	d := NewDeviceFlowAuth(mock)
	d.output = &bytes.Buffer{}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately
	cancel()

	_, err := d.Authenticate(ctx)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestDeviceFlowAuth_DeviceCodeError(t *testing.T) {
	mock := &mockAuthService{
		deviceCodeErr: fmt.Errorf("network error"),
	}

	d := NewDeviceFlowAuth(mock)
	d.output = &bytes.Buffer{}

	_, err := d.Authenticate(context.Background())
	if err == nil {
		t.Fatal("expected error from device code request")
	}
}

func TestDeviceFlowAuth_FallsBackToGitHubWhenGoogleUnavailable(t *testing.T) {
	requestedProviders := []string{}
	mock := &mockAuthService{
		requestDeviceCodeFunc: func(ctx context.Context, provider string) (*hubclient.DeviceCodeResponse, error) {
			requestedProviders = append(requestedProviders, provider)
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

	d := NewDeviceFlowAuth(mock)
	d.output = &bytes.Buffer{}

	resp, err := d.Authenticate(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.AccessToken != "github-access-token" {
		t.Fatalf("expected github access token, got %q", resp.AccessToken)
	}
	if len(requestedProviders) != 2 || requestedProviders[0] != "google" || requestedProviders[1] != "github" {
		t.Fatalf("expected provider fallback google -> github, got %v", requestedProviders)
	}
}
