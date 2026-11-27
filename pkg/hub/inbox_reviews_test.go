//go:build !no_sqlite

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

package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func enableInboxAndArtifacts(t *testing.T, srv *Server) artifacts.Store {
	t.Helper()
	st, _ := enableArtifactsForTest(t, srv)
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.InboxDashboard: true,
		experiments.Artifacts:      true,
	})
	require.True(t, srv.experimentEnabled(experiments.InboxDashboard))
	require.True(t, srv.experimentEnabled(experiments.Artifacts))
	return st
}

func seedHubReviewArtifact(t *testing.T, st artifacts.Store, projectID, ownerKind, ownerRef, title, currentKind string, updatedAt time.Time, extra ...artifacts.Grant) string {
	t.Helper()
	id := uuid.NewString()
	a := &artifacts.Artifact{
		ID: id, ScopeKind: artifacts.ScopeKindProject, ScopeRef: projectID,
		OwnerKind: ownerKind, OwnerRef: ownerRef, Title: title,
		CurrentSeq: 1, CreatedAt: updatedAt, UpdatedAt: updatedAt,
	}
	v := &artifacts.Version{
		ID: uuid.NewString(), ArtifactID: id, Seq: 1, Kind: currentKind,
		EntryPath: "doc.md", TotalBytes: 4, FileCount: 1,
		CreatedByKind: ownerKind, CreatedByRef: ownerRef,
		CreatedAt: updatedAt, State: artifacts.VersionStateReady,
	}
	grants := []artifacts.Grant{{
		ID: uuid.NewString(), ArtifactID: id, SubjectKind: artifacts.SubjectScope,
		SubjectRef: projectID, Permission: artifacts.GrantRead,
		CreatedByRef: artifacts.PrincipalRef(ownerKind, ownerRef), CreatedAt: updatedAt,
	}}
	for _, g := range extra {
		g.ID = uuid.NewString()
		g.ArtifactID = id
		g.CreatedAt = updatedAt
		grants = append(grants, g)
	}
	require.NoError(t, st.CreatePublished(context.Background(), a, v, nil, grants))
	return id
}

func seedReviewRequestNotification(t *testing.T, s store.Store, subscriberUserID, projectID, artifactID, requesterLabel string, createdAt time.Time, acked bool) string {
	t.Helper()
	id := api.NewUUID()
	n := &store.Notification{
		ID:             id,
		SubscriptionID: uuid.Nil.String(),
		AgentID:        uuid.Nil.String(),
		ProjectID:      projectID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   subscriberUserID,
		Kind:           store.NotificationKindArtifactGrant,
		ResourceType:   store.NotificationResourceArtifact,
		ResourceID:     artifactID,
		Status:         store.NotificationStatusArtifactReviewRequested,
		Message:        requesterLabel + " asked you to review an artifact (Can review): " + artifacts.FormatRef(artifactID, 0),
		Acknowledged:   acked,
		CreatedAt:      createdAt,
	}
	require.NoError(t, s.CreateNotification(context.Background(), n))
	return id
}

func decodeReviewPreviews(t *testing.T, cat InboxCategory) []InboxReviewPreview {
	t.Helper()
	out := make([]InboxReviewPreview, 0, len(cat.Previews))
	for _, raw := range cat.Previews {
		b, err := json.Marshal(raw)
		require.NoError(t, err)
		var p InboxReviewPreview
		require.NoError(t, json.Unmarshal(b, &p))
		out = append(out, p)
	}
	return out
}

