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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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
	setServerExperimentDefault(t, f.srv, experiments.Artifacts, false)
	ctx := context.Background()

	memberUser, err := f.s.GetUser(ctx, f.member)
	require.NoError(t, err)
	// Grant f.member access to projB as well so waiting spans two readable projects
	// while unreadableProj below stays excluded.
	createTestUserWithProjectRole(t, f.s, f.member, memberUser.Email, f.projB, store.ProjectRoleMember)

	// Seed a pending secret owned by f.member so we can verify session sees it
	// and UAT marks pending_secrets unavailable and excludes it from total.
	now := time.Now().UTC()
	_, err = f.s.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
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
	}, now)
	require.NoError(t, err)

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

	// 6. Hub-bounded UAT with inbox:read -> 200, pending_secrets unavailable & excluded from total.
	tokHubInbox := f.mint(t, f.member, hubBoundary(), "inbox:read")
	uatHubRec := f.call(t, tokHubInbox, http.MethodGet, userMeInboxSummaryPath, nil)
	uatHubResp := decodeInboxSummary(t, uatHubRec)
	assert.Equal(t, "no-store", uatHubRec.Header().Get("Cache-Control"))
	assert.Equal(t, 0, uatHubResp.Total)
	require.Len(t, uatHubResp.Categories, 6)
	assert.Equal(t, InboxStateUnavailable, uatHubResp.Categories[InboxCategoryPendingSecrets].State)
	assert.Equal(t, 0, uatHubResp.Categories[InboxCategoryPendingSecrets].Count)
	assert.False(t, uatHubResp.Categories[InboxCategoryPendingSecrets].CountsTowardTotal, "unavailable category must not count toward total")
	assert.Empty(t, uatHubResp.Categories[InboxCategoryPendingSecrets].Previews)

	// 7. Project-bounded UAT with inbox:read -> 200, pending_secrets unavailable.
	tokProjInbox := f.mint(t, f.member, projectBoundary(f.projA), "inbox:read")
	uatProjResp := decodeInboxSummary(t, f.call(t, tokProjInbox, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, 0, uatProjResp.Total)
	assert.Equal(t, InboxStateUnavailable, uatProjResp.Categories[InboxCategoryPendingSecrets].State)

	// 8. Session user -> 200 with all six category keys, pending_secrets ok with count 1.
	sessRec := doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath, nil)
	sessResp := decodeInboxSummary(t, sessRec)
	assert.Equal(t, "no-store", sessRec.Header().Get("Cache-Control"))
	assert.NotEmpty(t, sessResp.GeneratedAt)
	assert.Equal(t, 1, sessResp.Total)
	assert.False(t, sessResp.TotalCapped)
	for _, key := range []string{InboxCategoryDecisions, InboxCategoryAgentSecrets, InboxCategoryWaiting, InboxCategoryReviews, InboxCategoryPendingSecrets, InboxCategoryChat} {
		assert.Contains(t, sessResp.Categories, key)
	}
	assert.Equal(t, InboxStateOK, sessResp.Categories[InboxCategoryPendingSecrets].State)
	assert.Equal(t, 1, sessResp.Categories[InboxCategoryPendingSecrets].Count)
	assert.Equal(t, map[string]int{InboxBreakdownAgents: 1, InboxBreakdownProjects: 1}, sessResp.Categories[InboxCategoryPendingSecrets].Breakdown)
	assert.NotNil(t, sessResp.Categories[InboxCategoryPendingSecrets].NextExpiresAt)
	assert.Equal(t, InboxStateOK, sessResp.Categories[InboxCategoryWaiting].State)
	assert.Equal(t, 0, sessResp.Categories[InboxCategoryWaiting].Count)
	assert.Equal(t, "/agents?q=activity%3Awaiting_for_input", sessResp.Categories[InboxCategoryWaiting].Href)
	assert.Equal(t, map[string]int{InboxBreakdownProjects: 0}, sessResp.Categories[InboxCategoryWaiting].Breakdown)
	assert.NotNil(t, sessResp.Categories[InboxCategoryWaiting].AgentIDs)
	assert.Empty(t, sessResp.Categories[InboxCategoryWaiting].AgentIDs)

	chatCat := sessResp.Categories[InboxCategoryChat]
	assert.Equal(t, InboxStateOK, chatCat.State)
	assert.Equal(t, 0, chatCat.Count)
	assert.Empty(t, chatCat.Previews)
	assert.Equal(t, map[string]int{"dms": 0, "mentions": 0, "unreadConversations": 0}, chatCat.Breakdown)

	// Verify raw JSON wire shape: waiting always emits "agentIds": [] when state=="ok"
	// and count==0, while other categories omit "agentIds".
	var rawResp struct {
		Categories map[string]map[string]any `json:"categories"`
	}
	require.NoError(t, json.Unmarshal(sessRec.Body.Bytes(), &rawResp))
	waitingRawAgentIDs, hasWaitingAgentIDs := rawResp.Categories[InboxCategoryWaiting]["agentIds"]
	assert.True(t, hasWaitingAgentIDs, "waiting category must always include agentIds when state is ok")
	assert.Equal(t, []any{}, waitingRawAgentIDs)
	for _, otherKey := range []string{InboxCategoryPendingSecrets, InboxCategoryDecisions, InboxCategoryReviews, InboxCategoryChat} {
		_, hasAgentIDs := rawResp.Categories[otherKey]["agentIds"]
		assert.False(t, hasAgentIDs, "category %s must omit agentIds", otherKey)
	}
	for _, disabledKey := range []string{InboxCategoryDecisions, InboxCategoryReviews} {
		cat := sessResp.Categories[disabledKey]
		assert.Equal(t, InboxStateDisabled, cat.State, "category %s", disabledKey)
		assert.Equal(t, 0, cat.Count, "category %s", disabledKey)
		assert.False(t, cat.CountsTowardTotal, "disabled category %s must not count toward total", disabledKey)
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

func TestUserMeInboxSummary_PendingSecretsOrderingAndPrivacy(t *testing.T) {
	f := newPendingFixture(t, "inbox-pending-secrets")
	setInboxDashboardExperiment(t, f.Server, true)
	ctx := context.Background()

	now := time.Now().UTC()
	seed := func(ownerID, key, valueRef string, expiresAt time.Time, replacesID string) {
		// CreateOrReplacePendingSecret computes ExpiresAt = writeTime + PendingSecretTTL.
		writeTime := expiresAt.Add(-store.PendingSecretTTL)
		_, err := f.Store.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
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
			Created:          writeTime,
			Updated:          writeTime,
			ExpiresAt:        expiresAt,
			ReplacesSecretID: replacesID,
		}, writeTime)
		require.NoError(t, err)
	}

	// 4 live secrets for owner with varying expiry times.
	firstExpiry := now.Add(1 * time.Hour)
	seed(f.UserID, "FOURTH_EXPIRING", "ref-never-leak-4", now.Add(4*time.Hour), "")
	seed(f.UserID, "FIRST_EXPIRING", "ref-never-leak-1", firstExpiry, "existing-id")
	seed(f.UserID, "THIRD_EXPIRING", "ref-never-leak-3", now.Add(3*time.Hour), "")
	seed(f.UserID, "SECOND_EXPIRING", "ref-never-leak-2", now.Add(2*time.Hour), "")
	// 1 expired secret for owner (must be excluded).
	seed(f.UserID, "EXPIRED_SECRET", "ref-never-leak-exp", now.Add(-5*time.Minute), "")
	// 1 live secret for another user (must be excluded).
	seed(tid("other-owner"), "OTHER_USER_SECRET", "ref-never-leak-other", now.Add(30*time.Minute), "")

	// Default previews=3.
	rec := ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodGet, userMeInboxSummaryPath, nil)
	resp := decodeInboxSummary(t, rec)
	assert.Equal(t, 4, resp.Total)
	pending := resp.Categories[InboxCategoryPendingSecrets]
	assert.Equal(t, InboxStateOK, pending.State)
	assert.Equal(t, 4, pending.Count)
	assert.Equal(t, "/profile/secrets", pending.Href)
	assert.Equal(t, map[string]int{InboxBreakdownAgents: 1, InboxBreakdownProjects: 1}, pending.Breakdown)
	require.NotNil(t, pending.NextExpiresAt)
	assert.WithinDuration(t, firstExpiry, *pending.NextExpiresAt, 2*time.Second)
	require.Len(t, pending.Previews, 3)

	// Verify ordering (soonest expiresAt first) and fields.
	var keys []string
	for _, raw := range pending.Previews {
		b, err := json.Marshal(raw)
		require.NoError(t, err)
		var p InboxPendingSecretPreview
		require.NoError(t, json.Unmarshal(b, &p))
		assert.Equal(t, InboxPendingSecretKind, p.Kind)
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

	// previews=0 returns count, breakdown, and nextExpiresAt with empty previews array.
	recZero := ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodGet, userMeInboxSummaryPath+"?previews=0", nil)
	respZero := decodeInboxSummary(t, recZero)
	assert.Equal(t, 4, respZero.Total)
	pendingZero := respZero.Categories[InboxCategoryPendingSecrets]
	assert.Equal(t, 4, pendingZero.Count)
	assert.Equal(t, map[string]int{InboxBreakdownAgents: 1, InboxBreakdownProjects: 1}, pendingZero.Breakdown)
	require.NotNil(t, pendingZero.NextExpiresAt)
	assert.WithinDuration(t, firstExpiry, *pendingZero.NextExpiresAt, 2*time.Second)
	assert.Empty(t, pendingZero.Previews)
}

