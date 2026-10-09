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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// artifactsOpsWith is artifactsOps plus a messaging section.
func artifactsOpsWith(t *testing.T, artifactsDoc, messagingDoc string) *OperationalSettings {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	if artifactsDoc != "" {
		fakeStore.seed("artifacts", json.RawMessage(artifactsDoc))
	}
	if messagingDoc != "" {
		fakeStore.seed("messaging", json.RawMessage(messagingDoc))
	}
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	return ops
}

// TestArtifactsGrantsOnRoutes: through the real hub, the owner shares an
// artifact with a user in another project and, once cross-project
// messaging is on, with that project, whose agents can then read it; the
// agent's own token still bounds what a grant can reach.
func TestArtifactsGrantsOnRoutes(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	ctx := context.Background()
	p1 := artifactProject(t, s, "grants-p1")
	p2 := artifactProject(t, s, "grants-p2")
	createTestUserWithProjectRole(t, s, tid("grants-owner"), "grants-owner@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, tid("grants-other"), "grants-other@test.com", p2.ID, store.ProjectRoleMember)
	owner, err := s.GetUser(ctx, tid("grants-owner"))
	require.NoError(t, err)
	other, err := s.GetUser(ctx, tid("grants-other"))
	require.NoError(t, err)
	_, agentTok := artifactAgent(t, srv, s, p2.ID, "grants-agent", AgentRoleBaseline)

	rec := userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts?name=g.md&scope="+p1.ID, []byte("# g\n"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)
	path := "/api/v1/artifacts/" + id
	grants := path + "/grants"

	assert.Equal(t, http.StatusNotFound, userArtifactRequest(t, srv, other, http.MethodGet, path, nil).Code)
	rec = userArtifactRequest(t, srv, owner, http.MethodPost, grants,
		[]byte(`{"subjectKind":"principal","subjectRef":"user:`+other.ID+`","permission":"read"}`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusOK, userArtifactRequest(t, srv, other, http.MethodGet, path, nil).Code)

	// Cross-project scope grants follow messaging.cross_project_messaging_enabled.
	scopeBody := []byte(`{"subjectKind":"scope","subjectRef":"` + p2.ID + `","permission":"read"}`)
	rec = userArtifactRequest(t, srv, owner, http.MethodPost, grants, scopeBody)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "cross_project_sharing_disabled")
	assert.Equal(t, http.StatusNotFound, doRawAgentRequest(t, srv, http.MethodGet, path, nil, agentTok).Code)

	srv.SetOperationalSettings(artifactsOpsWith(t, "", `{"cross_project_messaging_enabled": true}`))
	rec = userArtifactRequest(t, srv, owner, http.MethodPost, grants, scopeBody)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusOK, doRawAgentRequest(t, srv, http.MethodGet, path, nil, agentTok).Code)

	// Agents never administer, even with read.
	assert.Equal(t, http.StatusForbidden, doRawAgentRequest(t, srv, http.MethodPost, grants, scopeBody, agentTok).Code)
	assert.Equal(t, http.StatusForbidden, doRawAgentRequest(t, srv, http.MethodPatch, path, []byte(`{"expiresAt":null}`), agentTok).Code)
}

// TestArtifactsRetentionAndSweepOnRoutes: the default retention setting
// gives new artifacts an expiry; the hub's maintenance pass deletes
// expired artifacts (their links stop working) and, after the grace
// period, their blobs.
func TestArtifactsRetentionAndSweepOnRoutes(t *testing.T) {
	srv, s := testServer(t)
	st, blobs := enableArtifactsForTest(t, srv)
	srv.SetOperationalSettings(artifactsOpsWith(t, `{"default_retention_days": 2}`, ""))
	ctx := context.Background()
	p1 := artifactProject(t, s, "retention-p1")
	createTestUserWithProjectRole(t, s, tid("retention-owner"), "retention-owner@test.com", p1.ID, store.ProjectRoleMember)
	owner, err := s.GetUser(ctx, tid("retention-owner"))
	require.NoError(t, err)

	content := []byte("# expiring\n")
	rec := userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts?name=e.md&scope="+p1.ID, content)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var pub artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pub))
	require.NotNil(t, pub.Artifact.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(48*time.Hour), *pub.Artifact.ExpiresAt, time.Minute)
	id := pub.Artifact.ID

	rec = userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts/"+id+"/links", nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var link artifacts.CreateLinkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &link))
	assert.True(t, link.ClampedToArtifactExpiry, "a 7-day link on a 2-day artifact is cut to the artifact's expiry")

	// Expire it now and run the maintenance pass.
	past := time.Now().Add(-time.Minute)
	_, err = st.SetExpiry(ctx, id, &past)
	require.NoError(t, err)
	srv.reapArtifactVersions(ctx)
	_, err = st.GetArtifact(ctx, id)
	assert.ErrorIs(t, err, artifacts.ErrNotFound, "the sweep deleted the expired artifact")
	assert.Equal(t, http.StatusNotFound, anonymous(srv, http.MethodGet, link.URL).Code)
	gs, err := st.ListGrants(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, gs, "grants and links go with the artifact")

	// The blob stays through the grace period (the pass ran at real time).
	digest := pub.Version.Files[0].SHA256
	exists, err := blobs.Exists(ctx, artifacts.BlobPath(srv.hubID, digest))
	require.NoError(t, err)
	assert.True(t, exists, "the blob is kept for the grace period")
}
