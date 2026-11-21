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
	"fmt"
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

// setReviewDraftsForTest builds srv's experiment registry with hub.artifacts
// on and hub.artifacts_review_drafts set to on, preserving any other active
// defaults on srv when srv.experiments is already initialized.
func setReviewDraftsForTest(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range srv.experimentRegistry().All() {
		switch e.Name {
		case experiments.Artifacts:
			e.Default = true
		case experiments.ArtifactsReviewDrafts:
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.ArtifactsReviewDrafts))
	require.True(t, srv.experimentEnabled(experiments.Artifacts))
}

// TestArtifactsReviewDraftsOnRoutes proves the hub wiring of review drafts
// (ptone/scion#7116, ptone/scion#8470):
//   - while hub.artifacts_review_drafts is off, GET/PUT/DELETE answer 404;
//   - while on, outsider -> 404, reader without review -> 403, review grantee
//     saves, reads, conflicts without body leak, and deletes conditionally;
//   - draft PUT and DELETE emit zero messages or notifications to the artifact
//     owner, whereas submitting one finalized review emits exactly one.
func TestArtifactsReviewDraftsOnRoutes(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "review-drafts.db")+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	st := artifacts.NewStore(db, "sqlite")
	require.NoError(t, st.Init(context.Background()))
	srv.SetArtifactStore(st)
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	ctx := context.Background()
	p1 := artifactProject(t, s, "drafts-p1")
	_, ownerTok := artifactAgent(t, srv, s, p1.ID, "drafts-owner", AgentRoleBaseline)
	createTestUserWithProjectRole(t, s, tid("drafts-reviewer"), "rita-draft@test.com", p1.ID, store.ProjectRoleMember)
	reviewer, err := s.GetUser(ctx, tid("drafts-reviewer"))
	require.NoError(t, err)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("drafts-outsider"), Email: "out-draft@test.com", DisplayName: "Out", Role: "member", Status: "active",
	}))
	outsider, err := s.GetUser(ctx, tid("drafts-outsider"))
	require.NoError(t, err)
	createTestUserWithProjectRole(t, s, tid("drafts-reader"), "reader-draft@test.com", p1.ID, store.ProjectRoleMember)
	reader, err := s.GetUser(ctx, tid("drafts-reader"))
	require.NoError(t, err)

	manifest := func(kind, key, body string) []byte {
		sum := sha256.Sum256([]byte(body))
		b, merr := json.Marshal(artifacts.CreateVersionRequest{Kind: kind, Key: key, Entry: "plan.md",
			Files: []artifacts.ManifestFile{{Path: "plan.md", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}}})
		require.NoError(t, merr)
		return b
	}
	const v1 = "# Plan\n\nWe ship in Q3.\n"
	rec := doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts", manifest("", "plan", v1), ownerTok)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var pend artifacts.PendingVersionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pend))
	id := pend.Artifact.ID
	require.Equal(t, http.StatusNoContent, doRawAgentRequest(t, srv, http.MethodPut, "/api/v1/artifacts/"+id+"/versions/1/files/plan.md", []byte(v1), ownerTok).Code)
	require.Equal(t, http.StatusOK, doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts/"+id+"/versions/1/finalize", nil, ownerTok).Code)

	_, err = db.Exec(`INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"g-draft-review", id, artifacts.SubjectPrincipal, artifacts.PrincipalRef(artifacts.PrincipalKindUser, reviewer.ID), artifacts.GrantReview,
		time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00"))
	require.NoError(t, err)

	draftURL := "/api/v1/artifacts/" + id + "/review-draft"
	const draftMarked = "# Plan\n\nWe {~~ship~>launch~~} in Q3.{>>confirm<<}\n"
	putRev0 := []byte(`{"rev":0,"base":1,"entry":"plan.md","body":` + mustJSONString(t, draftMarked) + `,"note":"wip note"}`)

	// Default / explicit experiment off -> 404 on every method, nothing stored.
	require.False(t, srv.experimentEnabled(experiments.ArtifactsReviewDrafts))
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec = userArtifactRequest(t, srv, reviewer, m, draftURL, putRev0)
		assert.Equal(t, http.StatusNotFound, rec.Code, "experiment off %s: %s", m, rec.Body.String())
	}
	_, err = st.GetReviewDraft(ctx, id, artifacts.PrincipalKindUser, reviewer.ID)
	require.ErrorIs(t, err, artifacts.ErrNotFound)

	// Enable hub.artifacts_review_drafts.
	setReviewDraftsForTest(t, srv, true)

	// Outsider -> 404; reader without review grant -> 403.
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		assert.Equal(t, http.StatusNotFound, userArtifactRequest(t, srv, outsider, m, draftURL, putRev0).Code)
		assert.Equal(t, http.StatusForbidden, userArtifactRequest(t, srv, reader, m, draftURL, putRev0).Code)
	}

	// Reviewer GET before any save -> 200 {"draft":null,"currentSeq":1}.
	rec = userArtifactRequest(t, srv, reviewer, http.MethodGet, draftURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"draft":null,"currentSeq":1}`, rec.Body.String())

	// Reviewer PUT rev=0 -> 200 rev=1; PUT rev=1 -> 200 rev=2.
	rec = userArtifactRequest(t, srv, reviewer, http.MethodPut, draftURL, putRev0)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var put1 artifacts.PutReviewDraftResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &put1))
	assert.Equal(t, 1, put1.Rev)
	assert.Equal(t, 1, put1.CurrentSeq)

	putRev1 := []byte(`{"rev":1,"base":1,"entry":"plan.md","body":` + mustJSONString(t, draftMarked) + `,"note":"wip note 2"}`)
	rec = userArtifactRequest(t, srv, reviewer, http.MethodPut, draftURL, putRev1)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Stale PUT rev=1 -> 409 draft_conflict without body or note.
	rec = userArtifactRequest(t, srv, reviewer, http.MethodPut, draftURL, putRev1)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), artifacts.CodeDraftConflict)
	assert.NotContains(t, rec.Body.String(), "launch")
	assert.NotContains(t, rec.Body.String(), "wip note")

	// Reviewer DELETE ?rev=2 -> 204.
	rec = userArtifactRequest(t, srv, reviewer, http.MethodDelete, draftURL+"?rev=2", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// Give background goroutines a window: draft PUT/DELETE must never emit any
	// agent dispatch or user notification.
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, dispatcher.getCalls(), "draft saves and deletes must emit no review notice")

	// Submitting a finalized review version emits exactly one notice.
	rec = userArtifactRequest(t, srv, reviewer, http.MethodPost, "/api/v1/artifacts/"+id+"/versions", manifest(artifacts.VersionKindReview, "", draftMarked))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var revPend artifacts.PendingVersionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &revPend))
	vbase := fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", id, revPend.Version.Seq)
	require.Equal(t, http.StatusNoContent, userArtifactRequest(t, srv, reviewer, http.MethodPut, vbase+"/files/plan.md", []byte(draftMarked)).Code)
	require.Equal(t, http.StatusOK, userArtifactRequest(t, srv, reviewer, http.MethodPost, vbase+"/finalize", []byte(`{"base":1}`)).Code)
	require.Eventually(t, func() bool { return len(dispatcher.getCalls()) == 1 }, 5*time.Second, 10*time.Millisecond)
}