type inboxPendingSecretFallbackStore struct {
	store.Store
	rows []store.PendingSecret
}

func (s *inboxPendingSecretFallbackStore) ListPendingSecretsByOwner(_ context.Context, _, _ string) ([]store.PendingSecret, error) {
	return s.rows, nil
}

func TestUserMeInboxSummary_PendingSecretsBreakdownDistinctCounts(t *testing.T) {
	f := newPendingFixture(t, "inbox-pending-breakdown")
	setInboxDashboardExperiment(t, f.Server, true)
	ctx := context.Background()

	now := time.Now().UTC()
	proj1 := f.ProjectID
	proj2 := api.NewUUID()
	projExpired := api.NewUUID()
	projOther := api.NewUUID()
	agent1 := f.AgentID
	agent2 := api.NewUUID()
	agent3 := api.NewUUID()

	seed := func(ownerID, key, agentID, agentSlug, projectID, projectName string, expiresAt time.Time) {
		writeTime := expiresAt.Add(-store.PendingSecretTTL)
		_, err := f.Store.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
			ID:            api.NewUUID(),
			Key:           key,
			SecretType:    store.SecretTypeEnvironment,
			Scope:         store.ScopeUser,
			ScopeID:       ownerID,
			AllowProgeny:  true,
			InjectionMode: store.InjectionModeAsNeeded,
			ValueRef:      "ref-" + key,
			ValueLen:      12,
			AgentID:       agentID,
			AgentSlug:     agentSlug,
			ProjectID:     projectID,
			ProjectName:   projectName,
			Created:       writeTime,
			Updated:       writeTime,
			ExpiresAt:     expiresAt,
		}, writeTime)
		require.NoError(t, err)
	}

	// 2 live secrets from agent1 in proj1 (same agent & project -> counts once each).
	seed(f.UserID, "BD_A1_1", agent1, "agent-1", proj1, "Proj 1", now.Add(1*time.Hour))
	seed(f.UserID, "BD_A1_2", agent1, "agent-1", proj1, "Proj 1", now.Add(2*time.Hour))
	// 1 live secret from agent2 in proj1 (2nd distinct agent, same project).
	seed(f.UserID, "BD_A2_1", agent2, "agent-2", proj1, "Proj 1", now.Add(3*time.Hour))
	// 1 live secret from agent3 in proj2 (3rd distinct agent, 2nd project).
	seed(f.UserID, "BD_A3_1", agent3, "agent-3", proj2, "Proj 2", now.Add(4*time.Hour))
	// Expired secret from a distinct agent/project (must be excluded).
	seed(f.UserID, "BD_EXPIRED", api.NewUUID(), "agent-exp", projExpired, "Proj Exp", now.Add(-10*time.Minute))
	// Live secret for another user from a distinct agent/project (must be excluded).
	seed(tid("other-owner-bd"), "BD_OTHER", api.NewUUID(), "agent-other", projOther, "Proj Other", now.Add(30*time.Minute))

	for _, path := range []string{userMeInboxSummaryPath, userMeInboxSummaryPath + "?previews=0"} {
		rec := ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodGet, path, nil)
		resp := decodeInboxSummary(t, rec)
		pending := resp.Categories[InboxCategoryPendingSecrets]
		assert.Equal(t, InboxStateOK, pending.State, path)
		assert.Equal(t, 4, pending.Count, path)
		assert.Equal(t, map[string]int{
			InboxBreakdownAgents:   3,
			InboxBreakdownProjects: 2,
		}, pending.Breakdown, path)
	}

	// Verify the AgentSlug fallback when a row carries an empty AgentID (Ent
	// rejects empty agent_id on write, so inject via a store wrapper).
	t.Run("AgentSlugFallback", func(t *testing.T) {
		installStoreFault(t, f.Server, func(inner store.Store, _ *storeFaultSwitch) *inboxPendingSecretFallbackStore {
			return &inboxPendingSecretFallbackStore{
				Store: inner,
				rows: []store.PendingSecret{
					{
						ID: api.NewUUID(), Key: "FB_1", Scope: store.ScopeUser, ScopeID: f.UserID,
						AgentID: agent1, AgentSlug: "agent-1", ProjectID: proj1, ProjectName: "Proj 1",
						ExpiresAt: now.Add(1 * time.Hour),
					},
					{
						ID: api.NewUUID(), Key: "FB_2", Scope: store.ScopeUser, ScopeID: f.UserID,
						AgentID: "", AgentSlug: "slug-fallback-agent", ProjectID: proj2, ProjectName: "Proj 2",
						ExpiresAt: now.Add(2 * time.Hour),
					},
					{
						ID: api.NewUUID(), Key: "FB_3", Scope: store.ScopeUser, ScopeID: f.UserID,
						AgentID: "", AgentSlug: "slug-fallback-agent", ProjectID: proj2, ProjectName: "Proj 2",
						ExpiresAt: now.Add(3 * time.Hour),
					},
				},
			}
		})

		rec := ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodGet, userMeInboxSummaryPath, nil)
		resp := decodeInboxSummary(t, rec)
		pending := resp.Categories[InboxCategoryPendingSecrets]
		assert.Equal(t, InboxStateOK, pending.State)
		assert.Equal(t, 3, pending.Count)
		assert.Equal(t, map[string]int{
			InboxBreakdownAgents:   2,
			InboxBreakdownProjects: 2,
		}, pending.Breakdown)
	})
}

