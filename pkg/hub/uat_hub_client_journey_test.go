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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file drive the hub through pkg/hubclient against the
// real server handler on an httptest server, as a CLI or other API client
// does.

// xpSessionClient returns a hubclient authenticated as user with an
// interactive session.
func xpSessionClient(t *testing.T, srv *Server, url string, user *store.User) hubclient.Client {
	t.Helper()
	session, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	require.NoError(t, err)
	c, err := hubclient.New(url, hubclient.WithBearerToken(session))
	require.NoError(t, err)
	return c
}

// xpTokenClient returns a hubclient authenticated with a user access token.
func xpTokenClient(t *testing.T, url, key string) hubclient.Client {
	t.Helper()
	c, err := hubclient.New(url, hubclient.WithBearerToken(key))
	require.NoError(t, err)
	return c
}

// xpAPIError returns err as an *apiclient.APIError.
func xpAPIError(t *testing.T, err error) *apiclient.APIError {
	t.Helper()
	require.Error(t, err)
	var apiErr *apiclient.APIError
	require.True(t, errors.As(err, &apiErr), "an API error: %v", err)
	return apiErr
}

// TestHubUATClient_CreateUseRevokeWithoutAgentBinding pins the API client
// journey for a hub token: a session creates it with a hub boundary,
// purpose and labels and no project or agent; the token lists agents in two
// projects and deletes one in a project its holder is not a member of; the
// session revokes it and the next call with the token is unauthorized.
func TestHubUATClient_CreateUseRevokeWithoutAgentBinding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectP, ownerP := tid("xpc-journey-project-p"), tid("xpc-journey-owner-p")
	projectQ, ownerQ := tid("xpc-journey-project-q"), tid("xpc-journey-owner-q")
	createRS1Project(t, s, projectP, ownerP)
	createRS1Project(t, s, projectQ, ownerQ)
	adminID := hubConfigTokenUser(t, s, "xpc-journey-admin", store.SystemRoleSuperAdmin)
	agentP := uatpAgent(t, s, projectP, ownerP, "xpc-journey-p", ownerP)
	agentQ := uatpAgent(t, s, projectQ, ownerQ, "xpc-journey-q", ownerQ)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	session := xpSessionClient(t, srv, httpSrv.URL, mustGetUser(t, s, adminID))

	req := &hubclient.CreateTokenRequest{
		Name:     "xpc-journey",
		Boundary: &hubclient.TokenBoundary{Kind: "hub"},
		Scopes:   []string{"agent:list", "agent:read", "agent:delete"},
		Purpose:  "xpc journey",
		Labels:   map[string]string{"team": "ops"},
	}
	encoded, err := json.Marshal(req)
	require.NoError(t, err)
	var sent map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &sent))
	var fields []string
	for field := range sent {
		fields = append(fields, field)
	}
	assert.ElementsMatch(t, []string{"name", "boundary", "scopes", "purpose", "labels"}, fields,
		"the create request names no project and no agent")

	created, err := session.Tokens().Create(ctx, req)
	require.NoError(t, err)
	require.NotEmpty(t, created.Token)
	info := created.AccessToken
	require.NotNil(t, info)
	require.NotNil(t, info.Boundary)
	assert.Equal(t, "hub", info.Boundary.Kind)
	assert.Empty(t, info.Boundary.ProjectID)
	assert.Empty(t, info.ProjectID)
	assert.Equal(t, "xpc journey", info.Purpose)
	assert.Equal(t, map[string]string{"team": "ops"}, info.Labels)

	stored, err := s.GetUserAccessToken(ctx, info.ID)
	require.NoError(t, err)
	assert.Equal(t, string(BoundaryKindHub), stored.BoundaryKind)
	assert.Empty(t, stored.ProjectID)

	bearer := xpTokenClient(t, httpSrv.URL, created.Token)
	listed, err := bearer.Agents().List(ctx, nil)
	require.NoError(t, err)
	var ids []string
	for _, a := range listed.Agents {
		ids = append(ids, a.ID)
	}
	assert.Contains(t, ids, agentP.ID)
	assert.Contains(t, ids, agentQ.ID)

	require.NoError(t, bearer.Agents().Delete(ctx, agentQ.ID, nil))
	_, err = s.GetAgent(ctx, agentQ.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, session.Tokens().Revoke(ctx, info.ID))
	got, err := session.Tokens().Get(ctx, info.ID)
	require.NoError(t, err)
	assert.True(t, got.Revoked)

	_, err = bearer.Agents().Get(ctx, agentP.ID)
	assert.Equal(t, http.StatusUnauthorized, xpAPIError(t, err).StatusCode, "a revoked token is refused on the next call")
	xpRequireAgentKept(t, s, agentP.ID, "revoked token")
}

