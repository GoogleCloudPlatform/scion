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
// AC5: toAddress matches /artifacts?mine=1&owner=me&review_pending=1,
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

	// 2 owned artifacts with review_pending=1 -> toAddress = 2.
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

	// AC3 parity check: GET /api/v1/artifacts?mine=1&owner=me&review_pending=1 length == reviews.breakdown.toAddress.
	listResp := decodeArtifactList(t, userArtifactRequest(t, srv, owner, http.MethodGet, "/api/v1/artifacts?mine=1&owner=me&review_pending=1", nil))
	require.Len(t, listResp.Artifacts, 2)

	// Fetch summary with default previews=3.
	rec := doRequestAsUser(t, srv, owner, http.MethodGet, userMeInboxSummaryPath, nil)
	resp := decodeInboxSummary(t, rec)
	rev := resp.Categories[InboxCategoryReviews]

	assert.Equal(t, InboxStateOK, rev.State)
	assert.Equal(t, 4, rev.Count)
	assert.Equal(t, 4, resp.Total)
	assert.False(t, rev.Capped)
	assert.Equal(t, "/artifacts?review_pending=1&owner=me", rev.Href)
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
	assert.Equal(t, p2.Name, previews[0].ProjectName)
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

	// 1. Hub-bounded UAT with inbox:read + artifact:read sees both projects (2 toAddress + 2 requested = 4).
	tokHubFull := f.mint(t, f.member, hubBoundary(), "inbox:read", "artifact:read")
	hubResp := decodeInboxSummary(t, f.call(t, tokHubFull, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateOK, hubResp.Categories[InboxCategoryReviews].State)
	assert.Equal(t, 4, hubResp.Categories[InboxCategoryReviews].Count)
	assert.Equal(t, map[string]int{"toAddress": 2, "requested": 2}, hubResp.Categories[InboxCategoryReviews].Breakdown)

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

	// Error isolation when hub.artifacts is on but artifact store is not configured.
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.InboxDashboard: true,
		experiments.Artifacts:      true,
	})
	respErr := decodeInboxSummary(t, doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateError, respErr.Categories[InboxCategoryReviews].State)
	assert.Equal(t, InboxErrorInternal, respErr.Categories[InboxCategoryReviews].ErrorCode)
	assert.Equal(t, InboxStateOK, respErr.Categories[InboxCategoryApprovals].State)

	// Error isolation when GetNotifications fails.
	enableInboxAndArtifacts(t, srv)
	_, fault := installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *inboxNotifFaultStore {
		return &inboxNotifFaultStore{Store: inner, sw: sw}
	})
	fault.Arm()
	respNotifErr := decodeInboxSummary(t, doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateError, respNotifErr.Categories[InboxCategoryReviews].State)
	assert.Equal(t, InboxErrorInternal, respNotifErr.Categories[InboxCategoryReviews].ErrorCode)
	assert.Equal(t, InboxStateOK, respNotifErr.Categories[InboxCategoryApprovals].State)
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
}

func (s *countingArtifactStore) ListCandidates(ctx context.Context, q artifacts.CandidateQuery) ([]artifacts.Candidate, error) {
	s.listCandidatesCalls.Add(1)
	return s.Store.ListCandidates(ctx, q)
}

// TestUserMeInboxSummary_ReviewsStoreCallCount verifies AC6: bounded store calls
// for the reviews category regardless of how many review-pending artifacts exist.
func TestUserMeInboxSummary_ReviewsStoreCallCount(t *testing.T) {
	srv, rawStore := testServer(t)
	rawArtStore := enableInboxAndArtifacts(t, srv)
	artCounter := &countingArtifactStore{Store: rawArtStore}
	srv.SetArtifactStore(artCounter)
	hubCounter, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *inboxReviewsCountingStore {
		return &inboxReviewsCountingStore{Store: inner}
	})

	p1 := artifactProject(t, rawStore, "rev-spy-p1")
	uID := tid("rev-spy-user")
	createTestUserWithProjectRole(t, rawStore, uID, "rev-spy@test.com", p1.ID, store.ProjectRoleMember)
	u, err := rawStore.GetUser(context.Background(), uID)
	require.NoError(t, err)

	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		seedHubReviewArtifact(t, rawArtStore, p1.ID, artifacts.PrincipalKindUser, u.ID, fmt.Sprintf("Rev %d", i), artifacts.VersionKindReview, now.Add(time.Duration(i)*time.Second))
	}

	hubCounter.getNotifsCalls.Store(0)
	hubCounter.getProjectCalls.Store(0)
	artCounter.listCandidatesCalls.Store(0)

	resp := decodeInboxSummary(t, doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath+"?previews=3", nil))
	assert.Equal(t, 10, resp.Categories[InboxCategoryReviews].Count)
	assert.Equal(t, int32(1), artCounter.listCandidatesCalls.Load(), "reviews must issue exactly 1 ListCandidates query")
	assert.Equal(t, int32(1), hubCounter.getNotifsCalls.Load(), "reviews must issue exactly 1 GetNotifications query")
	assert.LessOrEqual(t, hubCounter.getProjectCalls.Load(), int32(1), "GetProject should be memoized per distinct project in previews")
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