func TestUserMeInboxSummary_SourceErrorIsolation(t *testing.T) {
	f := newChatFixture(t)
	setInboxDashboardExperiment(t, f.srv, true)
	memberUser, err := f.s.GetUser(context.Background(), f.member)
	require.NoError(t, err)

	f.srv.inboxSourcesForTest = []inboxSourceEntry{
		{
			key: InboxCategoryPendingSecrets,
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
					CountsTowardTotal: false,
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
					CountsTowardTotal: false,
					Href:              "/artifacts?review_on_latest=1&owner=me",
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
	assert.Equal(t, InboxStateOK, resp.Categories[InboxCategoryPendingSecrets].State)
	assert.True(t, resp.Categories[InboxCategoryPendingSecrets].CountsTowardTotal)
	assert.Equal(t, InboxStateError, resp.Categories[InboxCategoryWaiting].State)
	assert.False(t, resp.Categories[InboxCategoryWaiting].CountsTowardTotal)
	assert.Equal(t, InboxErrorTimeout, resp.Categories[InboxCategoryWaiting].ErrorCode)
	assert.Equal(t, InboxStateError, resp.Categories[InboxCategoryReviews].State)
	assert.False(t, resp.Categories[InboxCategoryReviews].CountsTowardTotal)
	assert.Equal(t, InboxErrorInternal, resp.Categories[InboxCategoryReviews].ErrorCode)
	assert.False(t, resp.Categories[InboxCategoryChat].CountsTowardTotal)
	assert.False(t, resp.Categories[InboxCategoryDecisions].CountsTowardTotal)
}

type inboxCountingStore struct {
	store.Store
	listPendingCalls  atomic.Int32
	listAgentsCalls   atomic.Int32
	listProjectsCalls atomic.Int32
}

func (s *inboxCountingStore) ListPendingSecretsByOwner(ctx context.Context, scope, scopeID string) ([]store.PendingSecret, error) {
	s.listPendingCalls.Add(1)
	return s.Store.ListPendingSecretsByOwner(ctx, scope, scopeID)
}

func (s *inboxCountingStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	s.listAgentsCalls.Add(1)
	return s.Store.ListAgents(ctx, filter, opts)
}

func (s *inboxCountingStore) ListProjectSummaries(ctx context.Context, filter store.ProjectFilter, opts store.ListOptions) (*store.ListResult[store.Project], error) {
	s.listProjectsCalls.Add(1)
	return s.Store.ListProjectSummaries(ctx, filter, opts)
}

func TestUserMeInboxSummary_StoreCallCount(t *testing.T) {
	srv, rawStore := testServer(t)
	counter, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *inboxCountingStore {
		return &inboxCountingStore{Store: inner}
	})
	setInboxDashboardExperiment(t, srv, true)

	ctx := context.Background()
	uid := tid("inbox-spy-user")
	createTestUserWithRole(t, rawStore, uid, "spy@example.com", "admin", store.SystemRoleSuperAdmin)
	u, err := rawStore.GetUser(ctx, uid)
	require.NoError(t, err)

	counter.listPendingCalls.Store(0)
	counter.listAgentsCalls.Store(0)
	counter.listProjectsCalls.Store(0)
	rec := doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath, nil)
	decodeInboxSummary(t, rec)
	assert.Equal(t, int32(1), counter.listPendingCalls.Load(), "summary must issue exactly one pending-secrets store query")
	assert.Equal(t, int32(1), counter.listAgentsCalls.Load(), "summary must issue exactly one waiting-agents store query")
	assert.Equal(t, int32(0), counter.listProjectsCalls.Load(), "no project lookup when there are no waiting previews")

	// Seed 2 projects and 4 waiting agents; store calls stay constant regardless of agent/project count.
	for i := 0; i < 2; i++ {
		p := &store.Project{
			ID:      api.NewUUID(),
			Slug:    "spy-proj-" + string(rune('a'+i)),
			Name:    "Spy Project " + string(rune('A'+i)),
			OwnerID: u.ID,
			Created: time.Now().UTC(),
		}
		require.NoError(t, rawStore.CreateProject(ctx, p))
		for j := 0; j < 2; j++ {
			a := &store.Agent{
				ID:        api.NewUUID(),
				Slug:      "spy-agent-" + string(rune('a'+i)) + "-" + string(rune('0'+j)),
				Name:      "Spy Agent",
				ProjectID: p.ID,
				OwnerID:   u.ID,
				Phase:     string(state.PhaseRunning),
				Activity:  string(state.ActivityWaitingForInput),
				Message:   "waiting",
			}
			require.NoError(t, rawStore.CreateAgent(ctx, a))
		}
	}

	counter.listPendingCalls.Store(0)
	counter.listAgentsCalls.Store(0)
	counter.listProjectsCalls.Store(0)
	rec = doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath, nil)
	resp := decodeInboxSummary(t, rec)
	assert.Equal(t, 4, resp.Categories[InboxCategoryWaiting].Count)
	assert.Equal(t, int32(1), counter.listPendingCalls.Load())
	assert.Equal(t, int32(1), counter.listAgentsCalls.Load())
	assert.Equal(t, int32(1), counter.listProjectsCalls.Load(), "preview project names resolved in one batched query")

	// Badge mode (previews=0) skips project name lookup entirely.
	counter.listPendingCalls.Store(0)
	counter.listAgentsCalls.Store(0)
	counter.listProjectsCalls.Store(0)
	rec = doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath+"?previews=0", nil)
	decodeInboxSummary(t, rec)
	assert.Equal(t, int32(1), counter.listPendingCalls.Load())
	assert.Equal(t, int32(1), counter.listAgentsCalls.Load())
	assert.Equal(t, int32(0), counter.listProjectsCalls.Load())
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
			key: InboxCategoryPendingSecrets,
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
		{key: InboxCategoryReviews, run: inboxDisabledCategory("/artifacts?review_on_latest=1&owner=me")},
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

func TestUserMeInboxSummary_SingleflightDistinctTokensDoNotCoalesce(t *testing.T) {
	f := newChatFixture(t)
	setInboxDashboardExperiment(t, f.srv, true)

	tokA := f.mint(t, f.member, hubBoundary(), "inbox:read")
	tokB := f.mint(t, f.member, hubBoundary(), "inbox:read", "project:read")

	var calls atomic.Int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})

	f.srv.inboxSourcesForTest = []inboxSourceEntry{
		{
			key: InboxCategoryPendingSecrets,
			run: func(_ context.Context, v inboxViewer, _ int, _ string) InboxCategory {
				calls.Add(1)
				entered <- struct{}{}
				<-release
				count := 1
				if v.token != nil && v.token.HasScope("project:read") {
					count = 2
				}
				return InboxCategory{
					State:             InboxStateOK,
					Count:             count,
					CountsTowardTotal: true,
					Href:              "/profile/secrets",
					Previews:          []any{},
				}
			},
		},
		{key: InboxCategoryWaiting, run: inboxDisabledCategory("/agents")},
		{key: InboxCategoryReviews, run: inboxDisabledCategory("/artifacts?review_on_latest=1&owner=me")},
		{key: InboxCategoryChat, run: inboxDisabledCategory("/chat?filter=unread")},
		{key: InboxCategoryDecisions, run: inboxDisabledCategory("/profile/decisions?tab=pending")},
	}

	var wg sync.WaitGroup
	var totalA, totalB int
	wg.Add(2)
	go func() {
		defer wg.Done()
		totalA = decodeInboxSummary(t, f.call(t, tokA, http.MethodGet, userMeInboxSummaryPath, nil)).Total
	}()
	go func() {
		defer wg.Done()
		totalB = decodeInboxSummary(t, f.call(t, tokB, http.MethodGet, userMeInboxSummaryPath, nil)).Total
	}()

	// Both distinct tokens must enter the source concurrently rather than coalescing.
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("timed out waiting for both distinct tokens to execute without coalescing")
		}
	}
	close(release)
	wg.Wait()

	assert.Equal(t, int32(2), calls.Load(), "distinct UATs for the same user and boundary must not coalesce")
	assert.Equal(t, 1, totalA)
	assert.Equal(t, 2, totalB)
}

type inboxStubPendingSecretsStore struct {
	store.Store
	rows []store.PendingSecret
}

func (s *inboxStubPendingSecretsStore) ListPendingSecretsByOwner(_ context.Context, _, _ string) ([]store.PendingSecret, error) {
	return s.rows, nil
}