// TestUserMeInboxSummary_ReviewsCountsParityPreviewsAndDedup verifies AC3 and
// AC5: toAddress matches /artifacts?mine=1&owner=me&review_on_latest=1,
// requested dedupes by ResourceID and drops unreadable/deleted artifacts,
// breakdown and total match, and previews are newest-first with the exact
// ReviewPreview shape.
func TestUserMeInboxSummary_ReviewsCountsParityPreviewsAndDedup(t *testing.T) {
	srv, s := testServer(t)
	st := enableInboxAndArtifacts(t, srv)
	ctx := context.Background()

	p1 := artifactProject(t, s, "rev-proj-1")
	p2 := artifactProject(t, s, "rev-proj-2")
	ownerID := tid("rev-owner")
	otherID := tid("rev-other")
	createTestUserWithProjectRole(t, s, ownerID, "owner@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, otherID, "other@test.com", p2.ID, store.ProjectRoleMember)
	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	// 2 owned artifacts with review_on_latest=1 -> toAddress = 2.
	toAddrOld := seedHubReviewArtifact(t, st, p1.ID, artifacts.PrincipalKindUser, owner.ID, "Owned Review Old", artifacts.VersionKindReview, base.Add(1*time.Minute))
	toAddrNew := seedHubReviewArtifact(t, st, p1.ID, artifacts.PrincipalKindUser, owner.ID, "Owned Review New", artifacts.VersionKindReview, base.Add(4*time.Minute))
	// Owned publish-only artifact -> excluded.
	seedHubReviewArtifact(t, st, p1.ID, artifacts.PrincipalKindUser, owner.ID, "Owned Publish", artifacts.VersionKindPublish, base.Add(5*time.Minute))
	// Another user's review-pending artifact in p1 -> excluded from toAddress (owner=me).
	seedHubReviewArtifact(t, st, p1.ID, artifacts.PrincipalKindUser, otherID, "Other Review In P1", artifacts.VersionKindReview, base.Add(6*time.Minute))

	// Requested review artifacts:
	// 1. Readable via review grant, with TWO unacked notifications -> deduped to 1 (newest kept).
	reqReadable1 := seedHubReviewArtifact(t, st, p2.ID, artifacts.PrincipalKindUser, otherID, "Design Doc A", artifacts.VersionKindPublish, base,
		artifacts.Grant{
			SubjectKind: artifacts.SubjectPrincipal,
			SubjectRef:  artifacts.PrincipalRef(artifacts.PrincipalKindUser, owner.ID),
			Permission:  artifacts.GrantReview,
		})
	seedReviewRequestNotification(t, s, owner.ID, p2.ID, reqReadable1, "Alice", base.Add(2*time.Minute), false)
	notifReq1Newest := seedReviewRequestNotification(t, s, owner.ID, p2.ID, reqReadable1, "Bob", base.Add(5*time.Minute), false)

	// 2. Readable via p1 membership -> counted.
	reqReadable2 := seedHubReviewArtifact(t, st, p1.ID, artifacts.PrincipalKindUser, otherID, "Design Doc B", artifacts.VersionKindPublish, base)
	notifReq2 := seedReviewRequestNotification(t, s, owner.ID, p1.ID, reqReadable2, "Carol", base.Add(3*time.Minute), false)

	// 3. Unreadable artifact (in p2, no grant) -> excluded!
	reqUnreadable := seedHubReviewArtifact(t, st, p2.ID, artifacts.PrincipalKindUser, otherID, "Secret Doc", artifacts.VersionKindPublish, base)
	seedReviewRequestNotification(t, s, owner.ID, p2.ID, reqUnreadable, "Mallory", base.Add(7*time.Minute), false)

	// 4. Deleted/missing artifact -> excluded!
	seedReviewRequestNotification(t, s, owner.ID, p1.ID, uuid.NewString(), "Ghost", base.Add(8*time.Minute), false)

	// 5. Acknowledged review request -> excluded!
	reqAcked := seedHubReviewArtifact(t, st, p1.ID, artifacts.PrincipalKindUser, otherID, "Acked Doc", artifacts.VersionKindPublish, base)
	seedReviewRequestNotification(t, s, owner.ID, p1.ID, reqAcked, "Dave", base.Add(9*time.Minute), true)

	// AC3 parity check: GET /api/v1/artifacts?mine=1&owner=me&review_on_latest=1 length == reviews.breakdown.toAddress.
	listResp := decodeArtifactList(t, userArtifactRequest(t, srv, owner, http.MethodGet, "/api/v1/artifacts?mine=1&owner=me&review_on_latest=1", nil))
	require.Len(t, listResp.Artifacts, 2)

	// Fetch summary with default previews=3.
	rec := doRequestAsUser(t, srv, owner, http.MethodGet, userMeInboxSummaryPath, nil)
	resp := decodeInboxSummary(t, rec)
	rev := resp.Categories[InboxCategoryReviews]

	assert.Equal(t, InboxStateOK, rev.State)
	assert.Equal(t, 4, rev.Count)
	assert.Equal(t, 4, resp.Total)
	assert.False(t, rev.Capped)
	assert.Equal(t, "/artifacts?review_on_latest=1&owner=me", rev.Href)
	assert.Equal(t, map[string]int{"toAddress": len(listResp.Artifacts), "requested": 2}, rev.Breakdown)

	// AC5: newest first, capped at 3:
	// t+5m: reqReadable1 (requested, Bob, notifReq1Newest)
	// t+4m: toAddrNew (to_address)
	// t+3m: reqReadable2 (requested, Carol, notifReq2)
	// (t+1m toAddrOld is 4th, so excluded at previews=3)
	previews := decodeReviewPreviews(t, rev)
	require.Len(t, previews, 3)

	assert.Equal(t, InboxReviewKindRequested, previews[0].Kind)
	assert.Equal(t, reqReadable1, previews[0].ArtifactID)
	assert.Equal(t, "Design Doc A", previews[0].Title)
	assert.Equal(t, p2.ID, previews[0].ScopeRef)
	assert.Equal(t, p2.ID, previews[0].ProjectID)
	// owner is NOT a member of p2 (readable only via review grant), so projectName must be hidden.
	assert.Empty(t, previews[0].ProjectName)
	assert.Equal(t, "Bob", previews[0].RequestedBy)
	assert.Equal(t, notifReq1Newest, previews[0].NotificationID)
	assert.Equal(t, "/projects/"+p2.ID+"/artifacts/"+reqReadable1, previews[0].Href)

	assert.Equal(t, InboxReviewKindToAddress, previews[1].Kind)
	assert.Equal(t, toAddrNew, previews[1].ArtifactID)
	assert.Equal(t, "Owned Review New", previews[1].Title)
	assert.Equal(t, p1.ID, previews[1].ScopeRef)
	assert.Equal(t, p1.ID, previews[1].ProjectID)
	assert.Equal(t, p1.Name, previews[1].ProjectName)
	assert.Empty(t, previews[1].RequestedBy)
	assert.Empty(t, previews[1].NotificationID)
	assert.Equal(t, "/projects/"+p1.ID+"/artifacts/"+toAddrNew, previews[1].Href)

	assert.Equal(t, InboxReviewKindRequested, previews[2].Kind)
	assert.Equal(t, reqReadable2, previews[2].ArtifactID)
	assert.Equal(t, "Design Doc B", previews[2].Title)
	assert.Equal(t, p1.Name, previews[2].ProjectName)
	assert.Equal(t, "Carol", previews[2].RequestedBy)
	assert.Equal(t, notifReq2, previews[2].NotificationID)

	// previews=0 returns count and breakdown with empty previews.
	respZero := decodeInboxSummary(t, doRequestAsUser(t, srv, owner, http.MethodGet, userMeInboxSummaryPath+"?previews=0", nil))
	assert.Equal(t, 4, respZero.Categories[InboxCategoryReviews].Count)
	assert.Empty(t, respZero.Categories[InboxCategoryReviews].Previews)

	// previews=10 returns all 4 including toAddrOld.
	respAll := decodeInboxSummary(t, doRequestAsUser(t, srv, owner, http.MethodGet, userMeInboxSummaryPath+"?previews=10", nil))
	allPreviews := decodeReviewPreviews(t, respAll.Categories[InboxCategoryReviews])
	require.Len(t, allPreviews, 4)
	assert.Equal(t, toAddrOld, allPreviews[3].ArtifactID)
}

// TestUserMeInboxSummary_ReviewsTokenScoping verifies §4.2 token view for reviews:
// - hub-bounded UAT with inbox:read + artifact:read sees all projects
// - project-bounded UAT with inbox:read + artifact:read sees only its project
// - UAT with inbox:read only (no artifact:read) sees 0 reviews
func TestUserMeInboxSummary_ReviewsTokenScoping(t *testing.T) {
	f := newChatFixture(t)
	st := enableInboxAndArtifacts(t, f.srv)

	now := time.Now().UTC().Truncate(time.Second)
	// Owned review-pending artifact in projA and projB.
	seedHubReviewArtifact(t, st, f.projA, artifacts.PrincipalKindUser, f.member, "Rev A", artifacts.VersionKindReview, now)
	seedHubReviewArtifact(t, st, f.projB, artifacts.PrincipalKindUser, f.member, "Rev B", artifacts.VersionKindReview, now.Add(time.Second))

	// Review-request notifications in projA (via project membership) and projB (via review grant).
	otherID := tid("rev-tok-other")
	reqA := seedHubReviewArtifact(t, st, f.projA, artifacts.PrincipalKindUser, otherID, "Req A", artifacts.VersionKindPublish, now)
	reqB := seedHubReviewArtifact(t, st, f.projB, artifacts.PrincipalKindUser, otherID, "Req B", artifacts.VersionKindPublish, now,
		artifacts.Grant{
			SubjectKind: artifacts.SubjectPrincipal,
			SubjectRef:  artifacts.PrincipalRef(artifacts.PrincipalKindUser, f.member),
			Permission:  artifacts.GrantReview,
		})
	seedReviewRequestNotification(t, f.s, f.member, f.projA, reqA, "Other", now, false)
	seedReviewRequestNotification(t, f.s, f.member, f.projB, reqB, "Other", now.Add(time.Second), false)

	// 1. Hub-bounded UAT with inbox:read + artifact:read (no project:read) sees
	// both projects (2 toAddress + 2 requested = 4), with projectName hidden.
	tokHubFull := f.mint(t, f.member, hubBoundary(), "inbox:read", "artifact:read")
	hubResp := decodeInboxSummary(t, f.call(t, tokHubFull, http.MethodGet, userMeInboxSummaryPath+"?previews=10", nil))
	assert.Equal(t, InboxStateOK, hubResp.Categories[InboxCategoryReviews].State)
	assert.Equal(t, 4, hubResp.Categories[InboxCategoryReviews].Count)
	assert.Equal(t, map[string]int{"toAddress": 2, "requested": 2}, hubResp.Categories[InboxCategoryReviews].Breakdown)
	for _, p := range decodeReviewPreviews(t, hubResp.Categories[InboxCategoryReviews]) {
		assert.Empty(t, p.ProjectName, "UAT without project:read must not receive projectName")
	}

	// 1b. Hub-bounded UAT with inbox:read + artifact:read + project:read sees
	// projectName only for projA (where member), not projB (non-member).
	tokHubWithProj := f.mint(t, f.member, hubBoundary(), "inbox:read", "artifact:read", "project:read")
	hubProjResp := decodeInboxSummary(t, f.call(t, tokHubWithProj, http.MethodGet, userMeInboxSummaryPath+"?previews=10", nil))
	projAObj, err := f.s.GetProject(context.Background(), f.projA)
	require.NoError(t, err)
	for _, p := range decodeReviewPreviews(t, hubProjResp.Categories[InboxCategoryReviews]) {
		if p.ScopeRef == f.projA {
			assert.Equal(t, projAObj.Name, p.ProjectName)
		} else {
			assert.Empty(t, p.ProjectName, "non-member project name must stay hidden")
		}
	}

	// 2. Project-bounded UAT (projA) with inbox:read + artifact:read sees only projA (1 toAddress + 1 requested = 2).
	tokProjA := f.mint(t, f.member, projectBoundary(f.projA), "inbox:read", "artifact:read")
	projAResp := decodeInboxSummary(t, f.call(t, tokProjA, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateOK, projAResp.Categories[InboxCategoryReviews].State)
	assert.Equal(t, 2, projAResp.Categories[InboxCategoryReviews].Count)
	assert.Equal(t, map[string]int{"toAddress": 1, "requested": 1}, projAResp.Categories[InboxCategoryReviews].Breakdown)
	for _, p := range decodeReviewPreviews(t, projAResp.Categories[InboxCategoryReviews]) {
		assert.Equal(t, f.projA, p.ScopeRef)
	}

	// 3. Hub-bounded UAT with inbox:read ONLY (no artifact:read) sees 0 reviews.
	tokNoArt := f.mint(t, f.member, hubBoundary(), "inbox:read")
	noArtResp := decodeInboxSummary(t, f.call(t, tokNoArt, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateOK, noArtResp.Categories[InboxCategoryReviews].State)
	assert.Equal(t, 0, noArtResp.Categories[InboxCategoryReviews].Count)
	assert.Equal(t, map[string]int{"toAddress": 0, "requested": 0}, noArtResp.Categories[InboxCategoryReviews].Breakdown)
}

// TestUserMeInboxSummary_ReviewsDisabledAndError verifies AC4:
// - hub.artifacts off -> reviews state:"disabled", count:0, excluded from total
// - artifact store missing/failing -> reviews state:"error", HTTP 200, other categories intact
func TestUserMeInboxSummary_ReviewsDisabledAndError(t *testing.T) {
	srv, s := testServer(t)
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.InboxDashboard: true,
		experiments.Artifacts:      false,
	})
	u := &store.User{
		ID: tid("rev-ac4-user"), Email: "ac4@test.com", DisplayName: "AC4",
		Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
	}
	require.NoError(t, s.CreateUser(context.Background(), u))

	// Disabled when hub.artifacts is off.
	resp := decodeInboxSummary(t, doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateDisabled, resp.Categories[InboxCategoryReviews].State)
	assert.Equal(t, 0, resp.Categories[InboxCategoryReviews].Count)
	assert.False(t, resp.Categories[InboxCategoryReviews].CountsTowardTotal)

	// Error isolation when hub.artifacts is on but artifact store is not configured.
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.InboxDashboard: true,
		experiments.Artifacts:      true,
	})
	respErr := decodeInboxSummary(t, doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateError, respErr.Categories[InboxCategoryReviews].State)
	assert.False(t, respErr.Categories[InboxCategoryReviews].CountsTowardTotal)
	assert.Equal(t, InboxErrorInternal, respErr.Categories[InboxCategoryReviews].ErrorCode)
	assert.Equal(t, InboxStateOK, respErr.Categories[InboxCategoryPendingSecrets].State)

	// Error isolation when GetNotifications fails.
	enableInboxAndArtifacts(t, srv)
	_, fault := installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *inboxNotifFaultStore {
		return &inboxNotifFaultStore{Store: inner, sw: sw}
	})
	fault.Arm()
	respNotifErr := decodeInboxSummary(t, doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateError, respNotifErr.Categories[InboxCategoryReviews].State)
	assert.False(t, respNotifErr.Categories[InboxCategoryReviews].CountsTowardTotal)
	assert.Equal(t, InboxErrorInternal, respNotifErr.Categories[InboxCategoryReviews].ErrorCode)
	assert.Equal(t, InboxStateOK, respNotifErr.Categories[InboxCategoryPendingSecrets].State)
}

