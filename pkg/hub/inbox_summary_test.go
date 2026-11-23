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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const userMeInboxSummaryPath = "/api/v1/users/me/inbox/summary"

func setInboxDashboardExperiment(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	setExperimentOverrides(t, srv, map[string]bool{experiments.InboxDashboard: enabled})
	require.Equal(t, enabled, srv.experimentEnabled(experiments.InboxDashboard))
}

func decodeInboxSummary(t *testing.T, rec *httptest.ResponseRecorder) InboxSummaryResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out InboxSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "body: %s", rec.Body.String())
	return out
}

func TestUserMeInboxSummary_FlagOffAndMethodCheck(t *testing.T) {
	f := newChatFixture(t)
	memberUser, err := f.s.GetUser(context.Background(), f.member)
	require.NoError(t, err)

	// Flag off: GET -> 404, non-GET -> 405 before experiment gate.
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath, nil).Code)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := doRequestAsUser(t, f.srv, memberUser, method, userMeInboxSummaryPath, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "method %s with flag off", method)
	}

	// Flag on: non-GET still -> 405.
	setInboxDashboardExperiment(t, f.srv, true)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := doRequestAsUser(t, f.srv, memberUser, method, userMeInboxSummaryPath, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "method %s with flag on", method)
	}
}