// TestArtifactsReviewDraftsAgentCreatorAndScopedTokenIsolation verifies C1
// end-to-end through the real Hub:
//   - an agent's credential cannot read, overwrite or delete its creator's
//     draft, and the creator (even as project owner / super-admin) cannot read,
//     overwrite or delete the agent's draft;
//   - a scoped user access token whose boundary excludes the artifact's home
//     project gets 404, and one lacking artifact:create gets 403, while the
//     user's stored draft row survives untouched (S4).
func TestArtifactsReviewDraftsAgentCreatorAndScopedTokenIsolation(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	setReviewDraftsForTest(t, srv, true)
	ctx := context.Background()

	p1 := artifactProject(t, s, "c1-p1")
	p2 := artifactProject(t, s, "c1-p2")
	createTestUserWithProjectRole(t, s, tid("c1-creator"), "creator@test.com", p1.ID, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, s, tid("c1-creator"), "creator@test.com", p2.ID, store.ProjectRoleOwner)
	creator, err := s.GetUser(ctx, tid("c1-creator"))
	require.NoError(t, err)
	creatorSession := NewAuthenticatedUser(creator.ID, creator.Email, creator.DisplayName, creator.Role, "web")

	// Create an agent in p1 recorded as created by creator.
	worker, workerTok := artifactAgent(t, srv, s, p1.ID, "c1-worker", AgentRoleBaseline)
	worker.CreatedBy = creator.ID
	require.NoError(t, s.UpdateAgent(ctx, worker))

	// Creator publishes an artifact in p1 and grants worker write access so both
	// creator and worker have full review/write access.
	rec := identityArtifactRequest(t, srv, creatorSession, http.MethodPost, "/api/v1/artifacts?name=spec.md&scope="+p1.ID, []byte("# Spec\n"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)
	rec = identityArtifactRequest(t, srv, creatorSession, http.MethodPost, "/api/v1/artifacts/"+id+"/grants",
		[]byte(`{"subjectKind":"principal","subjectRef":"agent:`+worker.ID+`","permission":"write"}`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	draftURL := "/api/v1/artifacts/" + id + "/review-draft"
	creatorDraft := []byte(`{"rev":0,"base":1,"entry":"spec.md","body":"# Spec\n{>>creator private<<}\n","note":"creator note"}`)
	workerDraft := []byte(`{"rev":0,"base":1,"entry":"spec.md","body":"# Spec\n{>>worker private<<}\n","note":"worker note"}`)

	// 1. Creator saves draft; worker cannot read, overwrite (rev=1 -> 409), or delete it.
	rec = identityArtifactRequest(t, srv, creatorSession, http.MethodPut, draftURL, creatorDraft)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRawAgentRequest(t, srv, http.MethodGet, draftURL, nil, workerTok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"draft":null,"currentSeq":1}`, rec.Body.String())

	rec = doRawAgentRequest(t, srv, http.MethodPut, draftURL,
		[]byte(`{"rev":1,"base":1,"entry":"spec.md","body":"hijack","note":""}`), workerTok)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "creator private")

	rec = doRawAgentRequest(t, srv, http.MethodDelete, draftURL, nil, workerTok)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// Creator's draft is still intact at rev=1.
	rec = identityArtifactRequest(t, srv, creatorSession, http.MethodGet, draftURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var gotCreator artifacts.ReviewDraftResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &gotCreator))
	require.NotNil(t, gotCreator.Draft)
	assert.Equal(t, 1, gotCreator.Draft.Rev)
	assert.Contains(t, gotCreator.Draft.Body, "creator private")

	// 2. Worker saves its own draft; creator (and a hub super-admin) cannot read,
	// overwrite or delete worker's draft.
	rec = doRawAgentRequest(t, srv, http.MethodPut, draftURL, workerDraft, workerTok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Delete creator's own draft so creator has none; creator GET sees null,
	// never worker's draft.
	require.Equal(t, http.StatusNoContent,
		identityArtifactRequest(t, srv, creatorSession, http.MethodDelete, draftURL, nil).Code)
	rec = identityArtifactRequest(t, srv, creatorSession, http.MethodGet, draftURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"draft":null,"currentSeq":1}`, rec.Body.String())

	// Super-admin user in p1 (without grant on creator's user-owned artifact -> 403;
	// even with admin grant -> sees only own null draft, never worker's).
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("c1-admin"), Email: "admin-c1@test.com", DisplayName: "Admin", Role: store.UserRoleAdmin, Status: "active",
	}))
	createTestUserWithProjectRole(t, s, tid("c1-admin"), "admin-c1@test.com", p1.ID, store.ProjectRoleOwner)
	adminUser, err := s.GetUser(ctx, tid("c1-admin"))
	require.NoError(t, err)
	rec = identityArtifactRequest(t, srv, creatorSession, http.MethodPost, "/api/v1/artifacts/"+id+"/grants",
		[]byte(`{"subjectKind":"principal","subjectRef":"user:`+adminUser.ID+`","permission":"admin"}`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = userArtifactRequest(t, srv, adminUser, http.MethodGet, draftURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"draft":null,"currentSeq":1}`, rec.Body.String())

	rec = doRawAgentRequest(t, srv, http.MethodGet, draftURL, nil, workerTok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var gotWorker artifacts.ReviewDraftResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &gotWorker))
	require.NotNil(t, gotWorker.Draft)
	assert.Equal(t, 1, gotWorker.Draft.Rev)
	assert.Contains(t, gotWorker.Draft.Body, "worker private")

	// 3. Scoped user access tokens (C1 & S4): save creator's draft again, then
	// send GET/PUT/DELETE with a token bounded to p2 (excludes p1 -> 404) and a
	// read-only token in p1 (-> 403). Creator's stored draft must survive intact.
	require.Equal(t, http.StatusOK,
		identityArtifactRequest(t, srv, creatorSession, http.MethodPut, draftURL, creatorDraft).Code)

	excludedUAT := artifactTestUAT(t, creatorSession, p2.ID, "artifact:read", "artifact:create", "artifact:manage")
	readOnlyUAT := artifactTestUAT(t, creatorSession, p1.ID, "artifact:read")
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		var b []byte
		if m == http.MethodPut {
			b = []byte(`{"rev":1,"base":1,"entry":"spec.md","body":"clobber","note":""}`)
		}
		assert.Equal(t, http.StatusNotFound, identityArtifactRequest(t, srv, excludedUAT, m, draftURL, b).Code, "excluded UAT %s", m)
		assert.Equal(t, http.StatusForbidden, identityArtifactRequest(t, srv, readOnlyUAT, m, draftURL, b).Code, "read-only UAT %s", m)
	}

	rec = identityArtifactRequest(t, srv, creatorSession, http.MethodGet, draftURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var survived artifacts.ReviewDraftResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &survived))
	require.NotNil(t, survived.Draft)
	assert.Equal(t, 1, survived.Draft.Rev)
	assert.Contains(t, survived.Draft.Body, "creator private")
}

// TestArtifactHostFeatureEnabledReviewDrafts verifies C5 for
// hub.artifacts_review_drafts.
func TestArtifactHostFeatureEnabledReviewDrafts(t *testing.T) {
	srv, _ := testServer(t)
	assert.Equal(t, experiments.ArtifactsReviewDrafts, artifacts.FeatureReviewDrafts)
	h := newArtifactHost(srv)
	ctx := context.Background()
	assert.False(t, h.FeatureEnabled(ctx, artifacts.FeatureReviewDrafts), "default off")
	assert.False(t, h.FeatureEnabled(ctx, "hub.unregistered_review_drafts"), "unregistered fails closed")
	setReviewDraftsForTest(t, srv, true)
	assert.True(t, h.FeatureEnabled(ctx, artifacts.FeatureReviewDrafts))
	setReviewDraftsForTest(t, srv, false)
	assert.False(t, h.FeatureEnabled(ctx, artifacts.FeatureReviewDrafts))
}

// TestReapArtifactVersionsReapsReviewDraftsWhenExperimentOff proves that
// reapArtifactVersions runs the 30-day review-draft TTL pass even while
// hub.artifacts_review_drafts is off.
func TestReapArtifactVersionsReapsReviewDraftsWhenExperimentOff(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	require.False(t, srv.experimentEnabled(experiments.ArtifactsReviewDrafts))
	ctx := context.Background()

	p1 := artifactProject(t, s, "reap-drafts-p1")
	createTestUserWithProjectRole(t, s, tid("reap-drafts-u"), "reap-u@test.com", p1.ID, store.ProjectRoleMember)
	u, err := s.GetUser(ctx, tid("reap-drafts-u"))
	require.NoError(t, err)

	rec := userArtifactRequest(t, srv, u, http.MethodPost, "/api/v1/artifacts?name=old.md&scope="+p1.ID, []byte("# old\n"))
	require.Equal(t, http.StatusCreated, rec.Code)
	oldArtID := decodeArtifactID(t, rec)
	rec = userArtifactRequest(t, srv, u, http.MethodPost, "/api/v1/artifacts?name=fresh.md&scope="+p1.ID, []byte("# fresh\n"))
	require.Equal(t, http.StatusCreated, rec.Code)
	freshArtID := decodeArtifactID(t, rec)

	st := srv.ArtifactStore()
	now := time.Now().UTC()
	_, err = st.PutReviewDraft(ctx, artifacts.ReviewDraft{
		ArtifactID: oldArtID, PrincipalKind: artifacts.PrincipalKindUser, PrincipalRef: u.ID,
		BaseSeq: 1, EntryPath: "old.md", Body: "expired", Note: "",
	}, 0, now.Add(-artifacts.DraftTTL-time.Hour), artifacts.MaxDraftsPerPrincipal)
	require.NoError(t, err)
	_, err = st.PutReviewDraft(ctx, artifacts.ReviewDraft{
		ArtifactID: freshArtID, PrincipalKind: artifacts.PrincipalKindUser, PrincipalRef: u.ID,
		BaseSeq: 1, EntryPath: "fresh.md", Body: "active", Note: "",
	}, 0, now.Add(-time.Hour), artifacts.MaxDraftsPerPrincipal)
	require.NoError(t, err)

	srv.reapArtifactVersions(ctx)

	_, err = st.GetReviewDraft(ctx, oldArtID, artifacts.PrincipalKindUser, u.ID)
	assert.ErrorIs(t, err, artifacts.ErrNotFound, "expired draft reaped while experiment is off")
	kept, err := st.GetReviewDraft(ctx, freshArtID, artifacts.PrincipalKindUser, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "active", kept.Body)
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}
