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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setOpenReviewCheckForTest builds the server's registry with
// hub.artifacts on and hub.artifacts_open_review_check set to the value of on.
func setOpenReviewCheckForTest(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.Artifacts:
			e.Default = true
		case experiments.ArtifactsOpenReviewCheck:
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.ArtifactsOpenReviewCheck))
}

// TestArtifactsOpenReviewOnRoutes proves the hub wiring of the open-review
// check (ptone/scion#6849): with the experiment off a heartbeat answers
// 404; with it on, a reviewer's
// heartbeat makes the owner's publish answer 409 review_open naming the
// reviewer by display name only, the refusal leaves no version behind, a
// forced publish goes through with a create-time warning, and DELETE ends
// the session.
func TestArtifactsOpenReviewOnRoutes(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	// A store on a database this test can also write grants to.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "open-review.db")+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	st := artifacts.NewStore(db, "sqlite")
	require.NoError(t, st.Init(context.Background()))
	srv.SetArtifactStore(st)
	ctx := context.Background()
	p1 := artifactProject(t, s, "open-review-p1")
	_, ownerTok := artifactAgent(t, srv, s, p1.ID, "open-review-owner", AgentRoleBaseline)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("art-open-reviewer"), Email: "rita@test.com", DisplayName: "Rita Reviewer", Role: "member", Status: "active",
	}))
	createTestUserWithProjectRole(t, s, tid("art-open-reviewer"), "rita@test.com", p1.ID, store.ProjectRoleMember)
	reviewer, err := s.GetUser(ctx, tid("art-open-reviewer"))
	require.NoError(t, err)

	manifest := func(body string, force bool) []byte {
		sum := sha256.Sum256([]byte(body))
		b, err := json.Marshal(artifacts.CreateVersionRequest{Key: "plan", Entry: "plan.md", Force: force,
			Files: []artifacts.ManifestFile{{Path: "plan.md", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}}})
		require.NoError(t, err)
		return b
	}
	const v1 = "# Plan\n"
	rec := doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts", manifest(v1, false), ownerTok)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var pend artifacts.PendingVersionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pend))
	id := pend.Artifact.ID
	require.Equal(t, http.StatusNoContent, doRawAgentRequest(t, srv, http.MethodPut, "/api/v1/artifacts/"+id+"/versions/1/files/plan.md", []byte(v1), ownerTok).Code)
	require.Equal(t, http.StatusOK, doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts/"+id+"/versions/1/finalize", nil, ownerTok).Code)
	_, err = db.Exec(`INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"g-open-review", id, artifacts.SubjectPrincipal, artifacts.PrincipalRef(artifacts.PrincipalKindUser, reviewer.ID), artifacts.GrantReview,
		time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00"))
	require.NoError(t, err)

	const sid = "tab-0123456789abcdefABCDEF"
	beatPath := "/api/v1/artifacts/" + id + "/review-sessions/" + sid
	beat := []byte(`{"base":1,"edited":true,"dirty":true}`)

	// Experiment off: a heartbeat answers 404 and nothing is recorded.
	setOpenReviewCheckForTest(t, srv, false)
	rec = userArtifactRequest(t, srv, reviewer, http.MethodPut, beatPath, beat)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	open, err := st.ListOpenReviewSessions(ctx, id, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Empty(t, open)

	setOpenReviewCheckForTest(t, srv, true)

	// Experiment on, refused callers through the hub's artifactHost: a user
	// who cannot read the artifact gets 404 as for a missing artifact, and a
	// reader without a review grant gets 403; neither records a session.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("art-open-outsider"), Email: "olly@test.com", DisplayName: "Olly Outsider", Role: "member", Status: "active",
	}))
	outsider, err := s.GetUser(ctx, tid("art-open-outsider"))
	require.NoError(t, err)
	createTestUserWithProjectRole(t, s, tid("art-open-reader"), "rhea@test.com", p1.ID, store.ProjectRoleMember)
	reader, err := s.GetUser(ctx, tid("art-open-reader"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"g-open-reader", id, artifacts.SubjectPrincipal, artifacts.PrincipalRef(artifacts.PrincipalKindUser, reader.ID), artifacts.GrantRead,
		time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00"))
	require.NoError(t, err)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		var body []byte
		if method == http.MethodPut {
			body = beat
		}
		rec = userArtifactRequest(t, srv, outsider, method, beatPath, body)
		assert.Equal(t, http.StatusNotFound, rec.Code, "outsider %s: %s", method, rec.Body.String())
		rec = userArtifactRequest(t, srv, reader, method, beatPath, body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "reader without a review grant %s: %s", method, rec.Body.String())
	}
	open, err = st.ListOpenReviewSessions(ctx, id, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Empty(t, open, "refused callers record no session")

	rec = userArtifactRequest(t, srv, reviewer, http.MethodPut, beatPath, beat)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"currentSeq":1}`, rec.Body.String())

	// The owner's publish is refused, naming the reviewer only.
	rec = doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts", manifest("# Plan v2\n", false), ownerTok)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var refusal struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Reviews []artifacts.ReviewOpenEntry `json:"reviews"`
				Base    int                         `json:"base"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &refusal))
	assert.Equal(t, artifacts.CodeReviewOpen, refusal.Error.Code)
	require.Len(t, refusal.Error.Details.Reviews, 1)
	assert.Equal(t, "Rita Reviewer", refusal.Error.Details.Reviews[0].Reviewer)
	assert.True(t, refusal.Error.Details.Reviews[0].UnsavedChanges)
	assert.Equal(t, 1, refusal.Error.Details.Base)
	for _, secret := range []string{reviewer.ID, reviewer.Email, sid} {
		assert.NotContains(t, rec.Body.String(), secret, "the refusal names no id, email or session")
	}
	var versions int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM artifact_version WHERE artifact_id = ?`, id).Scan(&versions))
	assert.Equal(t, 1, versions, "a refused publish leaves no version behind, pending or failed")

	// Forced: the create answers with the warning.
	rec = doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts", manifest("# Plan v2\n", true), ownerTok)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var forced artifacts.PendingVersionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &forced))
	require.Len(t, forced.Warnings, 1)
	assert.Contains(t, forced.Warnings[0], "Rita Reviewer")

	rec = userArtifactRequest(t, srv, reviewer, http.MethodDelete, beatPath, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	open, err = st.ListOpenReviewSessions(ctx, id, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Empty(t, open)
}

// TestArtifactHostFeatureEnabledFailsClosed: the hub's FeatureGate answers
// only registered server-layer experiments, and the package constant names
// the registered experiment.
func TestArtifactHostFeatureEnabledFailsClosed(t *testing.T) {
	srv, _ := testServer(t)
	assert.Equal(t, experiments.ArtifactsOpenReviewCheck, artifacts.FeatureOpenReviewCheck)
	h := newArtifactHost(srv)
	ctx := context.Background()
	assert.False(t, h.FeatureEnabled(ctx, "hub.not_registered"), "an unknown name is off")
	assert.False(t, h.FeatureEnabled(ctx, ""), "an empty name is off")
	assert.False(t, h.FeatureEnabled(ctx, experiments.ArtifactReviewEditor), "a web-only experiment is off for the hub")
	setOpenReviewCheckForTest(t, srv, false)
	assert.False(t, h.FeatureEnabled(ctx, artifacts.FeatureOpenReviewCheck))
	setOpenReviewCheckForTest(t, srv, true)
	assert.True(t, h.FeatureEnabled(ctx, artifacts.FeatureOpenReviewCheck))
	assert.False(t, (&artifactHost{}).FeatureEnabled(ctx, artifacts.FeatureOpenReviewCheck), "no server, no feature")
}

// TestArtifactHostPrincipalDisplayNames: a user's display name and an
// agent's name are answered; an email-like display name, an unknown id and
// a malformed ref are left out, so the service says "a reviewer".
func TestArtifactHostPrincipalDisplayNames(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	p := artifactProject(t, s, "names-p")
	agent, _ := artifactAgent(t, srv, s, p.ID, "names-agent", AgentRoleBaseline)
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: tid("names-named"), Email: "n@test.com", DisplayName: "Nora Named", Role: "member", Status: "active"}))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: tid("names-email"), Email: "e@test.com", DisplayName: "e@test.com", Role: "member", Status: "active"}))
	named := artifacts.PrincipalRef(artifacts.PrincipalKindUser, tid("names-named"))
	emailOnly := artifacts.PrincipalRef(artifacts.PrincipalKindUser, tid("names-email"))
	agentRef := artifacts.PrincipalRef(artifacts.PrincipalKindAgent, agent.ID)
	unknown := artifacts.PrincipalRef(artifacts.PrincipalKindUser, "00000000-0000-4000-8000-000000000000")
	got := newArtifactHost(srv).PrincipalDisplayNames(ctx, []string{named, emailOnly, agentRef, unknown, "user:not-a-uuid"})
	assert.Equal(t, map[string]string{named: "Nora Named", agentRef: agent.Name}, got)
}
