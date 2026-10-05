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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func artifactTestAgent(agentID, projectID string, scopes ...AgentTokenScope) *agentIdentityWrapper {
	return &agentIdentityWrapper{AgentTokenClaims: &AgentTokenClaims{
		Claims:      jwt.Claims{Subject: agentID},
		ProjectID:   projectID,
		Scopes:      scopes,
		ScopeSchema: CurrentAgentScopeSchema,
	}}
}

func TestArtifactHostPrincipal(t *testing.T) {
	host := newArtifactHost(&Server{})
	tests := []struct {
		name                       string
		identity                   Identity
		wantKind, wantRef, wantHom string
		wantOK                     bool
	}{
		{name: "no identity"},
		{
			name:     "user maps to its stable id and has no home scope",
			identity: NewAuthenticatedUser("user-uuid-1", "alice@example.com", "Alice", "member", "web"),
			wantKind: artifacts.PrincipalKindUser, wantRef: "user-uuid-1", wantOK: true,
		},
		{
			name:     "scoped user token maps to the underlying user id",
			identity: NewScopedUserIdentity(NewAuthenticatedUser("user-uuid-2", "bob@example.com", "Bob", "member", "api"), "proj-1", []string{"artifact:read"}),
			wantKind: artifacts.PrincipalKindUser, wantRef: "user-uuid-2", wantOK: true,
		},
		{
			name:     "dev user is a user",
			identity: NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@example.com"}),
			wantKind: artifacts.PrincipalKindUser, wantRef: DevUserID, wantOK: true,
		},
		{
			name:     "agent maps to its id and is homed in its project",
			identity: artifactTestAgent("agent-uuid-1", "proj-1", ScopeProjectRead, ScopeProjectArtifactRead),
			wantKind: artifacts.PrincipalKindAgent, wantRef: "agent-uuid-1", wantHom: "proj-1", wantOK: true,
		},
		{
			name:     "federated agent is not served",
			identity: NewFederatedAgentIdentity("https://other.example.com", "remote-agent", "remote-proj", "remote", "root", nil, nil),
		},
		{name: "broker is not served", identity: NewBrokerIdentity("broker-1")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.identity != nil {
				ctx = contextWithIdentity(ctx, tt.identity)
			}
			kind, ref, home, ok := host.Principal(ctx)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantKind, kind)
			assert.Equal(t, tt.wantRef, ref)
			assert.Equal(t, tt.wantHom, home)
		})
	}
}

// TestArtifactHostAuthorizeFailsClosed covers every deny that the adapter
// decides before reaching the authz engine.
func TestArtifactHostAuthorizeFailsClosed(t *testing.T) {
	srv, _ := testServer(t)
	host := newArtifactHost(srv)
	user := contextWithIdentity(context.Background(), NewAuthenticatedUser("u1", "u1@example.com", "U1", "member", "web"))

	assert.False(t, host.Authorize(context.Background(), "proj-1", artifacts.PermissionRead), "no identity")
	assert.False(t, host.Authorize(user, "", artifacts.PermissionRead), "empty scope")
	assert.False(t, host.Authorize(user, "proj-1", "agent.read"), "non-artifact permission")
	assert.False(t, host.Authorize(user, "proj-1", "artifact.nonexistent"), "unknown permission")
	assert.False(t, newArtifactHost(&Server{}).Authorize(user, "proj-1", artifacts.PermissionRead), "no authz service")
	assert.False(t, newArtifactHost(nil).Authorize(user, "proj-1", artifacts.PermissionRead), "no server")
}

