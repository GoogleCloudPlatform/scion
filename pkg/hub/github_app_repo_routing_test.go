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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setGitHubTriggersExperimentForRouting(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.GitHubTriggers || e.Name == experiments.Triggers {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, experiments.Default().Retired())
	require.NoError(t, err)
	srv.mu.Lock()
	srv.experiments = reg
	srv.mu.Unlock()
	require.Equal(t, on, srv.githubTriggersEnabled())
}

func doRepoRoutingAsUser(t *testing.T, srv *Server, user *AuthenticatedUser, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	ctx := contextWithCredentialContext(
		contextWithIdentity(req.Context(), user),
		CredentialContext{Kind: CredentialKindInteractive},
	)
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req.WithContext(ctx))
	return rr
}

func TestApplyPrimaryRouting(t *testing.T) {
	p1 := store.Project{ID: "proj-1", Name: "alpha"}
	p2 := store.Project{ID: "proj-2", Name: "beta"}
	p3 := store.Project{ID: "proj-3", Name: "gamma"}

	tests := []struct {
		name         string
		candidates   []store.Project
		routing      map[string]store.GitHubTriggerRoute
		repo         string
		wantSelected []store.Project
		wantBypassed []string
		wantStatus   string
	}{
		{
			name:         "no routing entry returns all candidates and none status",
			candidates:   []store.Project{p1, p2},
			routing:      nil,
			repo:         "Acme/Widgets",
			wantSelected: []store.Project{p1, p2},
			wantBypassed: nil,
			wantStatus:   GitHubTriggerRoutingStatusNone,
		},
		{
			name:       "routing entry for another repo leaves candidates untouched",
			candidates: []store.Project{p1, p2},
			routing: map[string]store.GitHubTriggerRoute{
				"acme/other": {PrimaryProjectID: "proj-1"},
			},
			repo:         "Acme/Widgets",
			wantSelected: []store.Project{p1, p2},
			wantBypassed: nil,
			wantStatus:   GitHubTriggerRoutingStatusNone,
		},
		{
			name:       "primary set and in multi-candidate set selects only primary and lists bypassed",
			candidates: []store.Project{p1, p2, p3},
			routing: map[string]store.GitHubTriggerRoute{
				"acme/widgets": {PrimaryProjectID: "proj-2"},
			},
			repo:         "Acme/Widgets",
			wantSelected: []store.Project{p2},
			wantBypassed: []string{"proj-1", "proj-3"},
			wantStatus:   GitHubTriggerRoutingStatusOK,
		},
		{
			name:       "primary set with single matching candidate returns primary and empty bypassed",
			candidates: []store.Project{p1},
			routing: map[string]store.GitHubTriggerRoute{
				"acme/widgets": {PrimaryProjectID: "proj-1"},
			},
			repo:         "acme/widgets",
			wantSelected: []store.Project{p1},
			wantBypassed: nil,
			wantStatus:   GitHubTriggerRoutingStatusOK,
		},
		{
			name:       "stale primary not in candidate set fails open to all candidates",
			candidates: []store.Project{p1, p3},
			routing: map[string]store.GitHubTriggerRoute{
				"acme/widgets": {PrimaryProjectID: "proj-deleted-or-moved"},
			},
			repo:         "Acme/Widgets",
			wantSelected: []store.Project{p1, p3},
			wantBypassed: nil,
			wantStatus:   GitHubTriggerRoutingStatusStalePrimary,
		},
		{
			name:       "stale primary when candidate list is empty returns empty slice and stale_primary",
			candidates: nil,
			routing: map[string]store.GitHubTriggerRoute{
				"acme/widgets": {PrimaryProjectID: "proj-1"},
			},
			repo:         "acme/widgets",
			wantSelected: []store.Project{},
			wantBypassed: nil,
			wantStatus:   GitHubTriggerRoutingStatusStalePrimary,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			selected, bypassed, status := ApplyPrimaryRouting(tc.candidates, tc.routing, tc.repo)
			assert.Equal(t, tc.wantSelected, selected)
			assert.Equal(t, tc.wantBypassed, bypassed)
			assert.Equal(t, tc.wantStatus, status)
		})
	}
}