type inboxNotifFaultStore struct {
	store.Store
	sw *storeFaultSwitch
}

func (s *inboxNotifFaultStore) GetNotifications(ctx context.Context, subType, subID string, onlyUnacked bool) ([]store.Notification, error) {
	if s.sw.Active() {
		return nil, errors.New("notif db down")
	}
	return s.Store.GetNotifications(ctx, subType, subID, onlyUnacked)
}

type inboxReviewsCountingStore struct {
	store.Store
	getNotifsCalls  atomic.Int32
	getProjectCalls atomic.Int32
}

func (s *inboxReviewsCountingStore) GetNotifications(ctx context.Context, subType, subID string, onlyUnacked bool) ([]store.Notification, error) {
	s.getNotifsCalls.Add(1)
	return s.Store.GetNotifications(ctx, subType, subID, onlyUnacked)
}

func (s *inboxReviewsCountingStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	s.getProjectCalls.Add(1)
	return s.Store.GetProject(ctx, id)
}

type countingArtifactStore struct {
	artifacts.Store
	listCandidatesCalls atomic.Int32
	getArtifactCalls    atomic.Int32
	getArtifactsCalls   atomic.Int32
	listGrantsForCalls  atomic.Int32
}

func (s *countingArtifactStore) ListCandidates(ctx context.Context, q artifacts.CandidateQuery) ([]artifacts.Candidate, error) {
	s.listCandidatesCalls.Add(1)
	return s.Store.ListCandidates(ctx, q)
}

func (s *countingArtifactStore) GetArtifact(ctx context.Context, id string) (*artifacts.Artifact, error) {
	s.getArtifactCalls.Add(1)
	return s.Store.GetArtifact(ctx, id)
}

func (s *countingArtifactStore) GetArtifacts(ctx context.Context, ids []string) (map[string]*artifacts.Artifact, error) {
	s.getArtifactsCalls.Add(1)
	return s.Store.GetArtifacts(ctx, ids)
}

func (s *countingArtifactStore) ListGrantsFor(ctx context.Context, artifactIDs []string) (map[string][]artifacts.Grant, error) {
	s.listGrantsForCalls.Add(1)
	return s.Store.ListGrantsFor(ctx, artifactIDs)
}

// TestUserMeInboxSummary_ReviewsStoreCallCount verifies AC6: artifact-store
// and hub-store reads stay constant as review-request notifications grow
// (same count at 2 requests and at 10, mixing readable and unreadable artifacts).
func TestUserMeInboxSummary_ReviewsStoreCallCount(t *testing.T) {
	srv, rawStore := testServer(t)
	rawArtStore := enableInboxAndArtifacts(t, srv)
	artCounter := &countingArtifactStore{Store: rawArtStore}
	srv.SetArtifactStore(artCounter)
	hubCounter, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *inboxReviewsCountingStore {
		return &inboxReviewsCountingStore{Store: inner}
	})

	p1 := artifactProject(t, rawStore, "rev-spy-p1")
	p2 := artifactProject(t, rawStore, "rev-spy-p2")
	uID := tid("rev-spy-user")
	otherID := tid("rev-spy-other")
	createTestUserWithProjectRole(t, rawStore, uID, "rev-spy@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, rawStore, otherID, "rev-spy-other@test.com", p2.ID, store.ProjectRoleMember)
	u, err := rawStore.GetUser(context.Background(), uID)
	require.NoError(t, err)

	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		seedHubReviewArtifact(t, rawArtStore, p1.ID, artifacts.PrincipalKindUser, u.ID, fmt.Sprintf("Rev %d", i), artifacts.VersionKindReview, now.Add(time.Duration(i)*time.Second))
	}

	seedRequest := func(idx int, readable bool) {
		var grants []artifacts.Grant
		if readable {
			grants = append(grants, artifacts.Grant{
				SubjectKind: artifacts.SubjectPrincipal,
				SubjectRef:  artifacts.PrincipalRef(artifacts.PrincipalKindUser, u.ID),
				Permission:  artifacts.GrantReview,
			})
		}
		artID := seedHubReviewArtifact(t, rawArtStore, p2.ID, artifacts.PrincipalKindUser, otherID, fmt.Sprintf("Req %d", idx), artifacts.VersionKindPublish, now, grants...)
		seedReviewRequestNotification(t, rawStore, u.ID, p2.ID, artID, "Other", now.Add(time.Duration(idx)*time.Second), false)
	}

	measure := func(wantRequested int) (getArt, getArts, listCand, listGrants, getNotifs int32) {
		hubCounter.getNotifsCalls.Store(0)
		hubCounter.getProjectCalls.Store(0)
		artCounter.getArtifactCalls.Store(0)
		artCounter.getArtifactsCalls.Store(0)
		artCounter.listCandidatesCalls.Store(0)
		artCounter.listGrantsForCalls.Store(0)

		resp := decodeInboxSummary(t, doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath+"?previews=3", nil))
		assert.Equal(t, 10+wantRequested, resp.Categories[InboxCategoryReviews].Count)
		assert.Equal(t, map[string]int{"toAddress": 10, "requested": wantRequested}, resp.Categories[InboxCategoryReviews].Breakdown)
		assert.LessOrEqual(t, hubCounter.getProjectCalls.Load(), int32(2), "GetProject should be memoized per distinct project in previews")
		return artCounter.getArtifactCalls.Load(), artCounter.getArtifactsCalls.Load(),
			artCounter.listCandidatesCalls.Load(), artCounter.listGrantsForCalls.Load(),
			hubCounter.getNotifsCalls.Load()
	}

	// Seed 2 review-request notifications (1 readable via grant, 1 unreadable in p2).
	seedRequest(0, true)
	seedRequest(1, false)
	g1, gs1, lc1, lg1, gn1 := measure(1)
	assert.Equal(t, int32(0), g1, "must not call per-ID GetArtifact")
	assert.Equal(t, int32(1), gs1, "must issue exactly 1 batched GetArtifacts call")
	assert.Equal(t, int32(1), lc1, "must issue exactly 1 ListCandidates call")
	assert.Equal(t, int32(1), lg1, "must issue exactly 1 batched ListGrantsFor call")
	assert.Equal(t, int32(1), gn1, "must issue exactly 1 GetNotifications call")

	// Grow to 10 distinct review-request notifications (5 readable, 5 unreadable).
	for i := 2; i < 10; i++ {
		seedRequest(i, i%2 == 0)
	}
	g2, gs2, lc2, lg2, gn2 := measure(5)
	assert.Equal(t, g1, g2, "GetArtifact count must stay constant (0) at 2 vs 10 requests")
	assert.Equal(t, gs1, gs2, "GetArtifacts count must stay constant (1) at 2 vs 10 requests")
	assert.Equal(t, lc1, lc2, "ListCandidates count must stay constant (1) at 2 vs 10 requests")
	assert.Equal(t, lg1, lg2, "ListGrantsFor count must stay constant (1) at 2 vs 10 requests")
	assert.Equal(t, gn1, gn2, "GetNotifications count must stay constant (1) at 2 vs 10 requests")
}

