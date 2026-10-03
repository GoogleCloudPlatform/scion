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

//go:build !no_sqlite

// Tests for the agent_secrets.user_scope_only hub setting (design
// ptone/scion#2291, Option B — blanket rule): pkg/hub/handlers_env_secrets.go
// (the enforcement check in handleAgentSecrets) and pkg/hub/server.go
// (Server.agentSecretsUserScopeOnly, ServerConfig.AgentSecretsUserScopeOnly).

package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// scopePolicyCountingBackend wraps a secret.SecretBackend and counts Set and
// GetMeta calls, so tests can assert that a rejected write never reaches the
// backend at all (design §6, §11 AC2).
type scopePolicyCountingBackend struct {
	secret.SecretBackend
	setCalls     int
	getMetaCalls int
}

func (c *scopePolicyCountingBackend) Set(ctx context.Context, input *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	c.setCalls++
	return c.SecretBackend.Set(ctx, input)
}

func (c *scopePolicyCountingBackend) GetMeta(ctx context.Context, name, scope, scopeID string) (*secret.SecretMeta, error) {
	c.getMetaCalls++
	return c.SecretBackend.GetMeta(ctx, name, scope, scopeID)
}

// TestServer_AgentSecretsUserScopeOnly_DefaultAndValues verifies the
// accessor's permissive default: nil and false both mean "agents may write
// project scope", true means restricted (design §5.2).
func TestServer_AgentSecretsUserScopeOnly_DefaultAndValues(t *testing.T) {
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	if srv.agentSecretsUserScopeOnly() {
		t.Error("want agentSecretsUserScopeOnly()=false when unset (nil)")
	}

	srv.config.AgentSecretsUserScopeOnly = boolPtr(false)
	if srv.agentSecretsUserScopeOnly() {
		t.Error("want agentSecretsUserScopeOnly()=false when explicitly false")
	}

	srv.config.AgentSecretsUserScopeOnly = boolPtr(true)
	if !srv.agentSecretsUserScopeOnly() {
		t.Error("want agentSecretsUserScopeOnly()=true when explicitly true")
	}
}

// TestAgentSecretScopePolicy_Table is the full design §10 test 4 table: for
// setting in {nil, false, true} x scope in {"", "project", "user"} x force
// in {false, true}, verify the expected outcome. With the setting nil or
// false, every case behaves as today (201). With the setting true, "" and
// "project" return 403 secret_scope_restricted regardless of force, and the
// backend Set/GetMeta are never reached; "user" still returns 201.
func TestAgentSecretScopePolicy_Table(t *testing.T) {
	type tc struct {
		name        string
		setting     *bool
		scope       string
		force       bool
		wantStatus  int
		wantErrCode string
	}

	cases := []tc{}
	for _, settingName := range []string{"nil", "false", "true"} {
		var setting *bool
		switch settingName {
		case "false":
			setting = boolPtr(false)
		case "true":
			setting = boolPtr(true)
		}
		for _, scope := range []string{"", "project", "user"} {
			for _, force := range []bool{false, true} {
				restricted := setting != nil && *setting && scope != "user"
				wantStatus := http.StatusCreated
				wantErrCode := ""
				if restricted {
					wantStatus = http.StatusForbidden
					wantErrCode = ErrCodeSecretScopeRestricted
				}
				cases = append(cases, tc{
					name:        "setting=" + settingName + "/scope=" + scope + "/force=" + boolStr(force),
					setting:     setting,
					scope:       scope,
					force:       force,
					wantStatus:  wantStatus,
					wantErrCode: wantErrCode,
				})
			}
		}
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, agentID, projectID, agentToken := setupAgentSecretTest(t)
			counting := &scopePolicyCountingBackend{SecretBackend: srv.secretBackend}
			srv.SetSecretBackend(counting)
			srv.config.AgentSecretsUserScopeOnly = c.setting

			// A user-scoped write needs a token whose ancestry carries an
			// origin user (OriginUserID = Ancestry[0]); the default token
			// from setupAgentSecretTest has none, and that unrelated 403
			// (F1: "agent token lacks user context") must not be confused
			// with the scope-policy 403 under test.
			if c.scope == "user" {
				var err error
				agentToken, err = srv.agentTokenService.GenerateAgentToken(agentID, projectID, nil, []string{tid("origin-user")})
				if err != nil {
					t.Fatalf("failed to generate agent token with ancestry: %v", err)
				}
			}

			body := AgentSetSecretRequest{
				Value: base64.StdEncoding.EncodeToString([]byte("policy-table-value")),
				Scope: c.scope,
				Force: c.force,
			}
			key := "POLICY_KEY_" + c.name
			rec := doRequestWithAgentToken(t, srv, http.MethodPut,
				"/api/v1/agents/"+agentID+"/secrets/"+sanitizeTestKey(key), body, agentToken)

			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if c.wantErrCode != "" {
				var errResp ErrorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if errResp.Error.Code != c.wantErrCode {
					t.Errorf("error code = %q, want %q", errResp.Error.Code, c.wantErrCode)
				}
				if counting.setCalls != 0 {
					t.Errorf("Set called %d times, want 0 on a restricted write", counting.setCalls)
				}
				if counting.getMetaCalls != 0 {
					t.Errorf("GetMeta called %d times, want 0 on a restricted write", counting.getMetaCalls)
				}
			}
		})
	}
}

