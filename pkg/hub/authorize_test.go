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

package hub

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Helpers in this file are prefixed authzHelper to avoid colliding with the
// fixtures other tests in package hub define.

const (
	authzHelperProjectA = "authz-project-a"
	authzHelperProjectB = "authz-project-b"
	authzHelperAgentID  = "authz-caller-agent"
)

// authzHelperCaptureLogs redirects the default slog logger into a buffer for the
// duration of the test and returns the buffer. The denial log line is a
// deliverable of #591, not decoration, so it is asserted on directly.
func authzHelperCaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// authzHelperDenialRecord returns the first "authorization denied" record in the
// captured log output, or nil if there is none.
func authzHelperDenialRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "authorization denied" {
			return rec
		}
	}
	return nil
}

// authzHelperRequest builds a request carrying the given identity. A nil
// identity produces an unauthenticated request.
func authzHelperRequest(identity Identity) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/authz-test", nil)
	if identity != nil {
		req = req.WithContext(contextWithIdentity(req.Context(), identity))
	}
	return req
}

// authzHelperAgent builds an agent identity in the given project with the given scopes.
func authzHelperAgent(projectID string, scopes ...AgentTokenScope) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: authzHelperAgentID},
		ProjectID: projectID,
		Scopes:    scopes,
	}}
}

func authzHelperAdmin() UserIdentity {
	return NewAuthenticatedUser("authz-admin", "admin@test.com", "Admin", store.UserRoleAdmin, "api")
}

func authzHelperMember() UserIdentity {
	return NewAuthenticatedUser("authz-member", "member@test.com", "Member", "member", "api")
}

// authzHelperTargetAgent is the agent acted upon in lifecycle tests.
func authzHelperTargetAgent() *store.Agent {
	return &store.Agent{
		ID:        "authz-target-agent",
		Name:      "target",
		Slug:      "target",
		ProjectID: authzHelperProjectA,
		OwnerID:   "some-other-user",
	}
}

// ---------------------------------------------------------------------------
// authorize / authorizeMsg
// ---------------------------------------------------------------------------