// TestUserMeInboxSummary_AuthAndValidation pins inbox.summary.read across
// session credentials, hub-bounded and project-bounded UATs (with and without
// inbox:read), unauthenticated callers, agents, visitors, federated users, and
// query parameter validation.
func TestUserMeInboxSummary_AuthAndValidation(t *testing.T) {
	f := newChatFixture(t)
	setInboxDashboardExperiment(t, f.srv, true)
	ctx := context.Background()

	memberUser, err := f.s.GetUser(ctx, f.member)
	require.NoError(t, err)

	// Seed a pending secret owned by f.member so we can verify session sees it
	// and UAT marks approvals unavailable and excludes it from total.
	now := time.Now().UTC()
	require.NoError(t, f.s.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
		ID:          api.NewUUID(),
		Key:         "API_TOKEN",
		SecretType:  store.SecretTypeEnvironment,
		Scope:       store.ScopeUser,
		ScopeID:     f.member,
		ValueRef:    "ref-secret-do-not-leak",
		ValueLen:    12,
		AgentID:     f.agentA.ID,
		AgentSlug:   f.agentA.Slug,
		ProjectID:   f.projA,
		ProjectName: "Project A",
		Created:     now,
		Updated:     now,
		ExpiresAt:   now.Add(24 * time.Hour),
	}, store.PendingSecretAgentCap, store.PendingSecretOwnerCap))

	// 1. Unauthenticated -> 401.
	unauthReq := httptest.NewRequest(http.MethodGet, userMeInboxSummaryPath, nil)
	unauthRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(unauthRec, unauthReq)
	assert.Equal(t, http.StatusUnauthorized, unauthRec.Code)

	// 2. Agent caller -> 403.
	agentReq := httptest.NewRequest(http.MethodGet, userMeInboxSummaryPath, nil)
	agentReq = agentReq.WithContext(agentCtx(agentReq.Context(), f.agentA))
	agentRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(agentRec, agentReq)
	assert.Equal(t, http.StatusForbidden, agentRec.Code)

	// 3. Visitor -> 403.
	visitor := NewVisitorIdentity(tid("inbox-visitor"), "https://issuer.test", "sub-v", "v@test.com", "Visitor")
	assert.Equal(t, http.StatusForbidden, requestAsIdentity(t, f.srv, visitor, http.MethodGet, userMeInboxSummaryPath, nil).Code)

	// 4. Federated user -> 403.
	fed := NewFederatedUserIdentity("https://issuer.test", "sub-fed", memberUser.Email, "Fed Member", "member", nil)
	assert.Equal(t, http.StatusForbidden, requestAsIdentity(t, f.srv, fed, http.MethodGet, userMeInboxSummaryPath, nil).Code)

	// 5. UAT without inbox:read -> 403.
	tokNoInbox := f.mint(t, f.member, hubBoundary(), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, tokNoInbox, http.MethodGet, userMeInboxSummaryPath, nil).Code)

	// 6. Hub-bounded UAT with inbox:read -> 200, approvals unavailable & excluded from total.
	tokHubInbox := f.mint(t, f.member, hubBoundary(), "inbox:read")
	uatHubRec := f.call(t, tokHubInbox, http.MethodGet, userMeInboxSummaryPath, nil)
	uatHubResp := decodeInboxSummary(t, uatHubRec)
	assert.Equal(t, "no-store", uatHubRec.Header().Get("Cache-Control"))
	assert.Equal(t, 0, uatHubResp.Total)
	require.Len(t, uatHubResp.Categories, 5)
	assert.Equal(t, InboxStateUnavailable, uatHubResp.Categories[InboxCategoryApprovals].State)
	assert.Equal(t, 0, uatHubResp.Categories[InboxCategoryApprovals].Count)
	assert.Empty(t, uatHubResp.Categories[InboxCategoryApprovals].Previews)

	// 7. Project-bounded UAT with inbox:read -> 200, approvals unavailable.
	tokProjInbox := f.mint(t, f.member, projectBoundary(f.projA), "inbox:read")
	uatProjResp := decodeInboxSummary(t, f.call(t, tokProjInbox, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, 0, uatProjResp.Total)
	assert.Equal(t, InboxStateUnavailable, uatProjResp.Categories[InboxCategoryApprovals].State)

	// 8. Session user -> 200 with all five category keys, approvals ok with count 1.
	sessRec := doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath, nil)
	sessResp := decodeInboxSummary(t, sessRec)
	assert.Equal(t, "no-store", sessRec.Header().Get("Cache-Control"))
	assert.NotEmpty(t, sessResp.GeneratedAt)
	assert.Equal(t, 1, sessResp.Total)
	assert.False(t, sessResp.TotalCapped)
	for _, key := range []string{InboxCategoryDecisions, InboxCategoryWaiting, InboxCategoryReviews, InboxCategoryApprovals, InboxCategoryChat} {
		assert.Contains(t, sessResp.Categories, key)
	}
	assert.Equal(t, InboxStateOK, sessResp.Categories[InboxCategoryApprovals].State)
	assert.Equal(t, 1, sessResp.Categories[InboxCategoryApprovals].Count)
	for _, disabledKey := range []string{InboxCategoryDecisions, InboxCategoryWaiting, InboxCategoryReviews, InboxCategoryChat} {
		cat := sessResp.Categories[disabledKey]
		assert.Equal(t, InboxStateDisabled, cat.State, "category %s", disabledKey)
		assert.Equal(t, 0, cat.Count, "category %s", disabledKey)
		assert.Empty(t, cat.Previews, "category %s", disabledKey)
	}

	// 9. Query parameter validation.
	for _, bad := range []string{
		userMeInboxSummaryPath + "?previews=-1",
		userMeInboxSummaryPath + "?previews=11",
		userMeInboxSummaryPath + "?previews=abc",
		userMeInboxSummaryPath + "?previews=",
		userMeInboxSummaryPath + "?chat=invalid",
	} {
		rec := doRequestAsUser(t, f.srv, memberUser, http.MethodGet, bad, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400 for %s", bad)
	}
	for _, good := range []string{
		userMeInboxSummaryPath + "?previews=0",
		userMeInboxSummaryPath + "?previews=10&chat=dms",
		userMeInboxSummaryPath + "?chat=mentions",
		userMeInboxSummaryPath + "?chat=all",
	} {
		rec := doRequestAsUser(t, f.srv, memberUser, http.MethodGet, good, nil)
		assert.Equal(t, http.StatusOK, rec.Code, "expected 200 for %s", good)
	}
}

