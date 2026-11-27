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
		case experiments.Artifacts, experiments.InboxDashboard:
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
	p2 := artifactProject(t, s, "rr-p2")
	createTestUserWithProjectRole(t, s, tid("rr-alice"), "alice-rr@test.com", p1.ID, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, s, tid("rr-alice"), "alice-rr@test.com", p2.ID, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, s, tid("rr-bob"), "bob-rr@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, tid("rr-bob"), "bob-rr@test.com", p2.ID, store.ProjectRoleMember)
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
	assert.NotContains(t, rec.Body.String(), `"requestedOfMe"`)
	assert.Contains(t, rec.Body.String(), `"reviewOnLatest":false`)

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

	// Bob's Inbox "reviews" category reflects the request and parses Alice as requester (O1/O2).
	bobSession := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, bob.Role, "web")
	inboxBefore := decodeInboxSummary(t, doRequestAsUser(t, srv, bob, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateOK, inboxBefore.Categories[InboxCategoryReviews].State)
	assert.Equal(t, 1, inboxBefore.Categories[InboxCategoryReviews].Breakdown["requested"])
	beforePreviews := decodeReviewPreviews(t, inboxBefore.Categories[InboxCategoryReviews])
	require.Len(t, beforePreviews, 1)
	assert.Equal(t, InboxReviewKindRequested, beforePreviews[0].Kind)
	assert.NotEmpty(t, beforePreviews[0].RequestedBy, "Inbox preview should parse requester from review-request notice")

	// Scoped UAT on dismiss (R1): boundary excluding p1 or ceiling lacking artifact:read
	// must answer 404 and leave the open request row untouched.
	bobOtherProjUAT := artifactTestUAT(t, bobSession, p2.ID, "artifact:read")
	bobNoReadUAT := artifactTestUAT(t, bobSession, p1.ID, "inbox:read")
	dismissBody, _ := json.Marshal(artifacts.DismissReviewRequestBody{Note: "should not apply"})
	rec = identityArtifactRequest(t, srv, bobOtherProjUAT, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-request/dismiss", dismissBody)
	assert.Equal(t, http.StatusNotFound, rec.Code, "dismiss with UAT bounded to another project must 404")
	rec = identityArtifactRequest(t, srv, bobNoReadUAT, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-request/dismiss", dismissBody)
	assert.Equal(t, http.StatusNotFound, rec.Code, "dismiss with UAT lacking artifact:read must 404")
	// Verify open row is still intact.
	rec = doAsUser(bob, http.MethodGet, "/api/v1/artifacts/"+artID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var stillOpen artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &stillOpen))
	require.NotNil(t, stillOpen.ReviewState)
	require.NotNil(t, stillOpen.ReviewState.RequestedOfMe, "refused UAT dismiss must not change the open request row")

	// Bob dismisses with a note.
	rec = doAsUser(bob, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-request/dismiss",
		artifacts.DismissReviewRequestBody{Note: "Out of office until Friday"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var dismissResp artifacts.DismissReviewRequestResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dismissResp))
	assert.Equal(t, artifacts.ReviewRequestStateDismissed, dismissResp.Request.State)
	assert.True(t, dismissResp.AuthorNotified)

	// Bob's Inbox "requested" count drops to 0 after dismiss (O1: unacked request notices acked).
	inboxAfter := decodeInboxSummary(t, doRequestAsUser(t, srv, bob, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateOK, inboxAfter.Categories[InboxCategoryReviews].State)
	assert.Equal(t, 0, inboxAfter.Categories[InboxCategoryReviews].Breakdown["requested"])

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
	assert.Contains(t, carolNotifs[0].Message, `dismissed your review request on "Launch Plan"`)
	assert.Equal(t, "Delegating to someone else", carolNotifs[0].Excerpt)

	aliceNotifs, err = s.GetNotifications(ctx, store.SubscriberTypeUser, alice.ID, false)
	require.NoError(t, err)
	require.Len(t, aliceNotifs, 2, "owner Alice should also receive dismiss notice when Carol was requester")
	// O4: non-requester owner copy uses "a review request" rather than "your review request".
	var ownerCopyFound bool
	for _, n := range aliceNotifs {
		if n.Excerpt == "Delegating to someone else" {
			assert.Contains(t, n.Message, `dismissed a review request on "Launch Plan"`)
			ownerCopyFound = true
		}
	}
	assert.True(t, ownerCopyFound, "expected owner dismiss notification with distinct wording")

	// 4. Scoped UATs on create (R2: MayNotifyReviewer requires interactiveUserSession)
	// and admitting dismiss (R1).
	aliceSession := NewAuthenticatedUser(alice.ID, alice.Email, alice.DisplayName, alice.Role, "web")
	aliceNoReqUAT := artifactTestUAT(t, aliceSession, p1.ID, "artifact:read", "artifact:create")
	aliceOtherProjUAT := artifactTestUAT(t, aliceSession, p2.ID, "artifact:read", "artifact:create", "artifact:request_review")
	aliceAdmitUAT := artifactTestUAT(t, aliceSession, p1.ID, "artifact:read", "artifact:create", "artifact:request_review")
	reqBody, _ := json.Marshal(artifacts.CreateReviewRequestsBody{Reviewers: []string{"user:" + bob.ID}, Note: "via UAT"})
	rec = identityArtifactRequest(t, srv, aliceNoReqUAT, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", reqBody)
	assert.Equal(t, http.StatusForbidden, rec.Code, "create with UAT lacking artifact:request_review must 403")
	rec = identityArtifactRequest(t, srv, aliceOtherProjUAT, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", reqBody)
	assert.Equal(t, http.StatusNotFound, rec.Code, "create with UAT bounded to another project must 404 (no existence oracle)")

	bobNotifsBeforeUAT, err := s.GetNotifications(ctx, store.SubscriberTypeUser, bob.ID, false)
	require.NoError(t, err)
	rec = identityArtifactRequest(t, srv, aliceAdmitUAT, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", reqBody)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), artifacts.CodeInteractiveSessionRequired,
		"scoped UAT create must be refused at route level with 403 interactive_session_required")
	bobNotifsAfterUAT, err := s.GetNotifications(ctx, store.SubscriberTypeUser, bob.ID, false)
	require.NoError(t, err)
	assert.Len(t, bobNotifsAfterUAT, len(bobNotifsBeforeUAT), "scoped UAT create must write no notification")

	// R1 + O3 on real hub routes: Alice requests Bob with note A, Carol reminds with note C.
	// Bob's requestedOfMe keeps Alice + note A, while Bob's reminder notice attributes note C to Carol.
	rec = doAsUser(alice, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests",
		artifacts.CreateReviewRequestsBody{Reviewers: []string{"user:" + bob.ID}, Note: "note A"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var uatCreateResp artifacts.CreateReviewRequestsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &uatCreateResp))
	require.Len(t, uatCreateResp.Requests, 1)
	require.Equal(t, artifacts.ReviewRequestOutcomeCreated, uatCreateResp.Requests[0].Outcome)

	rec = doAsUser(carol, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests",
		artifacts.CreateReviewRequestsBody{Reviewers: []string{"user:" + bob.ID}, Note: "note C"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var carolRemResp artifacts.CreateReviewRequestsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &carolRemResp))
	require.Len(t, carolRemResp.Requests, 1)
	assert.Equal(t, artifacts.ReviewRequestOutcomeReminded, carolRemResp.Requests[0].Outcome)
	assert.Equal(t, "note A", carolRemResp.Requests[0].Request.Note)
	assert.Equal(t, alice.ID, carolRemResp.Requests[0].Request.Requester.Ref)

	rec = doAsUser(bob, http.MethodGet, "/api/v1/artifacts/"+artID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var bobAfterCarol artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &bobAfterCarol))
	require.NotNil(t, bobAfterCarol.ReviewState)
	require.NotNil(t, bobAfterCarol.ReviewState.RequestedOfMe)
	assert.Equal(t, alice.ID, bobAfterCarol.ReviewState.RequestedOfMe.Requester.Ref)
	assert.Equal(t, "note A", bobAfterCarol.ReviewState.RequestedOfMe.Note)

	bobNotifsAfterRemind, err := s.GetNotifications(ctx, store.SubscriberTypeUser, bob.ID, false)
	require.NoError(t, err)
	var carolRemindNoticeFound bool
	for _, n := range bobNotifsAfterRemind {
		if n.Excerpt == "note C" {
			assert.Contains(t, n.Message, "carol-rr@test.com")
			carolRemindNoticeFound = true
		}
	}
	assert.True(t, carolRemindNoticeFound, "expected Bob's reminder notice to attribute note C to Carol")

	bobAdmitUAT := artifactTestUAT(t, bobSession, p1.ID, "artifact:read")
	rec = identityArtifactRequest(t, srv, bobAdmitUAT, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-request/dismiss", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// CLI interactive session (ClientTypeCLI -> CredentialKindInteractive) is admitted by MayNotifyReviewer.
	cliTok, _, _, err := srv.userTokenService.GenerateTokenPair(alice.ID, alice.Email, alice.DisplayName, alice.Role, ClientTypeCLI)
	require.NoError(t, err)
	cliReq := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", bytes.NewReader(reqBody))
	cliReq.Header.Set("Authorization", "Bearer "+cliTok)
	cliRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(cliRec, cliReq)
	require.Equal(t, http.StatusOK, cliRec.Code, cliRec.Body.String())
	var cliResp artifacts.CreateReviewRequestsResponse
	require.NoError(t, json.Unmarshal(cliRec.Body.Bytes(), &cliResp))
	require.Len(t, cliResp.Requests, 1)
	assert.Equal(t, artifacts.ReviewRequestOutcomeCreated, cliResp.Requests[0].Outcome, "CLI interactive session must be admitted by MayNotifyReviewer")
	assert.True(t, cliResp.Requests[0].Notified)
	cliReqID := cliResp.Requests[0].Request.ID

	// GET /review-requests: publisher view vs self-only view.
	listRec := doAsUser(alice, http.MethodGet, "/api/v1/artifacts/"+artID+"/review-requests", nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	var pubList artifacts.ListReviewRequestsResponse
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &pubList))
	require.NotEmpty(t, pubList.Requests)
	assert.Equal(t, cliReqID, pubList.Requests[0].ID)
	require.NotNil(t, pubList.Requests[0].ReviewerCanReview)
	assert.True(t, *pubList.Requests[0].ReviewerCanReview)

	selfListRec := doAsUser(bob, http.MethodGet, "/api/v1/artifacts/"+artID+"/review-requests", nil)
	require.Equal(t, http.StatusOK, selfListRec.Code, selfListRec.Body.String())
	var selfList artifacts.ListReviewRequestsResponse
	require.NoError(t, json.Unmarshal(selfListRec.Body.Bytes(), &selfList))
	require.NotEmpty(t, selfList.Requests)
	assert.Nil(t, selfList.Requests[0].ReviewerCanReview, "self-only view must omit reviewerCanReview")

	// Withdraw auto-acks Bob's unacked ARTIFACT_REVIEW_REQUESTED notifications via RequestClosed.
	withdrawRec := doAsUser(alice, http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests/"+cliReqID+"/withdraw", nil)
	require.Equal(t, http.StatusOK, withdrawRec.Code, withdrawRec.Body.String())
	bobUnackedAfterWithdraw, err := s.GetNotifications(ctx, store.SubscriberTypeUser, bob.ID, true)
	require.NoError(t, err)
	for _, n := range bobUnackedAfterWithdraw {
		assert.NotEqual(t, store.NotificationStatusArtifactReviewRequested, n.Status,
			"withdraw must auto-ack unacked ARTIFACT_REVIEW_REQUESTED notifications")
	}

	// POST /grants with notify:true and permission:"review" -> 400 use_review_requests while gate is on.
	grantNotifyRec := doAsUser(alice, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants",
		artifacts.GrantRequest{SubjectKind: "principal", SubjectRef: "user:" + bob.ID, Permission: "review", Notify: true})
	assert.Equal(t, http.StatusBadRequest, grantNotifyRec.Code, grantNotifyRec.Body.String())
	assert.Contains(t, grantNotifyRec.Body.String(), artifacts.CodeUseReviewRequests)

	// 5. Agent requester matrix:
	//    - non-owning agent on Alice's artifact -> 403;
	//    - owning agent on its own artifact:
	//      - reviewer lacking home role (Dave in p2) -> reviewer_cannot_review;
	//      - reviewer holding home role (Bob in p1) -> bounded, notification labelled "agent rr-agent";
	//      - dismiss delivers SystemCategoryArtifactReview message to the running agent.
	createTestUserWithProjectRole(t, s, tid("rr-dave"), "dave-rr@test.com", p2.ID, store.ProjectRoleMember)
	dave, err := s.GetUser(ctx, tid("rr-dave"))
	require.NoError(t, err)

	agentBody, _ := json.Marshal(artifacts.CreateReviewRequestsBody{Reviewers: []string{"user:" + bob.ID}})
	agentReq := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+artID+"/review-requests", bytes.NewReader(agentBody))
	agentReq.Header.Set("Authorization", "Bearer "+agentTok)
	agentRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(agentRec, agentReq)
	assert.Equal(t, http.StatusForbidden, agentRec.Code, "non-owning agent must receive 403")

	// Publish an artifact owned by rr-agent in p1.
	agentPubReq := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts?name=agent.md&title=Agent+Report&scope="+p1.ID, bytes.NewReader([]byte("# Report\n")))
	agentPubReq.Header.Set("Authorization", "Bearer "+agentTok)
	agentPubRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(agentPubRec, agentPubReq)
	require.Equal(t, http.StatusCreated, agentPubRec.Code, agentPubRec.Body.String())
	var agentPubResp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(agentPubRec.Body.Bytes(), &agentPubResp))
	agentArtID := agentPubResp.Artifact.ID

	agentReqBody, _ := json.Marshal(artifacts.CreateReviewRequestsBody{
		Reviewers: []string{"user:" + dave.ID, "user:" + bob.ID},
		Note:      "Please check section 4",
	})
	agentReq = httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-requests", bytes.NewReader(agentReqBody))
	agentReq.Header.Set("Authorization", "Bearer "+agentTok)
	agentRec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(agentRec, agentReq)
	require.Equal(t, http.StatusOK, agentRec.Code, agentRec.Body.String())
	var agentCreateResp artifacts.CreateReviewRequestsResponse
	require.NoError(t, json.Unmarshal(agentRec.Body.Bytes(), &agentCreateResp))
	require.Len(t, agentCreateResp.Requests, 2)
	assert.Equal(t, artifacts.ReviewRequestOutcomeRefused, agentCreateResp.Requests[0].Outcome)
	assert.Equal(t, artifacts.CodeReviewerCannotReview, agentCreateResp.Requests[0].Code)
	assert.Equal(t, artifacts.ReviewRequestOutcomeCreated, agentCreateResp.Requests[1].Outcome)
	assert.Equal(t, artifacts.ReviewRequestAccessBounded, agentCreateResp.Requests[1].Access)
	assert.True(t, agentCreateResp.Requests[1].Notified)

	// Bob received ARTIFACT_REVIEW_REQUESTED labelled "agent rr-agent".
	bobNotifsAfterAgent, err := s.GetNotifications(ctx, store.SubscriberTypeUser, bob.ID, false)
	require.NoError(t, err)
	var agentNoticeFound bool
	for _, n := range bobNotifsAfterAgent {
		if n.ResourceID == agentArtID && n.Status == store.NotificationStatusArtifactReviewRequested {
			assert.Contains(t, n.Message, `agent rr-agent asked you to review "Agent Report" (v1)`)
			assert.Equal(t, "Please check section 4", n.Excerpt)
			agentNoticeFound = true
		}
	}
	assert.True(t, agentNoticeFound, "expected Bob to receive review request notice from agent rr-agent")

	// Bob dismisses -> running agent receives a SystemCategoryArtifactReview system message.
	disp := &recordingDispatcher{}
	srv.SetDispatcher(disp)
	dismissAgentRec := doAsUser(bob, http.MethodPost, "/api/v1/artifacts/"+agentArtID+"/review-request/dismiss",
		artifacts.DismissReviewRequestBody{Note: "Needs benchmark numbers"})
	require.Equal(t, http.StatusOK, dismissAgentRec.Code, dismissAgentRec.Body.String())
	var dismissAgentResp artifacts.DismissReviewRequestResponse
	require.NoError(t, json.Unmarshal(dismissAgentRec.Body.Bytes(), &dismissAgentResp))
	assert.True(t, dismissAgentResp.AuthorNotified)
	calls := disp.getCalls()
	require.Len(t, calls, 1)
	require.NotNil(t, calls[0].StructuredMessage)
	assert.Contains(t, calls[0].StructuredMessage.Msg, `dismissed your review request on "Agent Report"`)
	assert.Contains(t, calls[0].StructuredMessage.Msg, "Needs benchmark numbers")

	// 6. FeatureEnabled fails closed.
	h := newArtifactHost(srv)
	assert.Equal(t, experiments.ArtifactsReviewRequests, artifacts.FeatureReviewRequests)
	assert.True(t, h.FeatureEnabled(ctx, artifacts.FeatureReviewRequests))
	setReviewRequestsForTest(t, srv, false)
	assert.False(t, h.FeatureEnabled(ctx, artifacts.FeatureReviewRequests))
}
