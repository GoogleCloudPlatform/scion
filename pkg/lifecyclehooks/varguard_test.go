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
// ClassifyVar
// ---------------------------------------------------------------------------

func TestClassifyVar(t *testing.T) {
	tests := []struct {
		name     string
		variable string
		want     VarTrust
	}{
		// Trusted
		{"trusted: HOOK_ID", "HOOK_ID", Trusted},
		{"trusted: PROJECT_ID", "PROJECT_ID", Trusted},
		{"trusted: AGENT_ID", "AGENT_ID", Trusted},
		{"trusted: SA_EMAIL", "SA_EMAIL", Trusted},

		// Untrusted
		{"untrusted: AGENT_NAME", "AGENT_NAME", Untrusted},
		{"untrusted: TASK_SUMMARY", "TASK_SUMMARY", Untrusted},
		{"untrusted: ERROR_MSG", "ERROR_MSG", Untrusted},

		// Unknown defaults to untrusted
		{"unknown: CUSTOM_VAR", "CUSTOM_VAR", Untrusted},
		{"unknown: RANDOM_THING", "RANDOM_THING", Untrusted},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyVar(tc.variable)
			if got != tc.want {
				t.Errorf("ClassifyVar(%q) = %d, want %d", tc.variable, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateActionVariables — static validation (security-critical tests)
// ---------------------------------------------------------------------------

func TestValidateActionVariables_SSRF(t *testing.T) {
	// SSRF / path manipulation: untrusted var in host or path → REJECTED.
	tests := []struct {
		name    string
		action  *store.LifecycleHookAction
		wantErr bool
		errMsg  string
	}{
		{
			name: "REJECTED: untrusted var in URL host (SSRF)",
			action: &store.LifecycleHookAction{
				Type:           "http",
				Method:         "POST",
				URL:            "https://${AGENT_NAME}.evil.com/api/register",
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "SSRF risk",
		},
		{
			name: "REJECTED: untrusted var in URL path",
			action: &store.LifecycleHookAction{
				Type:           "http",
				Method:         "POST",
				URL:            "https://registry.example.com/${TASK_SUMMARY}/register",
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "SSRF risk",
		},
		{
			name: "REJECTED: untrusted var as entire host",
			action: &store.LifecycleHookAction{
				Type:           "http",
				Method:         "POST",
				URL:            "https://${AGENT_NAME}/api",
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "SSRF risk",
		},
		{
			name: "REJECTED: unknown var in path defaults to untrusted",
			action: &store.LifecycleHookAction{
				Type:           "http",
				Method:         "POST",
				URL:            "https://registry.example.com/${UNKNOWN_VAR}/register",
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "SSRF risk",
		},
		{
			name: "REJECTED: ERROR_MSG in path (untrusted)",
			action: &store.LifecycleHookAction{
				Type:           "http",
				Method:         "POST",
				URL:            "https://registry.example.com/agents/${ERROR_MSG}",
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "SSRF risk",
		},
		{
			name: "PASSES: trusted var in URL path",
			action: &store.LifecycleHookAction{
				Type:           "http",
				Method:         "POST",
				URL:            "https://registry.example.com/${PROJECT_ID}/agents",
				TimeoutSeconds: 10,
			},
			wantErr: false,
		},
		{
			name: "PASSES: trusted var in URL host",
			action: &store.LifecycleHookAction{
				Type:           "http",
				Method:         "POST",
				URL:            "https://${AGENT_SLUG}.registry.example.com/agents",
				TimeoutSeconds: 10,
			},
			wantErr: false,
		},
		{
			// B4: Untrusted var in query is now also rejected.
			name: "REJECTED: untrusted var in query (no allow-list)",
			action: &store.LifecycleHookAction{
				Type:           "http",
				Method:         "POST",
				URL:            "https://registry.example.com/agents?name=${AGENT_NAME}",
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "not allowed in URL query",
		},
		{
			// B4: Even allow-listed untrusted var in query is rejected.
			name: "REJECTED: allow-listed untrusted var in query (allowed only in body)",
			action: &store.LifecycleHookAction{
				Type:                 "http",
				Method:               "POST",
				URL:                  "https://registry.example.com/agents?name=${AGENT_NAME}",
				AllowedUntrustedVars: []string{"AGENT_NAME"},
				TimeoutSeconds:       10,
			},
			wantErr: true,
			errMsg:  "allowed only in body",
		},
		{
			name: "PASSES: no variables at all",
			action: &store.LifecycleHookAction{
				Type:           "http",
				Method:         "POST",
				URL:            "https://registry.example.com/agents",
				TimeoutSeconds: 10,
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateActionVariables(tc.action)
			if tc.wantErr {
				if len(errs) == 0 {
					t.Fatalf("expected validation error containing %q, got none", tc.errMsg)
				}
				found := false
				for _, e := range errs {
					if strings.Contains(e.Message, tc.errMsg) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected error containing %q, got: %v", tc.errMsg, errs)
				}
			} else if len(errs) > 0 {
				t.Errorf("unexpected errors: %v", errs)
			}
		})
	}
}

// T1: Untrusted var in a NON-auth header value -> REJECTED at validation.
func TestValidateActionVariables_UntrustedInNonAuthHeader(t *testing.T) {
	tests := []struct {
		name    string
		action  *store.LifecycleHookAction
		wantErr bool
		errMsg  string
	}{
		{
			name: "REJECTED: untrusted var in X-Forwarded-For header (B1)",
			action: &store.LifecycleHookAction{
				Type:   "http",
				Method: "POST",
				URL:    "https://registry.example.com/agents",
				Headers: map[string]string{
					"X-Forwarded-For": "${AGENT_NAME}",
				},
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "untrusted variable ${AGENT_NAME} not allowed in header value",
		},
		{
			name: "REJECTED: untrusted var in X-Note header (B1)",
			action: &store.LifecycleHookAction{
				Type:   "http",
				Method: "POST",
				URL:    "https://registry.example.com/agents",
				Headers: map[string]string{
					"X-Note": "${TASK_SUMMARY}",
				},
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "untrusted variable ${TASK_SUMMARY} not allowed in header value",
		},
		{
			// T1: Cookie header with untrusted var, but Cookie is also an
			// auth header (B3) — rejected either way.
			name: "REJECTED: untrusted var in Cookie header (B1 + B3)",
			action: &store.LifecycleHookAction{
				Type:   "http",
				Method: "POST",
				URL:    "https://registry.example.com/agents",
				Headers: map[string]string{
					"Cookie": "session=${AGENT_NAME}",
				},
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "not allowed in header value",
		},
		{
			name: "REJECTED: unknown var in arbitrary header (defaults to untrusted)",
			action: &store.LifecycleHookAction{
				Type:   "http",
				Method: "POST",
				URL:    "https://registry.example.com/agents",
				Headers: map[string]string{
					"X-Custom": "${UNKNOWN_VAR}",
				},
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "not allowed in header value",
		},
		{
			// Even allow-listed untrusted vars must not appear in headers.
			name: "REJECTED: allow-listed untrusted var in header (allowed only in body)",
			action: &store.LifecycleHookAction{
				Type:   "http",
				Method: "POST",
				URL:    "https://registry.example.com/agents",
				Headers: map[string]string{
					"X-Note": "${AGENT_NAME}",
				},
				AllowedUntrustedVars: []string{"AGENT_NAME"},
				TimeoutSeconds:       10,
			},
			wantErr: true,
			errMsg:  "allowed only in body",
		},
		{
			name: "PASSES: trusted var in non-auth header",
			action: &store.LifecycleHookAction{
				Type:   "http",
				Method: "POST",
				URL:    "https://registry.example.com/agents",
				Headers: map[string]string{
					"X-Project": "${PROJECT_ID}",
				},
				TimeoutSeconds: 10,
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateActionVariables(tc.action)
			if tc.wantErr {
				if len(errs) == 0 {
					t.Fatalf("expected validation error containing %q, got none", tc.errMsg)
				}
				found := false
				for _, e := range errs {
					if strings.Contains(e.Message, tc.errMsg) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected error containing %q, got: %v", tc.errMsg, errs)
				}
			} else if len(errs) > 0 {
				t.Errorf("unexpected errors: %v", errs)
			}
		})
	}
}

func TestValidateActionVariables_AuthHeaderInjection(t *testing.T) {
	// Auth-header injection: untrusted var in auth header value → REJECTED (B1 covers all headers).
	tests := []struct {
		name    string
		action  *store.LifecycleHookAction
		wantErr bool
		errMsg  string
	}{
		{
			name: "REJECTED: untrusted var in Authorization header",
			action: &store.LifecycleHookAction{
				Type:   "http",
				Method: "POST",
				URL:    "https://registry.example.com/agents",
				Headers: map[string]string{
					"Authorization": "Bearer ${AGENT_NAME}",
				},
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "not allowed in header value",
		},
		{
			name: "REJECTED: untrusted var in X-Api-Key",
			action: &store.LifecycleHookAction{
				Type:   "http",
				Method: "POST",
				URL:    "https://registry.example.com/agents",
				Headers: map[string]string{
					"X-Api-Key": "${TASK_SUMMARY}",
				},
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "not allowed in header value",
		},
		{
			name: "REJECTED: untrusted var in X-Auth-Token",
			action: &store.LifecycleHookAction{
				Type:   "http",
				Method: "POST",
				URL:    "https://registry.example.com/agents",
				Headers: map[string]string{
					"X-Auth-Token": "${ERROR_MSG}",
				},
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "not allowed in header value",
		},
		{
			name: "REJECTED: unknown var in auth header (defaults to untrusted)",
			action: &store.LifecycleHookAction{
				Type:   "http",
				Method: "POST",
				URL:    "https://registry.example.com/agents",
				Headers: map[string]string{
					"Authorization": "Bearer ${UNKNOWN_VAR}",
				},
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "not allowed in header value",
		},
		{
			name: "PASSES: trusted var in Authorization header",
			action: &store.LifecycleHookAction{
				Type:   "http",
				Method: "POST",
				URL:    "https://registry.example.com/agents",
				Headers: map[string]string{
					"Authorization": "Bearer ${SA_EMAIL}",
				},
				TimeoutSeconds: 10,
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateActionVariables(tc.action)
			if tc.wantErr {
				if len(errs) == 0 {
					t.Fatalf("expected validation error containing %q, got none", tc.errMsg)
				}
				found := false
				for _, e := range errs {
					if strings.Contains(e.Message, tc.errMsg) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected error containing %q, got: %v", tc.errMsg, errs)
				}
			} else if len(errs) > 0 {
				t.Errorf("unexpected errors: %v", errs)
			}
		})
	}
}

func TestValidateActionVariables_HeaderNameInjection(t *testing.T) {
	// Header-name injection: any var in header name → REJECTED.
	tests := []struct {
		name    string
		action  *store.LifecycleHookAction
		wantErr bool
		errMsg  string
	}{
		{
			name: "REJECTED: variable in header name",
			action: &store.LifecycleHookAction{
				Type:   "http",
				Method: "POST",
				URL:    "https://registry.example.com/agents",
				Headers: map[string]string{
					"X-${AGENT_NAME}": "value",
				},
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "not allowed in header name",
		},
		{
			name: "REJECTED: trusted variable in header name (still not allowed)",
			action: &store.LifecycleHookAction{
				Type:   "http",
				Method: "POST",
				URL:    "https://registry.example.com/agents",
				Headers: map[string]string{
					"X-${PROJECT_ID}": "value",
				},
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "not allowed in header name",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateActionVariables(tc.action)
			if tc.wantErr {
				if len(errs) == 0 {
					t.Fatalf("expected validation error containing %q, got none", tc.errMsg)
				}
				found := false
				for _, e := range errs {
					if strings.Contains(e.Message, tc.errMsg) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected error containing %q, got: %v", tc.errMsg, errs)
				}
			} else if len(errs) > 0 {
				t.Errorf("unexpected errors: %v", errs)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// T3: Body allow-list tests (B4)
// ---------------------------------------------------------------------------

func TestValidateActionVariables_BodyAllowList(t *testing.T) {
	tests := []struct {
		name    string
		action  *store.LifecycleHookAction
		wantErr bool
		errMsg  string
	}{
		{
			name: "REJECTED: untrusted var in body NOT in AllowedUntrustedVars",
			action: &store.LifecycleHookAction{
				Type:           "http",
				Method:         "POST",
				URL:            "https://registry.example.com/agents",
				Body:           `{"name": "${AGENT_NAME}"}`,
				TimeoutSeconds: 10,
			},
			wantErr: true,
			errMsg:  "not in AllowedUntrustedVars",
		},
		{
			name: "PASSES: untrusted var in body IN AllowedUntrustedVars, inside JSON string",
			action: &store.LifecycleHookAction{
				Type:                 "http",
				Method:               "POST",
				URL:                  "https://registry.example.com/agents",
				Body:                 `{"name": "${AGENT_NAME}"}`,
				AllowedUntrustedVars: []string{"AGENT_NAME"},
				TimeoutSeconds:       10,
			},
			wantErr: false,
		},
		{
			name: "PASSES: multiple allow-listed untrusted vars in body",
			action: &store.LifecycleHookAction{
				Type:                 "http",
				Method:               "POST",
				URL:                  "https://registry.example.com/agents",
				Body:                 `{"name": "${AGENT_NAME}", "error": "${ERROR_MSG}"}`,
				AllowedUntrustedVars: []string{"AGENT_NAME", "ERROR_MSG"},
				TimeoutSeconds:       10,
			},
			wantErr: false,
		},
		{
			name: "REJECTED: one untrusted var allow-listed, another not",
			action: &store.LifecycleHookAction{
				Type:                 "http",
				Method:               "POST",
				URL:                  "https://registry.example.com/agents",
				Body:                 `{"name": "${AGENT_NAME}", "error": "${ERROR_MSG}"}`,
				AllowedUntrustedVars: []string{"AGENT_NAME"}, // ERROR_MSG not listed
				TimeoutSeconds:       10,
			},
			wantErr: true,
			errMsg:  "ERROR_MSG",
		},
		{
			name: "PASSES: trusted var in body (no allow-list needed)",
			action: &store.LifecycleHookAction{
				Type:           "http",
				Method:         "POST",
				URL:            "https://registry.example.com/agents",
				Body:           `{"project": "${PROJECT_ID}"}`,
				TimeoutSeconds: 10,
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateActionVariables(tc.action)
			if tc.wantErr {
				if len(errs) == 0 {
					t.Fatalf("expected validation error containing %q, got none", tc.errMsg)
				}
				found := false
				for _, e := range errs {
					if strings.Contains(e.Message, tc.errMsg) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected error containing %q, got: %v", tc.errMsg, errs)
				}
			} else if len(errs) > 0 {
				t.Errorf("unexpected errors: %v", errs)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// T4: Untrusted var in non-string body position (B5)
// ---------------------------------------------------------------------------

func TestValidateActionVariables_BodyPositionalSafety(t *testing.T) {
	tests := []struct {
		name    string
		action  *store.LifecycleHookAction
		wantErr bool
		errMsg  string
	}{
		{
			// JSON keys are syntactically string literals, so the var IS
			// inside a quoted context. jsonEncodeValue still escapes the
			// value, so structural injection is prevented. This passes
			// the positional check (keys are quoted strings).
			name: "PASSES: untrusted var as JSON key (keys are quoted strings)",
			action: &store.LifecycleHookAction{
				Type:                 "http",
				Method:               "POST",
				URL:                  "https://registry.example.com/agents",
				Body:                 `{"${AGENT_NAME}": "value"}`,
				AllowedUntrustedVars: []string{"AGENT_NAME"},
				TimeoutSeconds:       10,
			},
			wantErr: false,
		},
		{
			name: "REJECTED: untrusted var in numeric position",
			action: &store.LifecycleHookAction{
				Type:                 "http",
				Method:               "POST",
				URL:                  "https://registry.example.com/agents",
				Body:                 `{"count": ${AGENT_NAME}}`,
				AllowedUntrustedVars: []string{"AGENT_NAME"},
				TimeoutSeconds:       10,
			},
			wantErr: true,
			errMsg:  "must be inside a JSON string literal",
		},
		{
			name: "REJECTED: untrusted var in boolean position",
			action: &store.LifecycleHookAction{
				Type:                 "http",
				Method:               "POST",
				URL:                  "https://registry.example.com/agents",
				Body:                 `{"active": ${AGENT_NAME}}`,
				AllowedUntrustedVars: []string{"AGENT_NAME"},
				TimeoutSeconds:       10,
			},
			wantErr: true,
			errMsg:  "must be inside a JSON string literal",
		},
		{
			name: "REJECTED: untrusted var at top level (not in quotes)",
			action: &store.LifecycleHookAction{
				Type:                 "http",
				Method:               "POST",
				URL:                  "https://registry.example.com/agents",
				Body:                 `${AGENT_NAME}`,
				AllowedUntrustedVars: []string{"AGENT_NAME"},
				TimeoutSeconds:       10,
			},
			wantErr: true,
			errMsg:  "must be inside a JSON string literal",
		},
		{
			name: "PASSES: untrusted var inside JSON string value",
			action: &store.LifecycleHookAction{
				Type:                 "http",
				Method:               "POST",
				URL:                  "https://registry.example.com/agents",
				Body:                 `{"name": "${AGENT_NAME}"}`,
				AllowedUntrustedVars: []string{"AGENT_NAME"},
				TimeoutSeconds:       10,
			},
			wantErr: false,
		},
		{
			name: "PASSES: untrusted var inside quoted string with prefix",
			action: &store.LifecycleHookAction{
				Type:                 "http",
				Method:               "POST",
				URL:                  "https://registry.example.com/agents",
				Body:                 `{"label": "agent-${AGENT_NAME}-prod"}`,
				AllowedUntrustedVars: []string{"AGENT_NAME"},
				TimeoutSeconds:       10,
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateActionVariables(tc.action)
			if tc.wantErr {
				if len(errs) == 0 {
					t.Fatalf("expected validation error containing %q, got none", tc.errMsg)
				}
				found := false
				for _, e := range errs {
					if strings.Contains(e.Message, tc.errMsg) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected error containing %q, got: %v", tc.errMsg, errs)
				}
			} else if len(errs) > 0 {
				t.Errorf("unexpected errors: %v", errs)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// T2: Cookie/Set-Cookie auth-header handling (via full ValidateHook)
// ---------------------------------------------------------------------------

func TestValidateHook_CookieAuthHeaders(t *testing.T) {
	tests := []struct {
		name    string
		hook    *store.LifecycleHook
		wantErr bool
		errMsg  string
	}{
		{
			name: "http: Cookie header with trusted var passes",
			hook: &store.LifecycleHook{
				ID:        "hook-cookie",
				ScopeType: "hub",
				Trigger:   "running",
				Action: &store.LifecycleHookAction{
					Type:   "http",
					Method: "POST",
					URL:    "https://registry.example.com/agents",
					Headers: map[string]string{
						"Cookie": "session=${SA_EMAIL}",
					},
					TimeoutSeconds: 10,
				},
				ExecutionIdentity: "sa-001",
			},
			wantErr: false,
		},
		{
			name: "webhook: Cookie header rejected as auth header (B3)",
			hook: &store.LifecycleHook{
				Trigger:   "running",
				ScopeType: "hub",
				Action: &store.LifecycleHookAction{
					Type: "webhook",
					URL:  "https://hooks.slack.com/services/T00/B00/xxx",
					Headers: map[string]string{
						"Cookie": "session=abc",
					},
					TimeoutSeconds: 5,
				},
			},
			wantErr: true,
			errMsg:  "authentication headers are not allowed on webhook",
		},
		{
			name: "webhook: Set-Cookie header rejected as auth header (B3)",
			hook: &store.LifecycleHook{
				Trigger:   "running",
				ScopeType: "hub",
				Action: &store.LifecycleHookAction{
					Type: "webhook",
					URL:  "https://hooks.slack.com/services/T00/B00/xxx",
					Headers: map[string]string{
						"Set-Cookie": "session=abc; Path=/",
					},
					TimeoutSeconds: 5,
				},
			},
			wantErr: true,
			errMsg:  "authentication headers are not allowed on webhook",
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
// RenderAction — execution-time encoding tests
// ---------------------------------------------------------------------------

func TestRenderAction_URLParamInjection(t *testing.T) {
	// URL param injection: untrusted var in query → PERCENT-ENCODED
	// (defense-in-depth even though validation now rejects).
	action := &store.LifecycleHookAction{
		Type:           "http",
		Method:         "POST",
		URL:            "https://registry.example.com/agents?name=${AGENT_NAME}&project=${PROJECT_ID}",
		TimeoutSeconds: 10,
	}

	vars := RenderVars{
		"AGENT_NAME": "evil&other=injected",
		"PROJECT_ID": "proj-123",
	}

	rendered := RenderAction(action, vars)

	// AGENT_NAME (untrusted) should be percent-encoded.
	if !strings.Contains(rendered.URL, "name=evil%26other%3Dinjected") {
		t.Errorf("expected percent-encoded AGENT_NAME in URL query, got: %s", rendered.URL)
	}

	// PROJECT_ID (trusted) should be verbatim.
	if !strings.Contains(rendered.URL, "project=proj-123") {
		t.Errorf("expected verbatim PROJECT_ID in URL query, got: %s", rendered.URL)
	}
}

func TestRenderAction_JSONBodyInjection(t *testing.T) {
	// JSON field/annotation injection: untrusted var in body → JSON-ENCODED.
	action := &store.LifecycleHookAction{
		Type:           "http",
		Method:         "POST",
		URL:            "https://registry.example.com/agents",
		Body:           `{"name": "${AGENT_NAME}", "project": "${PROJECT_ID}"}`,
		TimeoutSeconds: 10,
	}

	vars := RenderVars{
		"AGENT_NAME": `evil", "admin": true, "x": "`,
		"PROJECT_ID": "proj-123",
	}

	rendered := RenderAction(action, vars)

	// AGENT_NAME (untrusted) should be JSON-encoded, preventing structure injection.
	// The malicious value should have its quotes escaped.
	if strings.Contains(rendered.Body, `"admin": true`) {
		t.Errorf("JSON injection succeeded — untrusted value broke out of JSON string: %s", rendered.Body)
	}

	// The rendered body should be valid JSON when the template is valid.
	// Specifically, the escaped value should contain backslash-escaped quotes.
	if !strings.Contains(rendered.Body, `evil\", \"admin\": true, \"x\": \"`) {
		t.Errorf("expected JSON-escaped AGENT_NAME in body, got: %s", rendered.Body)
	}

	// PROJECT_ID (trusted) should be verbatim.
	if !strings.Contains(rendered.Body, `"project": "proj-123"`) {
		t.Errorf("expected verbatim PROJECT_ID in body, got: %s", rendered.Body)
	}
}

func TestRenderAction_BodyWithNewlinesAndSpecialChars(t *testing.T) {
	action := &store.LifecycleHookAction{
		Type:           "http",
		Method:         "POST",
		URL:            "https://registry.example.com/agents",
		Body:           `{"summary": "${TASK_SUMMARY}"}`,
		TimeoutSeconds: 10,
	}

	vars := RenderVars{
		"TASK_SUMMARY": "line1\nline2\ttab\"quote\\backslash",
	}

	rendered := RenderAction(action, vars)

	// Should not contain raw newline or tab (they'd be JSON-encoded).
	if strings.Contains(rendered.Body, "\n") {
		t.Errorf("raw newline found in rendered body (should be JSON-encoded): %s", rendered.Body)
	}
	if strings.Contains(rendered.Body, "\t") {
		t.Errorf("raw tab found in rendered body (should be JSON-encoded): %s", rendered.Body)
	}
}

func TestRenderAction_TrustedHeaderSubstitution(t *testing.T) {
	action := &store.LifecycleHookAction{
		Type:   "http",
		Method: "POST",
		URL:    "https://registry.example.com/agents",
		Headers: map[string]string{
			"Authorization": "Bearer ${SA_EMAIL}",
			"X-Project":     "${PROJECT_ID}",
		},
		TimeoutSeconds: 10,
	}

	vars := RenderVars{
		"SA_EMAIL":   "hooks@example.iam.gserviceaccount.com",
		"PROJECT_ID": "proj-123",
	}

	rendered := RenderAction(action, vars)

	if rendered.Headers["Authorization"] != "Bearer hooks@example.iam.gserviceaccount.com" {
		t.Errorf("expected SA_EMAIL substituted in auth header, got: %s", rendered.Headers["Authorization"])
	}
	if rendered.Headers["X-Project"] != "proj-123" {
		t.Errorf("expected PROJECT_ID substituted in header, got: %s", rendered.Headers["X-Project"])
	}
}

// T6: Render-time: confirm no untrusted value can reach a header verbatim.
func TestRenderAction_UntrustedVarBlankedInHeader(t *testing.T) {
	action := &store.LifecycleHookAction{
		Type:   "http",
		Method: "POST",
		URL:    "https://registry.example.com/agents",
		Headers: map[string]string{
			"X-Agent-Name": "agent-${AGENT_NAME}",
			"X-Status":     "${AGENT_STATUS}",
			"X-Error":      "${ERROR_MSG}",
		},
		TimeoutSeconds: 10,
	}

	vars := RenderVars{
		"AGENT_NAME":   "evil-value",
		"AGENT_STATUS": "compromised",
		"ERROR_MSG":    "attack\r\nX-Injected: true",
	}

	rendered := RenderAction(action, vars)

	// B2: Untrusted vars should be blanked in headers.
	if strings.Contains(rendered.Headers["X-Agent-Name"], "evil-value") {
		t.Errorf("untrusted AGENT_NAME leaked into header: %s", rendered.Headers["X-Agent-Name"])
	}
	if rendered.Headers["X-Status"] == "compromised" {
		t.Errorf("untrusted AGENT_STATUS leaked into header verbatim: %s", rendered.Headers["X-Status"])
	}
	if strings.Contains(rendered.Headers["X-Error"], "attack") {
		t.Errorf("untrusted ERROR_MSG leaked into header: %s", rendered.Headers["X-Error"])
	}
}

// T6 additional: trusted header values have CR/LF stripped.
func TestRenderAction_HeaderCRLFSanitization(t *testing.T) {
	action := &store.LifecycleHookAction{
		Type:   "http",
		Method: "POST",
		URL:    "https://registry.example.com/agents",
		Headers: map[string]string{
			"X-Project": "${PROJECT_ID}",
		},
		TimeoutSeconds: 10,
	}

	vars := RenderVars{
		"PROJECT_ID": "proj-123\r\nX-Injected: true",
	}

	rendered := RenderAction(action, vars)

	// The critical safety property: CR (\r) and LF (\n) are stripped so
	// the value cannot inject a new HTTP header line.
	if strings.Contains(rendered.Headers["X-Project"], "\r") || strings.Contains(rendered.Headers["X-Project"], "\n") {
		t.Errorf("CR/LF not stripped from trusted header value: %q", rendered.Headers["X-Project"])
	}
	// After stripping, the value collapses to "proj-123X-Injected: true"
	// which is a single (malformed but harmless) header value — the newline
	// that would have split it into a separate header is gone.
	want := "proj-123X-Injected: true"
	if rendered.Headers["X-Project"] != want {
		t.Errorf("expected sanitized value %q, got %q", want, rendered.Headers["X-Project"])
	}
}

// CR/LF present in the STATIC part of a header template (not inside a variable
// value) must also be stripped, since sanitization runs on the fully rendered
// value.
func TestRenderAction_HeaderCRLFSanitization_StaticTemplate(t *testing.T) {
	action := &store.LifecycleHookAction{
		Type:   "http",
		Method: "POST",
		URL:    "https://registry.example.com/agents",
		Headers: map[string]string{
			"X-Static": "safe\r\nX-Injected: true",
		},
		TimeoutSeconds: 10,
	}

	rendered := RenderAction(action, RenderVars{})

	got := rendered.Headers["X-Static"]
	if strings.Contains(got, "\r") || strings.Contains(got, "\n") {
		t.Errorf("CR/LF not stripped from static header template: %q", got)
	}
	if want := "safeX-Injected: true"; got != want {
		t.Errorf("expected sanitized static value %q, got %q", want, got)
	}
}

func TestRenderAction_UnresolvedVarsLeftAsIs(t *testing.T) {
	action := &store.LifecycleHookAction{
		Type:           "http",
		Method:         "POST",
		URL:            "https://registry.example.com/${PROJECT_ID}/agents?name=${AGENT_NAME}",
		Body:           `{"hook": "${HOOK_ID}"}`,
		TimeoutSeconds: 10,
	}

	// Provide no vars — all should remain as-is.
	rendered := RenderAction(action, RenderVars{})

	if !strings.Contains(rendered.URL, "${PROJECT_ID}") {
		t.Errorf("expected unresolved ${PROJECT_ID} in URL, got: %s", rendered.URL)
	}
	if !strings.Contains(rendered.URL, "${AGENT_NAME}") {
		t.Errorf("expected unresolved ${AGENT_NAME} in URL query, got: %s", rendered.URL)
	}
	if !strings.Contains(rendered.Body, "${HOOK_ID}") {
		t.Errorf("expected unresolved ${HOOK_ID} in body, got: %s", rendered.Body)
	}
}

func TestRenderAction_PreservesNonVarFields(t *testing.T) {
	action := &store.LifecycleHookAction{
		Type:           "webhook",
		Method:         "POST",
		URL:            "https://hooks.slack.com/services/T00/B00/xxx",
		TimeoutSeconds: 5,
		OnError:        "log",
	}

	rendered := RenderAction(action, RenderVars{})

	if rendered.Type != "webhook" {
		t.Errorf("expected type 'webhook', got: %s", rendered.Type)
	}
	if rendered.Method != "POST" {
		t.Errorf("expected method 'POST', got: %s", rendered.Method)
	}
	if rendered.TimeoutSeconds != 5 {
		t.Errorf("expected timeout 5, got: %d", rendered.TimeoutSeconds)
	}
	if rendered.OnError != "log" {
		t.Errorf("expected onError 'log', got: %s", rendered.OnError)
	}
}

// ---------------------------------------------------------------------------
// T3: RenderAction — allow-listed body usage with encoding
// ---------------------------------------------------------------------------

func TestRenderAction_AllowListedBodyUsage(t *testing.T) {
	// This tests that an allow-listed untrusted var in the body
	// passes validation AND is JSON-encoded at render time.
	action := &store.LifecycleHookAction{
		Type:   "http",
		Method: "POST",
		URL:    "https://registry.example.com/v1/agents",
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		Body:                 `{"agentId": "${AGENT_ID}", "agentName": "${AGENT_NAME}", "project": "${PROJECT_ID}"}`,
		AllowedUntrustedVars: []string{"AGENT_NAME"},
		TimeoutSeconds:       10,
	}

	// Static validation should pass.
	errs := ValidateActionVariables(action)
	if len(errs) > 0 {
		t.Fatalf("static validation failed for allow-listed hook: %v", errs)
	}

	vars := RenderVars{
		"AGENT_ID":   "agent-uuid-123",
		"AGENT_NAME": `My "Test" Agent`,
		"PROJECT_ID": "proj-456",
	}

	rendered := RenderAction(action, vars)

	// Trusted vars substituted verbatim.
	if !strings.Contains(rendered.Body, `"agentId": "agent-uuid-123"`) {
		t.Errorf("AGENT_ID not substituted in body: %s", rendered.Body)
	}
	if !strings.Contains(rendered.Body, `"project": "proj-456"`) {
		t.Errorf("PROJECT_ID not substituted in body: %s", rendered.Body)
	}

	// Untrusted var (AGENT_NAME) is JSON-encoded — quotes escaped.
	if strings.Contains(rendered.Body, `"Test"`) {
		t.Errorf("AGENT_NAME not JSON-encoded (raw quotes present): %s", rendered.Body)
	}
	if !strings.Contains(rendered.Body, `My \"Test\" Agent`) {
		t.Errorf("AGENT_NAME not properly JSON-encoded: %s", rendered.Body)
	}
}

// ---------------------------------------------------------------------------
// End-to-end: full hook validation + render pipeline
// ---------------------------------------------------------------------------

func TestEndToEnd_RegisterHookValidateAndRender(t *testing.T) {
	// Simulate the full flow: create a hook, validate it, then render it.
	hook := &store.LifecycleHook{
		ID:        "hook-e2e",
		Name:      "register-agent",
		ScopeType: "hub",
		Trigger:   "running",
		Action: &store.LifecycleHookAction{
			Type:   "http",
			Method: "POST",
			URL:    "https://registry.corp.internal/v1/agents/${AGENT_ID}",
			Headers: map[string]string{
				"Content-Type":  "application/json",
				"Authorization": "Bearer ${SA_EMAIL}",
			},
			Body:                 `{"id": "${AGENT_ID}", "name": "${AGENT_NAME}", "project": "${PROJECT_ID}", "error": "${ERROR_MSG}"}`,
			AllowedUntrustedVars: []string{"AGENT_NAME", "ERROR_MSG"},
			OnError:              "retry",
			TimeoutSeconds:       15,
		},
		ExecutionIdentity: "sa-001",
		Enabled:           true,
	}

	// Step 1: validate
	err := ValidateHook(context.Background(), hook, defaultResolver())
	if err != nil {
		t.Fatalf("hook validation failed: %v", err)
	}

	// Step 2: render
	vars := RenderVars{
		"AGENT_ID":   "agt-789",
		"AGENT_NAME": `Agent "Foo" & <Bar>`,
		"PROJECT_ID": "proj-abc",
		"SA_EMAIL":   "hooks@example.iam.gserviceaccount.com",
		"ERROR_MSG":  `crash: "null pointer"`,
	}

	rendered := RenderAction(hook.Action, vars)

	// Trusted vars in path → verbatim
	if !strings.Contains(rendered.URL, "/v1/agents/agt-789") {
		t.Errorf("AGENT_ID not in path: %s", rendered.URL)
	}

	// Trusted var in auth header → verbatim
	if rendered.Headers["Authorization"] != "Bearer hooks@example.iam.gserviceaccount.com" {
		t.Errorf("SA_EMAIL not in auth header: %s", rendered.Headers["Authorization"])
	}

	// Untrusted vars in body → JSON-encoded (no structure injection)
	if strings.Contains(rendered.Body, `"Foo" &`) {
		t.Errorf("AGENT_NAME not JSON-encoded in body: %s", rendered.Body)
	}
}

// ---------------------------------------------------------------------------
// extractVars
// ---------------------------------------------------------------------------

func TestExtractVars(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"no vars", nil},
		{"${FOO}", []string{"FOO"}},
		{"${FOO} and ${BAR}", []string{"FOO", "BAR"}},
		{"${FOO} ${FOO}", []string{"FOO"}}, // deduplication
		{"${A_B_C}", []string{"A_B_C"}},
		{"$FOO", nil},   // no braces
		{"${}", nil},    // empty
		{"${123}", nil}, // starts with digit
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := extractVars(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("extractVars(%q) = %v, want %v", tc.input, got, tc.want)
			}
			for i, v := range got {
				if v != tc.want[i] {
					t.Errorf("extractVars(%q)[%d] = %q, want %q", tc.input, i, v, tc.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// jsonEncodeValue
// ---------------------------------------------------------------------------

func TestJSONEncodeValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple", "hello", "hello"},
		{"with quotes", `say "hello"`, `say \"hello\"`},
		{"with backslash", `path\to\file`, `path\\to\\file`},
		{"with newline", "line1\nline2", `line1\nline2`},
		{"with tab", "col1\tcol2", `col1\tcol2`},
		{"json injection attempt", `", "admin": true, "x": "`, `\", \"admin\": true, \"x\": \"`},
		{"unicode", "café ☕", "café ☕"},
		{"empty", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := jsonEncodeValue(tc.input)
			if got != tc.want {
				t.Errorf("jsonEncodeValue(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// isInsideJSONString
// ---------------------------------------------------------------------------

func TestIsInsideJSONString(t *testing.T) {
	tests := []struct {
		name string
		s    string
		pos  int
		want bool
	}{
		{"outside: before any quote", `{"key": "val"}`, 0, false},
		{"inside: after opening quote", `{"key": "val"}`, 10, true},
		{"outside: between key and value", `{"key": "val"}`, 6, false},
		{"inside: key position", `{"key": "val"}`, 2, true},
		// Position 5 is the 'e' inside the key "k\"ey" — still inside the JSON string
		// because \" is an escape sequence, not a closing quote.
		{"inside: after escaped quote in key", `{"k\"ey": "val"}`, 5, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isInsideJSONString(tc.s, tc.pos)
			if got != tc.want {
				t.Errorf("isInsideJSONString(%q, %d) = %v, want %v", tc.s, tc.pos, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// D1: renderTrustedSubstitution blanks untrusted vars (defense-in-depth)
// ---------------------------------------------------------------------------

func TestRenderTrustedSubstitution_BlanksUntrustedVars(t *testing.T) {
	// D1: If an untrusted variable somehow appears in a URL host/path
	// position (bypassing static validation), it should be blanked
	// at render time as defense-in-depth.
	tests := []struct {
		name  string
		input string
		vars  RenderVars
		want  string
	}{
		{
			name:  "trusted var substituted verbatim",
			input: "https://registry.example.com/${PROJECT_ID}/agents",
			vars:  RenderVars{"PROJECT_ID": "proj-123"},
			want:  "https://registry.example.com/proj-123/agents",
		},
		{
			name:  "untrusted var blanked (defense-in-depth)",
			input: "https://registry.example.com/${AGENT_NAME}/agents",
			vars:  RenderVars{"AGENT_NAME": "evil-host.attacker.com"},
			want:  "https://registry.example.com//agents",
		},
		{
			name:  "unknown var blanked (defaults to untrusted)",
			input: "https://registry.example.com/${UNKNOWN_VAR}/agents",
			vars:  RenderVars{"UNKNOWN_VAR": "injected"},
			want:  "https://registry.example.com//agents",
		},
		{
			name:  "mix of trusted and untrusted",
			input: "https://${AGENT_SLUG}.example.com/${AGENT_NAME}/api",
			vars:  RenderVars{"AGENT_SLUG": "my-agent", "AGENT_NAME": "evil"},
			want:  "https://my-agent.example.com//api",
		},
		{
			name:  "unresolved var left as-is",
			input: "https://registry.example.com/${NOT_PROVIDED}/agents",
			vars:  RenderVars{},
			want:  "https://registry.example.com/${NOT_PROVIDED}/agents",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := renderTrustedSubstitution(tc.input, tc.vars)
			if got != tc.want {
				t.Errorf("renderTrustedSubstitution(%q, ...) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
