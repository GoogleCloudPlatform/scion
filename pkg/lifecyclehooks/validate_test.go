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

package lifecyclehooks

import (
	"context"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Mock GCP SA resolver
// ---------------------------------------------------------------------------

type mockSAResolver struct {
	accounts map[string]*store.GCPServiceAccount
}

func (m *mockSAResolver) GetGCPServiceAccount(_ context.Context, id string) (*store.GCPServiceAccount, error) {
	sa, ok := m.accounts[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return sa, nil
}

// newVerifiedHubSA returns a mock verified hub-scoped GCP SA.
func newVerifiedHubSA(id, email string) *store.GCPServiceAccount {
	return &store.GCPServiceAccount{
		ID:                 id,
		Scope:              "hub",
		ScopeID:            "",
		Email:              email,
		Verified:           true,
		VerificationStatus: "verified",
	}
}

// newUnverifiedSA returns a mock unverified GCP SA.
func newUnverifiedSA(id, email string) *store.GCPServiceAccount {
	return &store.GCPServiceAccount{
		ID:                 id,
		Scope:              "hub",
		ScopeID:            "",
		Email:              email,
		Verified:           false,
		VerificationStatus: "unverified",
	}
}

// newProjectSA returns a mock verified project-scoped GCP SA.
func newProjectSA(id, email, scopeID string) *store.GCPServiceAccount {
	return &store.GCPServiceAccount{
		ID:                 id,
		Scope:              "project",
		ScopeID:            scopeID,
		Email:              email,
		Verified:           true,
		VerificationStatus: "verified",
	}
}

// defaultResolver returns a resolver with a single verified hub SA.
func defaultResolver() *mockSAResolver {
	return &mockSAResolver{
		accounts: map[string]*store.GCPServiceAccount{
			"sa-001": newVerifiedHubSA("sa-001", "hooks@example.iam.gserviceaccount.com"),
			"sa-002": newUnverifiedSA("sa-002", "pending@example.iam.gserviceaccount.com"),
			"sa-003": newProjectSA("sa-003", "proj@example.iam.gserviceaccount.com", "proj-123"),
		},
	}
}

// validHTTPHook returns a minimal valid http hook for test setup.
func validHTTPHook() *store.LifecycleHook {
	return &store.LifecycleHook{
		ID:        "hook-001",
		Name:      "test-hook",
		ScopeType: store.LifecycleHookScopeHub,
		Trigger:   store.LifecycleHookTriggerRunning,
		Action: &store.LifecycleHookAction{
			Type:           store.LifecycleHookActionHTTP,
			Method:         "POST",
			URL:            "https://registry.example.com/agents",
			TimeoutSeconds: 10,
		},
		ExecutionIdentity: "sa-001",
		Enabled:           true,
	}
}

// ---------------------------------------------------------------------------
// ValidateHook — trigger validation
// ---------------------------------------------------------------------------

func TestValidateHook_Triggers(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
		wantErr bool
	}{
		{"valid: running", "running", false},
		{"valid: suspended", "suspended", false},
		{"valid: stopped", "stopped", false},
		{"valid: error", "error", false},
		{"invalid: stopping", "stopping", true},
		{"invalid: created", "created", true},
		{"invalid: empty", "", true},
		{"invalid: arbitrary", "foobar", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := validHTTPHook()
			h.Trigger = tc.trigger
			err := ValidateHook(context.Background(), h, defaultResolver())
			if tc.wantErr && err == nil {
				t.Errorf("expected validation error for trigger %q, got nil", tc.trigger)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for trigger %q: %v", tc.trigger, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateHook — scope validation
// ---------------------------------------------------------------------------

func TestValidateHook_ScopeType(t *testing.T) {
	tests := []struct {
		name      string
		scopeType string
		scopeID   string
		wantErr   bool
	}{
		{"valid: hub", store.LifecycleHookScopeHub, "", false},
		{"valid: empty defaults to hub", "", "", false},
		{"invalid: arbitrary", "datacenter", "", true},
		{"invalid: project without scopeId", store.LifecycleHookScopeProject, "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := validHTTPHook()
			h.ScopeType = tc.scopeType
			h.ScopeID = tc.scopeID
			err := ValidateHook(context.Background(), h, defaultResolver())
			if tc.wantErr && err == nil {
				t.Errorf("expected validation error for scopeType=%q scopeID=%q, got nil", tc.scopeType, tc.scopeID)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for scopeType=%q scopeID=%q: %v", tc.scopeType, tc.scopeID, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateHook — action type validation
// ---------------------------------------------------------------------------

func TestValidateHook_ActionTypes(t *testing.T) {
	tests := []struct {
		name    string
		aType   string
		wantErr bool
	}{
		{"valid: http", "http", false},
		{"valid: webhook", "webhook", false},
		{"invalid: script", "script", true},
		{"invalid: empty", "", true},
		{"invalid: grpc", "grpc", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := validHTTPHook()
			h.Action.Type = tc.aType
			if tc.aType == "webhook" {
				h.Action.Method = "POST"
				h.ExecutionIdentity = "" // webhook doesn't require it
			}
			err := ValidateHook(context.Background(), h, defaultResolver())
			if tc.wantErr && err == nil {
				t.Errorf("expected validation error for type %q, got nil", tc.aType)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for type %q: %v", tc.aType, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateHook — HTTP method validation
// ---------------------------------------------------------------------------

func TestValidateHook_HTTPMethods(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		wantErr bool
	}{
		{"valid: GET", "GET", false},
		{"valid: POST", "POST", false},
		{"valid: PUT", "PUT", false},
		{"valid: PATCH", "PATCH", false},
		{"valid: DELETE", "DELETE", false},
		{"valid: HEAD", "HEAD", false},
		{"invalid: OPTIONS", "OPTIONS", true},
		{"invalid: CONNECT", "CONNECT", true},
		{"invalid: empty", "", true},
		{"invalid: lowercase post", "post", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := validHTTPHook()
			h.Action.Method = tc.method
			err := ValidateHook(context.Background(), h, defaultResolver())
			if tc.wantErr && err == nil {
				t.Errorf("expected validation error for method %q, got nil", tc.method)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for method %q: %v", tc.method, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateHook — webhook-specific validation
// ---------------------------------------------------------------------------

func TestValidateHook_WebhookRules(t *testing.T) {
	tests := []struct {
		name    string
		hook    *store.LifecycleHook
		wantErr bool
		errMsg  string
	}{
		{
			name: "webhook: valid minimal",
			hook: &store.LifecycleHook{
				Trigger:   "running",
				ScopeType: "hub",
				Action: &store.LifecycleHookAction{
					Type:           "webhook",
					URL:            "https://hooks.slack.com/services/T00/B00/xxx",
					TimeoutSeconds: 5,
				},
			},
			wantErr: false,
		},
		{
			name: "webhook: method must be POST (canonical)",
			hook: &store.LifecycleHook{
				Trigger:   "running",
				ScopeType: "hub",
				Action: &store.LifecycleHookAction{
					Type:           "webhook",
					Method:         "GET",
					URL:            "https://hooks.slack.com/services/T00/B00/xxx",
					TimeoutSeconds: 5,
				},
			},
			wantErr: true,
			errMsg:  "webhook actions must use POST",
		},
		{
			// C5: lowercase "post" is now rejected (canonical uppercase required).
			name: "webhook: lowercase post rejected",
			hook: &store.LifecycleHook{
				Trigger:   "running",
				ScopeType: "hub",
				Action: &store.LifecycleHookAction{
					Type:           "webhook",
					Method:         "post",
					URL:            "https://hooks.slack.com/services/T00/B00/xxx",
					TimeoutSeconds: 5,
				},
			},
			wantErr: true,
			errMsg:  "webhook actions must use POST",
		},
		{
			name: "webhook: auth header rejected",
			hook: &store.LifecycleHook{
				Trigger:   "running",
				ScopeType: "hub",
				Action: &store.LifecycleHookAction{
					Type: "webhook",
					URL:  "https://hooks.slack.com/services/T00/B00/xxx",
					Headers: map[string]string{
						"Authorization": "Bearer secret-token",
					},
					TimeoutSeconds: 5,
				},
			},
			wantErr: true,
			errMsg:  "authentication headers are not allowed on webhook",
		},
		{
			name: "webhook: proxy-authorization rejected",
			hook: &store.LifecycleHook{
				Trigger:   "running",
				ScopeType: "hub",
				Action: &store.LifecycleHookAction{
					Type: "webhook",
					URL:  "https://hooks.slack.com/services/T00/B00/xxx",
					Headers: map[string]string{
						"Proxy-Authorization": "Basic abc",
					},
					TimeoutSeconds: 5,
				},
			},
			wantErr: true,
			errMsg:  "authentication headers are not allowed on webhook",
		},
		{
			name: "webhook: x-api-key rejected",
			hook: &store.LifecycleHook{
				Trigger:   "running",
				ScopeType: "hub",
				Action: &store.LifecycleHookAction{
					Type: "webhook",
					URL:  "https://hooks.slack.com/services/T00/B00/xxx",
					Headers: map[string]string{
						"X-Api-Key": "secret",
					},
					TimeoutSeconds: 5,
				},
			},
			wantErr: true,
			errMsg:  "authentication headers are not allowed on webhook",
		},
		// T2: Cookie/Set-Cookie auth-header handling
		{
			name: "webhook: Cookie header rejected (B3)",
			hook: &store.LifecycleHook{
				Trigger:   "running",
				ScopeType: "hub",
				Action: &store.LifecycleHookAction{
					Type: "webhook",
					URL:  "https://hooks.slack.com/services/T00/B00/xxx",
					Headers: map[string]string{
						"Cookie": "session=abc123",
					},
					TimeoutSeconds: 5,
				},
			},
			wantErr: true,
			errMsg:  "authentication headers are not allowed on webhook",
		},
		{
			name: "webhook: Set-Cookie header rejected (B3)",
			hook: &store.LifecycleHook{
				Trigger:   "running",
				ScopeType: "hub",
				Action: &store.LifecycleHookAction{
					Type: "webhook",
					URL:  "https://hooks.slack.com/services/T00/B00/xxx",
					Headers: map[string]string{
						"Set-Cookie": "session=abc123; Path=/; HttpOnly",
					},
					TimeoutSeconds: 5,
				},
			},
			wantErr: true,
			errMsg:  "authentication headers are not allowed on webhook",
		},
		{
			name: "webhook: non-auth custom headers allowed",
			hook: &store.LifecycleHook{
				Trigger:   "running",
				ScopeType: "hub",
				Action: &store.LifecycleHookAction{
					Type: "webhook",
					URL:  "https://hooks.slack.com/services/T00/B00/xxx",
					Headers: map[string]string{
						"Content-Type":    "application/json",
						"X-Custom-Header": "value",
					},
					TimeoutSeconds: 5,
				},
			},
			wantErr: false,
		},
		{
			name: "webhook: execution_identity optional (empty OK)",
			hook: &store.LifecycleHook{
				Trigger:           "running",
				ScopeType:         "hub",
				ExecutionIdentity: "",
				Action: &store.LifecycleHookAction{
					Type:           "webhook",
					URL:            "https://hooks.slack.com/services/T00/B00/xxx",
					TimeoutSeconds: 5,
				},
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateHook(context.Background(), tc.hook, defaultResolver())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errMsg)
				}
				if tc.errMsg != "" && !strings.Contains(err.Error(), tc.errMsg) {
					t.Errorf("expected error containing %q, got: %v", tc.errMsg, err)
				}
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateHook — URL validation
// ---------------------------------------------------------------------------

func TestValidateHook_URLValidation(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid: https", "https://registry.example.com/agents", false},
		{"rejected: http scheme with http action type (S2)", "http://internal.corp/api/register", true},
		{"valid: with port", "https://registry.example.com:8443/agents", false},
		{"valid: with query", "https://registry.example.com/agents?env=prod", false},
		{"invalid: no scheme", "registry.example.com/agents", true},
		{"invalid: no host", "https:///agents", true},
		{"invalid: ftp scheme", "ftp://registry.example.com/agents", true},
		{"invalid: empty", "", true},
		{"valid: with trusted var in path", "https://registry.example.com/${PROJECT_ID}/agents", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := validHTTPHook()
			h.Action.URL = tc.url
			err := ValidateHook(context.Background(), h, defaultResolver())
			if tc.wantErr && err == nil {
				t.Errorf("expected validation error for URL %q, got nil", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for URL %q: %v", tc.url, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateHook — timeout validation
// ---------------------------------------------------------------------------

func TestValidateHook_Timeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout int
		wantErr bool
	}{
		{"valid: 1s", 1, false},
		{"valid: 30s (max)", 30, false},
		{"valid: 15s", 15, false},
		{"invalid: 0", 0, true},
		{"invalid: negative", -1, true},
		{"invalid: 31s (over max)", 31, true},
		{"invalid: 120s", 120, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := validHTTPHook()
			h.Action.TimeoutSeconds = tc.timeout
			err := ValidateHook(context.Background(), h, defaultResolver())
			if tc.wantErr && err == nil {
				t.Errorf("expected validation error for timeout %d, got nil", tc.timeout)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for timeout %d: %v", tc.timeout, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateHook — execution_identity validation
// ---------------------------------------------------------------------------

func TestValidateHook_ExecutionIdentity(t *testing.T) {
	tests := []struct {
		name    string
		hook    *store.LifecycleHook
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid: verified hub SA",
			hook:    validHTTPHook(),
			wantErr: false,
		},
		{
			name: "invalid: SA not found",
			hook: func() *store.LifecycleHook {
				h := validHTTPHook()
				h.ExecutionIdentity = "nonexistent"
				return h
			}(),
			wantErr: true,
			errMsg:  "not found",
		},
		{
			name: "invalid: SA not verified",
			hook: func() *store.LifecycleHook {
				h := validHTTPHook()
				h.ExecutionIdentity = "sa-002"
				return h
			}(),
			wantErr: true,
			errMsg:  "not verified",
		},
		{
			name: "invalid: project-scoped SA for hub-scoped hook",
			hook: func() *store.LifecycleHook {
				h := validHTTPHook()
				h.ExecutionIdentity = "sa-003"
				return h
			}(),
			wantErr: true,
			errMsg:  "hub-scoped hook requires a hub-scoped service account",
		},
		{
			name: "valid: project-scoped SA for project-scoped hook (matching scope)",
			hook: func() *store.LifecycleHook {
				h := validHTTPHook()
				h.ScopeType = store.LifecycleHookScopeProject
				h.ScopeID = "proj-123"
				h.ExecutionIdentity = "sa-003"
				return h
			}(),
			wantErr: false,
		},
		{
			name: "invalid: project-scoped hook with wrong scope SA",
			hook: func() *store.LifecycleHook {
				h := validHTTPHook()
				h.ScopeType = store.LifecycleHookScopeProject
				h.ScopeID = "proj-999"
				h.ExecutionIdentity = "sa-003" // scoped to proj-123
				return h
			}(),
			wantErr: true,
			errMsg:  "project-scoped hook requires a service account in the same project",
		},
		{
			name: "invalid: http action without execution_identity",
			hook: func() *store.LifecycleHook {
				h := validHTTPHook()
				h.ExecutionIdentity = ""
				return h
			}(),
			wantErr: true,
			errMsg:  "required for http action type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateHook(context.Background(), tc.hook, defaultResolver())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errMsg)
				}
				if tc.errMsg != "" && !strings.Contains(err.Error(), tc.errMsg) {
					t.Errorf("expected error containing %q, got: %v", tc.errMsg, err)
				}
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateHook — header name injection
// ---------------------------------------------------------------------------

func TestValidateHook_HeaderNameInjection(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		wantErr bool
	}{
		{
			name:    "valid: standard headers",
			headers: map[string]string{"Content-Type": "application/json", "X-Custom": "value"},
			wantErr: false,
		},
		{
			name:    "invalid: header with space",
			headers: map[string]string{"Invalid Header": "value"},
			wantErr: true,
		},
		{
			name:    "invalid: header with colon",
			headers: map[string]string{"Invalid:Header": "value"},
			wantErr: true,
		},
		{
			name:    "invalid: header with newline",
			headers: map[string]string{"Invalid\nHeader": "value"},
			wantErr: true,
		},
		{
			name:    "invalid: empty header name",
			headers: map[string]string{"": "value"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := validHTTPHook()
			h.Action.Headers = tc.headers
			err := ValidateHook(context.Background(), h, defaultResolver())
			if tc.wantErr && err == nil {
				t.Error("expected validation error for header name injection, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateHook — on_error validation (T5: empty defaults to log)
// ---------------------------------------------------------------------------

func TestValidateHook_OnError(t *testing.T) {
	tests := []struct {
		name    string
		onError string
		wantErr bool
	}{
		{"valid: log", "log", false},
		{"valid: retry", "retry", false},
		{"valid: empty (defaults to log)", "", false},
		{"invalid: fail", "fail", true},
		{"invalid: ignore", "ignore", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := validHTTPHook()
			h.Action.OnError = tc.onError
			err := ValidateHook(context.Background(), h, defaultResolver())
			if tc.wantErr && err == nil {
				t.Errorf("expected validation error for onError %q, got nil", tc.onError)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for onError %q: %v", tc.onError, err)
			}
		})
	}
}

// T5: Verify that empty on_error is normalized to "log" after validation.
func TestValidateHook_OnErrorDefaultsToLog(t *testing.T) {
	h := validHTTPHook()
	h.Action.OnError = ""

	err := ValidateHook(context.Background(), h, defaultResolver())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.Action.OnError != "log" {
		t.Errorf("expected on_error to default to %q, got %q", "log", h.Action.OnError)
	}
}

// ---------------------------------------------------------------------------
// ValidateHook — nil action
// ---------------------------------------------------------------------------

func TestValidateHook_NilAction(t *testing.T) {
	h := &store.LifecycleHook{
		Trigger:   "running",
		ScopeType: "hub",
		Action:    nil,
	}
	err := ValidateHook(context.Background(), h, defaultResolver())
	if err == nil {
		t.Fatal("expected error for nil action, got nil")
	}
	if !strings.Contains(err.Error(), "action") {
		t.Errorf("expected error about action, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// S2: http action type requires https (bearer token protection)
// ---------------------------------------------------------------------------

func TestValidateHook_HTTPActionRequiresHTTPS(t *testing.T) {
	tests := []struct {
		name    string
		hook    *store.LifecycleHook
		wantErr bool
		errMsg  string
	}{
		{
			name: "S2: http:// with http action type -> REJECTED (bearer in cleartext)",
			hook: &store.LifecycleHook{
				ID:        "hook-s2-1",
				ScopeType: "hub",
				Trigger:   "running",
				Action: &store.LifecycleHookAction{
					Type:           "http",
					Method:         "POST",
					URL:            "http://internal.corp/api/register",
					TimeoutSeconds: 10,
				},
				ExecutionIdentity: "sa-001",
			},
			wantErr: true,
			errMsg:  "requires https",
		},
		{
			name: "S2: https:// with http action type -> OK",
			hook: &store.LifecycleHook{
				ID:        "hook-s2-2",
				ScopeType: "hub",
				Trigger:   "running",
				Action: &store.LifecycleHookAction{
					Type:           "http",
					Method:         "POST",
					URL:            "https://registry.example.com/agents",
					TimeoutSeconds: 10,
				},
				ExecutionIdentity: "sa-001",
			},
			wantErr: false,
		},
		{
			name: "S2: http:// with webhook action type -> OK (no bearer attached)",
			hook: &store.LifecycleHook{
				ID:        "hook-s2-3",
				ScopeType: "hub",
				Trigger:   "running",
				Action: &store.LifecycleHookAction{
					Type:           "webhook",
					Method:         "POST",
					URL:            "http://internal.corp/webhook",
					TimeoutSeconds: 5,
				},
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateHook(context.Background(), tc.hook, defaultResolver())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errMsg)
				}
				if !strings.Contains(err.Error(), tc.errMsg) {
					t.Errorf("expected error containing %q, got: %v", tc.errMsg, err)
				}
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// IsValidationError
// ---------------------------------------------------------------------------

func TestIsValidationError(t *testing.T) {
	ve := &ValidationError{Errors: []FieldError{{Field: "test", Message: "msg"}}}
	if !IsValidationError(ve) {
		t.Error("expected IsValidationError to return true for *ValidationError")
	}
	if IsValidationError(store.ErrNotFound) {
		t.Error("expected IsValidationError to return false for non-ValidationError")
	}
}