func TestUserMeInboxSummary_PendingSecretsCappedUsesLiveCount(t *testing.T) {
	srv, rawStore := testServer(t)
	now := time.Now().UTC()

	var rows []store.PendingSecret
	for i := 0; i < 3; i++ {
		rows = append(rows, store.PendingSecret{
			ID:        api.NewUUID(),
			Key:       "LIVE_" + string(rune('A'+i)),
			Scope:     store.ScopeUser,
			Created:   now.Add(-time.Hour),
			ExpiresAt: now.Add(time.Duration(i+1) * time.Hour),
		})
	}
	for i := 0; i < store.PendingSecretOwnerCap; i++ {
		rows = append(rows, store.PendingSecret{
			ID:        api.NewUUID(),
			Key:       "EXPIRED_" + api.NewUUID(),
			Scope:     store.ScopeUser,
			Created:   now.Add(-48 * time.Hour),
			ExpiresAt: now.Add(-time.Minute),
		})
	}

	installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *inboxStubPendingSecretsStore {
		return &inboxStubPendingSecretsStore{Store: inner, rows: rows}
	})
	setInboxDashboardExperiment(t, srv, true)

	u := &store.User{
		ID:          tid("inbox-capped-user"),
		Email:       "capped@example.com",
		DisplayName: "Capped User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     now,
	}
	require.NoError(t, rawStore.CreateUser(context.Background(), u))

	rec := doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath, nil)
	resp := decodeInboxSummary(t, rec)
	pending := resp.Categories[InboxCategoryPendingSecrets]
	assert.Equal(t, InboxStateOK, pending.State)
	assert.Equal(t, 3, pending.Count)
	assert.False(t, pending.Capped, "expired rows must not mark pending_secrets capped")
	assert.False(t, resp.TotalCapped)
	assert.Equal(t, 3, resp.Total)
}

func TestUserMeInboxSummary_PendingSecretApproveRejectHints(t *testing.T) {
	f := newPendingFixture(t, "inbox-hints")
	pub := NewChannelEventPublisher()
	defer pub.Close()
	f.Server.SetEventPublisher(pub)
	otherUserID := tid("inbox-hints-other")

	ownerCh, unsubOwner := pub.Subscribe("user." + f.UserID + ".inbox")
	defer unsubOwner()
	otherCh, unsubOther := pub.Subscribe("user." + otherUserID + ".inbox")
	defer unsubOther()

	drainNoEvent := func(ch <-chan Event, msg string) {
		select {
		case evt := <-ch:
			t.Fatalf("%s: unexpected event on %s: %s", msg, evt.Subject, string(evt.Data))
		case <-time.After(50 * time.Millisecond):
		}
	}

	expectPendingSecretsHint := func(ch <-chan Event) {
		select {
		case evt := <-ch:
			assert.Equal(t, "user."+f.UserID+".inbox", evt.Subject)
			var hint InboxHint
			require.NoError(t, json.Unmarshal(evt.Data, &hint))
			assert.Equal(t, InboxCategoryPendingSecrets, hint.Category)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for inbox pending_secrets hint")
		}
	}

	// Flag OFF: approve and reject must NOT publish user.*.inbox hints.
	p1 := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_OFF_1", userScopeWrite("val1", false)))
	rec := ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost, "/api/v1/pending-secrets/"+p1.PendingID+"/approve", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	drainNoEvent(ownerCh, "flag off approve")

	p2 := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_OFF_2", userScopeWrite("val2", false)))
	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost, "/api/v1/pending-secrets/"+p2.PendingID+"/reject", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	drainNoEvent(ownerCh, "flag off reject")

	// Flag ON: approve and reject publish {category:"pending_secrets"} to owner only.
	setInboxDashboardExperiment(t, f.Server, true)

	p3 := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_ON_1", userScopeWrite("val3", false)))
	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost, "/api/v1/pending-secrets/"+p3.PendingID+"/approve", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	expectPendingSecretsHint(ownerCh)
	drainNoEvent(otherCh, "non-owner on approve")

	p4 := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "KEY_ON_2", userScopeWrite("val4", false)))
	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPost, "/api/v1/pending-secrets/"+p4.PendingID+"/reject", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	expectPendingSecretsHint(ownerCh)
	drainNoEvent(otherCh, "non-owner on reject")
}