// TestArtifactHostAuthorizeAgentScopes checks the adapter's own agent-scope
// gate: an agent needs one of the permission's AgentScopes on its token,
// delete/manage are never agent-callable, and an agent without
// project:artifact:read is not served at all.
func TestArtifactHostAuthorizeAgentScopes(t *testing.T) {
	srv, s := testServer(t)
	host := newArtifactHost(srv)
	// The authz kernel resolves agent principals from the store, so the
	// agents and their projects must exist. createTestAgent makes a fresh
	// project for each agent.
	agent := createTestAgent(t, s)
	project := agent.ProjectID
	otherProject := createTestAgent(t, s).ProjectID
	require.NotEqual(t, project, otherProject)

	allScopes := []AgentTokenScope{ScopeProjectRead, ScopeProjectArtifactRead, ScopeProjectArtifactWrite, ScopeAgentLifecycle, ScopeAgentCreate, ScopeProjectTemplateWrite}
	full := contextWithIdentity(context.Background(), artifactTestAgent(agent.ID, project, allScopes...))
	for _, p := range []string{artifacts.PermissionDelete, artifacts.PermissionManage} {
		assert.False(t, host.Authorize(full, project, p), "%s must never be agent-callable", p)
	}

	noScopes := contextWithIdentity(context.Background(), artifactTestAgent(agent.ID, project))
	for _, p := range []string{artifacts.PermissionRead, artifacts.PermissionCreate, artifacts.PermissionUpdate} {
		assert.False(t, host.Authorize(noScopes, project, p), "%s without a scope", p)
	}

	reader := contextWithIdentity(context.Background(), artifactTestAgent(agent.ID, project, ScopeProjectArtifactRead))
	assert.True(t, host.Authorize(reader, project, artifacts.PermissionRead), "project:artifact:read grants artifact.read in the agent's project")
	assert.False(t, host.Authorize(reader, otherProject, artifacts.PermissionRead), "agent scopes apply to its own project only, not to another real project")
	assert.False(t, host.Authorize(reader, project, artifacts.PermissionCreate), "project:artifact:read does not grant artifact.create")

	projectReadOnly := contextWithIdentity(context.Background(), artifactTestAgent(agent.ID, project, ScopeProjectRead))
	assert.False(t, host.Authorize(projectReadOnly, project, artifacts.PermissionRead), "project:read alone does not grant artifact.read")

	writer := contextWithIdentity(context.Background(), artifactTestAgent(agent.ID, project, ScopeProjectArtifactRead, ScopeProjectArtifactWrite))
	assert.True(t, host.Authorize(writer, project, artifacts.PermissionCreate), "project:artifact:write grants artifact.create in the agent's project")
	assert.True(t, host.Authorize(writer, project, artifacts.PermissionUpdate), "project:artifact:write grants artifact.update in the agent's project")
	assert.False(t, host.Authorize(writer, otherProject, artifacts.PermissionCreate), "artifact.create in another real project")

	writeOnly := contextWithIdentity(context.Background(), artifactTestAgent(agent.ID, project, ScopeProjectArtifactWrite))
	assert.False(t, host.Authorize(writeOnly, project, artifacts.PermissionRead), "project:artifact:write does not carry artifact.read")
}

// TestArtifactHostAgentWithoutReadScopeIsNotServed pins the invariant that
// the scope decides whether an agent may use the artifact service at all:
// without project:artifact:read, Principal reports no caller, so no artifact
// grant the service holds can reach the agent.
func TestArtifactHostAgentWithoutReadScopeIsNotServed(t *testing.T) {
	host := newArtifactHost(&Server{})
	for name, scopes := range map[string][]AgentTokenScope{
		"no scopes":         nil,
		"project:read only": {ScopeProjectRead},
		"write only":        {ScopeProjectRead, ScopeProjectArtifactWrite},
	} {
		ctx := contextWithIdentity(context.Background(), artifactTestAgent("agent-1", "proj-1", scopes...))
		_, _, _, ok := host.Principal(ctx)
		assert.False(t, ok, name)
	}
}

// TestArtifactHostAuthorizeUsers checks that user decisions come from the
// authz engine against the artifact's home project.
func TestArtifactHostAuthorizeUsers(t *testing.T) {
	srv, s, _, bob, project := setupTemplateAuthzTest(t)
	host := newArtifactHost(srv)

	admin := createScopeSuperAdmin(t, s, "artifact-superadmin")
	adminCtx := contextWithIdentity(context.Background(), NewAuthenticatedUser(admin.ID, admin.Email, admin.DisplayName, admin.Role, "web"))
	for _, p := range []string{artifacts.PermissionRead, artifacts.PermissionCreate, artifacts.PermissionUpdate, artifacts.PermissionDelete, artifacts.PermissionManage} {
		assert.True(t, host.Authorize(adminCtx, project.ID, p), "super-admin holds %s", p)
	}

	bobCtx := contextWithIdentity(context.Background(), NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, bob.Role, "web"))
	assert.False(t, host.Authorize(bobCtx, project.ID, artifacts.PermissionRead), "a non-member has no access to the project's artifacts")
}