func TestGitHubAppRepoRouting(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	adminID := tid("routing-admin")
	memberID := tid("routing-member")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID, Email: "routing-admin@example.com", DisplayName: "Admin",
		Role: store.UserRoleAdmin, Status: store.UserStatusActive, Created: time.Now(),
	}))
	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, "hub-admin", store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        adminID,
	})
	require.NoError(t, err)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: "routing-member@example.com", DisplayName: "Member",
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
	}))
	admin := NewAuthenticatedUser(adminID, "routing-admin@example.com", "Admin", store.UserRoleAdmin, "api")
	member := NewAuthenticatedUser(memberID, "routing-member@example.com", "Member", store.UserRoleMember, "api")

	instID := int64(5001)
	otherInstID := int64(5002)
	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "Acme",
		AccountType:    "Organization",
		AppID:          99,
		Repositories:   []string{"Acme/Widgets", "Acme/Gadgets"},
		Status:         store.GitHubInstallationStatusActive,
	}))
	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: otherInstID,
		AccountLogin:   "BetaOrg",
		AccountType:    "Organization",
		AppID:          99,
		Repositories:   []string{"BetaOrg/Service"},
		Status:         store.GitHubInstallationStatusActive,
	}))

	projA := tid("routing-proj-a")
	projB := tid("routing-proj-b")
	projOther := tid("routing-proj-other")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:                   projA,
		Name:                 "alpha",
		Slug:                 "alpha",
		GitRemote:            "https://github.com/Acme/Widgets.git",
		GitHubInstallationID: &instID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:                   projB,
		Name:                 "bravo",
		Slug:                 "bravo",
		GitRemote:            "git@github.com:acme/widgets.git",
		GitHubInstallationID: &instID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:                   projOther,
		Name:                 "other",
		Slug:                 "other",
		GitRemote:            "https://github.com/BetaOrg/Service.git",
		GitHubInstallationID: &otherInstID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}))

	// 1. Experiment off -> 404 for admin on GET/PUT/DELETE, 403 for non-admin.
	rr := doRepoRoutingAsUser(t, srv, admin, http.MethodGet, "/api/v1/github-app/repo-routing", nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodPut, "/api/v1/github-app/repo-routing/Acme/Widgets", GitHubRepoRoutingPutRequest{
		InstallationID: instID, PrimaryProjectID: projA,
	})
	assert.Equal(t, http.StatusNotFound, rr.Code)
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodDelete, "/api/v1/github-app/repo-routing/Acme/Widgets", nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)
	rr = doRepoRoutingAsUser(t, srv, member, http.MethodPut, "/api/v1/github-app/repo-routing/Acme/Widgets", GitHubRepoRoutingPutRequest{
		InstallationID: instID, PrimaryProjectID: projA,
	})
	assert.Equal(t, http.StatusForbidden, rr.Code)

	// Enable hub.github_triggers.
	setGitHubTriggersExperimentForRouting(t, srv, true)

	// 2. Non-admin member gets 403 even when experiment is enabled.
	rr = doRepoRoutingAsUser(t, srv, member, http.MethodGet, "/api/v1/github-app/repo-routing", nil)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	rr = doRepoRoutingAsUser(t, srv, member, http.MethodPut, "/api/v1/github-app/repo-routing/Acme/Widgets", GitHubRepoRoutingPutRequest{
		InstallationID: instID, PrimaryProjectID: projA,
	})
	assert.Equal(t, http.StatusForbidden, rr.Code)
	rr = doRepoRoutingAsUser(t, srv, member, http.MethodDelete, "/api/v1/github-app/repo-routing/Acme/Widgets", nil)
	assert.Equal(t, http.StatusForbidden, rr.Code)

	// 3. Initial GET lists candidate repositories with status "none", emits "primary_project_id": null,
	// and supports ?installation= filter.
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodGet, "/api/v1/github-app/repo-routing?installation=5001", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var rawList []map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &rawList))
	require.Len(t, rawList, 1)
	primaryVal, hasPrimaryKey := rawList[0]["primary_project_id"]
	assert.True(t, hasPrimaryKey, "primary_project_id key must always be present on wire (null when unset): %s", rr.Body.String())
	assert.Nil(t, primaryVal, "primary_project_id must serialize as JSON null when unset")

	var list []GitHubRepoRoutingEntry
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, instID, list[0].InstallationID)
	assert.Equal(t, "acme/widgets", list[0].Repo)
	assert.Equal(t, GitHubTriggerRoutingStatusNone, list[0].Status)
	assert.Nil(t, list[0].PrimaryProjectID)
	require.Len(t, list[0].Projects, 2)
	assert.Equal(t, projA, list[0].Projects[0].ID)
	assert.Equal(t, projB, list[0].Projects[1].ID)

	// 4. PUT with non-candidate project returns 409 not_a_candidate.
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodPut, "/api/v1/github-app/repo-routing/Acme/Widgets", GitHubRepoRoutingPutRequest{
		InstallationID:   instID,
		PrimaryProjectID: projOther,
	})
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), ErrCodeNotACandidate)

	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodPut, "/api/v1/github-app/repo-routing/Acme/Widgets", GitHubRepoRoutingPutRequest{
		InstallationID:   instID,
		PrimaryProjectID: tid("nonexistent-project"),
	})
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), ErrCodeNotACandidate)

	// 5. PUT with valid candidate stores lowercased repo key, writes audit record, and GET reports status "ok".
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodPut, "/api/v1/github-app/repo-routing/Acme/Widgets", GitHubRepoRoutingPutRequest{
		InstallationID:   instID,
		PrimaryProjectID: projB,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var putResp GitHubRepoRoutingEntry
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &putResp))
	assert.Equal(t, "acme/widgets", putResp.Repo)
	require.NotNil(t, putResp.PrimaryProjectID)
	assert.Equal(t, projB, *putResp.PrimaryProjectID)
	assert.Equal(t, GitHubTriggerRoutingStatusOK, putResp.Status)

	storedInst, err := s.GetGitHubInstallation(ctx, instID)
	require.NoError(t, err)
	require.Contains(t, storedInst.TriggerRouting, "acme/widgets")
	assert.Equal(t, projB, storedInst.TriggerRouting["acme/widgets"].PrimaryProjectID)
	assert.Equal(t, adminID, storedInst.TriggerRouting["acme/widgets"].SetBy)

	audits, total, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: githubRepoRoutingAuditMutation,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, audits, 1)
	assert.Equal(t, "5001", audits[0].TargetID)
	assert.Equal(t, adminID, audits[0].ActorPrincipalID)
	assert.Contains(t, audits[0].AfterSummary, projB)

	// 5b. Cached Repositories gaps or repo_not_accessible status do NOT exclude a §4.2 candidate:
	// GET still reports status "ok" and PUT still accepts the candidate.
	pB, err := s.GetProject(ctx, projB)
	require.NoError(t, err)
	now := time.Now()
	pB.GitHubAppStatus = &store.GitHubAppProjectStatus{
		State:       store.GitHubAppStateError,
		ErrorCode:   githubapp.ErrCodeRepoNotAccessible,
		LastChecked: now,
	}
	require.NoError(t, s.UpdateProject(ctx, pB))
	srv.invalidateGitHubAssociation(projB)

	instForStaleRepoCache, err := s.GetGitHubInstallation(ctx, instID)
	require.NoError(t, err)
	instForStaleRepoCache.Repositories = []string{"Acme/Gadgets"} // Acme/Widgets absent from cached list
	require.NoError(t, s.UpdateGitHubInstallation(ctx, instForStaleRepoCache))
	srv.invalidateGitHubAssociation(projA)
	srv.invalidateGitHubAssociation(projB)

	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodGet, "/api/v1/github-app/repo-routing?installation=5001", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, GitHubTriggerRoutingStatusOK, list[0].Status,
		"repo_not_accessible or stale Repositories cache must not disagree with §4.2 candidacy")
	require.Len(t, list[0].Projects, 2)

	// 6. When primary project loses §4.2 candidacy (disassociated from installation or remote changed),
	// GET reports status "stale_primary".
	pB, err = s.GetProject(ctx, projB)
	require.NoError(t, err)
	pB.GitHubInstallationID = nil
	require.NoError(t, s.UpdateProject(ctx, pB))
	srv.invalidateGitHubAssociation(projB)

	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodGet, "/api/v1/github-app/repo-routing?installation=5001", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, GitHubTriggerRoutingStatusStalePrimary, list[0].Status)
	require.NotNil(t, list[0].PrimaryProjectID)
	assert.Equal(t, projB, *list[0].PrimaryProjectID)
	require.Len(t, list[0].Projects, 1)
	assert.Equal(t, projA, list[0].Projects[0].ID)

	// 7. DELETE removes the entry and writes a second mutation audit record.
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodDelete, "/api/v1/github-app/repo-routing/ACME/WIDGETS", nil)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

	storedInst, err = s.GetGitHubInstallation(ctx, instID)
	require.NoError(t, err)
	assert.NotContains(t, storedInst.TriggerRouting, "acme/widgets")

	audits, total, err = s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: githubRepoRoutingAuditMutation,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, audits, 2)

	// Repeating DELETE without ?installation= when no route exists is idempotent (204)
	// and writes no spurious no-op audit record.
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodDelete, "/api/v1/github-app/repo-routing/ACME/WIDGETS", nil)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	audits, total, err = s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: githubRepoRoutingAuditMutation,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, total, "no-op DELETE without ?installation= must not write a mutation audit record")
	require.Len(t, audits, 2)

	// 8. Project deletion cascade: setting projA as primary and deleting projA removes its routing entry.
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodPut, "/api/v1/github-app/repo-routing/acme/widgets", GitHubRepoRoutingPutRequest{
		InstallationID:   instID,
		PrimaryProjectID: projA,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	require.NoError(t, s.DeleteProject(ctx, projA))
	srv.invalidateGitHubAssociation(projA)

	storedInst, err = s.GetGitHubInstallation(ctx, instID)
	require.NoError(t, err)
	assert.Empty(t, storedInst.TriggerRouting, "deleting primary project must cascade-delete its trigger_routing entry")
}