func TestAuthorize_IdentityKinds(t *testing.T) {
	srv, _ := testServer(t)

	// An agent passes on ancestry: the caller agent is listed in the target
	// resource's ancestor chain.
	allowedForAgent := Resource{
		Type:       "agent",
		ID:         "authz-descendant",
		ParentType: "project",
		ParentID:   authzHelperProjectA,
		Ancestry:   []string{"root-user", authzHelperAgentID},
	}
	deniedResource := Resource{
		Type:       "agent",
		ID:         "authz-unrelated",
		ParentType: "project",
		ParentID:   authzHelperProjectA,
	}

	tests := []struct {
		name       string
		identity   Identity
		resource   Resource
		wantAllow  bool
		wantStatus int
	}{
		{
			name:       "nil identity is unauthenticated",
			identity:   nil,
			resource:   deniedResource,
			wantAllow:  false,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:      "user allowed by policy",
			identity:  authzHelperAdmin(),
			resource:  deniedResource,
			wantAllow: true,
		},
		{
			name:       "user denied by policy",
			identity:   authzHelperMember(),
			resource:   deniedResource,
			wantAllow:  false,
			wantStatus: http.StatusForbidden,
		},
		{
			name:      "agent allowed by policy",
			identity:  authzHelperAgent(authzHelperProjectA),
			resource:  allowedForAgent,
			wantAllow: true,
		},
		{
			name:       "agent denied by policy",
			identity:   authzHelperAgent(authzHelperProjectA),
			resource:   deniedResource,
			wantAllow:  false,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "broker identity is denied",
			identity:   NewBrokerIdentity("authz-broker"),
			resource:   deniedResource,
			wantAllow:  false,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			got := srv.authorize(rec, authzHelperRequest(tc.identity), tc.resource, ActionDelete)

			if got != tc.wantAllow {
				t.Fatalf("authorize() = %v, want %v (body: %s)", got, tc.wantAllow, rec.Body.String())
			}
			if tc.wantAllow {
				if rec.Body.Len() != 0 {
					t.Errorf("expected no response body on allow, got %q", rec.Body.String())
				}
				return
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestAuthorize_DenialIsLogged asserts the structured denial log line required by
// design §4.1. The #591 bypass was invisible precisely because it was silent.
func TestAuthorize_DenialIsLogged(t *testing.T) {
	srv, _ := testServer(t)
	buf := authzHelperCaptureLogs(t)

	resource := Resource{Type: "agent", ID: "authz-unrelated", ParentType: "project", ParentID: authzHelperProjectA}
	rec := httptest.NewRecorder()
	req := authzHelperRequest(authzHelperAgent(authzHelperProjectA))

	if srv.authorize(rec, req, resource, ActionDelete) {
		t.Fatal("expected denial for an agent with no matching policy")
	}

	found := authzHelperDenialRecord(t, buf)
	if found == nil {
		t.Fatalf("expected an 'authorization denied' log record, got: %s", buf.String())
	}
	for key, want := range map[string]any{
		"principal_type": "agent",
		"principal_id":   authzHelperAgentID,
		"resource_type":  "agent",
		"resource_id":    "authz-unrelated",
		"action":         string(ActionDelete),
		"path":           "/api/v1/authz-test",
	} {
		if got := found[key]; got != want {
			t.Errorf("denial log %q = %v, want %v", key, got, want)
		}
	}
	if _, ok := found["reason"]; !ok {
		t.Errorf("denial log is missing 'reason': %v", found)
	}
}

func TestAuthorize_NoDenialLogWhenAllowed(t *testing.T) {
	srv, _ := testServer(t)
	buf := authzHelperCaptureLogs(t)

	rec := httptest.NewRecorder()
	if !srv.authorize(rec, authzHelperRequest(authzHelperAdmin()), Resource{Type: "agent", ID: "x"}, ActionRead) {
		t.Fatal("expected admin to be allowed")
	}
	if r := authzHelperDenialRecord(t, buf); r != nil {
		t.Errorf("unexpected denial log on allow: %v", r)
	}
}

func TestAuthorizeMsg_DenialBodyCarriesMessage(t *testing.T) {
	srv, _ := testServer(t)

	const msg = "Assign a GCP service account to this project first"
	rec := httptest.NewRecorder()
	req := authzHelperRequest(authzHelperMember())

	if srv.authorizeMsg(rec, req, Resource{Type: "gcp_service_account", ID: "sa-1"}, ActionRead, msg) {
		t.Fatal("expected denial for a member with no matching policy")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), msg) {
		t.Errorf("403 body %q does not contain the supplied message %q", rec.Body.String(), msg)
	}
}

func TestAuthorizeMsg_UnauthenticatedStillGets401(t *testing.T) {
	srv, _ := testServer(t)

	rec := httptest.NewRecorder()
	if srv.authorizeMsg(rec, authzHelperRequest(nil), Resource{Type: "agent", ID: "x"}, ActionRead, "some guidance") {
		t.Fatal("expected denial for a nil identity")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// authorizeAgentCreate
// ---------------------------------------------------------------------------

func TestAuthorizeAgentCreate_IdentityKinds(t *testing.T) {
	srv, _ := testServer(t)

	tests := []struct {
		name       string
		identity   Identity
		wantAllow  bool
		wantStatus int
		wantBody   string
	}{
		{
			name:       "nil identity is unauthenticated",
			identity:   nil,
			wantAllow:  false,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:      "user allowed by policy",
			identity:  authzHelperAdmin(),
			wantAllow: true,
		},
		{
			name:       "user denied by policy",
			identity:   authzHelperMember(),
			wantAllow:  false,
			wantStatus: http.StatusForbidden,
		},
		{
			name:      "agent with create scope in its own project",
			identity:  authzHelperAgent(authzHelperProjectA, ScopeAgentCreate),
			wantAllow: true,
		},
		{
			name:       "agent with create scope in a different project",
			identity:   authzHelperAgent(authzHelperProjectB, ScopeAgentCreate),
			wantAllow:  false,
			wantStatus: http.StatusForbidden,
			wantBody:   "own project",
		},
		{
			name:       "agent without create scope",
			identity:   authzHelperAgent(authzHelperProjectA, ScopeAgentLifecycle),
			wantAllow:  false,
			wantStatus: http.StatusForbidden,
			wantBody:   "Missing required scope",
		},
		{
			name:       "agent with no scopes at all",
			identity:   authzHelperAgent(authzHelperProjectA),
			wantAllow:  false,
			wantStatus: http.StatusForbidden,
			wantBody:   "Missing required scope",
		},
		{
			name:       "broker identity is denied",
			identity:   NewBrokerIdentity("authz-broker"),
			wantAllow:  false,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			got := srv.authorizeAgentCreate(rec, authzHelperRequest(tc.identity), authzHelperProjectA)

			if got != tc.wantAllow {
				t.Fatalf("authorizeAgentCreate() = %v, want %v (body: %s)", got, tc.wantAllow, rec.Body.String())
			}
			if tc.wantAllow {
				if rec.Body.Len() != 0 {
					t.Errorf("expected no response body on allow, got %q", rec.Body.String())
				}
				return
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body %q does not contain %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

func TestAuthorizeAgentCreate_DenialIsLogged(t *testing.T) {
	srv, _ := testServer(t)
	buf := authzHelperCaptureLogs(t)

	rec := httptest.NewRecorder()
	req := authzHelperRequest(authzHelperAgent(authzHelperProjectB, ScopeAgentCreate))
	if srv.authorizeAgentCreate(rec, req, authzHelperProjectA) {
		t.Fatal("expected cross-project agent creation to be denied")
	}

	found := authzHelperDenialRecord(t, buf)
	if found == nil {
		t.Fatalf("expected an 'authorization denied' log record, got: %s", buf.String())
	}
	if found["principal_type"] != "agent" {
		t.Errorf("denial log principal_type = %v, want \"agent\"", found["principal_type"])
	}
}

// ---------------------------------------------------------------------------
// authorizeAgentLifecycle
// ---------------------------------------------------------------------------

func TestAuthorizeAgentLifecycle_IdentityKinds(t *testing.T) {
	srv, _ := testServer(t)
	target := authzHelperTargetAgent()

	tests := []struct {
		name       string
		identity   Identity
		wantAllow  bool
		wantStatus int
		wantBody   string
	}{
		{
			name:       "nil identity is unauthenticated",
			identity:   nil,
			wantAllow:  false,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:      "user allowed by policy",
			identity:  authzHelperAdmin(),
			wantAllow: true,
		},
		{
			name:       "user denied by policy",
			identity:   authzHelperMember(),
			wantAllow:  false,
			wantStatus: http.StatusForbidden,
		},
		{
			name:      "agent with lifecycle scope in the target's project",
			identity:  authzHelperAgent(authzHelperProjectA, ScopeAgentLifecycle),
			wantAllow: true,
		},
		{
			name:       "agent with lifecycle scope in a different project",
			identity:   authzHelperAgent(authzHelperProjectB, ScopeAgentLifecycle),
			wantAllow:  false,
			wantStatus: http.StatusForbidden,
			wantBody:   "own project",
		},
		{
			name:       "agent without lifecycle scope",
			identity:   authzHelperAgent(authzHelperProjectA, ScopeAgentCreate),
			wantAllow:  false,
			wantStatus: http.StatusForbidden,
			wantBody:   "Missing required scope",
		},
		{
			name:       "broker identity is denied",
			identity:   NewBrokerIdentity("authz-broker"),
			wantAllow:  false,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			got := srv.authorizeAgentLifecycle(rec, authzHelperRequest(tc.identity), target)

			if got != tc.wantAllow {
				t.Fatalf("authorizeAgentLifecycle() = %v, want %v (body: %s)", got, tc.wantAllow, rec.Body.String())
			}
			if tc.wantAllow {
				if rec.Body.Len() != 0 {
					t.Errorf("expected no response body on allow, got %q", rec.Body.String())
				}
				return
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body %q does not contain %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestAuthorizeAgentLifecycle_PeerWithinProject pins the deliberate Q3 decision:
// the lifecycle scope covers project peers, not only descendants.
func TestAuthorizeAgentLifecycle_PeerWithinProject(t *testing.T) {
	srv, _ := testServer(t)
	peer := authzHelperTargetAgent()
	peer.Ancestry = []string{"unrelated-user", "unrelated-agent"}

	rec := httptest.NewRecorder()
	req := authzHelperRequest(authzHelperAgent(authzHelperProjectA, ScopeAgentLifecycle))
	if !srv.authorizeAgentLifecycle(rec, req, peer) {
		t.Fatalf("expected a scoped agent to reach a project peer, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthorizeAgentLifecycle_NilAgentDenied(t *testing.T) {
	srv, _ := testServer(t)

	rec := httptest.NewRecorder()
	req := authzHelperRequest(authzHelperAdmin())
	if srv.authorizeAgentLifecycle(rec, req, nil) {
		t.Fatal("expected a nil agent to be denied")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestAuthorizeAgentLifecycle_DenialIsLogged(t *testing.T) {
	srv, _ := testServer(t)
	buf := authzHelperCaptureLogs(t)

	rec := httptest.NewRecorder()
	req := authzHelperRequest(NewBrokerIdentity("authz-broker"))
	if srv.authorizeAgentLifecycle(rec, req, authzHelperTargetAgent()) {
		t.Fatal("expected a broker identity to be denied")
	}

	found := authzHelperDenialRecord(t, buf)
	if found == nil {
		t.Fatalf("expected an 'authorization denied' log record, got: %s", buf.String())
	}
	if found["principal_type"] != "broker" {
		t.Errorf("denial log principal_type = %v, want \"broker\"", found["principal_type"])
	}
}

// ---------------------------------------------------------------------------
// requireAdmin (promoted out of skill_registry_handlers.go)
// ---------------------------------------------------------------------------

func TestRequireAdmin_IdentityKinds(t *testing.T) {
	srv, _ := testServer(t)

	tests := []struct {
		name       string
		identity   Identity
		wantOK     bool
		wantStatus int
		// wantPrincipalType is the principal_type expected on the denial log
		// line, or "" when no denial log is expected (allow, or 401 before any
		// principal exists).
		wantPrincipalType string
	}{
		{
			name: "nil identity is unauthenticated", identity: nil,
			wantOK: false, wantStatus: http.StatusUnauthorized,
		},
		{
			name: "admin user", identity: authzHelperAdmin(),
			wantOK: true,
		},
		{
			name: "scoped admin user access token is forbidden", identity: NewScopedUserIdentity(authzHelperAdmin(), "project-1", []string{"project:read"}),
			wantOK: false, wantStatus: http.StatusForbidden, wantPrincipalType: "user",
		},
		{
			name: "federated admin user is forbidden", identity: NewFederatedUserIdentity("https://issuer.example", "admin", "admin@example.com", "Admin", "admin", nil),
			wantOK: false, wantStatus: http.StatusForbidden, wantPrincipalType: "federated_user",
		},
		{
			name: "dev-auth identity is treated as a user", identity: NewDevUser(DevUserConfig{Username: "dev"}),
			wantOK: true,
		},
		{
			name: "non-admin user is forbidden, not unauthorized", identity: authzHelperMember(),
			wantOK: false, wantStatus: http.StatusForbidden, wantPrincipalType: "user",
		},
		{
			// Regression: this returned 401 "Authentication required" to an
			// authenticated agent, because requireAdmin resolved the identity
			// with GetUserIdentityFromContext.
			name: "authenticated agent is forbidden, not unauthorized", identity: authzHelperAgent(authzHelperProjectA, ScopeAgentCreate),
			wantOK: false, wantStatus: http.StatusForbidden, wantPrincipalType: "agent",
		},
		{
			name: "authenticated broker is forbidden, not unauthorized", identity: NewBrokerIdentity("authz-broker"),
			wantOK: false, wantStatus: http.StatusForbidden, wantPrincipalType: "broker",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf := authzHelperCaptureLogs(t)
			rec := httptest.NewRecorder()

			_, ok := srv.requireAdmin(rec, authzHelperRequest(tc.identity))
			if ok != tc.wantOK {
				t.Fatalf("requireAdmin() ok = %v, want %v (body: %s)", ok, tc.wantOK, rec.Body.String())
			}
			if !tc.wantOK && rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}

			// requireAdmin gates the hub's admin endpoints, so every denial
			// must leave an audit trail, exactly as authorize does.
			found := authzHelperDenialRecord(t, buf)
			if tc.wantPrincipalType == "" {
				if found != nil {
					t.Errorf("unexpected denial log: %v", found)
				}
				return
			}
			if found == nil {
				t.Fatalf("expected an 'authorization denied' log record, got: %s", buf.String())
			}
			if found["principal_type"] != tc.wantPrincipalType {
				t.Errorf("denial log principal_type = %v, want %q", found["principal_type"], tc.wantPrincipalType)
			}
			if found["resource_type"] != "hub" {
				t.Errorf("denial log resource_type = %v, want \"hub\"", found["resource_type"])
			}
			if found["path"] != "/api/v1/authz-test" {
				t.Errorf("denial log path = %v, want the request path", found["path"])
			}
			if reason, _ := found["reason"].(string); reason == "" {
				t.Errorf("denial log is missing 'reason': %v", found)
			}
		})
	}
}

// TestRequireAdmin_DenialReasons pins the two distinct denial reasons apart, so
// an operator reading the log can tell "wrong kind of caller" from "right kind,
// wrong role".
func TestRequireAdmin_DenialReasons(t *testing.T) {
	srv, _ := testServer(t)

	tests := []struct {
		name       string
		identity   Identity
		wantReason string
	}{
		{"non-user caller", authzHelperAgent(authzHelperProjectA), "non-user identity"},
		{"non-admin user", authzHelperMember(), "not an admin"},
		{"scoped admin user access token", NewScopedUserIdentity(authzHelperAdmin(), "project-1", []string{"project:read"}), "scoped user access token"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf := authzHelperCaptureLogs(t)
			rec := httptest.NewRecorder()

			if _, ok := srv.requireAdmin(rec, authzHelperRequest(tc.identity)); ok {
				t.Fatal("expected denial")
			}
			found := authzHelperDenialRecord(t, buf)
			if found == nil {
				t.Fatalf("expected an 'authorization denied' log record, got: %s", buf.String())
			}
			if found["reason"] != tc.wantReason {
				t.Errorf("denial log reason = %v, want %q", found["reason"], tc.wantReason)
			}
		})
	}
}

func TestRequireAdmin_ScopedAdminForbiddenAtAllRoleOnlyGates(t *testing.T) {
	srv, _ := testServer(t)
	scopedAdmin := NewScopedUserIdentity(authzHelperAdmin(), "project-1", []string{"project:read", "agent:manage"})

	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{
			name:    "policy collection list",
			method:  http.MethodGet,
			path:    "/api/v1/policies",
			handler: srv.handlePolicies,
		},
		{
			name:    "policy collection create",
			method:  http.MethodPost,
			path:    "/api/v1/policies",
			body:    `{"name":"deny-uat","scopeType":"hub","actions":["read"],"effect":"allow"}`,
			handler: srv.handlePolicies,
		},
		{
			name:    "policy detail get",
			method:  http.MethodGet,
			path:    "/api/v1/policies/policy-1",
			handler: srv.handlePolicyRoutes,
		},
		{
			name:    "policy detail update",
			method:  http.MethodPatch,
			path:    "/api/v1/policies/policy-1",
			body:    `{"name":"updated"}`,
			handler: srv.handlePolicyRoutes,
		},
		{
			name:    "policy detail delete",
			method:  http.MethodDelete,
			path:    "/api/v1/policies/policy-1",
			handler: srv.handlePolicyRoutes,
		},
		{
			name:    "policy bindings list",
			method:  http.MethodGet,
			path:    "/api/v1/policies/policy-1/bindings",
			handler: srv.handlePolicyRoutes,
		},
		{
			name:    "policy bindings create",
			method:  http.MethodPost,
			path:    "/api/v1/policies/policy-1/bindings",
			body:    `{"principalType":"user","principalId":"user-1"}`,
			handler: srv.handlePolicyRoutes,
		},
		{
			name:    "policy binding delete",
			method:  http.MethodDelete,
			path:    "/api/v1/policies/policy-1/bindings/user/user-1",
			handler: srv.handlePolicyRoutes,
		},
		{
			name:    "skill registry list",
			method:  http.MethodGet,
			path:    "/api/v1/skill-registries",
			handler: srv.handleSkillRegistries,
		},
		{
			name:    "skill registry create",
			method:  http.MethodPost,
			path:    "/api/v1/skill-registries",
			body:    `{"name":"reg","endpoint":"https://registry.example.com"}`,
			handler: srv.handleSkillRegistries,
		},
		{
			name:    "skill registry get",
			method:  http.MethodGet,
			path:    "/api/v1/skill-registries/reg",
			handler: srv.handleSkillRegistryByID,
		},
		{
			name:    "skill registry update",
			method:  http.MethodPatch,
			path:    "/api/v1/skill-registries/reg",
			body:    `{"status":"disabled"}`,
			handler: srv.handleSkillRegistryByID,
		},
		{
			name:    "skill registry delete",
			method:  http.MethodDelete,
			path:    "/api/v1/skill-registries/reg",
			handler: srv.handleSkillRegistryByID,
		},
		{
			name:    "skill registry pin",
			method:  http.MethodPost,
			path:    "/api/v1/skill-registries/reg/pin",
			body:    `{"uri":"skill://reg/core/test@1.0","hash":"sha256:abc123"}`,
			handler: srv.handleSkillRegistryByID,
		},
		{
			name:    "skill registry pins list",
			method:  http.MethodGet,
			path:    "/api/v1/skill-registries/reg/pins",
			handler: srv.handleSkillRegistryByID,
		},
		{
			name:    "skill registry unpin",
			method:  http.MethodPost,
			path:    "/api/v1/skill-registries/reg/unpin",
			body:    `{"uri":"skill://reg/core/test@1.0"}`,
			handler: srv.handleSkillRegistryByID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			req = req.WithContext(contextWithIdentity(req.Context(), scopedAdmin))

			tc.handler(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusForbidden, rec.Body.String())
			}
		})
	}
}