func TestUserMeInboxSummary_WaitingCategory(t *testing.T) {
	f := newChatFixture(t)
	setInboxDashboardExperiment(t, f.srv, true)
	ctx := context.Background()

	memberUser, err := f.s.GetUser(ctx, f.member)
	require.NoError(t, err)
	// Grant f.member access to projB as well so waiting spans two readable projects
	// while unreadableProj below stays excluded.
	createTestUserWithProjectRole(t, f.s, f.member, memberUser.Email, f.projB, store.ProjectRoleMember)

	seedAgent := func(projectID, ownerID, slug, name, phase, activity, msg string, deleted bool) *store.Agent {
		a := &store.Agent{
			ID:        api.NewUUID(),
			Slug:      slug,
			Name:      name,
			ProjectID: projectID,
			OwnerID:   ownerID,
			Phase:     phase,
			Activity:  activity,
			Message:   msg,
		}
		require.NoError(t, f.s.CreateAgent(ctx, a))
		if deleted {
			got, gErr := f.s.GetAgent(ctx, a.ID)
			require.NoError(t, gErr)
			got.DeletedAt = time.Now().UTC()
			require.NoError(t, f.s.UpdateAgent(ctx, got))
		}
		return a
	}

	// Create 4 waiting agents owned by f.member across projA (3) and projB (1)
	// with staggered creation/activity times so oldest ActivitySince comes first.
	longQuestion := strings.Repeat("q", 250)
	a1 := seedAgent(f.projA, f.member, "wait-1", "Wait One", string(state.PhaseRunning), string(state.ActivityWaitingForInput), longQuestion, false)
	time.Sleep(5 * time.Millisecond)
	a2 := seedAgent(f.projB, f.member, "wait-2", "Wait Two", string(state.PhaseRunning), string(state.ActivityWaitingForInput), "Need staging creds?", false)
	time.Sleep(5 * time.Millisecond)
	a3 := seedAgent(f.projA, f.member, "wait-3", "Wait Three", string(state.PhaseRunning), string(state.ActivityWaitingForInput), "Approve deploy?", false)
	time.Sleep(5 * time.Millisecond)
	a4 := seedAgent(f.projA, f.member, "wait-4", "Wait Four", string(state.PhaseRunning), string(state.ActivityWaitingForInput), "Fourth agent", false)

	// Non-matching agents that must be excluded:
	// - owned, running, different activity
	seedAgent(f.projA, f.member, "think-1", "Thinking", string(state.PhaseRunning), string(state.ActivityThinking), "working", false)
	// - owned, stopped, no waiting activity
	seedAgent(f.projA, f.member, "stop-1", "Stopped", string(state.PhaseStopped), "", "stopped", false)
	// - owned, soft-deleted while waiting
	seedAgent(f.projA, f.member, "del-1", "Deleted", string(state.PhaseRunning), string(state.ActivityWaitingForInput), "deleted", true)

	// Other user's waiting agent in a readable project (projA): MUST be included
	// because /agents?q=activity:waiting_for_input shows all readable agents (ptone/scion#9300).
	time.Sleep(5 * time.Millisecond)
	a5 := seedAgent(f.projA, f.owner, "other-wait", "Other Wait", string(state.PhaseRunning), string(state.ActivityWaitingForInput), "other user", false)

	// Unreadable project (f.member has no membership): waiting agent MUST be excluded.
	unreadableProj := &store.Project{
		ID:        api.NewUUID(),
		Slug:      "unreadable-proj",
		Name:      "Unreadable Project",
		OwnerID:   f.owner,
		CreatedBy: f.owner,
	}
	require.NoError(t, f.s.CreateProject(ctx, unreadableProj))
	seedAgent(unreadableProj.ID, f.owner, "hidden-wait", "Hidden Wait", string(state.PhaseRunning), string(state.ActivityWaitingForInput), "hidden", false)

	// 1. Session view: sees all 5 readable waiting agents (4 owned + 1 shared in projA),
	// ordered oldest first, capped at 3 previews, with breakdown.projects == 2.
	rec := doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath, nil)
	resp := decodeInboxSummary(t, rec)
	assert.Equal(t, 5, resp.Total)
	assert.False(t, resp.TotalCapped)

	waiting := resp.Categories[InboxCategoryWaiting]
	assert.Equal(t, InboxStateOK, waiting.State)
	assert.Equal(t, 5, waiting.Count)
	assert.False(t, waiting.Capped)
	assert.True(t, waiting.CountsTowardTotal)
	assert.Equal(t, "/agents?q=activity%3Awaiting_for_input", waiting.Href)
	assert.Equal(t, []string{a1.ID, a2.ID, a3.ID, a4.ID, a5.ID}, waiting.AgentIDs)
	assert.Equal(t, map[string]int{InboxBreakdownProjects: 2}, waiting.Breakdown)
	require.Len(t, waiting.Previews, 3)

	// Parity check (ptone/scion#9300): GET /api/v1/agents?q=activity:waiting_for_input
	// returns the exact same count as waiting.Count.
	setExperimentOverrides(t, f.srv, map[string]bool{
		experiments.InboxDashboard: true,
	})
	agentsRec := doRequestAsUser(t, f.srv, memberUser, http.MethodGet, "/api/v1/agents?q=activity%3Awaiting_for_input", nil)
	require.Equal(t, http.StatusOK, agentsRec.Code, agentsRec.Body.String())
	var agentsList ListAgentsResponse
	require.NoError(t, json.Unmarshal(agentsRec.Body.Bytes(), &agentsList))
	assert.Equal(t, len(agentsList.Agents), waiting.Count, "waiting count must match /api/v1/agents?q=activity:waiting_for_input")

	var previews []InboxWaitingAgentPreview
	for _, raw := range waiting.Previews {
		b, mErr := json.Marshal(raw)
		require.NoError(t, mErr)
		var p InboxWaitingAgentPreview
		require.NoError(t, json.Unmarshal(b, &p))
		previews = append(previews, p)
	}
	assert.Equal(t, []string{a1.ID, a2.ID, a3.ID}, []string{previews[0].AgentID, previews[1].AgentID, previews[2].AgentID})
	assert.Equal(t, a1.ID, previews[0].ID)
	assert.Equal(t, f.projA, previews[0].ProjectID)
	assert.NotEmpty(t, previews[0].ProjectName)
	assert.Equal(t, "Wait One", previews[0].AgentName)
	assert.Equal(t, "wait-1", previews[0].AgentSlug)
	assert.Equal(t, "/agents/"+a1.ID, previews[0].Href)
	assert.Equal(t, 200, len([]rune(previews[0].Question)), "question must be truncated to 200 runes")
	assert.True(t, strings.HasSuffix(previews[0].Question, "…"))
	assert.False(t, previews[0].Since.IsZero())
	assert.Equal(t, "Need staging creds?", previews[1].Question)
	assert.Equal(t, "/agents/"+a2.ID, previews[1].Href)

	// 2. previews=0 returns count, breakdown, and agentIds with empty previews.
	respZero := decodeInboxSummary(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath+"?previews=0", nil))
	assert.Equal(t, 5, respZero.Categories[InboxCategoryWaiting].Count)
	assert.Equal(t, []string{a1.ID, a2.ID, a3.ID, a4.ID, a5.ID}, respZero.Categories[InboxCategoryWaiting].AgentIDs)
	assert.Equal(t, map[string]int{InboxBreakdownProjects: 2}, respZero.Categories[InboxCategoryWaiting].Breakdown)
	assert.Empty(t, respZero.Categories[InboxCategoryWaiting].Previews)

	// 3. UAT with only inbox:read (ceiling lacks agent.list) receives
	// state:"unavailable" with countsTowardTotal:false so callers (and scion
	// inbox) do not see a false "Nothing Waiting" when agents are waiting.
	tokInboxOnly := f.mint(t, f.member, hubBoundary(), "inbox:read")
	inboxOnlyResp := decodeInboxSummary(t, f.call(t, tokInboxOnly, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, 0, inboxOnlyResp.Total)
	assert.Equal(t, InboxStateUnavailable, inboxOnlyResp.Categories[InboxCategoryWaiting].State)
	assert.False(t, inboxOnlyResp.Categories[InboxCategoryWaiting].CountsTowardTotal)
	assert.Equal(t, 0, inboxOnlyResp.Categories[InboxCategoryWaiting].Count)
	assert.Empty(t, inboxOnlyResp.Categories[InboxCategoryWaiting].AgentIDs)
	assert.Empty(t, inboxOnlyResp.Categories[InboxCategoryWaiting].Previews)

	// 4. UAT with inbox:read + agent:read (ceiling still lacks agent.list) also
	// receives state:"unavailable" and is excluded from total.
	tokInboxAgentRead := f.mint(t, f.member, hubBoundary(), "inbox:read", "agent:read")
	inboxReadResp := decodeInboxSummary(t, f.call(t, tokInboxAgentRead, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, 0, inboxReadResp.Total)
	assert.Equal(t, InboxStateUnavailable, inboxReadResp.Categories[InboxCategoryWaiting].State)
	assert.False(t, inboxReadResp.Categories[InboxCategoryWaiting].CountsTowardTotal)
	assert.Equal(t, 0, inboxReadResp.Categories[InboxCategoryWaiting].Count)
	assert.Empty(t, inboxReadResp.Categories[InboxCategoryWaiting].AgentIDs)

	// 5. Hub-bounded UAT with inbox:read + agent:list sees all 5 readable waiting agents,
	// matching GET /api/v1/agents?q=activity:waiting_for_input for the same token.
	tokHub := f.mint(t, f.member, hubBoundary(), "inbox:read", "agent:list")
	hubResp := decodeInboxSummary(t, f.call(t, tokHub, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, 5, hubResp.Total)
	assert.Equal(t, 5, hubResp.Categories[InboxCategoryWaiting].Count)
	assert.Equal(t, []string{a1.ID, a2.ID, a3.ID, a4.ID, a5.ID}, hubResp.Categories[InboxCategoryWaiting].AgentIDs)

	tokHubAgentsRec := f.call(t, tokHub, http.MethodGet, "/api/v1/agents?q=activity%3Awaiting_for_input", nil)
	require.Equal(t, http.StatusOK, tokHubAgentsRec.Code, tokHubAgentsRec.Body.String())
	var tokHubAgents ListAgentsResponse
	require.NoError(t, json.Unmarshal(tokHubAgentsRec.Body.Bytes(), &tokHubAgents))
	assert.Equal(t, len(tokHubAgents.Agents), hubResp.Categories[InboxCategoryWaiting].Count)

	// 6. Project-bounded UAT for projA with inbox:read + agent:list sees ONLY projA's
	// 4 waiting agents (not a2 in projB), matching GET /api/v1/agents for that token.
	tokProjA := f.mint(t, f.member, projectBoundary(f.projA), "inbox:read", "agent:list")
	projAResp := decodeInboxSummary(t, f.call(t, tokProjA, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, 4, projAResp.Total)
	assert.Equal(t, 4, projAResp.Categories[InboxCategoryWaiting].Count)
	assert.Equal(t, map[string]int{InboxBreakdownProjects: 1}, projAResp.Categories[InboxCategoryWaiting].Breakdown)
	assert.Equal(t, []string{a1.ID, a3.ID, a4.ID, a5.ID}, projAResp.Categories[InboxCategoryWaiting].AgentIDs)

	tokProjAAgentsRec := f.call(t, tokProjA, http.MethodGet, "/api/v1/agents?q=activity%3Awaiting_for_input", nil)
	require.Equal(t, http.StatusOK, tokProjAAgentsRec.Code, tokProjAAgentsRec.Body.String())
	var tokProjAAgents ListAgentsResponse
	require.NoError(t, json.Unmarshal(tokProjAAgentsRec.Body.Bytes(), &tokProjAAgents))
	assert.Equal(t, len(tokProjAAgents.Agents), projAResp.Categories[InboxCategoryWaiting].Count)

	// 7. Per-row agent read denial inside an in-scope project: a user granted only
	// agent.list (not agent.read) on projA sees only their own waiting agent, not
	// other users' waiting agents in projA, for both session and token callers.
	listOnlyUser := &store.User{
		ID:          tid("inbox-list-only-user"),
		Email:       "inbox-list-only@test.com",
		DisplayName: "List Only",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, f.s.CreateUser(ctx, listOnlyUser))
	ensureHubMembership(ctx, f.s, listOnlyUser.ID)
	grantProjectListOnly(t, f.s, listOnlyUser.ID, f.projA, "inbox-wait-list-only")
	ownWait := seedAgent(f.projA, listOnlyUser.ID, "list-only-own", "List Only Own", string(state.PhaseRunning), string(state.ActivityWaitingForInput), "my agent", false)

	listOnlySessResp := decodeInboxSummary(t, doRequestAsUser(t, f.srv, listOnlyUser, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, 1, listOnlySessResp.Categories[InboxCategoryWaiting].Count)
	assert.Equal(t, []string{ownWait.ID}, listOnlySessResp.Categories[InboxCategoryWaiting].AgentIDs)

	tokListOnly := f.mint(t, listOnlyUser.ID, hubBoundary(), "inbox:read", "agent:list")
	listOnlyTokResp := decodeInboxSummary(t, f.call(t, tokListOnly, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, 1, listOnlyTokResp.Categories[InboxCategoryWaiting].Count)
	assert.Equal(t, []string{ownWait.ID}, listOnlyTokResp.Categories[InboxCategoryWaiting].AgentIDs)
}

type inboxStubAgentsStore struct {
	store.Store
	agents []store.Agent
	err    error
}

func (s *inboxStubAgentsStore) ListAgents(_ context.Context, _ store.AgentFilter, _ store.ListOptions) (*store.ListResult[store.Agent], error) {
	if s.err != nil {
		return nil, s.err
	}
	return &store.ListResult[store.Agent]{Items: s.agents}, nil
}

func TestUserMeInboxSummary_WaitingCappedAndErrors(t *testing.T) {
	srv, rawStore := testServer(t)
	now := time.Now().UTC()

	agents := make([]store.Agent, inboxWaitingLimit)
	for i := range agents {
		since := now.Add(time.Duration(i) * time.Second)
		agents[i] = store.Agent{
			ID:            api.NewUUID(),
			Slug:          "cap-agent",
			Name:          "Cap Agent",
			ProjectID:     api.NewUUID(),
			Phase:         string(state.PhaseRunning),
			Activity:      string(state.ActivityWaitingForInput),
			ActivitySince: &since,
			Created:       since,
		}
	}

	stub, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *inboxStubAgentsStore {
		return &inboxStubAgentsStore{Store: inner, agents: agents}
	})
	setInboxDashboardExperiment(t, srv, true)

	uid := tid("inbox-waiting-cap-user")
	createTestUserWithRole(t, rawStore, uid, "waitcap@example.com", "admin", store.SystemRoleSuperAdmin)
	u, err := rawStore.GetUser(context.Background(), uid)
	require.NoError(t, err)

	// 200 returned -> capped=true, totalCapped=true, agentIds has all 200, previews capped at 3.
	resp := decodeInboxSummary(t, doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath, nil))
	waiting := resp.Categories[InboxCategoryWaiting]
	assert.Equal(t, InboxStateOK, waiting.State)
	assert.Equal(t, inboxWaitingLimit, waiting.Count)
	assert.True(t, waiting.Capped)
	assert.True(t, resp.TotalCapped)
	assert.Equal(t, inboxWaitingLimit, resp.Total)
	assert.Len(t, waiting.AgentIDs, inboxWaitingLimit)
	assert.Len(t, waiting.Previews, inboxSummaryDefaultPreviews)

	// Store error -> waiting state="error" (internal), HTTP 200, pending_secrets intact.
	stub.err = errors.New("db unavailable")
	errResp := decodeInboxSummary(t, doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateError, errResp.Categories[InboxCategoryWaiting].State)
	assert.False(t, errResp.Categories[InboxCategoryWaiting].CountsTowardTotal)
	assert.Equal(t, InboxErrorInternal, errResp.Categories[InboxCategoryWaiting].ErrorCode)
	assert.Equal(t, 0, errResp.Categories[InboxCategoryWaiting].Count)
	assert.Equal(t, InboxStateOK, errResp.Categories[InboxCategoryPendingSecrets].State)
	assert.True(t, errResp.Categories[InboxCategoryPendingSecrets].CountsTowardTotal)

	// Timeout error -> waiting state="error" (timeout).
	stub.err = context.DeadlineExceeded
	timeoutResp := decodeInboxSummary(t, doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath, nil))
	assert.Equal(t, InboxStateError, timeoutResp.Categories[InboxCategoryWaiting].State)
	assert.False(t, timeoutResp.Categories[InboxCategoryWaiting].CountsTowardTotal)
	assert.Equal(t, InboxErrorTimeout, timeoutResp.Categories[InboxCategoryWaiting].ErrorCode)
}

type inboxMemberLookupCountingStore struct {
	store.Store
	listMembers atomic.Int32
}

func (s *inboxMemberLookupCountingStore) ListProjectMembers(ctx context.Context, projectID string) ([]*store.ProjectMembership, error) {
	s.listMembers.Add(1)
	return s.Store.ListProjectMembers(ctx, projectID)
}

func TestUserMeInboxSummary_WaitingStatusHints(t *testing.T) {
	srv, rawStore := testServer(t)
	memberCounter, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *inboxMemberLookupCountingStore {
		return &inboxMemberLookupCountingStore{Store: inner}
	})
	pub := NewChannelEventPublisher()
	defer pub.Close()
	srv.SetEventPublisher(pub)

	ctx := context.Background()
	projectID := api.NewUUID()
	require.NoError(t, rawStore.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Slug: "inbox-wait-hint-proj",
		Name: "Inbox Wait Hint Project",
	}))

	ownerID := tid("inbox-wait-hint-owner")
	teammateID := tid("inbox-wait-hint-teammate")
	otherID := tid("inbox-wait-hint-other")

	createTestUserWithProjectRole(t, rawStore, ownerID, "hint-owner@test.com", projectID, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, rawStore, teammateID, "hint-teammate@test.com", projectID, store.ProjectRoleMember)

	ownerCh, unsubOwner := pub.Subscribe("user." + ownerID + ".inbox")
	defer unsubOwner()
	teammateCh, unsubTeammate := pub.Subscribe("user." + teammateID + ".inbox")
	defer unsubTeammate()
	otherCh, unsubOther := pub.Subscribe("user." + otherID + ".inbox")
	defer unsubOther()

	drainNoEvent := func(ch <-chan Event, label string) {
		t.Helper()
		select {
		case evt := <-ch:
			t.Fatalf("%s: unexpected event on %s: %s", label, evt.Subject, string(evt.Data))
		case <-time.After(50 * time.Millisecond):
		}
	}

	expectWaitingHint := func(ch <-chan Event, wantUserID, wantAgentID, wantActivity string) {
		t.Helper()
		select {
		case evt := <-ch:
			assert.Equal(t, "user."+wantUserID+".inbox", evt.Subject)
			var hint InboxHint
			require.NoError(t, json.Unmarshal(evt.Data, &hint))
			assert.Equal(t, InboxCategoryWaiting, hint.Category)
			assert.Equal(t, wantAgentID, hint.AgentID)
			assert.Equal(t, wantActivity, hint.Activity)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for waiting hint for %s (%s, %s)", wantUserID, wantAgentID, wantActivity)
		}
	}

	agent := &store.Agent{
		ID:        api.NewUUID(),
		ProjectID: projectID,
		OwnerID:   ownerID,
		Phase:     string(state.PhaseRunning),
		Activity:  string(state.ActivityWaitingForInput),
	}

	// Flag OFF: PublishAgentStatus emits no inbox hint and does not query members.
	memberCounter.listMembers.Store(0)
	pub.PublishAgentStatus(ctx, agent)
	drainNoEvent(ownerCh, "flag off enter waiting owner")
	drainNoEvent(teammateCh, "flag off enter waiting teammate")
	assert.Equal(t, int32(0), memberCounter.listMembers.Load(), "flag off must not look up project members")

	// Flag ON: entering and leaving waiting_for_input emit the full hint
	// (agentId + activity) to the owner and a content-free hint ({category:"waiting"}
	// with no agentId or activity) to project members, and never to non-members.
	setInboxDashboardExperiment(t, srv, true)

	expectContentFreeWaitingHint := func(ch <-chan Event, wantUserID string) {
		t.Helper()
		select {
		case evt := <-ch:
			assert.Equal(t, "user."+wantUserID+".inbox", evt.Subject)
			var hint InboxHint
			require.NoError(t, json.Unmarshal(evt.Data, &hint))
			assert.Equal(t, InboxCategoryWaiting, hint.Category)
			assert.Empty(t, hint.AgentID, "member waiting hint must omit agentId")
			assert.Empty(t, hint.Activity, "member waiting hint must omit activity")
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for content-free waiting hint for %s", wantUserID)
		}
	}

	memberCounter.listMembers.Store(0)
	pub.PublishAgentStatus(ctx, agent)
	expectWaitingHint(ownerCh, ownerID, agent.ID, string(state.ActivityWaitingForInput))
	expectContentFreeWaitingHint(teammateCh, teammateID)
	drainNoEvent(otherCh, "non-member on enter")
	assert.Equal(t, int32(1), memberCounter.listMembers.Load(), "entering waiting_for_input should resolve project members once")

	memberCounter.listMembers.Store(0)
	agent.Activity = string(state.ActivityThinking)
	pub.PublishAgentStatus(ctx, agent)
	expectWaitingHint(ownerCh, ownerID, agent.ID, string(state.ActivityThinking))
	expectContentFreeWaitingHint(teammateCh, teammateID)
	drainNoEvent(otherCh, "non-member on leave")
	assert.Equal(t, int32(1), memberCounter.listMembers.Load(), "leaving waiting_for_input should resolve project members once")

	// Non-waiting -> non-waiting transitions must NOT query project members or fan out
	// to teammates, while still delivering the unconditional owner hint.
	memberCounter.listMembers.Store(0)
	agent.Activity = string(state.ActivityExecuting)
	pub.PublishAgentStatus(ctx, agent)
	expectWaitingHint(ownerCh, ownerID, agent.ID, string(state.ActivityExecuting))
	drainNoEvent(teammateCh, "non-waiting transition teammate")
	assert.Equal(t, int32(0), memberCounter.listMembers.Load(), "non-waiting -> non-waiting must not query project members")

	// Fresh publisher (simulating a hub restart or another replica that did not observe
	// the prior waiting_for_input publish): leaving waiting_for_input still delivers the
	// owner hint without querying project members.
	pub2 := NewChannelEventPublisher()
	defer pub2.Close()
	srv.SetEventPublisher(pub2)
	ownerCh2, unsubOwner2 := pub2.Subscribe("user." + ownerID + ".inbox")
	defer unsubOwner2()
	teammateCh2, unsubTeammate2 := pub2.Subscribe("user." + teammateID + ".inbox")
	defer unsubTeammate2()

	memberCounter.listMembers.Store(0)
	agent.Activity = string(state.ActivityThinking)
	pub2.PublishAgentStatus(ctx, agent)
	expectWaitingHint(ownerCh2, ownerID, agent.ID, string(state.ActivityThinking))
	drainNoEvent(teammateCh2, "fresh publisher leave teammate")
	assert.Equal(t, int32(0), memberCounter.listMembers.Load(), "fresh publisher non-waiting publish must not query project members")

	// Restore pub and verify PublishAgentDeleted sends no inbox hint (§5.2) and clears tracked state.
	srv.SetEventPublisher(pub)
	agent.Activity = string(state.ActivityWaitingForInput)
	pub.PublishAgentStatus(ctx, agent)
	expectWaitingHint(ownerCh, ownerID, agent.ID, string(state.ActivityWaitingForInput))
	expectContentFreeWaitingHint(teammateCh, teammateID)

	memberCounter.listMembers.Store(0)
	pub.PublishAgentDeleted(ctx, agent.ID, agent.ProjectID)
	drainNoEvent(ownerCh, "agent deleted owner")
	drainNoEvent(teammateCh, "agent deleted teammate")
	assert.Equal(t, int32(0), memberCounter.listMembers.Load(), "delete must not query project members")
}