// TestGitHubAppRepoRouting_ScopedTokens pins bearer-token boundary and selector
// enforcement for the three repo-routing catalog entry points excluded from the
// generic matrix while hub.github_triggers defaults to off.
func TestGitHubAppRepoRouting_ScopedTokens(t *testing.T) {
	srv, s := testServer(t)
	setGitHubTriggersExperimentForRouting(t, srv, true)
	ctx := context.Background()

	adminID := tid("routing-tok-admin")
	projID := tid("routing-tok-proj")
	createRS1Project(t, s, projID, adminID)

	u, err := s.GetUser(ctx, adminID)
	require.NoError(t, err)
	u.Role = store.UserRoleAdmin
	require.NoError(t, s.UpdateUser(ctx, u))

	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, "hub-admin", store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        adminID,
	})
	require.NoError(t, err)

	instID := int64(6001)
	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          99,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
	}))

	proj, err := s.GetProject(ctx, projID)
	require.NoError(t, err)
	proj.GitRemote = "https://github.com/acme/widgets.git"
	proj.GitHubInstallationID = &instID
	require.NoError(t, s.UpdateProject(ctx, proj))

	mintHub := func(scopes ...string) string {
		key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
			UserID: adminID, Name: "gh-route-" + tid("tok"), Boundary: hubBoundary(), Scopes: scopes,
		})
		require.NoError(t, err)
		return key
	}
	mintProj := func(scopes ...string) string {
		key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
			UserID: adminID, Name: "gh-route-proj-" + tid("tok"), Boundary: projectBoundary(projID), Scopes: scopes,
		})
		require.NoError(t, err)
		return key
	}

	readTok := mintHub("hub_github_app:read")
	updateTok := mintHub("hub_github_app:update")
	unrelatedHubTok := mintHub("template:read")
	projTok := mintProj("project:read")

	// GET requires hub boundary + hub_github_app:read.
	rec := doRequestWithToken(t, srv, readTok, http.MethodGet, "/api/v1/github-app/repo-routing", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, unrelatedHubTok, http.MethodGet, "/api/v1/github-app/repo-routing", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, projTok, http.MethodGet, "/api/v1/github-app/repo-routing", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// PUT requires hub boundary + hub_github_app:update.
	putBody := GitHubRepoRoutingPutRequest{InstallationID: instID, PrimaryProjectID: projID}
	rec = doRequestWithToken(t, srv, readTok, http.MethodPut, "/api/v1/github-app/repo-routing/acme/widgets", putBody)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, projTok, http.MethodPut, "/api/v1/github-app/repo-routing/acme/widgets", putBody)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, updateTok, http.MethodPut, "/api/v1/github-app/repo-routing/acme/widgets", putBody)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// DELETE requires hub boundary + hub_github_app:update.
	rec = doRequestWithToken(t, srv, readTok, http.MethodDelete, "/api/v1/github-app/repo-routing/acme/widgets", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, projTok, http.MethodDelete, "/api/v1/github-app/repo-routing/acme/widgets", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, updateTok, http.MethodDelete, "/api/v1/github-app/repo-routing/acme/widgets", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestGitHubAppInstallations_TriggerRoutingSingleWriter verifies that the
// generic GitHub App installation endpoints (POST/PUT/GET /api/v1/github-app/installations[/{id}])
// neither accept nor echo trigger_routing, and that status-only updates (such as
// webhook suspend/unsuspend or stale read-modify-write snapshots) preserve
// routes set in between.
func TestGitHubAppInstallations_TriggerRoutingSingleWriter(t *testing.T) {
	srv, s := webhookTestServer(t)
	ctx := context.Background()

	adminID := tid("inst-sw-admin")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID, Email: "inst-sw-admin@example.com", DisplayName: "Admin",
		Role: store.UserRoleAdmin, Status: store.UserStatusActive, Created: time.Now(),
	}))
	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, "hub-admin", store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        adminID,
	})
	require.NoError(t, err)
	admin := NewAuthenticatedUser(adminID, "inst-sw-admin@example.com", "Admin", store.UserRoleAdmin, "api")

	doRawAsAdmin := func(method, path, rawJSON string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(rawJSON))
		req.Header.Set("Content-Type", "application/json")
		rctx := contextWithCredentialContext(
			contextWithIdentity(req.Context(), admin),
			CredentialContext{Kind: CredentialKindInteractive},
		)
		rr := httptest.NewRecorder()
		srv.mux.ServeHTTP(rr, req.WithContext(rctx))
		return rr
	}

	const instID = int64(7101)
	projA := tid("inst-sw-proj-a")
	projB := tid("inst-sw-proj-b")

	// 1. POST /api/v1/github-app/installations with trigger_routing in body (even with experiment off)
	// ignores trigger_routing and does not echo it.
	rr := doRawAsAdmin(http.MethodPost, "/api/v1/github-app/installations", `{
		"installation_id": 7101,
		"account_login": "Acme",
		"account_type": "Organization",
		"app_id": 42,
		"repositories": ["Acme/Widgets"],
		"status": "active",
		"trigger_routing": {"acme/widgets": {"primary_project_id": "injected-on-create", "set_by": "spoofed"}}
	}`)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "trigger_routing")

	stored, err := s.GetGitHubInstallation(ctx, instID)
	require.NoError(t, err)
	assert.Empty(t, stored.TriggerRouting)

	instRef := instID
	for _, p := range []*store.Project{
		{ID: projA, Name: "alpha", Slug: "alpha", GitRemote: "https://github.com/Acme/Widgets.git", GitHubInstallationID: &instRef, Created: time.Now(), Updated: time.Now()},
		{ID: projB, Name: "bravo", Slug: "bravo", GitRemote: "https://github.com/Acme/Widgets.git", GitHubInstallationID: &instRef, Created: time.Now(), Updated: time.Now()},
	} {
		require.NoError(t, s.CreateProject(ctx, p))
	}

	// Set a legitimate route via the repo-routing API.
	setGitHubTriggersExperimentForRouting(t, srv, true)
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodPut, "/api/v1/github-app/repo-routing/Acme/Widgets", GitHubRepoRoutingPutRequest{
		InstallationID:   instID,
		PrimaryProjectID: projA,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// 2. GET /api/v1/github-app/installations and GET .../{id} never echo trigger_routing.
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodGet, "/api/v1/github-app/installations", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "trigger_routing")

	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodGet, "/api/v1/github-app/installations/7101", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "trigger_routing")

	// 3. PUT /api/v1/github-app/installations/{id} with a conflicting trigger_routing map or empty {}
	// leaves stored routing untouched and writes no routing audit record.
	for _, body := range []string{
		`{"account_login":"Acme","account_type":"Organization","app_id":42,"repositories":["Acme/Widgets"],"status":"active","trigger_routing":{"acme/widgets":{"primary_project_id":"unauthorized-override","set_by":"spoofed"}}}`,
		`{"account_login":"Acme","account_type":"Organization","app_id":42,"repositories":["Acme/Widgets"],"status":"active","trigger_routing":{}}`,
	} {
		rr = doRawAsAdmin(http.MethodPut, "/api/v1/github-app/installations/7101", body)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.NotContains(t, rr.Body.String(), "trigger_routing")

		stored, err = s.GetGitHubInstallation(ctx, instID)
		require.NoError(t, err)
		require.Contains(t, stored.TriggerRouting, "acme/widgets")
		assert.Equal(t, projA, stored.TriggerRouting["acme/widgets"].PrimaryProjectID)
		assert.Equal(t, adminID, stored.TriggerRouting["acme/widgets"].SetBy)
	}
	_, auditCount, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: githubRepoRoutingAuditMutation})
	require.NoError(t, err)
	assert.Equal(t, 1, auditCount, "generic installation PUT must not write repo_routing audit records")

	// 4. Stale read-modify-write snapshot (health-check style) + webhook status-only update
	// after a route change in between both preserve the new route.
	staleSnapshot, err := s.GetGitHubInstallation(ctx, instID)
	require.NoError(t, err)
	require.Equal(t, projA, staleSnapshot.TriggerRouting["acme/widgets"].PrimaryProjectID)

	// Route changes to projB while staleSnapshot is in memory.
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodPut, "/api/v1/github-app/repo-routing/Acme/Widgets", GitHubRepoRoutingPutRequest{
		InstallationID:   instID,
		PrimaryProjectID: projB,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// Health-check / background caller writes back staleSnapshot with a status update.
	staleSnapshot.Status = store.GitHubInstallationStatusSuspended
	require.NoError(t, s.UpdateGitHubInstallation(ctx, staleSnapshot))

	stored, err = s.GetGitHubInstallation(ctx, instID)
	require.NoError(t, err)
	assert.Equal(t, store.GitHubInstallationStatusSuspended, stored.Status)
	require.Contains(t, stored.TriggerRouting, "acme/widgets")
	assert.Equal(t, projB, stored.TriggerRouting["acme/widgets"].PrimaryProjectID,
		"stale read-modify-write snapshot must not revert a route set in between")

	// Webhook status-only update (unsuspend) also preserves the route.
	whPayload := mustJSON(t, map[string]any{
		"action": "unsuspend",
		"installation": map[string]any{
			"id":     instID,
			"app_id": 42,
			"account": map[string]any{
				"login": "Acme",
				"type":  "Organization",
			},
		},
	})
	whReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(whPayload))
	whReq.Header.Set("X-GitHub-Event", "installation")
	whReq.Header.Set("X-GitHub-Delivery", "delivery-unsuspend-sw")
	whReq.Header.Set("X-Hub-Signature-256", signWebhookPayload(whPayload, "test-webhook-secret"))
	whRec := httptest.NewRecorder()
	srv.handleGitHubWebhook(whRec, whReq)
	require.Equal(t, http.StatusOK, whRec.Code, whRec.Body.String())

	stored, err = s.GetGitHubInstallation(ctx, instID)
	require.NoError(t, err)
	assert.Equal(t, store.GitHubInstallationStatusActive, stored.Status)
	require.Contains(t, stored.TriggerRouting, "acme/widgets")
	assert.Equal(t, projB, stored.TriggerRouting["acme/widgets"].PrimaryProjectID,
		"installation webhook status update must preserve trigger_routing")
}