// TestArtifactRoutesMatchService pins the literal artifact registrations in
// server.go to artifacts.RoutePatterns(), and every pattern to a
// routeMetadataTable row with the classification the design gives it.
func TestArtifactRoutesMatchService(t *testing.T) {
	source, err := os.ReadFile("server.go")
	require.NoError(t, err)
	re := regexp.MustCompile(`s\.mux\.Handle(?:Func)?\("(/api/v1/artifacts[^"]*)"`)
	var registered []string
	for _, m := range re.FindAllStringSubmatch(string(source), -1) {
		registered = append(registered, m[1])
	}
	want := artifacts.RoutePatterns()
	sort.Strings(registered)
	sort.Strings(want)
	require.Equal(t, want, registered)

	for _, pattern := range []string{artifacts.RouteCollection, artifacts.RouteByID} {
		meta := routeMetadataTable[pattern]
		assert.Equal(t, RoutePolicy, meta.Classification, pattern)
		assert.Equal(t, artifacts.PermissionRead, meta.Permission, pattern)
		assert.Equal(t, permissions.ResourceArtifact, meta.Resource, pattern)
	}
	assert.Equal(t, RoutePublic, routeMetadataTable[artifacts.RouteShared].Classification)
}

var artifactRequestPaths = []struct{ method, path string }{
	{http.MethodGet, "/api/v1/artifacts"},
	{http.MethodPost, "/api/v1/artifacts"},
	{http.MethodGet, "/api/v1/artifacts/art-1"},
	{http.MethodDelete, "/api/v1/artifacts/art-1"},
	{http.MethodGet, "/api/v1/artifacts/art-1/files/index.html"},
	{http.MethodGet, "/api/v1/artifacts/shared/some-token"},
}

func serveArtifactRequests(t *testing.T, mux http.Handler, identity Identity) {
	t.Helper()
	for _, rq := range artifactRequestPaths {
		req := httptest.NewRequest(rq.method, rq.path, nil)
		if identity != nil {
			req = req.WithContext(contextWithIdentity(req.Context(), identity))
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s: %s", rq.method, rq.path, rec.Body.String())
		var body ErrorResponse
		if assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "%s %s", rq.method, rq.path) {
			assert.Equal(t, ErrCodeNotFound, body.Error.Code, "%s %s", rq.method, rq.path)
		}
	}
}

// TestArtifactRoutes404WhileExperimentOff covers all three patterns through
// the hub's real mux: with hub.artifacts off (its default), every route
// answers 404, for an authenticated caller and an anonymous one alike.
func TestArtifactRoutes404WhileExperimentOff(t *testing.T) {
	srv := &Server{config: DefaultServerConfig(), mux: http.NewServeMux()}
	srv.registerRoutes()
	require.False(t, srv.experimentEnabled("hub.artifacts"))

	serveArtifactRequests(t, srv.mux, NewAuthenticatedUser("u1", "u1@example.com", "U1", "member", "web"))
	serveArtifactRequests(t, srv.mux, nil)
}

// TestArtifactRoutes404WhileExperimentOn: P0 has no behaviour, so the
// mounted handler answers 404 behind the gate too.
func TestArtifactRoutes404WhileExperimentOn(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", json.RawMessage(`{"overrides":{"hub.artifacts":true}}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv := &Server{config: DefaultServerConfig(), mux: http.NewServeMux()}
	srv.SetOperationalSettings(ops)
	srv.registerRoutes()
	require.True(t, srv.experimentEnabled("hub.artifacts"))

	serveArtifactRequests(t, srv.mux, NewAuthenticatedUser("u1", "u1@example.com", "U1", "member", "web"))
}

// TestArtifactsGuardViaRegisterRoutes mounts the service the way a
// standalone host would (RegisterRoutes with the hub's guard) and checks the
// same 404-while-off contract holds for every pattern. An unmounted path
// would get ServeMux's plain-text 404, which fails the JSON check.
func TestArtifactsGuardViaRegisterRoutes(t *testing.T) {
	srv := &Server{config: DefaultServerConfig()}
	mux := http.NewServeMux()
	artifacts.NewService(newArtifactHost(srv)).RegisterRoutes(mux, srv.artifactsGuard)
	serveArtifactRequests(t, mux, NewAuthenticatedUser("u1", "u1@example.com", "U1", "member", "web"))
}