func TestUserMeInboxSummary_WaitingSQLScopeWindowNotStarvedByUnreadable(t *testing.T) {
	f := newChatFixture(t)
	setInboxDashboardExperiment(t, f.srv, true)
	ctx := context.Background()

	memberUser, err := f.s.GetUser(ctx, f.member)
	require.NoError(t, err)

	// Create a project that f.member cannot read and seed more waiting agents
	// than the waiting window limit, all with older timestamps than the
	// caller's readable agent. Without SQL AuthorizedProjectIDs narrowing, the
	// oldest-first window would fill with unreadable rows and drop the readable one.
	hiddenProj := &store.Project{
		ID:      api.NewUUID(),
		Slug:    "inbox-wait-hidden-window",
		Name:    "Inbox Wait Hidden Window",
		OwnerID: f.owner,
		Created: time.Now().UTC(),
		Updated: time.Now().UTC(),
	}
	require.NoError(t, f.s.CreateProject(ctx, hiddenProj))

	base := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		require.NoError(t, f.s.CreateAgent(ctx, &store.Agent{
			ID:                api.NewUUID(),
			Slug:              "hidden-wait-" + string(rune('a'+i)),
			Name:              "Hidden Wait",
			ProjectID:         hiddenProj.ID,
			OwnerID:           f.owner,
			Phase:             string(state.PhaseRunning),
			Activity:          string(state.ActivityWaitingForInput),
			Created:           ts,
			Updated:           ts,
			LastActivityEvent: ts,
		}))
	}

	readableTS := base.Add(30 * time.Minute)
	readableAgent := &store.Agent{
		ID:                api.NewUUID(),
		Slug:              "readable-wait-newer",
		Name:              "Readable Wait Newer",
		ProjectID:         f.projA,
		OwnerID:           f.member,
		Phase:             string(state.PhaseRunning),
		Activity:          string(state.ActivityWaitingForInput),
		Created:           readableTS,
		Updated:           readableTS,
		LastActivityEvent: readableTS,
	}
	require.NoError(t, f.s.CreateAgent(ctx, readableAgent))

	f.srv.inboxWaitingLimitForTest = 2
	resp := decodeInboxSummary(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath, nil))
	waiting := resp.Categories[InboxCategoryWaiting]
	assert.Equal(t, InboxStateOK, waiting.State)
	assert.Equal(t, 1, waiting.Count, "unreadable agents in other projects must not starve the waiting SQL window")
	assert.False(t, waiting.Capped)
	assert.Equal(t, []string{readableAgent.ID}, waiting.AgentIDs)
}