// TestUserMeInboxSummary_ReviewsRefusesUnboundIdentity verifies R1: a context
// whose identity was injected/replaced via ContextWithIdentity (not bound to
// the authentication middleware's credential context) gets no artifact data.
func TestUserMeInboxSummary_ReviewsRefusesUnboundIdentity(t *testing.T) {
	srv, s := testServer(t)
	st := enableInboxAndArtifacts(t, srv)
	ctx := context.Background()

	p1 := artifactProject(t, s, "unbound-p1")
	ownerID := tid("unbound-owner")
	createTestUserWithProjectRole(t, s, ownerID, "unbound@test.com", p1.ID, store.ProjectRoleMember)
	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)

	seedHubReviewArtifact(t, st, p1.ID, artifacts.PrincipalKindUser, owner.ID, "Sensitive Review", artifacts.VersionKindReview, time.Now().UTC())

	// Context with identity set via contextWithIdentity (no bound request credential).
	userIdent := NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "")
	unboundCtx := contextWithIdentity(ctx, userIdent)
	cat := srv.inboxReviewsCategory(unboundCtx, inboxViewer{userID: owner.ID, user: userIdent}, 3, InboxChatFilterAll)
	assert.Equal(t, InboxStateUnavailable, cat.State)
	assert.Equal(t, 0, cat.Count)
	assert.False(t, cat.CountsTowardTotal)
	assert.Empty(t, cat.Previews)
}

