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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setGitHubTriggersExperimentForRouting(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.GitHubTriggers {
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

	// 3. Initial GET lists candidate repositories with status "none" and supports ?installation= filter.
	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodGet, "/api/v1/github-app/repo-routing?installation=5001", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var list []GitHubRepoRoutingEntry
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, instID, list[0].InstallationID)
	assert.Equal(t, "acme/widgets", list[0].Repo)
	assert.Equal(t, GitHubTriggerRoutingStatusNone, list[0].Status)
	assert.Empty(t, list[0].PrimaryProjectID)
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
	assert.Equal(t, projB, putResp.PrimaryProjectID)
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

	// 6. When primary project loses repository candidacy (e.g. repo_not_accessible / disassociation),
	// GET reports status "stale_primary".
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

	rr = doRepoRoutingAsUser(t, srv, admin, http.MethodGet, "/api/v1/github-app/repo-routing?installation=5001", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, GitHubTriggerRoutingStatusStalePrimary, list[0].Status)
	assert.Equal(t, projB, list[0].PrimaryProjectID)
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