func TestUserMeInboxSummary_WaitingOrderingByActivitySince(t *testing.T) {
	srv, rawStore := testServer(t)
	base := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	t0 := base
	t1 := base.Add(1 * time.Minute)
	t2 := base.Add(2 * time.Minute)
	t3 := base.Add(3 * time.Minute)
	t4 := base.Add(4 * time.Minute)
	t5 := base.Add(5 * time.Minute)

	// Created earlier (t0), but entered waiting_for_input later (ActivitySince = t3).
	olderCreatedNewerSince := store.Agent{
		ID:            api.NewUUID(),
		Slug:          "older-created",
		Name:          "Older Created",
		ProjectID:     api.NewUUID(),
		Phase:         string(state.PhaseRunning),
		Activity:      string(state.ActivityWaitingForInput),
		Created:       t0,
		ActivitySince: &t3,
	}
	// Created later (t2), but has an older ActivitySince (t1) -> MUST sort first.
	newerCreatedOlderSince := store.Agent{
		ID:            api.NewUUID(),
		Slug:          "newer-created-older-since",
		Name:          "Newer Created Older Since",
		ProjectID:     api.NewUUID(),
		Phase:         string(state.PhaseRunning),
		Activity:      string(state.ActivityWaitingForInput),
		Created:       t2,
		ActivitySince: &t1,
	}
	// Nil ActivitySince falls back to LastActivityEvent (t2) -> sorts second.
	nilSinceLastActivity := store.Agent{
		ID:                api.NewUUID(),
		Slug:              "nil-since-last-activity",
		Name:              "Nil Since LastActivity",
		ProjectID:         api.NewUUID(),
		Phase:             string(state.PhaseRunning),
		Activity:          string(state.ActivityWaitingForInput),
		Created:           t4,
		LastActivityEvent: t2,
	}
	// Nil ActivitySince and zero LastActivityEvent falls back to Updated (t4) -> sorts fourth.
	nilSinceUpdated := store.Agent{
		ID:        api.NewUUID(),
		Slug:      "nil-since-updated",
		Name:      "Nil Since Updated",
		ProjectID: api.NewUUID(),
		Phase:     string(state.PhaseRunning),
		Activity:  string(state.ActivityWaitingForInput),
		Created:   t0,
		Updated:   t4,
	}
	// Nil ActivitySince and zero LastActivityEvent/Updated falls back to Created (t5) -> sorts fifth.
	nilSinceCreated := store.Agent{
		ID:        api.NewUUID(),
		Slug:      "nil-since-created",
		Name:      "Nil Since Created",
		ProjectID: api.NewUUID(),
		Phase:     string(state.PhaseRunning),
		Activity:  string(state.ActivityWaitingForInput),
		Created:   t5,
	}

	installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *inboxStubAgentsStore {
		return &inboxStubAgentsStore{
			Store: inner,
			agents: []store.Agent{
				olderCreatedNewerSince,
				nilSinceCreated,
				nilSinceUpdated,
				newerCreatedOlderSince,
				nilSinceLastActivity,
			},
		}
	})
	setInboxDashboardExperiment(t, srv, true)

	uid := tid("inbox-wait-order-user")
	createTestUserWithRole(t, rawStore, uid, "waitorder@example.com", "admin", store.SystemRoleSuperAdmin)
	u, err := rawStore.GetUser(context.Background(), uid)
	require.NoError(t, err)

	resp := decodeInboxSummary(t, doRequestAsUser(t, srv, u, http.MethodGet, userMeInboxSummaryPath+"?previews=5", nil))
	waiting := resp.Categories[InboxCategoryWaiting]
	assert.Equal(t, InboxStateOK, waiting.State)
	assert.Equal(t, 5, waiting.Count)

	wantOrder := []string{
		newerCreatedOlderSince.ID,
		nilSinceLastActivity.ID,
		olderCreatedNewerSince.ID,
		nilSinceUpdated.ID,
		nilSinceCreated.ID,
	}
	assert.Equal(t, wantOrder, waiting.AgentIDs)

	require.Len(t, waiting.Previews, 5)
	gotPreviewIDs := make([]string, 5)
	gotSince := make([]time.Time, 5)
	for i, raw := range waiting.Previews {
		b, err := json.Marshal(raw)
		require.NoError(t, err)
		var p InboxWaitingAgentPreview
		require.NoError(t, json.Unmarshal(b, &p))
		gotPreviewIDs[i] = p.AgentID
		gotSince[i] = p.Since
	}
	assert.Equal(t, wantOrder, gotPreviewIDs)
	assert.Equal(t, []time.Time{t1, t2, t3, t4, t5}, gotSince)
}