// TestUserMeInboxSummary_ReviewsRevokedGrantDropsRequested verifies N2: an
// artifact that was readable via a review grant disappears from requested
// count and previews once that grant is revoked.
func TestUserMeInboxSummary_ReviewsRevokedGrantDropsRequested(t *testing.T) {
	srv, s := testServer(t)
	enableInboxAndArtifacts(t, srv)
	ctx := context.Background()

	p1 := artifactProject(t, s, "revoke-p1")
	ownerID := tid("revoke-owner")
	reviewerID := tid("revoke-reviewer")
	createTestUserWithProjectRole(t, s, ownerID, "rev-o@test.com", p1.ID, store.ProjectRoleOwner)
	// reviewer is NOT a member of p1, so readability depends solely on the grant.
	reviewer := &store.User{
		ID: reviewerID, Email: "rev-r@test.com", DisplayName: "Reviewer",
		Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
	}
	require.NoError(t, s.CreateUser(ctx, reviewer))
	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)

	artID := decodeArtifactID(t, userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts?name=doc.md&scope="+p1.ID, []byte("hello\n")))
	grantBody, _ := json.Marshal(artifacts.GrantRequest{
		SubjectKind: artifacts.SubjectPrincipal,
		SubjectRef:  artifacts.PrincipalRef(artifacts.PrincipalKindUser, reviewer.ID),
		Permission:  artifacts.GrantReview,
	})
	gRec := userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts/"+artID+"/grants", grantBody)
	require.Equal(t, http.StatusCreated, gRec.Code, gRec.Body.String())
	var gResp artifacts.GrantResponse
	require.NoError(t, json.Unmarshal(gRec.Body.Bytes(), &gResp))

	seedReviewRequestNotification(t, s, reviewer.ID, p1.ID, artID, "Owner", time.Now().UTC(), false)

	// Before revocation: artifact is readable -> requested=1, but non-member reviewer gets projectName == "".
	before := decodeInboxSummary(t, doRequestAsUser(t, srv, reviewer, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, 1, before.Categories[InboxCategoryReviews].Count)
	assert.Equal(t, map[string]int{"toAddress": 0, "requested": 1}, before.Categories[InboxCategoryReviews].Breakdown)
	beforePreviews := decodeReviewPreviews(t, before.Categories[InboxCategoryReviews])
	require.Len(t, beforePreviews, 1)
	assert.Empty(t, beforePreviews[0].ProjectName, "non-member reviewer must not learn project name")

	// Revoke the grant.
	delRec := userArtifactRequest(t, srv, owner, http.MethodDelete, "/api/v1/artifacts/"+artID+"/grants/"+gResp.Grant.ID, nil)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	// After revocation: artifact is no longer readable -> requested=0 and absent from previews.
	after := decodeInboxSummary(t, doRequestAsUser(t, srv, reviewer, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, 0, after.Categories[InboxCategoryReviews].Count)
	assert.Equal(t, map[string]int{"toAddress": 0, "requested": 0}, after.Categories[InboxCategoryReviews].Breakdown)
	assert.Empty(t, after.Categories[InboxCategoryReviews].Previews)
}

// TestReviewRequestedByFromMessage_RoundTrip verifies N1: the parser in
// inbox_reviews.go round-trips labels formatted by grantNoticeMessage.
func TestReviewRequestedByFromMessage_RoundTrip(t *testing.T) {
	artID := uuid.NewString()
	for _, label := range []string{"Alice", "Bob (bob@example.com)", "a user"} {
		msg := grantNoticeMessage(label, artifacts.GrantReview, artID)
		assert.Equal(t, label, reviewRequestedByFromMessage(msg))
	}
	// Non-review share notice returns empty string.
	shareMsg := grantNoticeMessage("Alice", artifacts.GrantRead, artID)
	assert.Empty(t, reviewRequestedByFromMessage(shareMsg))
}

// TestUserMeInboxSummary_ReviewsAndNotificationHints verifies AC7:
// - review-clearing publish emits {category:"reviews"} to owner only (not non-owner, not when flag off)
// - artifact delete emits {category:"reviews"} to owner only (not non-owner, not when flag off)
// - grant revocation emits {category:"reviews"} to subject user only (not owner, not when flag off)
// - notification ack and ack-all emit {category:"notifications"} to caller only (not non-owner, not when flag off)
func TestUserMeInboxSummary_ReviewsAndNotificationHints(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	pub := NewChannelEventPublisher()
	defer pub.Close()
	srv.SetEventPublisher(pub)

	ctx := context.Background()
	p1 := artifactProject(t, s, "hints-p1")
	ownerID := tid("hints-owner")
	reviewerID := tid("hints-reviewer")
	bystanderID := tid("hints-bystander")
	createTestUserWithProjectRole(t, s, ownerID, "h-owner@test.com", p1.ID, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, s, reviewerID, "h-rev@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, bystanderID, "h-by@test.com", p1.ID, store.ProjectRoleMember)
	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)
	reviewer, err := s.GetUser(ctx, reviewerID)
	require.NoError(t, err)

	ownerCh, unsubOwner := pub.Subscribe("user." + ownerID + ".inbox")
	defer unsubOwner()
	revCh, unsubRev := pub.Subscribe("user." + reviewerID + ".inbox")
	defer unsubRev()
	byCh, unsubBy := pub.Subscribe("user." + bystanderID + ".inbox")
	defer unsubBy()

	drainNoEvent := func(ch <-chan Event, label string) {
		t.Helper()
		select {
		case evt := <-ch:
			t.Fatalf("%s: unexpected event on %s: %s", label, evt.Subject, string(evt.Data))
		case <-time.After(40 * time.Millisecond):
		}
	}

	expectHint := func(ch <-chan Event, wantSubject, wantCategory string) {
		t.Helper()
		select {
		case evt := <-ch:
			assert.Equal(t, wantSubject, evt.Subject)
			var hint InboxHint
			require.NoError(t, json.Unmarshal(evt.Data, &hint))
			assert.Equal(t, wantCategory, hint.Category)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s hint on %s", wantCategory, wantSubject)
		}
	}

	shaHex := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}

	appendAndFinalize := func(caller *store.User, artID, kind string, content []byte, base int) {
		t.Helper()
		manifest, _ := json.Marshal(artifacts.CreateVersionRequest{
			Kind: kind, Entry: "doc.md",
			Files: []artifacts.ManifestFile{{Path: "doc.md", Size: int64(len(content)), SHA256: shaHex(content)}},
		})
		rec := userArtifactRequest(t, srv, caller, http.MethodPost, "/api/v1/artifacts/"+artID+"/versions", manifest)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var vresp artifacts.PendingVersionResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &vresp))
		putRec := userArtifactRequest(t, srv, caller, http.MethodPut, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/files/doc.md", artID, vresp.Version.Seq), content)
		require.Equal(t, http.StatusNoContent, putRec.Code, putRec.Body.String())
		var finBody []byte
		if base > 0 {
			finBody, _ = json.Marshal(artifacts.FinalizeRequest{Base: base})
		}
		finRec := userArtifactRequest(t, srv, caller, http.MethodPost, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/finalize", artID, vresp.Version.Seq), finBody)
		require.Equal(t, http.StatusOK, finRec.Code, finRec.Body.String())
	}

	// --- Flag OFF: none of the mutations emit user.*.inbox hints ---
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.InboxDashboard: false,
		experiments.Artifacts:      true,
	})
	artOff := decodeArtifactID(t, userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts?name=doc.md&scope="+p1.ID, []byte("v1\n")))
	grantBody, _ := json.Marshal(artifacts.GrantRequest{
		SubjectKind: artifacts.SubjectPrincipal,
		SubjectRef:  artifacts.PrincipalRef(artifacts.PrincipalKindUser, reviewerID),
		Permission:  artifacts.GrantReview,
	})
	gOffRec := userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts/"+artOff+"/grants", grantBody)
	require.Equal(t, http.StatusCreated, gOffRec.Code, gOffRec.Body.String())
	var gOffResp artifacts.GrantResponse
	require.NoError(t, json.Unmarshal(gOffRec.Body.Bytes(), &gOffResp))

	appendAndFinalize(reviewer, artOff, artifacts.VersionKindReview, []byte("v1{>>fix<<}\n"), 1)
	appendAndFinalize(owner, artOff, artifacts.VersionKindPublish, []byte("v2 fixed\n"), 0)
	require.Equal(t, http.StatusNoContent, userArtifactRequest(t, srv, owner, http.MethodDelete, "/api/v1/artifacts/"+artOff+"/grants/"+gOffResp.Grant.ID, nil).Code)
	require.Equal(t, http.StatusNoContent, userArtifactRequest(t, srv, owner, http.MethodDelete, "/api/v1/artifacts/"+artOff, nil).Code)

	nOff := seedReviewRequestNotification(t, s, ownerID, p1.ID, uuid.NewString(), "Someone", time.Now().UTC(), false)
	require.Equal(t, http.StatusOK, doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/notifications/"+nOff+"/ack", nil).Code)
	require.Equal(t, http.StatusOK, doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/notifications/ack-all", nil).Code)

	drainNoEvent(ownerCh, "flag off owner")
	drainNoEvent(revCh, "flag off reviewer")
	drainNoEvent(byCh, "flag off bystander")

	// --- Flag ON: each mutation emits to the exact target user only ---
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.InboxDashboard: true,
		experiments.Artifacts:      true,
	})
	artOn := decodeArtifactID(t, userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts?name=doc.md&scope="+p1.ID, []byte("v1\n")))
	gOnRec := userArtifactRequest(t, srv, owner, http.MethodPost, "/api/v1/artifacts/"+artOn+"/grants", grantBody)
	require.Equal(t, http.StatusCreated, gOnRec.Code, gOnRec.Body.String())
	var gOnResp artifacts.GrantResponse
	require.NoError(t, json.Unmarshal(gOnRec.Body.Bytes(), &gOnResp))

	// Review submission does not emit inbox hint (uses existing notification).
	appendAndFinalize(reviewer, artOn, artifacts.VersionKindReview, []byte("v1{>>fix<<}\n"), 1)
	drainNoEvent(ownerCh, "review submit")

	// Review-clearing publish -> {category:"reviews"} to owner only.
	appendAndFinalize(owner, artOn, artifacts.VersionKindPublish, []byte("v2 fixed\n"), 0)
	expectHint(ownerCh, "user."+ownerID+".inbox", InboxCategoryReviews)
	drainNoEvent(revCh, "non-owner on review-clearing publish")
	drainNoEvent(byCh, "bystander on review-clearing publish")

	// Second publish (prior was publish, not review) -> no hint.
	appendAndFinalize(owner, artOn, artifacts.VersionKindPublish, []byte("v3 update\n"), 0)
	drainNoEvent(ownerCh, "publish after publish")

	// Grant revocation -> {category:"reviews"} to subject user (reviewer) only.
	require.Equal(t, http.StatusNoContent, userArtifactRequest(t, srv, owner, http.MethodDelete, "/api/v1/artifacts/"+artOn+"/grants/"+gOnResp.Grant.ID, nil).Code)
	expectHint(revCh, "user."+reviewerID+".inbox", InboxCategoryReviews)
	drainNoEvent(ownerCh, "owner on grant revocation")
	drainNoEvent(byCh, "bystander on grant revocation")

	// Artifact delete -> {category:"reviews"} to owner only.
	require.Equal(t, http.StatusNoContent, userArtifactRequest(t, srv, owner, http.MethodDelete, "/api/v1/artifacts/"+artOn, nil).Code)
	expectHint(ownerCh, "user."+ownerID+".inbox", InboxCategoryReviews)
	drainNoEvent(revCh, "non-owner on delete")
	drainNoEvent(byCh, "bystander on delete")

	// Notification ack -> {category:"notifications"} to caller only.
	n1 := seedReviewRequestNotification(t, s, ownerID, p1.ID, uuid.NewString(), "Alice", time.Now().UTC(), false)
	require.Equal(t, http.StatusOK, doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/notifications/"+n1+"/ack", nil).Code)
	expectHint(ownerCh, "user."+ownerID+".inbox", InboxCategoryNotifications)
	drainNoEvent(byCh, "bystander on ack")

	// Notification ack-all -> {category:"notifications"} to caller only.
	seedReviewRequestNotification(t, s, ownerID, p1.ID, uuid.NewString(), "Bob", time.Now().UTC(), false)
	require.Equal(t, http.StatusOK, doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/notifications/ack-all", nil).Code)
	expectHint(ownerCh, "user."+ownerID+".inbox", InboxCategoryNotifications)
	drainNoEvent(byCh, "bystander on ack-all")
}

// TestUserMeInboxSummary_DismissReviewRequestAcksDuplicateNotifications pins
// that acknowledging one ARTIFACT_REVIEW_REQUESTED notification also
// acknowledges any older unacked ARTIFACT_REVIEW_REQUESTED notifications for
// the same user and artifact (ResourceID), so inbox Dismiss sticks across
// revoke/re-grant cycles and the bell notifications list stays consistent,
// without touching other notification kinds, other artifacts, or other users.
func TestUserMeInboxSummary_DismissReviewRequestAcksDuplicateNotifications(t *testing.T) {
	srv, s := testServer(t)
	st := enableInboxAndArtifacts(t, srv)
	ctx := context.Background()

	p1 := artifactProject(t, s, "rev-dismiss-p1")
	reviewerID := tid("rev-dismiss-reviewer")
	otherUserID := tid("rev-dismiss-other")
	authorID := tid("rev-dismiss-author")
	createTestUserWithProjectRole(t, s, reviewerID, "reviewer@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, otherUserID, "other@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, authorID, "author@test.com", p1.ID, store.ProjectRoleMember)
	reviewer, err := s.GetUser(ctx, reviewerID)
	require.NoError(t, err)

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	artDup := seedHubReviewArtifact(t, st, p1.ID, artifacts.PrincipalKindUser, authorID, "Regranted Spec", artifacts.VersionKindPublish, base)
	artKeep := seedHubReviewArtifact(t, st, p1.ID, artifacts.PrincipalKindUser, authorID, "Other Spec", artifacts.VersionKindPublish, base.Add(time.Minute))

	// Two unacked review-request notifications for artDup (e.g. revoke + re-grant).
	oldNotifID := seedReviewRequestNotification(t, s, reviewerID, p1.ID, artDup, "Author", base.Add(1*time.Minute), false)
	newNotifID := seedReviewRequestNotification(t, s, reviewerID, p1.ID, artDup, "Author", base.Add(5*time.Minute), false)
	// One unacked review-request notification for a different artifact (must stay unacked).
	keepNotifID := seedReviewRequestNotification(t, s, reviewerID, p1.ID, artKeep, "Author", base.Add(2*time.Minute), false)
	// One unacked non-review notification for the same user (must stay unacked).
	otherKindNotifID := api.NewUUID()
	require.NoError(t, s.CreateNotification(ctx, &store.Notification{
		ID:             otherKindNotifID,
		SubscriptionID: uuid.Nil.String(),
		AgentID:        uuid.Nil.String(),
		ProjectID:      p1.ID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   reviewerID,
		Kind:           store.NotificationKindArtifactGrant,
		ResourceType:   store.NotificationResourceArtifact,
		ResourceID:     artDup,
		Status:         store.NotificationStatusArtifactReviewSubmitted,
		Message:        "Author submitted a review",
		CreatedAt:      base.Add(3 * time.Minute),
	}))
	// Another user's unacked review-request notification for artDup (must stay unacked).
	otherUserNotifID := seedReviewRequestNotification(t, s, otherUserID, p1.ID, artDup, "Author", base.Add(4*time.Minute), false)

	// Before dismiss: breakdown.requested == 2 (artDup deduped to newNotifID, plus artKeep).
	before := decodeInboxSummary(t, doRequestAsUser(t, srv, reviewer, http.MethodGet, userMeInboxSummaryPath+"?previews=10", nil))
	assert.Equal(t, map[string]int{"toAddress": 0, "requested": 2}, before.Categories[InboxCategoryReviews].Breakdown)
	beforePreviews := decodeReviewPreviews(t, before.Categories[InboxCategoryReviews])
	require.Len(t, beforePreviews, 2)
	assert.Equal(t, newNotifID, beforePreviews[0].NotificationID)
	assert.Equal(t, artDup, beforePreviews[0].ArtifactID)

	// Dismiss artDup by acking the newest notification shown in the preview.
	ackRec := doRequestAsUser(t, srv, reviewer, http.MethodPost, "/api/v1/notifications/"+newNotifID+"/ack", nil)
	require.Equal(t, http.StatusOK, ackRec.Code, ackRec.Body.String())

	// After dismiss: artDup is completely gone from the summary and breakdown.requested dropped by 1.
	after := decodeInboxSummary(t, doRequestAsUser(t, srv, reviewer, http.MethodGet, userMeInboxSummaryPath+"?previews=10", nil))
	assert.Equal(t, map[string]int{"toAddress": 0, "requested": 1}, after.Categories[InboxCategoryReviews].Breakdown)
	afterPreviews := decodeReviewPreviews(t, after.Categories[InboxCategoryReviews])
	require.Len(t, afterPreviews, 1)
	assert.Equal(t, artKeep, afterPreviews[0].ArtifactID)
	assert.Equal(t, keepNotifID, afterPreviews[0].NotificationID)

	// Bell/notifications list is consistent: both duplicate review-request rows for
	// artDup are acked, while artKeep and the non-review notification remain unacked.
	listRec := doRequestAsUser(t, srv, reviewer, http.MethodGet, "/api/v1/notifications?acknowledged=false", nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	var unacked []store.Notification
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &unacked))
	unackedIDs := make([]string, 0, len(unacked))
	for _, n := range unacked {
		unackedIDs = append(unackedIDs, n.ID)
	}
	assert.NotContains(t, unackedIDs, oldNotifID, "older duplicate review-request notification must be acked")
	assert.NotContains(t, unackedIDs, newNotifID, "acked review-request notification must not be unacked")
	assert.Contains(t, unackedIDs, keepNotifID, "different artifact review-request must remain unacked")
	assert.Contains(t, unackedIDs, otherKindNotifID, "non-review-request notification on same artifact must remain unacked")

	// Other user's notification for artDup is untouched.
	otherUnacked, err := s.GetNotifications(ctx, store.SubscriberTypeUser, otherUserID, true)
	require.NoError(t, err)
	require.Len(t, otherUnacked, 1)
	assert.Equal(t, otherUserNotifID, otherUnacked[0].ID)
}

// TestNotificationAck_ReviewRequestSiblingsGatedByInboxExperiment pins R1:
// when web.inbox_dashboard is OFF, acknowledging one ARTIFACT_REVIEW_REQUESTED
// notification leaves any other unacked review-request rows for the same
// artifact untouched.
func TestNotificationAck_ReviewRequestSiblingsGatedByInboxExperiment(t *testing.T) {
	srv, s := testServer(t)
	setExperimentOverrides(t, srv, map[string]bool{experiments.InboxDashboard: false})
	require.False(t, srv.experimentEnabled(experiments.InboxDashboard))
	ctx := context.Background()

	p1 := artifactProject(t, s, "rev-gate-p1")
	reviewerID := tid("rev-gate-reviewer")
	createTestUserWithProjectRole(t, s, reviewerID, "gate@test.com", p1.ID, store.ProjectRoleMember)
	reviewer, err := s.GetUser(ctx, reviewerID)
	require.NoError(t, err)

	artID := uuid.NewString()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	oldNotifID := seedReviewRequestNotification(t, s, reviewerID, p1.ID, artID, "Author", base.Add(1*time.Minute), false)
	newNotifID := seedReviewRequestNotification(t, s, reviewerID, p1.ID, artID, "Author", base.Add(5*time.Minute), false)

	rec := doRequestAsUser(t, srv, reviewer, http.MethodPost, "/api/v1/notifications/"+newNotifID+"/ack", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	unacked, err := s.GetNotifications(ctx, store.SubscriberTypeUser, reviewerID, true)
	require.NoError(t, err)
	require.Len(t, unacked, 1, "with web.inbox_dashboard off, sibling review-request row must remain unacked")
	assert.Equal(t, oldNotifID, unacked[0].ID)
}

// TestNotificationAck_ReviewRequestSiblingsRespectTokenBoundary pins O1:
// a project-bounded UAT (inbox:write on P2) acking a review-request row in P2
// acks a sibling for the same artifact in P2, but leaves a sibling for the same
// artifact with ProjectID=P1 unacked.
func TestNotificationAck_ReviewRequestSiblingsRespectTokenBoundary(t *testing.T) {
	f := newChatFixture(t)
	setExperimentOverrides(t, f.srv, map[string]bool{experiments.InboxDashboard: true})
	ctx := context.Background()

	bindProjectMember(t, f.s, f.projB, f.member)

	artID := uuid.NewString()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	p1SiblingID := seedReviewRequestNotification(t, f.s, f.member, f.projA, artID, "Author", base.Add(1*time.Minute), false)
	p2SiblingID := seedReviewRequestNotification(t, f.s, f.member, f.projB, artID, "Author", base.Add(2*time.Minute), false)
	p2PrimaryID := seedReviewRequestNotification(t, f.s, f.member, f.projB, artID, "Author", base.Add(5*time.Minute), false)

	tokProjB := f.mint(t, f.member, projectBoundary(f.projB), "inbox:write")
	rec := f.call(t, tokProjB, http.MethodPost, "/api/v1/notifications/"+p2PrimaryID+"/ack", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	unacked, err := f.s.GetNotifications(ctx, store.SubscriberTypeUser, f.member, true)
	require.NoError(t, err)
	unackedIDs := make([]string, 0, len(unacked))
	for _, n := range unacked {
		unackedIDs = append(unackedIDs, n.ID)
	}
	assert.NotContains(t, unackedIDs, p2PrimaryID, "primary P2 notification must be acked")
	assert.NotContains(t, unackedIDs, p2SiblingID, "sibling P2 notification inside token boundary must be acked")
	assert.Contains(t, unackedIDs, p1SiblingID, "sibling P1 notification outside token boundary must stay unacked")
}

type inboxSiblingAckFaultStore struct {
	store.Store
	failID string
}

func (s *inboxSiblingAckFaultStore) AcknowledgeNotification(ctx context.Context, id string) error {
	if id == s.failID {
		return store.ErrNotFound
	}
	return s.Store.AcknowledgeNotification(ctx, id)
}

// TestNotificationAck_ReviewRequestSiblingErrBestEffort pins O2: once the
// primary ack succeeds, a sibling AcknowledgeNotification returning ErrNotFound
// is logged and ignored, returning 200 and publishing exactly one notifications
// inbox hint.
func TestNotificationAck_ReviewRequestSiblingErrBestEffort(t *testing.T) {
	srv, s := testServer(t)
	pub := NewChannelEventPublisher()
	defer pub.Close()
	srv.SetEventPublisher(pub)
	setExperimentOverrides(t, srv, map[string]bool{experiments.InboxDashboard: true})
	ctx := context.Background()

	p1 := artifactProject(t, s, "rev-besteffort-p1")
	reviewerID := tid("rev-besteffort-reviewer")
	createTestUserWithProjectRole(t, s, reviewerID, "besteffort@test.com", p1.ID, store.ProjectRoleMember)
	reviewer, err := s.GetUser(ctx, reviewerID)
	require.NoError(t, err)

	artID := uuid.NewString()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	siblingID := seedReviewRequestNotification(t, s, reviewerID, p1.ID, artID, "Author", base.Add(1*time.Minute), false)
	primaryID := seedReviewRequestNotification(t, s, reviewerID, p1.ID, artID, "Author", base.Add(5*time.Minute), false)

	installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *inboxSiblingAckFaultStore {
		return &inboxSiblingAckFaultStore{Store: inner, failID: siblingID}
	})

	hintCh, unsub := pub.Subscribe("user." + reviewerID + ".inbox")
	defer unsub()

	rec := doRequestAsUser(t, srv, reviewer, http.MethodPost, "/api/v1/notifications/"+primaryID+"/ack", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Expect exactly one notifications inbox hint.
	select {
	case evt := <-hintCh:
		var hint InboxHint
		require.NoError(t, json.Unmarshal(evt.Data, &hint))
		assert.Equal(t, InboxCategoryNotifications, hint.Category)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for notifications inbox hint")
	}
	select {
	case extra := <-hintCh:
		t.Fatalf("unexpected second inbox hint: %s", string(extra.Data))
	case <-time.After(50 * time.Millisecond):
	}
}

// TestUserMeInboxSummary_ReviewsStateSummaryAndGates verifies §13 item 6:
//   - when hub.artifacts_review_requests and hub.artifacts_review_drafts are on,
//     breakdown has {toAddress, requested, drafts} matching the filtered lists,
//     count == toAddress + requested (drafts excluded from count/total),
//     requested previews carry RequestedBy with no NotificationID, and legacy
//     notifications are not double-counted;
//   - when hub.artifacts_review_drafts is off, breakdown omits "drafts";
//   - when hub.artifacts_review_requests is off, requested falls back to the
//     notification-scan source.
func TestUserMeInboxSummary_ReviewsStateSummaryAndGates(t *testing.T) {
	srv, s := testServer(t)
	st := enableInboxAndArtifacts(t, srv)
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.InboxDashboard:          true,
		experiments.Artifacts:               true,
		experiments.ArtifactsReviewRequests: true,
		experiments.ArtifactsReviewDrafts:   true,
	})
	ctx := context.Background()

	p1 := artifactProject(t, s, "rev-p3-p1")
	p2 := artifactProject(t, s, "rev-p3-p2")
	viewerID := tid("rev-p3-viewer")
	authorID := tid("rev-p3-author")
	createTestUserWithProjectRole(t, s, viewerID, "viewer@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, authorID, "author@test.com", p2.ID, store.ProjectRoleMember)
	author, err := s.GetUser(ctx, authorID)
	require.NoError(t, err)
	author.DisplayName = "Author Alice"
	require.NoError(t, s.UpdateUser(ctx, author))
	viewer, err := s.GetUser(ctx, viewerID)
	require.NoError(t, err)

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	// 1 owned artifact with review on latest -> toAddress = 1.
	seedHubReviewArtifact(t, st, p1.ID, artifacts.PrincipalKindUser, viewer.ID, "My Reviewed Doc", artifacts.VersionKindReview, base.Add(1*time.Minute))

	// 1 readable artifact with an open durable review request -> requested = 1.
	reqArt := seedHubReviewArtifact(t, st, p1.ID, artifacts.PrincipalKindUser, author.ID, "Requested Doc", artifacts.VersionKindPublish, base.Add(2*time.Minute))
	_, _, err = st.CreateOrRemindReviewRequest(ctx, artifacts.ReviewRequest{
		ArtifactID:    reqArt,
		ReviewerKind:  artifacts.PrincipalKindUser,
		ReviewerRef:   viewer.ID,
		RequesterKind: artifacts.PrincipalKindUser,
		RequesterRef:  author.ID,
		Note:          "Please review section 1",
		Access:        artifacts.ReviewRequestAccessExisting,
	}, 24*time.Hour, base.Add(3*time.Minute), &artifacts.Actor{Kind: artifacts.PrincipalKindUser, Ref: author.ID, Authority: artifacts.AuthorityOwner}, nil)
	require.NoError(t, err)

	// 1 unreadable artifact (in p2, no grant) with an open durable review request -> excluded.
	unreadableArt := seedHubReviewArtifact(t, st, p2.ID, artifacts.PrincipalKindUser, author.ID, "Hidden Requested Doc", artifacts.VersionKindPublish, base.Add(4*time.Minute))
	_, _, err = st.CreateOrRemindReviewRequest(ctx, artifacts.ReviewRequest{
		ArtifactID:    unreadableArt,
		ReviewerKind:  artifacts.PrincipalKindUser,
		ReviewerRef:   viewer.ID,
		RequesterKind: artifacts.PrincipalKindUser,
		RequesterRef:  author.ID,
		Access:        artifacts.ReviewRequestAccessExisting,
	}, 24*time.Hour, base.Add(5*time.Minute), &artifacts.Actor{Kind: artifacts.PrincipalKindUser, Ref: author.ID, Authority: artifacts.AuthorityOwner}, nil)
	require.NoError(t, err)

	// 1 legacy unacked notification on a DIFFERENT readable artifact (no durable request row).
	// Must NOT be counted when requests gate is ON, and MUST be counted when gate is OFF.
	legacyNotifArt := seedHubReviewArtifact(t, st, p1.ID, artifacts.PrincipalKindUser, author.ID, "Legacy Notif Doc", artifacts.VersionKindPublish, base.Add(6*time.Minute))
	legacyNotifID := seedReviewRequestNotification(t, s, viewer.ID, p1.ID, legacyNotifArt, "Legacy Requester", base.Add(7*time.Minute), false)

	// 2 readable drafts + 1 unreadable draft -> drafts = 2.
	for _, artID := range []string{reqArt, legacyNotifArt, unreadableArt} {
		_, _, err = st.PutReviewDraft(ctx, artifacts.ReviewDraft{
			ArtifactID:    artID,
			PrincipalKind: artifacts.PrincipalKindUser,
			PrincipalRef:  viewer.ID,
			BaseSeq:       1,
			EntryPath:     "doc.md",
			Body:          "draft",
		}, 0, base.Add(8*time.Minute), artifacts.MaxDraftsPerPrincipal)
		require.NoError(t, err)
	}

	// Parity with the three filtered artifact list endpoints.
	toAddrList := decodeArtifactList(t, userArtifactRequest(t, srv, viewer, http.MethodGet, "/api/v1/artifacts?mine=1&owner=me&review_on_latest=1", nil))
	reqList := decodeArtifactList(t, userArtifactRequest(t, srv, viewer, http.MethodGet, "/api/v1/artifacts?mine=1&requested=1", nil))
	draftList := decodeArtifactList(t, userArtifactRequest(t, srv, viewer, http.MethodGet, "/api/v1/artifacts?mine=1&drafts=1", nil))
	require.Len(t, toAddrList.Artifacts, 1)
	require.Len(t, reqList.Artifacts, 1)
	require.Len(t, draftList.Artifacts, 2)

	// 1. Both gates ON.
	resp := decodeInboxSummary(t, doRequestAsUser(t, srv, viewer, http.MethodGet, userMeInboxSummaryPath+"?previews=5", nil))
	rev := resp.Categories[InboxCategoryReviews]
	assert.Equal(t, InboxStateOK, rev.State)
	assert.Equal(t, 2, rev.Count, "count must be toAddress + requested (drafts excluded)")
	assert.Equal(t, 2, resp.Total)
	assert.Equal(t, "/artifacts?review_on_latest=1&owner=me", rev.Href)
	assert.Equal(t, map[string]int{
		InboxBreakdownToAddress: 1,
		InboxBreakdownRequested: 1,
		InboxBreakdownDrafts:    2,
	}, rev.Breakdown)

	previews := decodeReviewPreviews(t, rev)
	require.Len(t, previews, 2)
	assert.Equal(t, InboxReviewKindRequested, previews[0].Kind)
	assert.Equal(t, reqArt, previews[0].ArtifactID)
	assert.True(t, previews[0].CreatedAt.Equal(base.Add(3*time.Minute)), "requested preview CreatedAt should match request CreatedAt")
	assert.Equal(t, "Author Alice", previews[0].RequestedBy)
	assert.Empty(t, previews[0].NotificationID, "request-sourced preview must not carry NotificationID")
	assert.Equal(t, InboxReviewKindToAddress, previews[1].Kind)

	// 2. Drafts gate OFF -> "drafts" key omitted from breakdown.
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.InboxDashboard:          true,
		experiments.Artifacts:               true,
		experiments.ArtifactsReviewRequests: true,
		experiments.ArtifactsReviewDrafts:   false,
	})
	respNoDrafts := decodeInboxSummary(t, doRequestAsUser(t, srv, viewer, http.MethodGet, userMeInboxSummaryPath, nil))
	revNoDrafts := respNoDrafts.Categories[InboxCategoryReviews]
	_, hasDrafts := revNoDrafts.Breakdown[InboxBreakdownDrafts]
	assert.False(t, hasDrafts, "drafts key must be omitted when hub.artifacts_review_drafts is off")
	assert.Equal(t, map[string]int{
		InboxBreakdownToAddress: 1,
		InboxBreakdownRequested: 1,
	}, revNoDrafts.Breakdown)

	// 3. Requests gate OFF -> requested falls back to notification scan.
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.InboxDashboard:          true,
		experiments.Artifacts:               true,
		experiments.ArtifactsReviewRequests: false,
		experiments.ArtifactsReviewDrafts:   true,
	})
	respFallback := decodeInboxSummary(t, doRequestAsUser(t, srv, viewer, http.MethodGet, userMeInboxSummaryPath+"?previews=5", nil))
	revFallback := respFallback.Categories[InboxCategoryReviews]
	assert.Equal(t, 2, revFallback.Count)
	assert.Equal(t, map[string]int{
		InboxBreakdownToAddress: 1,
		InboxBreakdownRequested: 1,
		InboxBreakdownDrafts:    2,
	}, revFallback.Breakdown)
	fallbackPreviews := decodeReviewPreviews(t, revFallback)
	require.Len(t, fallbackPreviews, 2)
	assert.Equal(t, InboxReviewKindRequested, fallbackPreviews[0].Kind)
	assert.Equal(t, legacyNotifArt, fallbackPreviews[0].ArtifactID)
	assert.Equal(t, legacyNotifID, fallbackPreviews[0].NotificationID)
}