func TestUserMeInboxSummary_ApprovalsOrderingAndPrivacy(t *testing.T) {
	f := newPendingFixture(t, "inbox-approvals")
	setInboxDashboardExperiment(t, f.Server, true)
	ctx := context.Background()

	now := time.Now().UTC()
	seed := func(ownerID, key, valueRef string, expiresAt time.Time, replacesID string) {
		require.NoError(t, f.Store.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
			ID:               api.NewUUID(),
			Key:              key,
			SecretType:       store.SecretTypeEnvironment,
			Scope:            store.ScopeUser,
			ScopeID:          ownerID,
			AllowProgeny:     true,
			InjectionMode:    store.InjectionModeAsNeeded,
			ValueRef:         valueRef,
			ValueLen:         16,
			AgentID:          f.AgentID,
			AgentSlug:        "agent-a",
			ProjectID:        f.ProjectID,
			ProjectName:      f.ProjectName,
			Created:          now.Add(-time.Hour),
			Updated:          now.Add(-time.Hour),
			ExpiresAt:        expiresAt,
			ReplacesSecretID: replacesID,
		}, store.PendingSecretAgentCap, store.PendingSecretOwnerCap))
	}

	// 4 live secrets for owner with varying expiry times.
	seed(f.UserID, "FOURTH_EXPIRING", "ref-never-leak-4", now.Add(4*time.Hour), "")
	seed(f.UserID, "FIRST_EXPIRING", "ref-never-leak-1", now.Add(1*time.Hour), "existing-id")
	seed(f.UserID, "THIRD_EXPIRING", "ref-never-leak-3", now.Add(3*time.Hour), "")
	seed(f.UserID, "SECOND_EXPIRING", "ref-never-leak-2", now.Add(2*time.Hour), "")
	// 1 expired secret for owner (must be excluded).
	seed(f.UserID, "EXPIRED_SECRET", "ref-never-leak-exp", now.Add(-5*time.Minute), "")
	// 1 live secret for another user (must be excluded).
	seed(tid("other-owner"), "OTHER_USER_SECRET", "ref-never-leak-other", now.Add(30*time.Minute), "")

	// Default previews=3.
	rec := ownerRequest(t, f.Server, f.User, CredentialKindSessionJWT, http.MethodGet, userMeInboxSummaryPath, nil)
	resp := decodeInboxSummary(t, rec)
	assert.Equal(t, 4, resp.Total)
	approvals := resp.Categories[InboxCategoryApprovals]
	assert.Equal(t, InboxStateOK, approvals.State)
	assert.Equal(t, 4, approvals.Count)
	assert.Equal(t, "/profile/secrets", approvals.Href)
	require.Len(t, approvals.Previews, 3)

	// Verify ordering (soonest expiresAt first) and fields.
	var keys []string
	for _, raw := range approvals.Previews {
		b, err := json.Marshal(raw)
		require.NoError(t, err)
		var p InboxApprovalPreview
		require.NoError(t, json.Unmarshal(b, &p))
		assert.Equal(t, InboxApprovalKindPendingSecret, p.Kind)
		assert.Equal(t, "/profile/secrets", p.Href)
		keys = append(keys, p.Key)
	}
	assert.Equal(t, []string{"FIRST_EXPIRING", "SECOND_EXPIRING", "THIRD_EXPIRING"}, keys)

	// Privacy: ValueRef must never appear in the JSON body.
	rawBody := rec.Body.String()
	assert.NotContains(t, rawBody, "ref-never-leak")
	assert.NotContains(t, rawBody, "valueRef")
	assert.NotContains(t, rawBody, "OTHER_USER_SECRET")
	assert.NotContains(t, rawBody, "EXPIRED_SECRET")

	// previews=0 returns count only and empty previews array.
	recZero := ownerRequest(t, f.Server, f.User, CredentialKindSessionJWT, http.MethodGet, userMeInboxSummaryPath+"?previews=0", nil)
	respZero := decodeInboxSummary(t, recZero)
	assert.Equal(t, 4, respZero.Total)
	assert.Equal(t, 4, respZero.Categories[InboxCategoryApprovals].Count)
	assert.Empty(t, respZero.Categories[InboxCategoryApprovals].Previews)
}