// TestUserMeInboxSummary_WaitingWindowKeepsOldestWhenCapped pins that when an
// owner has more waiting agents than the store window limit, ListAgents orders
// ascending by oldest-waiting (updated ASC) so the longest-waiting agents are
// kept in previews and agentIds rather than dropped by a created DESC window.
func TestUserMeInboxSummary_WaitingWindowKeepsOldestWhenCapped(t *testing.T) {
	f := newChatFixture(t)
	setInboxDashboardExperiment(t, f.srv, true)
	f.srv.inboxWaitingLimitForTest = 3
	ctx := context.Background()

	memberUser, err := f.s.GetUser(ctx, f.member)
	require.NoError(t, err)
	// Grant f.member access to projB as well so waiting spans two readable projects
	// while unreadableProj below stays excluded.
	createTestUserWithProjectRole(t, f.s, f.member, memberUser.Email, f.projB, store.ProjectRoleMember)

	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	seed := func(slug string, lastActivity time.Time) *store.Agent {
		a := &store.Agent{
			ID:                api.NewUUID(),
			Slug:              slug,
			Name:              slug,
			ProjectID:         f.projA,
			OwnerID:           f.member,
			Phase:             string(state.PhaseRunning),
			Activity:          string(state.ActivityWaitingForInput),
			Message:           "waiting " + slug,
			LastActivityEvent: lastActivity,
		}
		require.NoError(t, f.s.CreateAgent(ctx, a))
		return a
	}

	// Create 5 waiting agents in oldest-to-newest creation order, with explicit
	// LastActivityEvent timestamps spanning oldest (t+0m) to newest (t+40m).
	// A created DESC window would drop the oldest-waiting agents; ascending
	// updated order must keep the 3 longest-waiting (w1, w2, w3).
	w1 := seed("wait-oldest-1", base.Add(0*time.Minute))
	time.Sleep(2 * time.Millisecond)
	w2 := seed("wait-oldest-2", base.Add(10*time.Minute))
	time.Sleep(2 * time.Millisecond)
	w3 := seed("wait-oldest-3", base.Add(20*time.Minute))
	time.Sleep(2 * time.Millisecond)
	_ = seed("wait-newer-4", base.Add(30*time.Minute))
	time.Sleep(2 * time.Millisecond)
	_ = seed("wait-newest-5", base.Add(40*time.Minute))

	resp := decodeInboxSummary(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, userMeInboxSummaryPath+"?previews=3", nil))
	waiting := resp.Categories[InboxCategoryWaiting]
	assert.Equal(t, InboxStateOK, waiting.State)
	assert.Equal(t, 3, waiting.Count)
	assert.True(t, waiting.Capped)
	assert.True(t, resp.TotalCapped)
	assert.Equal(t, []string{w1.ID, w2.ID, w3.ID}, waiting.AgentIDs)

	require.Len(t, waiting.Previews, 3)
	gotIDs := make([]string, 3)
	for i, raw := range waiting.Previews {
		b, mErr := json.Marshal(raw)
		require.NoError(t, mErr)
		var p InboxWaitingAgentPreview
		require.NoError(t, json.Unmarshal(b, &p))
		gotIDs[i] = p.AgentID
	}
	assert.Equal(t, []string{w1.ID, w2.ID, w3.ID}, gotIDs)
}