// TestUserMeInboxSummary_ReviewsCappedByRequestedOrDraftsOnly verifies that
// inboxReviewsCategory sets Capped=true when only the requested bucket or only
// the drafts bucket hits the maxListScan (500) budget while ToAddress is empty.
func TestUserMeInboxSummary_ReviewsCappedByRequestedOrDraftsOnly(t *testing.T) {
	srv, s := testServer(t)
	st := enableInboxAndArtifacts(t, srv)
	ctx := context.Background()

	p1 := artifactProject(t, s, "rev-cap-p1")
	viewerID := tid("rev-cap-viewer")
	authorID := tid("rev-cap-author")
	createTestUserWithProjectRole(t, s, viewerID, "cap-viewer@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, authorID, "cap-author@test.com", p1.ID, store.ProjectRoleMember)
	viewer, err := s.GetUser(ctx, viewerID)
	require.NoError(t, err)

	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	const overBudget = 501
	artIDs := make([]string, overBudget)
	for i := 0; i < overBudget; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		id := seedHubReviewArtifact(t, st, p1.ID, artifacts.PrincipalKindUser, authorID, fmt.Sprintf("Doc %d", i), artifacts.VersionKindPublish, ts)
		artIDs[i] = id
		_, _, err = st.CreateOrRemindReviewRequest(ctx, artifacts.ReviewRequest{
			ArtifactID:    id,
			ReviewerKind:  artifacts.PrincipalKindUser,
			ReviewerRef:   viewer.ID,
			RequesterKind: artifacts.PrincipalKindUser,
			RequesterRef:  authorID,
			Access:        artifacts.ReviewRequestAccessExisting,
		}, 24*time.Hour, ts, &artifacts.Actor{Kind: artifacts.PrincipalKindUser, Ref: authorID, Authority: artifacts.AuthorityOwner}, nil)
		require.NoError(t, err)
	}

	// 1. Only requested is capped (toAddress=0, drafts gate off).
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.InboxDashboard:          true,
		experiments.Artifacts:               true,
		experiments.ArtifactsReviewRequests: true,
		experiments.ArtifactsReviewDrafts:   false,
	})
	respReqCap := decodeInboxSummary(t, doRequestAsUser(t, srv, viewer, http.MethodGet, userMeInboxSummaryPath+"?previews=0", nil))
	revReqCap := respReqCap.Categories[InboxCategoryReviews]
	assert.Equal(t, InboxStateOK, revReqCap.State)
	assert.True(t, revReqCap.Capped, "category Capped must be true when only requested is capped")
	assert.Equal(t, 0, revReqCap.Breakdown[InboxBreakdownToAddress])
	assert.Equal(t, 500, revReqCap.Breakdown[InboxBreakdownRequested])

	// 2. Only drafts is capped (toAddress=0, requests gate off with 0 legacy notifications).
	for i, id := range artIDs {
		ts := base.Add(time.Duration(i) * time.Second)
		_, _, err = st.PutReviewDraft(ctx, artifacts.ReviewDraft{
			ArtifactID:    id,
			PrincipalKind: artifacts.PrincipalKindUser,
			PrincipalRef:  viewer.ID,
			BaseSeq:       1,
			EntryPath:     "doc.md",
			Body:          "wip",
		}, 0, ts, overBudget+10)
		require.NoError(t, err)
	}
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.InboxDashboard:          true,
		experiments.Artifacts:               true,
		experiments.ArtifactsReviewRequests: false,
		experiments.ArtifactsReviewDrafts:   true,
	})
	respDraftCap := decodeInboxSummary(t, doRequestAsUser(t, srv, viewer, http.MethodGet, userMeInboxSummaryPath+"?previews=0", nil))
	revDraftCap := respDraftCap.Categories[InboxCategoryReviews]
	assert.Equal(t, InboxStateOK, revDraftCap.State)
	assert.True(t, revDraftCap.Capped, "category Capped must be true when only drafts is capped")
	assert.Equal(t, 0, revDraftCap.Breakdown[InboxBreakdownToAddress])
	assert.Equal(t, 0, revDraftCap.Breakdown[InboxBreakdownRequested])
	assert.Equal(t, 500, revDraftCap.Breakdown[InboxBreakdownDrafts])
}