func TestUserMeInboxSummary_SourceErrorIsolation(t *testing.T) {
	f := newChatFixture(t)
	setInboxDashboardExperiment(t, f.srv, true)
	memberUser, err := f.s.GetUser(context.Background(), f.member)
	require.NoError(t, err)

	f.srv.inboxSourcesForTest = []inboxSourceEntry{
		{
			key: InboxCategoryApprovals,
			run: func(_ context.Context, _ inboxViewer, _ int, _ string) InboxCategory {
				return InboxCategory{
					State:             InboxStateOK,
					Count:             2,
					CountsTowardTotal: true,
					Href:              "/profile/secrets",
					Previews:          []any{},
				}
			},
		},
		{
			key: InboxCategoryWaiting,
			run: func(ctx context.Context, _ inboxViewer, _ int, _ string) InboxCategory {
				return InboxCategory{
					State:             InboxStateError,
					Count:             0,
					CountsTowardTotal: true,
					Href:              "/agents",
					ErrorCode:         inboxCategoryErrorCode(ctx, context.DeadlineExceeded),
					Previews:          []any{},
				}
			},
		},
		{
			key: InboxCategoryReviews,
			run: func(ctx context.Context, _ inboxViewer, _ int, _ string) InboxCategory {
				return InboxCategory{
					State:             InboxStateError,
					Count:             0,
					CountsTowardTotal: true,
					Href:              "/artifacts?review_pending=1&owner=me",
					ErrorCode:         inboxCategoryErrorCode(ctx, errors.New("boom")),
					Previews:          []any{},
				}
			},
		},
		{key: InboxCategoryChat, run: inboxDisabledCategory("/chat?filter=unread")},
		{key: InboxCategoryDecisions, run: inboxDisabledCategory("/profile/decisions?tab=pending")},
	}

	rec := doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath, nil)
	resp := decodeInboxSummary(t, rec)
	assert.Equal(t, 2, resp.Total)
	assert.Equal(t, InboxStateOK, resp.Categories[InboxCategoryApprovals].State)
	assert.Equal(t, InboxStateError, resp.Categories[InboxCategoryWaiting].State)
	assert.Equal(t, InboxErrorTimeout, resp.Categories[InboxCategoryWaiting].ErrorCode)
	assert.Equal(t, InboxStateError, resp.Categories[InboxCategoryReviews].State)
	assert.Equal(t, InboxErrorInternal, resp.Categories[InboxCategoryReviews].ErrorCode)
}

type inboxCountingStore struct {
	store.Store
	listPendingCalls atomic.Int32
}

func (s *inboxCountingStore) ListPendingSecretsByOwner(ctx context.Context, scope, scopeID string) ([]store.PendingSecret, error) {
	s.listPendingCalls.Add(1)
	return s.Store.ListPendingSecretsByOwner(ctx, scope, scopeID)
}

func TestUserMeInboxSummary_StoreCallCount(t *testing.T) {
	srv, rawStore := testServer(t)
	counter, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *inboxCountingStore {
		return &inboxCountingStore{Store: inner}
	})
	setInboxDashboardExperiment(t, srv, true)

	ctx := context.Background()
	u := &store.User{
		ID:          tid("inbox-spy-user"),
		Email:       "spy@example.com",
		DisplayName: "Spy User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now().UTC(),
	}
	require.NoError(t, rawStore.CreateUser(ctx, u))

	counter.listPendingCalls.Store(0)
	rec := doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath, nil)
	decodeInboxSummary(t, rec)
	assert.Equal(t, int32(1), counter.listPendingCalls.Load(), "P1 summary must issue exactly one pending-secrets store query")
}

