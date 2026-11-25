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
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setReviewRequestsForTest(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range srv.experimentRegistry().All() {
		switch e.Name {
		case experiments.Artifacts, experiments.FiltersQueryDialect:
			e.Default = true
		case experiments.ArtifactsReviewRequests:
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.ArtifactsReviewRequests))
}

// TestArtifactsReviewRequestsOnRoutes exercises the full Phase 1 hub flow:
//   - gate off: routes 404, fields absent, requested=1 empty;
//   - gate on: user A requests review from user B (who already has review grant) ->
//     B gets ARTIFACT_REVIEW_REQUESTED notification with title, version & excerpt ->
//     B sees reviewState.requestedOfMe on GET and reviewRequested on list --requested / is:requested ->
//     B dismisses with a note -> A gets ARTIFACT_REVIEW_DISMISSED with the note ->
//     B's state clears;
//   - non-requester owner: publisher C requests review from B -> B dismisses with note ->
//     both C (requester) and A (owner) receive ARTIFACT_REVIEW_DISMISSED;
//   - agent requester refused (403);
//   - FeatureEnabled fails closed.
func TestArtifactsReviewRequestsOnRoutes(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "review-requests.db")+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	st := artifacts.NewStore(db, "sqlite")
	require.NoError(t, st.Init(context.Background()))
	srv.SetArtifactStore(st)

	ctx := context.Background()
	p1 := artifactProject(t, s, "rr-p1")
	createTestUserWithProjectRole(t, s, tid("rr-alice"), "alice-rr@test.com", p1.ID, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, s, tid("rr-bob"), "bob-rr@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, tid("rr-carol"), "carol-rr@test.com", p1.ID, store.ProjectRoleMember)
	alice, err := s.GetUser(ctx, tid("rr-alice"))
	require.NoError(t, err)
	bob, err := s.GetUser(ctx, tid("rr-bob"))
	require.NoError(t, err)
	carol, err := s.GetUser(ctx, tid("rr-carol"))
	require.NoError(t, err)
	hubMembers, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)
	for _, u := range []*store.User{alice, bob, carol} {
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    hubMembers.ID,
			MemberType: store.GroupMemberTypeUser,
			MemberID:   u.ID,
			Role:       store.GroupMemberRoleMember,
		}))
	}
	_, agentTok := artifactAgent(t, srv, s, p1.ID, "rr-agent", AgentRoleBaseline)

	doAsUser := func(u *store.User, method, target string, body any) *httptest.ResponseRecorder {
		var raw []byte
		if body != nil {
			var err error
			raw, err = json.Marshal(body)
			require.NoError(t, err)
		}
		return userArtifactRequest(t, srv, u, method, target, raw)
	}

	// Publish an artifact as Alice.
	pubRec := userArtifactRequest(t, srv, alice, http.MethodPost, "/api/v1/artifacts?name=plan.md&title=Launch+Plan&scope="+p1.ID, []byte("# Plan\n"))
	require.Equal(t, http.StatusCreated, pubRec.Code, pubRec.Body.String())
	var pubResp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(pubRec.Body.Bytes(), &pubResp))
	artID := pubResp.Artifact.ID

	// Grant Bob review access and Carol write access.
	rec := doAsUser(alice, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants",
		artifacts.GrantRequest{SubjectKind: "principal", SubjectRef: "user:" + bob.ID, Permission: "review"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = doAsUser(alice, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants",
		artifacts.GrantRequest{SubjectKind: "principal", SubjectRef: "user:" + carol.ID, Permission: "write"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// 1. Gate OFF: routes 404, fields absent, requested=1 empty.
	setReviewRequestsForTest(t, srv, false)
	rec = doAsUser(alice, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests",
		artifacts.CreateReviewRequestsBody{Reviewers: []string{"user:" + bob.ID}})
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = doAsUser(bob, http.MethodGet, "/api/v1/artifacts/"+artID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"reviewState"`)

	rec = doAsUser(bob, http.MethodGet, "/api/v1/artifacts?mine=1&requested=1", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var listResp artifacts.ArtifactListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	assert.Empty(t, listResp.Artifacts)

	// 2. Gate ON: Alice requests review from Bob with a note.
	setReviewRequestsForTest(t, srv, true)
	rec = doAsUser(alice, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests",
		artifacts.CreateReviewRequestsBody{
			Reviewers: []string{"user:" + bob.ID},
			Note:      "Please verify rollback steps",
		})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var createResp artifacts.CreateReviewRequestsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createResp))
	require.Len(t, createResp.Requests, 1)
	assert.Equal(t, artifacts.ReviewRequestOutcomeCreated, createResp.Requests[0].Outcome)
	assert.Equal(t, artifacts.ReviewRequestAccessExisting, createResp.Requests[0].Access)
	assert.True(t, createResp.Requests[0].Notified)

	// Bob received ARTIFACT_REVIEW_REQUESTED notification with title, version, and excerpt.
	bobNotifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, bob.ID, false)
	require.NoError(t, err)
	require.Len(t, bobNotifs, 1)
	assert.Equal(t, store.NotificationKindArtifactReviewRequest, bobNotifs[0].Kind)
	assert.Equal(t, store.NotificationStatusArtifactReviewRequested, bobNotifs[0].Status)
	assert.Equal(t, artID, bobNotifs[0].ResourceID)
	assert.Contains(t, bobNotifs[0].Message, `"Launch Plan"`)
	assert.Contains(t, bobNotifs[0].Message, "(v1)")
	assert.Equal(t, "Please verify rollback steps", bobNotifs[0].Excerpt)

	// Bob sees reviewState.requestedOfMe on GET and reviewRequested on list --requested and is:requested.
	rec = doAsUser(bob, http.MethodGet, "/api/v1/artifacts/"+artID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var getResp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &getResp))
	require.NotNil(t, getResp.ReviewState)
	require.NotNil(t, getResp.ReviewState.RequestedOfMe)
	assert.Equal(t, "Please verify rollback steps", getResp.ReviewState.RequestedOfMe.Note)

	rec = doAsUser(bob, http.MethodGet, "/api/v1/artifacts?mine=1&requested=1", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	require.Len(t, listResp.Artifacts, 1)
	assert.Equal(t, artID, listResp.Artifacts[0].ID)
	assert.True(t, listResp.Artifacts[0].ReviewRequested)

	// Bob dismisses with a note.
	rec = doAsUser(bob, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-request/dismiss",
		artifacts.DismissReviewRequestBody{Note: "Out of office until Friday"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var dismissResp artifacts.DismissReviewRequestResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dismissResp))
	assert.Equal(t, artifacts.ReviewRequestStateDismissed, dismissResp.Request.State)
	assert.True(t, dismissResp.AuthorNotified)

	// Alice received ARTIFACT_REVIEW_DISMISSED with the note.
	aliceNotifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, alice.ID, false)
	require.NoError(t, err)
	require.Len(t, aliceNotifs, 1)
	assert.Equal(t, store.NotificationKindArtifactReviewRequest, aliceNotifs[0].Kind)
	assert.Equal(t, store.NotificationStatusArtifactReviewDismissed, aliceNotifs[0].Status)
	assert.Contains(t, aliceNotifs[0].Message, `"Launch Plan"`)
	assert.Equal(t, "Out of office until Friday", aliceNotifs[0].Excerpt)

	// Bob's state is cleared.
	rec = doAsUser(bob, http.MethodGet, "/api/v1/artifacts?mine=1&requested=1", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	assert.Empty(t, listResp.Artifacts)

	// 3. Non-requester owner: Carol (write grantee) requests Bob; Bob dismisses ->
	// both Carol (requester) and Alice (owner) receive ARTIFACT_REVIEW_DISMISSED.
	rec = doAsUser(carol, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests",
		artifacts.CreateReviewRequestsBody{Reviewers: []string{"user:" + bob.ID}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doAsUser(bob, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-request/dismiss",
		artifacts.DismissReviewRequestBody{Note: "Delegating to someone else"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	carolNotifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, carol.ID, false)
	require.NoError(t, err)
	require.Len(t, carolNotifs, 1)
	assert.Equal(t, store.NotificationStatusArtifactReviewDismissed, carolNotifs[0].Status)
	assert.Equal(t, "Delegating to someone else", carolNotifs[0].Excerpt)

	aliceNotifs, err = s.GetNotifications(ctx, store.SubscriberTypeUser, alice.ID, false)
	require.NoError(t, err)
	assert.Len(t, aliceNotifs, 2, "owner Alice should also receive dismiss notice when Carol was requester")

	// 4. Agent requester refused with 403.
	agentBody, _ := json.Marshal(artifacts.CreateReviewRequestsBody{Reviewers: []string{"user:" + bob.ID}})
	agentReq := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", bytes.NewReader(agentBody))
	agentReq.Header.Set("Authorization", "Bearer "+agentTok)
	agentRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(agentRec, agentReq)
	assert.Equal(t, http.StatusForbidden, agentRec.Code)

	// 5. FeatureEnabled fails closed.
	h := newArtifactHost(srv)
	assert.Equal(t, experiments.ArtifactsReviewRequests, artifacts.FeatureReviewRequests)
	assert.True(t, h.FeatureEnabled(ctx, artifacts.FeatureReviewRequests))
	setReviewRequestsForTest(t, srv, false)
	assert.False(t, h.FeatureEnabled(ctx, artifacts.FeatureReviewRequests))
}