// TestAgentSecretScopePolicy_InvalidEncodingStillForbidden verifies that a
// restricted project-scope write is rejected before the value is even
// base64-decoded — a value that is not valid base64 (default encoding)
// does not change the outcome or leak past the policy check (design §6:
// "before ... decode").
func TestAgentSecretScopePolicy_InvalidEncodingStillForbidden(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)
	counting := &scopePolicyCountingBackend{SecretBackend: srv.secretBackend}
	srv.SetSecretBackend(counting)
	srv.config.AgentSecretsUserScopeOnly = boolPtr(true)

	body := AgentSetSecretRequest{
		Value: "not-valid-base64!!!",
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/BAD_ENCODING_KEY", body, agentToken)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Error.Code != ErrCodeSecretScopeRestricted {
		t.Errorf("error code = %q, want %q (not a validation_error for the bad encoding)", errResp.Error.Code, ErrCodeSecretScopeRestricted)
	}
	if counting.setCalls != 0 || counting.getMetaCalls != 0 {
		t.Errorf("backend reached: setCalls=%d getMetaCalls=%d, want 0/0", counting.setCalls, counting.getMetaCalls)
	}
}

// TestAgentSecretScopePolicy_UserIdentityProjectWriteUnaffected is the
// non-goal guard (design §2, §11 AC3): a user-identity PUT on the
// project-scoped secrets route must still succeed when the agent policy
// setting is on, because that route only serves user-originated writes,
// never agent writes (F1).
func TestAgentSecretScopePolicy_UserIdentityProjectWriteUnaffected(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	srv.config.AgentSecretsUserScopeOnly = boolPtr(true)
	ctx := context.Background()

	project := &store.Project{
		ID:      tid("project-policy-user-write"),
		Name:    "Policy User Write Project",
		Slug:    "policy-user-write-project",
		OwnerID: DevUserID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	createBody := SetSecretRequest{
		Value:         base64.StdEncoding.EncodeToString([]byte("user-write-value")),
		InjectionMode: "as_needed",
		Type:          "environment",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/USER_WRITE_KEY", createBody)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("user-identity project secret write expected success even with the agent policy on, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentSecretScopePolicy_LiveToggleViaApplySnapshot proves the setting
// takes effect immediately, without a restart, whether turned on or off
// (design §5.2, §11 AC4).
func TestAgentSecretScopePolicy_LiveToggleViaApplySnapshot(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	reqBody := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("toggle-value")),
		Scope: "project",
	}

	// Off (default): project write succeeds. Each phase uses its own key so
	// every write is a fresh create (201), not an update (204) — the
	// distinction under test is enforcement, not create-vs-update semantics.
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/TOGGLE_KEY_1", reqBody, agentToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 before toggling the setting on, got %d: %s", rec.Code, rec.Body.String())
	}

	// Flip on live via ApplySnapshot, as the admin PUT / propagation path does.
	ApplySnapshot(srv, Layer1Snapshot{AgentSecretsUserScopeOnly: boolPtr(true)})

	rec = doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/TOGGLE_KEY_2", reqBody, agentToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 immediately after ApplySnapshot turned the setting on, got %d: %s", rec.Code, rec.Body.String())
	}

	// Flip back off (cleared) live via ApplySnapshot.
	ApplySnapshot(srv, Layer1Snapshot{AgentSecretsUserScopeOnly: nil})

	rec = doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/TOGGLE_KEY_3", reqBody, agentToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 immediately after ApplySnapshot cleared the setting, got %d: %s", rec.Code, rec.Body.String())
	}
}

func sanitizeTestKey(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
