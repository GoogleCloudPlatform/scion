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
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArtifactsList_FilterQueryDialect(t *testing.T) {
	srv, s := testServer(t)
	st, _ := enableArtifactsForTest(t, srv)
	ctx := context.Background()

	p1 := artifactProject(t, s, "fq-alpha")
	p2 := artifactProject(t, s, "fq-beta")
	userID := tid("fq-user")
	createTestUserWithProjectRole(t, s, userID, "fq-user@test.com", p1.ID, store.ProjectRoleMember)
	otherID := tid("fq-other")
	createTestUserWithProjectRole(t, s, otherID, "fq-other@test.com", p2.ID, store.ProjectRoleMember)

	user, err := s.GetUser(ctx, userID)
	require.NoError(t, err)
	other, err := s.GetUser(ctx, otherID)
	require.NoError(t, err)
	agent1, _ := artifactAgent(t, srv, s, p1.ID, "fq-agent-1", AgentRoleBaseline)
	agent2, _ := artifactAgent(t, srv, s, p2.ID, "fq-agent-2", AgentRoleBaseline)

	// Seed: owned by user in p1 (review pending), agent1 in p1, shared from p2, private in p2.
	ownID := tid("fq-own")
	now := time.Now()
	require.NoError(t, st.CreatePublished(ctx,
		&artifacts.Artifact{ID: ownID, ScopeKind: artifacts.ScopeKindProject, ScopeRef: p1.ID,
			OwnerKind: artifacts.PrincipalKindUser, OwnerRef: user.ID, Title: "Weekly Review", CreatedAt: now, UpdatedAt: now},
		&artifacts.Version{ID: tid("fq-own-v1"), ArtifactID: ownID, Seq: 1, Kind: artifacts.VersionKindReview,
			EntryPath: "w.md", TotalBytes: 1, FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady},
		nil,
		[]artifacts.Grant{{ID: tid("fq-own-home"), ArtifactID: ownID, SubjectKind: artifacts.SubjectScope,
			SubjectRef: p1.ID, Permission: artifacts.GrantRead, CreatedAt: now}},
	))
	projID := seedListedArtifact(t, st, "is:reviewed literal", p1.ID, artifacts.PrincipalKindAgent, agent1.ID)
	sharedID := seedListedArtifact(t, st, "shared-beta", p2.ID, artifacts.PrincipalKindAgent, agent2.ID,
		principalGrant(artifacts.PrincipalKindUser, user.ID))
	_ = seedListedArtifact(t, st, "private-beta", p2.ID, artifacts.PrincipalKindUser, other.ID)

	list := func(target string) []string {
		t.Helper()
		return listedIDs(decodeArtifactList(t, userArtifactRequest(t, srv, user, http.MethodGet, target, nil)))
	}

	// Structured queries work end-to-end.
	assert.Equal(t, sortedIDs(ownID), list(artifactListPath+"&q="+url.QueryEscape("is:reviewed")), "is:reviewed")
	assert.Equal(t, sortedIDs(ownID), list(artifactListPath+"&q="+url.QueryEscape("is:mine")), "is:mine")
	assert.Equal(t, sortedIDs(sharedID), list(artifactListPath+"&q="+url.QueryEscape("is:shared")), "is:shared")
	assert.Equal(t, sortedIDs(ownID, projID), list(artifactListPath+"&q="+url.QueryEscape("project:"+p1.Slug)), "authorized project slug")

	// Unauthorized project slug (p2) returns empty 200 (indistinguishable from nonexistent slug).
	assert.Empty(t, list(artifactListPath+"&q="+url.QueryEscape("project:"+p2.Slug)), "unauthorized project slug")
	assert.Empty(t, list(artifactListPath+"&q="+url.QueryEscape("project:no-such-slug")), "nonexistent project slug")

	// Invalid query returns 400 invalid_filter_query.
	rec := userArtifactRequest(t, srv, user, http.MethodGet, artifactListPath+"&q="+url.QueryEscape("is:mine is:shared"), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var errBody struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.Equal(t, ErrCodeInvalidFilterQuery, errBody.Error.Code)
	assert.Contains(t, errBody.Error.Details, "offset")
	assert.Contains(t, errBody.Error.Details, "length")

	// Cursor HMAC binds Canonical(ast): reordered query succeeds on page 2.
	p1Resp := decodeArtifactList(t, userArtifactRequest(t, srv, user, http.MethodGet,
		artifactListPath+"&limit=1&q="+url.QueryEscape("project:"+p1.Slug+" -is:shared"), nil))
	require.Len(t, p1Resp.Artifacts, 1)
	require.NotEmpty(t, p1Resp.NextCursor)
	p2Resp := decodeArtifactList(t, userArtifactRequest(t, srv, user, http.MethodGet,
		artifactListPath+"&limit=1&q="+url.QueryEscape("-IS:SHARED PROJECT:"+p1.Slug)+"&cursor="+url.QueryEscape(p1Resp.NextCursor), nil))
	require.Len(t, p2Resp.Artifacts, 1)
	assert.NotEqual(t, p1Resp.Artifacts[0].ID, p2Resp.Artifacts[0].ID)
}
