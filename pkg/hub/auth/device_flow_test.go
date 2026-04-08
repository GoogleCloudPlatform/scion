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

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// mockAuthService implements hubclient.AuthService for testing.
type mockAuthService struct {
	deviceCodeResp     *hubclient.DeviceCodeResponse
	deviceCodeErr      error
	pollResponses      []*hubclient.DeviceTokenPollResponse
	pollErrors         []error
	pollIndex          int
	requestedProviders []string
	polledProviders    []string
}

type mockDeviceFlowNotImplementedError struct{}

func (mockDeviceFlowNotImplementedError) Error() string {
	return "not implemented"
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
func (m *mockAuthService) GetAuthProviders(ctx context.Context, clientType string) (*hubclient.AuthProvidersResponse, error) {
	return nil, mockDeviceFlowNotImplementedError{}
}
func (m *mockAuthService) GetAuthURL(ctx context.Context, callbackURL, state, provider string) (*hubclient.AuthURLResponse, error) {
	return nil, mockDeviceFlowNotImplementedError{}
}
func (m *mockAuthService) ExchangeCode(ctx context.Context, code, callbackURL, provider string) (*hubclient.CLITokenResponse, error) {
	return nil, mockDeviceFlowNotImplementedError{}
}

func (m *mockAuthService) RequestDeviceCode(ctx context.Context, provider string) (*hubclient.DeviceCodeResponse, error) {
	m.requestedProviders = append(m.requestedProviders, provider)
	return m.deviceCodeResp, m.deviceCodeErr
}

func (m *mockAuthService) PollDeviceToken(ctx context.Context, deviceCode, provider string) (*hubclient.DeviceTokenPollResponse, error) {
	m.polledProviders = append(m.polledProviders, provider)
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

	d := NewDeviceFlowAuth(mock, "github")
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
	if len(mock.requestedProviders) != 1 || mock.requestedProviders[0] != "github" {
		t.Fatalf("expected RequestDeviceCode to use provider github, got %v", mock.requestedProviders)
	}
	for _, provider := range mock.polledProviders {
		if provider != "github" {
			t.Fatalf("expected PollDeviceToken provider github, got %v", mock.polledProviders)
		}
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

	d := NewDeviceFlowAuth(mock, "google")
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

	d := NewDeviceFlowAuth(mock, "google")
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

	d := NewDeviceFlowAuth(mock, "google")
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

	d := NewDeviceFlowAuth(mock, "google")
	d.output = &bytes.Buffer{}

	_, err := d.Authenticate(context.Background())
	if err == nil {
		t.Fatal("expected error from device code request")
	}
}