// TestGitHubAppRepoRouting_ConsecutiveWritesPreserveRebuildLimiter verifies that
// consecutive repo-routing PUT/DELETE requests mark the association report
// invalid without resetting lastFullBuild, so back-to-back routing writes do
// not trigger back-to-back full association-report rebuilds.
func TestGitHubAppRepoRouting_ConsecutiveWritesPreserveRebuildLimiter(t *testing.T) {
	srv, s := testServer(t)
	setGitHubTriggersExperimentForRouting(t, srv, true)
	ctx := context.Background()

	adminID := tid("routing-lim-admin")
	creatorID := tid("routing-lim-creator")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID, Email: "routing-lim-admin@example.com", DisplayName: "Admin",
		Role: store.UserRoleAdmin, Status: store.UserStatusActive, Created: time.Now(),
	}))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: creatorID, Email: "routing-lim-creator@example.com", DisplayName: "Creator",
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
	}))
	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, "hub-admin", store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        adminID,
	})
	require.NoError(t, err)
	admin := NewAuthenticatedUser(adminID, "routing-lim-admin@example.com", "Admin", store.UserRoleAdmin, "api")

	require.NoError(t, s.CreateSecret(ctx, &store.Secret{
		ID:             tid("routing-lim-gh-token"),
		Key:            "GITHUB_TOKEN",
		EncryptedValue: "creator-token",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "GITHUB_TOKEN",
		Scope:          store.ScopeUser,
		ScopeID:        creatorID,
	}))
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	var repoAccessChecks atomic.Int64
	srv.githubRepoAccessCheck = func(_ context.Context, _, _, _, _ string) (githubapp.RepoAccess, error) {
		repoAccessChecks.Add(1)
		// Return an inconclusive error so githubAccessCache caches nothing and
		// every full association-report rebuild increments repoAccessChecks.
		return githubapp.RepoAccess{}, errors.New("upstream unavailable")
	}

	instID := int64(7201)
	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "Acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"Acme/Widgets"},
		Status:         store.GitHubInstallationStatusActive,
	}))

	projA := tid("routing-lim-proj-a")
	projB := tid("routing-lim-proj-b")
	for _, tc := range []struct{ id, name string }{
		{projA, "alpha"},
		{projB, "bravo"},
	} {
		require.NoError(t, s.CreateProject(ctx, &store.Project{
			ID:                   tc.id,
			Name:                 tc.name,
			Slug:                 tc.name,
			GitRemote:            "https://github.com/Acme/Widgets.git",
			GitHubInstallationID: &instID,
			CreatedBy:            creatorID,
			Created:              time.Now(),
			Updated:              time.Now(),
		}))
	}

	// First PUT: cold report cache -> runs 1 full rebuild (1 access check per associated project = 2).
	rr := doRepoRoutingAsUser(t, srv, admin, http.MethodPut, "/api/v1/github-app/repo-routing/Acme/Widgets", GitHubRepoRoutingPutRequest{
		InstallationID:   instID,
		PrimaryProjectID: projA,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var entry GitHubRepoRoutingEntry
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &entry))
	require.NotNil(t, entry.PrimaryProjectID)
	assert.Equal(t, projA, *entry.PrimaryProjectID)
	assert.Equal(t, GitHubTriggerRoutingStatusOK, entry.Status)
	require.Len(t, entry.Projects, 2)
	assert.Equal(t, int64(2), repoAccessChecks.Load(), "cold report build must check each associated project once")

	// Plain association-report GET right after first PUT serves the warm report (200, never 429).
	reportRR := doRepoRoutingAsUser(t, srv, admin, http.MethodGet, "/api/v1/github-app/installations/association-report", nil)
	require.Equal(t, http.StatusOK, reportRR.Code, "plain association-report GET after first routing PUT must return 200: %s", reportRR.Body.String())
	assert.Equal(t, int64(2), repoAccessChecks.Load())

	srv.githubAssociationReportCache.mu.Lock()
	firstBuildAt := srv.githubAssociationReportCache.lastFullBuild
	srv.githubAssociationReportCache.mu.Unlock()
	require.False(t, firstBuildAt.IsZero(), "lastFullBuild must be recorded after initial build")

	// Second PUT immediately after: must NOT reset lastFullBuild or run a second full rebuild,
	// and plain GET /api/v1/github-app/installations/association-report must continue serving 200 (never 429).
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodPut, "/api/v1/github-app/repo-routing/Acme/Widgets", GitHubRepoRoutingPutRequest{
		InstallationID:   instID,
		PrimaryProjectID: projB,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &entry))
	require.NotNil(t, entry.PrimaryProjectID)
	assert.Equal(t, projB, *entry.PrimaryProjectID)
	assert.Equal(t, GitHubTriggerRoutingStatusOK, entry.Status)
	require.Len(t, entry.Projects, 2)
	assert.Equal(t, int64(2), repoAccessChecks.Load(), "second routing PUT inside rebuild interval must not trigger a second full rebuild")

	reportRR = doRepoRoutingAsUser(t, srv, admin, http.MethodGet, "/api/v1/github-app/installations/association-report", nil)
	require.Equal(t, http.StatusOK, reportRR.Code, "plain association-report GET after routing PUT must serve cached report (not 429): %s", reportRR.Body.String())
	assert.Equal(t, int64(2), repoAccessChecks.Load())

	srv.githubAssociationReportCache.mu.Lock()
	afterSecondPutBuildAt := srv.githubAssociationReportCache.lastFullBuild
	srv.githubAssociationReportCache.mu.Unlock()
	assert.True(t, firstBuildAt.Equal(afterSecondPutBuildAt), "routing PUT must preserve lastFullBuild")

	// DELETE + GET immediately after also keep the cached association report valid (200 on association-report, no rebuild).
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodDelete, "/api/v1/github-app/repo-routing/Acme/Widgets", nil)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

	reportRR = doRepoRoutingAsUser(t, srv, admin, http.MethodGet, "/api/v1/github-app/installations/association-report", nil)
	require.Equal(t, http.StatusOK, reportRR.Code, "plain association-report GET after routing DELETE must serve cached report (not 429): %s", reportRR.Body.String())

	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodGet, "/api/v1/github-app/repo-routing?installation=7201", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var list []GitHubRepoRoutingEntry
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, GitHubTriggerRoutingStatusNone, list[0].Status)
	assert.Nil(t, list[0].PrimaryProjectID)
	require.Len(t, list[0].Projects, 2)
	assert.Equal(t, int64(2), repoAccessChecks.Load(), "DELETE + GET inside rebuild interval must not trigger another full rebuild")
}
