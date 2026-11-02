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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Authentication-only reads: any signed-in identity reads these, and a
// user access token reads them whatever its selectors and boundary. Each
// test pins the admit_self disposition of its catalog operation.

// authenticatedReadTokens mints a hub token with an unrelated selector and
// a project token for the holder's project.
func authenticatedReadTokens(t *testing.T, srv *Server, s store.Store, name string) []string {
	t.Helper()
	user := hubConfigTokenUser(t, s, name, store.SystemRoleHubMember)
	project := igProject(t, s, name+"-project")
	igProjectBinding(t, s, user, project, store.ProjectRoleMember)
	return []string{
		mintHubConfigToken(t, srv, user, hubBoundary(), "user:read"),
		mintHubConfigToken(t, srv, user, projectBoundary(project), "project:read"),
	}
}

func TestExperimentsRead_AnyTokenReads(t *testing.T) {
	srv, s := testServer(t)
	for _, key := range authenticatedReadTokens(t, srv, s, "ar-experiments") {
		rec := doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/experiments", nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
}

func TestProfilingRead_AnyTokenReads(t *testing.T) {
	srv, s := testServer(t)
	for _, key := range authenticatedReadTokens(t, srv, s, "ar-profiling") {
		rec := doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/profiling", nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
}

func TestHubInjectedSkillsRead_AnyTokenReads(t *testing.T) {
	srv, s := testServer(t)
	for _, key := range authenticatedReadTokens(t, srv, s, "ar-injected") {
		rec := doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/hub/settings/injected-skills", nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
}

func TestConduitGrantKeysRead_AnyTokenReads(t *testing.T) {
	srv, s := testServer(t)
	setConduitExperiment(t, srv, true)
	srv.conduitGrants = newConduitGrantKeys(&memoryConduitGrantKeyStore{}, time.Now)
	for _, key := range authenticatedReadTokens(t, srv, s, "ar-grantkeys") {
		rec := doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/conduit/grant-keys", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body conduitGrantKeysResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.NotEmpty(t, body.Keys)
	}
}

// TestHubPreStartHookRead_MemberAndProjectTokensRead pins admit_self on the
// hub pre-start hook reads: a member's hub token with an unrelated selector
// and a project token read the list and the hook, with the script redacted.
func TestHubPreStartHookRead_MemberAndProjectTokensRead(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin := hubConfigTokenUser(t, s, "ar-hook-admin", store.SystemRoleSuperAdmin)
	hook, err := s.CreateHubPreStartHook(context.Background(), &store.ProjectPreStartHook{
		Scope: store.PreStartHookScopeHub, Name: "ar-hook", Slug: "ar-hook", Script: "#!/bin/sh\necho ar-hook\n",
		CreatedBy: admin + "@test.com", UpdatedBy: admin + "@test.com",
	})
	require.NoError(t, err)
	for _, key := range authenticatedReadTokens(t, srv, s, "ar-hook-member") {
		rec := doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/pre-start-hooks", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var list ListProjectPreStartHooksResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
		require.Len(t, list.Hooks, 1)
		assert.Equal(t, "ar-hook", list.Hooks[0].Name)
		assert.Empty(t, list.Hooks[0].Script, "a token gets the script redacted")

		rec = doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/pre-start-hooks/"+hook.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got store.ProjectPreStartHook
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, "ar-hook", got.Name)
		assert.Empty(t, got.Script, "a token gets the script redacted")
	}
}

// TestAuthenticatedReads_AgentTokenReads pins the agent principal on the
// experiment and profiling reads: an agent token receives both.
func TestAuthenticatedReads_AgentTokenReads(t *testing.T) {
	srv, s, user, project := setupCredentialTestServer(t)
	agentID := tid("ar-agent-reads")
	createCredTestAgent(t, s, agentID, project.ID, user.ID)
	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	for _, path := range []string{"/api/v1/experiments", "/api/v1/profiling"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Scion-Agent-Token", token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
	}
}

// TestAuthenticatedReads_BrokerReads pins the broker principal on the
// experiment and profiling reads: a request signed with a broker's HMAC
// secret receives both.
func TestAuthenticatedReads_BrokerReads(t *testing.T) {
	srv, s := testServer(t)
	require.NotNil(t, srv.brokerAuthService, "the test server must run broker HMAC auth")
	_, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)
	for _, path := range []string{"/api/v1/experiments", "/api/v1/profiling"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, signReq(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
	}
}
