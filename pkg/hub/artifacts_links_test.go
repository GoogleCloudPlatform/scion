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
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// anonymous serves an unauthenticated request through the full hub
// handler (authentication included).
func anonymous(srv *Server, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// TestArtifactsShareLinkOnRoutes: through the real hub, a project member
// creates a share link on its artifact; a request with no credentials at
// all is sent to the view route and served there with the view policy;
// revoking the link ends it; the routes stay closed while the experiment
// or the settings switch is off; the token never appears in the hub's
// request log.
func TestArtifactsShareLinkOnRoutes(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	require.NotEmpty(t, srv.artifactViewKey)
	ctx := context.Background()
	p1 := artifactProject(t, s, "share-p1")
	createTestUserWithProjectRole(t, s, tid("share-owner"), "share-owner@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, tid("share-member"), "share-member@test.com", p1.ID, store.ProjectRoleMember)
	owner, err := s.GetUser(ctx, tid("share-owner"))
	require.NoError(t, err)
	member, err := s.GetUser(ctx, tid("share-member"))
	require.NoError(t, err)
	_, agentTok := artifactAgent(t, srv, s, p1.ID, "share-agent", AgentRoleBaseline)

	rec := userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts?name=report.md&scope="+p1.ID, []byte("# Shared report\n"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)
	links := "/api/v1/artifacts/" + id + "/links"

	// Only the owner (a user) manages links: a reading member and the
	// project's agent get 403, an agent never mints.
	assert.Equal(t, http.StatusForbidden, userArtifactRequest(t, srv, member, http.MethodPost, links, nil).Code)
	assert.Equal(t, http.StatusForbidden, doRawAgentRequest(t, srv, http.MethodPost, links, nil, agentTok).Code)

	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	rec = userArtifactRequest(t, srv, owner, http.MethodPost, links, []byte(`{"ttlHours": 2}`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created artifacts.CreateLinkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	token := strings.TrimPrefix(created.URL, artifacts.RouteShared)
	require.Len(t, token, 43)
	assert.WithinDuration(t, time.Now().Add(2*time.Hour), created.Link.ExpiresAt, time.Minute)

	shared := anonymous(srv, http.MethodGet, created.URL)
	require.Equal(t, http.StatusSeeOther, shared.Code, shared.Body.String())
	assert.Equal(t, "no-referrer", shared.Header().Get("Referrer-Policy"))
	loc := shared.Header().Get("Location")
	require.True(t, strings.HasPrefix(loc, artifacts.RouteView), loc)
	view := anonymous(srv, http.MethodGet, loc)
	require.Equal(t, http.StatusOK, view.Code, view.Body.String())
	assert.Equal(t, "# Shared report\n", view.Body.String())
	csp := view.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "sandbox allow-scripts;")
	assert.NotContains(t, csp, "allow-same-origin")
	assert.Contains(t, csp, "default-src 'none'")
	assert.Contains(t, csp, "connect-src 'none'")

	// An unknown token answers like a revoked one, below.
	unknown := anonymous(srv, http.MethodGet, artifacts.RouteShared+strings.Repeat("A", 43))
	assert.Equal(t, http.StatusNotFound, unknown.Code)
	// Only reads pass without credentials, and only on the unescaped route.
	assert.Equal(t, http.StatusUnauthorized, anonymous(srv, http.MethodPost, created.URL).Code)
	assert.Equal(t, http.StatusUnauthorized, anonymous(srv, http.MethodGet, strings.Replace(created.URL, "/shared/", "/shared%2F", 1)).Code)
	assert.Equal(t, http.StatusUnauthorized, anonymous(srv, http.MethodGet, links).Code)

	assert.NotContains(t, logs.String(), token, "the share token reached the hub log")
	assert.Contains(t, logs.String(), "/api/v1/artifacts/shared/REDACTED")

	// The list shows the link without its token.
	rec = userArtifactRequest(t, srv, owner, http.MethodGet, links, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), token)
	assert.Contains(t, rec.Body.String(), created.Link.ID)

	// Revoking ends the link and the view it opened.
	rec = userArtifactRequest(t, srv, owner, http.MethodDelete, links+"/"+created.Link.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	revoked := anonymous(srv, http.MethodGet, created.URL)
	assert.Equal(t, http.StatusNotFound, revoked.Code)
	assert.Equal(t, unknown.Body.String(), revoked.Body.String())
	assert.Equal(t, http.StatusNotFound, anonymous(srv, http.MethodGet, loc).Code)

	// Settings off, then experiment off: 404 before anything else.
	rec = userArtifactRequest(t, srv, owner, http.MethodPost, links, nil)
	require.Equal(t, http.StatusCreated, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	srv.SetOperationalSettings(artifactsOps(t, `{"enabled":false}`))
	assert.Equal(t, http.StatusNotFound, anonymous(srv, http.MethodGet, created.URL).Code, "settings off")
	srv.SetOperationalSettings(artifactsOps(t, ""))
	reg, err := experiments.NewRegistry(experiments.Default().All(), nil)
	require.NoError(t, err)
	srv.experiments = reg
	assert.Equal(t, http.StatusNotFound, anonymous(srv, http.MethodGet, created.URL).Code, "experiment off")
}

// TestArtifactsShareLinkTTLFromSettings: the settings section's link
// lifetimes reach the service.
func TestArtifactsShareLinkTTLFromSettings(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	srv.SetOperationalSettings(artifactsOps(t, `{"link_default_ttl_hours": 3, "link_max_ttl_hours": 4}`))
	ctx := context.Background()
	p1 := artifactProject(t, s, "share-ttl")
	createTestUserWithProjectRole(t, s, tid("share-ttl-owner"), "share-ttl-owner@test.com", p1.ID, store.ProjectRoleMember)
	owner, err := s.GetUser(ctx, tid("share-ttl-owner"))
	require.NoError(t, err)
	rec := userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts?name=r.md&scope="+p1.ID, []byte("r"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	links := "/api/v1/artifacts/" + decodeArtifactID(t, rec) + "/links"

	rec = userArtifactRequest(t, srv, owner, http.MethodPost, links, nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created artifacts.CreateLinkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.WithinDuration(t, time.Now().Add(3*time.Hour), created.Link.ExpiresAt, time.Minute)
	assert.Equal(t, http.StatusBadRequest, userArtifactRequest(t, srv, owner, http.MethodPost, links, []byte(`{"ttlHours": 5}`)).Code)
}

// TestIsArtifactSharedRequest: the share-link pass admits the same shapes
// as the view pass, under its own route.
func TestIsArtifactSharedRequest(t *testing.T) {
	for _, tc := range []struct {
		method, target string
		want           bool
	}{
		{http.MethodGet, "/api/v1/artifacts/shared/tok", true},
		{http.MethodHead, "/api/v1/artifacts/shared/tok", true},
		{http.MethodGet, "/api/v1/artifacts/shared/tok/files/a.png", true},
		{http.MethodPost, "/api/v1/artifacts/shared/tok", false},
		{http.MethodDelete, "/api/v1/artifacts/shared/tok", false},
		{http.MethodGet, "/api/v1/artifacts/shared/", false},
		{http.MethodGet, "/api/v1/artifacts/shared/tok/../../x", false},
		{http.MethodGet, "/api/v1/artifacts/shared%2Ftok", false},
		{http.MethodGet, "/api/v1/artifacts/shared/t%6Fk", false},
		{http.MethodGet, "/api/v1/artifacts/shared/tok/files/a%2Fb", false},
		{http.MethodGet, "/api/v1/artifacts/view/cap/index.html", false},
		{http.MethodGet, "/api/v1/artifacts/abc", false},
	} {
		r := httptest.NewRequest(tc.method, tc.target, nil)
		assert.Equal(t, tc.want, isArtifactSharedRequest(r), "%s %s", tc.method, tc.target)
	}
}

// TestCredentialPathsAreNotTraced: requests whose path carries a share
// token or a view capability are left out of tracing.
func TestCredentialPathsAreNotTraced(t *testing.T) {
	for target, want := range map[string]bool{
		"/api/v1/artifacts/shared/tok":                           false,
		"/api/v1/artifacts/%73hared/tok":                         false,
		"/api/v1/artifacts/view/cap/index.html":                  false,
		"/api/v1/artifacts/00000000-0000-4000-8000-000000000001": true,
		"/api/v1/agents":                                         true,
	} {
		assert.Equal(t, want, traceableRequest(httptest.NewRequest(http.MethodGet, target, nil)), target)
	}
}