func TestUserMeInboxSummary_SingleflightCoalescing(t *testing.T) {
	f := newChatFixture(t)
	setInboxDashboardExperiment(t, f.srv, true)
	memberUser, err := f.s.GetUser(context.Background(), f.member)
	require.NoError(t, err)

	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once

	f.srv.inboxSourcesForTest = []inboxSourceEntry{
		{
			key: InboxCategoryApprovals,
			run: func(_ context.Context, _ inboxViewer, _ int, _ string) InboxCategory {
				calls.Add(1)
				startOnce.Do(func() { close(started) })
				<-release
				return InboxCategory{
					State:             InboxStateOK,
					Count:             3,
					CountsTowardTotal: true,
					Href:              "/profile/secrets",
					Previews:          []any{},
				}
			},
		},
		{key: InboxCategoryWaiting, run: inboxDisabledCategory("/agents")},
		{key: InboxCategoryReviews, run: inboxDisabledCategory("/artifacts?review_pending=1&owner=me")},
		{key: InboxCategoryChat, run: inboxDisabledCategory("/chat?filter=unread")},
		{key: InboxCategoryDecisions, run: inboxDisabledCategory("/profile/decisions?tab=pending")},
	}

	const n = 5
	var wg sync.WaitGroup
	results := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath, nil)
			resp := decodeInboxSummary(t, rec)
			results[idx] = resp.Total
		}(i)
	}

	<-started
	time.Sleep(30 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int32(1), calls.Load(), "concurrent identical requests should coalesce via singleflight")
	for i := 0; i < n; i++ {
		assert.Equal(t, 3, results[i])
	}
}

func TestUserMeInboxSummary_PendingSecretApproveRejectHints(t *testing.T) {
	f := newPendingFixture(t, "inbox-hints")
	otherUserID := tid("inbox-hints-other")

	ownerCh, unsubOwner := f.Server.events.Subscribe("user." + f.UserID + ".inbox")
	defer unsubOwner()
	otherCh, unsubOther := f.Server.events.Subscribe("user." + otherUserID + ".inbox")
	defer unsubOther()

	drainNoEvent := func(ch <-chan Event, msg string) {
		select {
		case evt := <-ch:
			t.Fatalf("%s: unexpected event on %s: %s", msg, evt.Subject, string(evt.Data))
		case <-time.After(50 * time.Millisecond):
		}
	}

	expectApprovalsHint := func(ch <-chan Event) {
		select {
		case evt := <-ch:
			assert.Equal(t, "user."+f.UserID+".inbox", evt.Subject)
			var hint InboxHint
			require.NoError(t, json.Unmarshal(evt.Data, &hint))
			assert.Equal(t, InboxCategoryApprovals, hint.Category)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for inbox approvals hint")
		}
	}

	// Flag OFF: approve and reject must NOT publish user.*.inbox hints.
	p1 := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_OFF_1", userScopeWrite("val1", false)))
	rec := ownerRequest(t, f.Server, f.User, CredentialKindSessionJWT, http.MethodPost, "/api/v1/pending-secrets/"+p1.PendingID+"/approve", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	drainNoEvent(ownerCh, "flag off approve")

	p2 := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_OFF_2", userScopeWrite("val2", false)))
	rec = ownerRequest(t, f.Server, f.User, CredentialKindSessionJWT, http.MethodPost, "/api/v1/pending-secrets/"+p2.PendingID+"/reject", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	drainNoEvent(ownerCh, "flag off reject")

	// Flag ON: approve and reject publish {category:"approvals"} to owner only.
	setInboxDashboardExperiment(t, f.Server, true)

	p3 := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_ON_1", userScopeWrite("val3", false)))
	rec = ownerRequest(t, f.Server, f.User, CredentialKindSessionJWT, http.MethodPost, "/api/v1/pending-secrets/"+p3.PendingID+"/approve", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	expectApprovalsHint(ownerCh)
	drainNoEvent(otherCh, "non-owner on approve")

	p4 := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_ON_2", userScopeWrite("val4", false)))
	rec = ownerRequest(t, f.Server, f.User, CredentialKindSessionJWT, http.MethodPost, "/api/v1/pending-secrets/"+p4.PendingID+"/reject", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	expectApprovalsHint(ownerCh)
	drainNoEvent(otherCh, "non-owner on reject")
}