// TestHubUATClient_ListScopesHubBoundary pins hub-boundary eligibility
// through the API client: a session reads it with authority_required for a
// selector its holder holds through no role, a refused hub mint names the
// selector and the reason, a refused project mint carries no scope
// violation, and a token reads the selector catalog only.
func TestHubUATClient_ListScopesHubBoundary(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectP, ownerP := tid("xpc-scopes-project-p"), tid("xpc-scopes-owner-p")
	projectQ, ownerQ := tid("xpc-scopes-project-q"), tid("xpc-scopes-owner-q")
	userID := tid("xpc-scopes-user")
	createRS1Project(t, s, projectP, ownerP)
	createRS1Project(t, s, projectQ, ownerQ)
	uatpMember(t, s, projectP, userID)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	session := xpSessionClient(t, srv, httpSrv.URL, mustGetUser(t, s, userID))

	scopes, err := session.Tokens().ListScopes(ctx, &hubclient.ListScopesOptions{Boundary: "hub"})
	require.NoError(t, err)
	byID := map[string]hubclient.ScopeInfo{}
	for _, sc := range scopes.Scopes {
		byID[sc.ID] = sc
	}
	require.Contains(t, byID, "inbox:read")
	require.NotNil(t, byID["inbox:read"].Eligibility)
	assert.True(t, byID["inbox:read"].Eligibility.Eligible)
	require.Contains(t, byID, "hub_config:read")
	elig := byID["hub_config:read"].Eligibility
	require.NotNil(t, elig)
	assert.False(t, elig.Eligible)
	assert.Equal(t, string(MintDenialAuthorityRequired), elig.Reason)
	assert.Equal(t, "hub", elig.Boundary.Kind)

	_, err = session.Tokens().Create(ctx, &hubclient.CreateTokenRequest{
		Name: "xpc-scopes-hub", Boundary: &hubclient.TokenBoundary{Kind: "hub"}, Scopes: []string{"hub_config:read"},
	})
	require.Error(t, err)
	selector, reason, ok := hubclient.AsScopeViolation(err)
	require.True(t, ok, "a refused hub mint is a scope violation: %v", err)
	assert.Equal(t, "hub_config:read", selector)
	assert.Equal(t, string(MintDenialAuthorityRequired), reason)

	_, err = session.Tokens().Create(ctx, &hubclient.CreateTokenRequest{
		Name: "xpc-scopes-project", ProjectID: projectQ, Scopes: []string{"agent:read"},
	})
	assert.Equal(t, http.StatusForbidden, xpAPIError(t, err).StatusCode)
	_, _, ok = hubclient.AsScopeViolation(err)
	assert.False(t, ok, "a refused project mint carries no scope violation")

	created, err := session.Tokens().Create(ctx, &hubclient.CreateTokenRequest{
		Name: "xpc-scopes-token", Boundary: &hubclient.TokenBoundary{Kind: "hub"}, Scopes: []string{"inbox:read"},
	})
	require.NoError(t, err)
	bearer := xpTokenClient(t, httpSrv.URL, created.Token)

	_, err = bearer.Tokens().ListScopes(ctx, &hubclient.ListScopesOptions{Boundary: "hub"})
	apiErr := xpAPIError(t, err)
	assert.Equal(t, http.StatusForbidden, apiErr.StatusCode)
	assert.Equal(t, "CREDENTIAL_MANAGEMENT", apiErr.Details["reason"])

	catalog, err := bearer.Tokens().ListScopes(ctx, nil)
	require.NoError(t, err)
	require.NotEmpty(t, catalog.Scopes)
	for _, sc := range catalog.Scopes {
		assert.Nil(t, sc.Eligibility, "a token reads the catalog only: %s", sc.ID)
	}

	count, err := s.CountUserAccessTokens(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "the refused mints store no token")
}
