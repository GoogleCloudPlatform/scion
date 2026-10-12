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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/brokersettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// quotaSelectors are the quota administration selectors, all hub-only.
var quotaSelectors = []string{"quota:read", "quota:create", "quota:update", "quota:delete"}

// quotaTokenLimitBody is a valid limit definition create body.
func quotaTokenLimitBody(name string) createLimitDefinitionRequest {
	return createLimitDefinitionRequest{Name: name, ResourceType: "agent", Unit: "count", Description: "token test", DefaultValue: 4}
}

// TestQuotaAdminToken_ResolvesHubTarget requires every quota check to
// resolve to the hub scope through the hub-scoped quota target, and a hub
// token to be admitted on the quota routes with the matching selectors:
// quota:read for reads, and quota:read plus quota:create, quota:update or
// quota:delete for writes (the route guard checks quota.read for every
// method). A token without the write selector is refused, and so is one
// without quota:read.
func TestQuotaAdminToken_ResolvesHubTarget(t *testing.T) {
	quotaRoutes := 0
	for key, meta := range routeMetadataTable {
		if meta.Resource != "quota" {
			continue
		}
		quotaRoutes++
		assert.Equal(t, routeBearerTargetHubInstance, meta.BearerTarget, "%s: quota route guard target", key)
		target, evidence, ok := routeGuardTarget(meta)
		require.True(t, ok, key)
		assert.Equal(t, TargetScope{Kind: TargetScopeHub}, ResolveTargetScope(target, evidence), "%s: quota route guard target scope", key)
	}
	assert.Equal(t, 5, quotaRoutes, "quota admin routes in the route metadata table")
	assert.Equal(t, TargetScope{Kind: TargetScopeHub}, ResolveTargetScope(brokerSettingsTarget(), TargetScopeEvidence{}), "broker settings write target scope")

	srv, s := testServer(t)
	ctx := context.Background()
	super := hubConfigTokenUser(t, s, "qat-super", store.SystemRoleSuperAdmin)

	readKey := mintHubConfigToken(t, srv, super, hubBoundary(), "quota:read")
	for _, path := range []string{"/api/v1/admin/limits", "/api/v1/admin/usage"} {
		rec := doRequestWithToken(t, srv, readKey, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
	}

	// Create needs quota:read (route guard) and quota:create.
	rec := doRequestWithToken(t, srv, readKey, http.MethodPost, "/api/v1/admin/limits", quotaTokenLimitBody("qat_read_only"))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	createOnly := mintHubConfigToken(t, srv, super, hubBoundary(), "quota:create")
	rec = doRequestWithToken(t, srv, createOnly, http.MethodPost, "/api/v1/admin/limits", quotaTokenLimitBody("qat_create_only"))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	createKey := mintHubConfigToken(t, srv, super, hubBoundary(), "quota:read", "quota:create")
	rec = doRequestWithToken(t, srv, createKey, http.MethodPost, "/api/v1/admin/limits", quotaTokenLimitBody("qat_created"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var def store.LimitDefinition
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &def))
	limitPath := "/api/v1/admin/limits/" + def.ID

	rec = doRequestWithToken(t, srv, readKey, http.MethodGet, limitPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, readKey, http.MethodGet, limitPath+"/entitlements", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, readKey, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Update needs quota:update.
	update := map[string]interface{}{"name": "qat_created", "resourceType": "agent", "unit": "count", "defaultValue": 6}
	rec = doRequestWithToken(t, srv, createKey, http.MethodPut, limitPath, update)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	updateKey := mintHubConfigToken(t, srv, super, hubBoundary(), "quota:read", "quota:update")
	rec = doRequestWithToken(t, srv, updateKey, http.MethodPut, limitPath, update)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetLimitDefinition(ctx, def.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 6, got.DefaultValue)

	// Entitlement bindings follow the same rules: create needs quota:read
	// and quota:create, update quota:update, delete quota:delete.
	entitlement := map[string]interface{}{"subjectType": store.EntitlementSubjectUser, "subjectId": super, "scopeType": store.QuotaScopeSystem, "value": 2}
	rec = doRequestWithToken(t, srv, readKey, http.MethodPost, limitPath+"/entitlements", entitlement)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, createOnly, http.MethodPost, limitPath+"/entitlements", entitlement)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, createKey, http.MethodPost, limitPath+"/entitlements", entitlement)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var binding store.EntitlementBinding
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &binding))
	entitlementPath := "/api/v1/admin/entitlements/" + binding.ID

	rec = doRequestWithToken(t, srv, readKey, http.MethodGet, entitlementPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	entitlementUpdate := map[string]interface{}{"subjectType": store.EntitlementSubjectUser, "subjectId": super, "scopeType": store.QuotaScopeSystem, "value": 5}
	rec = doRequestWithToken(t, srv, createKey, http.MethodPut, entitlementPath, entitlementUpdate)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	updateOnly := mintHubConfigToken(t, srv, super, hubBoundary(), "quota:update")
	rec = doRequestWithToken(t, srv, updateOnly, http.MethodPut, entitlementPath, entitlementUpdate)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, updateKey, http.MethodPut, entitlementPath, entitlementUpdate)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	gotBinding, err := s.GetEntitlementBinding(ctx, binding.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 5, gotBinding.Value)

	deleteOnly := mintHubConfigToken(t, srv, super, hubBoundary(), "quota:delete")
	rec = doRequestWithToken(t, srv, deleteOnly, http.MethodDelete, entitlementPath, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, updateKey, http.MethodDelete, entitlementPath, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	deleteKey := mintHubConfigToken(t, srv, super, hubBoundary(), "quota:read", "quota:delete")
	rec = doRequestWithToken(t, srv, deleteKey, http.MethodDelete, entitlementPath, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err = s.GetEntitlementBinding(ctx, binding.ID)
	require.ErrorIs(t, err, store.ErrNotFound)

	// Delete needs quota:delete.
	rec = doRequestWithToken(t, srv, updateKey, http.MethodDelete, limitPath, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, deleteKey, http.MethodDelete, limitPath, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err = s.GetLimitDefinition(ctx, def.ID)
	require.ErrorIs(t, err, store.ErrNotFound)

	// A token with an unrelated selector reaches none of the routes.
	unrelated := mintHubConfigToken(t, srv, super, hubBoundary(), "project:read")
	rec = doRequestWithToken(t, srv, unrelated, http.MethodGet, "/api/v1/admin/limits", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestQuotaAdminToken_RequiresLiveAuthority requires a quota selector to be
// minted only by a user who holds the permission, and a minted token to be
// refused once its holder loses the permission: a hub member, whose hub
// membership carries quota.read, mints quota:read and reads limits, and
// cannot mint quota:create, quota:update or quota:delete; a user whose only
// quota.read grant is removed after mint is refused on the next request.
func TestQuotaAdminToken_RequiresLiveAuthority(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	member := hubConfigTokenUser(t, s, "qat-member", store.SystemRoleHubMember)

	key := mintHubConfigToken(t, srv, member, hubBoundary(), "quota:read")
	rec := doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/admin/limits", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	for _, sel := range []string{"quota:create", "quota:update", "quota:delete"} {
		_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(member), CreateTokenParams{
			UserID: member, Name: "qat-" + tid("live"), Boundary: hubBoundary(), Scopes: []string{"quota:read", sel},
		})
		require.Error(t, err, "a hub member must not mint %s", sel)
	}

	// The grant is the user's only source of quota.read (no hub
	// membership), so removing it leaves the token without live authority.
	granted := newPlainUser(t, s, "qat-live-granted")
	grantUserActionOnResource(t, s, granted.ID, "quota", "hub", ActionRead)
	grantedKey := mintHubConfigToken(t, srv, granted.ID, hubBoundary(), "quota:read")
	rec = doRequestWithToken(t, srv, grantedKey, http.MethodGet, "/api/v1/admin/limits", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, granted.ID)
	require.NoError(t, err)
	require.Len(t, bindings, 1, "the quota.read grant is the user's only binding")
	require.NoError(t, s.DeleteRoleBinding(ctx, bindings[0].ID))

	rec = doRequestWithToken(t, srv, grantedKey, http.MethodGet, "/api/v1/admin/limits", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestQuotaAdminToken_ProjectBoundaryDenied requires the quota selectors to
// be hub-only: a super-admin who owns a project mints each on a hub token
// but not on a project token, and a project token reaches no quota route.
func TestQuotaAdminToken_ProjectBoundaryDenied(t *testing.T) {
	srv, s := testServer(t)
	project := tid("qat-project")
	require.NoError(t, s.CreateProject(context.Background(), &store.Project{ID: project, Name: "QAT", Slug: "qat-project"}))
	super := hubConfigTokenUser(t, s, "qat-pb-super", store.SystemRoleSuperAdmin)
	createTestUserWithProjectRole(t, s, super, super+"@test.com", project, store.ProjectRoleOwner)

	for _, sel := range quotaSelectors {
		_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(super), CreateTokenParams{
			UserID: super, Name: "qat-" + tid("pb"), Boundary: projectBoundary(project), Scopes: []string{sel},
		})
		require.Error(t, err, "a project token with %s must not mint", sel)
		mintHubConfigToken(t, srv, super, hubBoundary(), sel)
	}

	limit, err := s.CreateLimitDefinition(context.Background(), &store.LimitDefinition{
		Name: "qat_pb_limit", ResourceType: "agent", Unit: "count", Description: "token test", DefaultValue: 3,
	})
	require.NoError(t, err)
	broker := newBrokerSettingsTestBroker(t, s, "qat-pb", super)

	// A project-boundary credential that does carry quota:read is refused
	// by the boundary, before the ceiling: the hub-scoped quota target is
	// outside every project boundary.
	ceiling := bearerCeiling(t, "quota:read")
	eval := srv.authzService.EvaluateBearerCeiling(context.Background(), activeUserPrincipal(super), projectBoundary(project), ceiling, "quota.read", hubScopedResource("quota", "hub"), BearerOptions{})
	assert.False(t, eval.Decision.Allowed)
	assert.Equal(t, BearerStageOutsideBoundary, eval.Stage, eval.Decision.Reason)
	scoped := NewScopedUserIdentityWithBoundary(NewAuthenticatedUser(super, super+"@test.com", "Super", "admin", string(ClientTypeAPI)),
		projectBoundary(project), []string{"quota:read"}, "qat-pb-scoped", ceiling)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/limits", nil).WithContext(contextWithIdentity(context.Background(), scoped))
	boundaryRec := httptest.NewRecorder()
	srv.guarded("/api/v1/admin/limits", srv.handleAdminLimits)(boundaryRec, req)
	require.Equal(t, http.StatusForbidden, boundaryRec.Code, boundaryRec.Body.String())

	key := mintHubConfigToken(t, srv, super, projectBoundary(project), "project:read")
	for _, tc := range []struct {
		method, path string
		body         interface{}
	}{
		{http.MethodGet, "/api/v1/admin/limits", nil},
		{http.MethodPost, "/api/v1/admin/limits", quotaTokenLimitBody("qat_pb_created")},
		{http.MethodGet, "/api/v1/admin/limits/" + limit.ID, nil},
		{http.MethodGet, "/api/v1/admin/limits/" + limit.ID + "/entitlements", nil},
		{http.MethodDelete, "/api/v1/admin/limits/" + limit.ID, nil},
		{http.MethodGet, "/api/v1/admin/usage", nil},
		{http.MethodGet, "/api/v1/admin/usage/" + limit.ID, nil},
		{http.MethodPut, settingsPath(broker.ID), map[string]interface{}{"settings": map[string]interface{}{"maxAgents": 2}, "expectedRevision": 0}},
	} {
		rec := doRequestWithToken(t, srv, key, tc.method, tc.path, tc.body)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", tc.method, tc.path, rec.Body.String())
	}
	_, err = s.GetLimitDefinition(context.Background(), limit.ID)
	require.NoError(t, err, "a refused delete leaves the limit")
	_, err = s.GetBrokerSettings(context.Background(), broker.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "a refused settings write creates no row")
}

// TestBrokerSettingsPut_RequiresQuotaUpdateEvenWithoutChanges requires
// quota.update for every broker settings PUT, including one that changes no
// key, on top of broker.read on the broker. A refused no-op PUT creates no
// settings row; an admitted one returns 200 and also creates none.
func TestBrokerSettingsPut_RequiresQuotaUpdateEvenWithoutChanges(t *testing.T) {
	srv, s := testServer(t)
	noop := map[string]interface{}{"settings": map[string]interface{}{}, "expectedRevision": 0}
	requireNoRow := func(t *testing.T, brokerID string) {
		t.Helper()
		_, err := s.GetBrokerSettings(context.Background(), brokerID)
		require.ErrorIs(t, err, store.ErrNotFound, "a no-op PUT creates no settings row")
	}

	t.Run("session", func(t *testing.T) {
		owner := newPlainUser(t, s, "bsq-owner")
		broker := newBrokerSettingsTestBroker(t, s, "bsq-owner", owner.ID)
		rec := doRequestAsUser(t, srv, owner, http.MethodPut, settingsPath(broker.ID), noop)
		require.Equal(t, http.StatusForbidden, rec.Code, "a broker owner without quota.update: %s", rec.Body.String())
		requireNoRow(t, broker.ID)

		granted := newPlainUser(t, s, "bsq-granted")
		grantUserActionOnResource(t, s, granted.ID, "quota", "hub", ActionUpdate)
		grantedBroker := newBrokerSettingsTestBroker(t, s, "bsq-granted", granted.ID)
		rec = doRequestAsUser(t, srv, granted, http.MethodPut, settingsPath(grantedBroker.ID), noop)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		requireNoRow(t, grantedBroker.ID)
	})

	t.Run("token", func(t *testing.T) {
		super := hubConfigTokenUser(t, s, "bsq-super", store.SystemRoleSuperAdmin)
		broker := newBrokerSettingsTestBroker(t, s, "bsq-token", "")
		for _, scopes := range [][]string{{"broker:read"}, {"quota:update"}, {"broker:read", "quota:read"}} {
			key := mintHubConfigToken(t, srv, super, hubBoundary(), scopes...)
			rec := doRequestWithToken(t, srv, key, http.MethodPut, settingsPath(broker.ID), noop)
			require.Equal(t, http.StatusForbidden, rec.Code, "a token with %v: %s", scopes, rec.Body.String())
			requireNoRow(t, broker.ID)
		}
		key := mintHubConfigToken(t, srv, super, hubBoundary(), "broker:read", "quota:update")
		rec := doRequestWithToken(t, srv, key, http.MethodPut, settingsPath(broker.ID), noop)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		requireNoRow(t, broker.ID)
	})
}

// TestBrokerSettingsPut_EachChangedKeyIsAuthorized requires a broker
// settings write to hold the declared permission of every key it changes,
// each checked on the hub-scoped quota target: a hub token with broker:read
// and quota:update changes maxAgents, a token without quota:update is
// refused and changes nothing, and a changed key that declares another
// permission is refused until the caller holds that permission too.
func TestBrokerSettingsPut_EachChangedKeyIsAuthorized(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	super := hubConfigTokenUser(t, s, "bsk-super", store.SystemRoleSuperAdmin)
	broker := newBrokerSettingsTestBroker(t, s, "bsk-token", "")
	set := func(v int64, rev int64) map[string]interface{} {
		return map[string]interface{}{"settings": map[string]interface{}{"maxAgents": v}, "expectedRevision": rev}
	}

	refused := mintHubConfigToken(t, srv, super, hubBoundary(), "broker:read", "quota:read")
	rec := doRequestWithToken(t, srv, refused, http.MethodPut, settingsPath(broker.ID), set(3, 0))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	_, err := s.GetBrokerSettings(ctx, broker.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "a refused write creates no settings row")

	key := mintHubConfigToken(t, srv, super, hubBoundary(), "broker:read", "quota:update")
	rec = doRequestWithToken(t, srv, key, http.MethodPut, settingsPath(broker.ID), set(3, 0))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp BrokerSettingsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Capabilities.Update, "a token with quota:update reports the update capability")
	got, err := s.GetBrokerSettings(ctx, broker.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Settings.MaxAgents)
	assert.EqualValues(t, 3, *got.Settings.MaxAgents)

	// Clearing the key is a change too.
	rec = doRequestWithToken(t, srv, refused, http.MethodPut, settingsPath(broker.ID), map[string]interface{}{"settings": map[string]interface{}{}, "expectedRevision": 1})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	got, err = s.GetBrokerSettings(ctx, broker.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Settings.MaxAgents, "a refused clear leaves the value")

	// Every changed key's declared permission is checked, after quota.update.
	user := newPlainUser(t, s, "bsk-user")
	grantUserActionOnResource(t, s, user.ID, "quota", "hub", ActionUpdate)
	otherKey := brokersettings.KeyDef{Name: "otherKey", Permission: "quota.delete"}
	authorize := func(changed []brokersettings.KeyDef) int {
		identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, "member", string(ClientTypeWeb))
		reqCtx := contextWithCredentialContext(contextWithIdentity(ctx, identity), CredentialContext{Kind: CredentialKindInteractive, ID: "bsk-session"})
		req := httptest.NewRequest(http.MethodPut, settingsPath(broker.ID), nil).WithContext(reqCtx)
		w := httptest.NewRecorder()
		if srv.authorizeBrokerSettingsWrite(w, req, changed) {
			return http.StatusOK
		}
		return w.Code
	}
	assert.Equal(t, http.StatusOK, authorize(nil), "no changed key needs quota.update only")
	assert.Equal(t, http.StatusOK, authorize([]brokersettings.KeyDef{brokersettings.MaxAgents}))
	assert.Equal(t, http.StatusForbidden, authorize([]brokersettings.KeyDef{brokersettings.MaxAgents, otherKey}),
		"a changed key whose declared permission the caller lacks is refused")
	grantUserActionOnResource(t, s, user.ID, "quota", "hub", ActionDelete)
	assert.Equal(t, http.StatusOK, authorize([]brokersettings.KeyDef{brokersettings.MaxAgents, otherKey}))
}

// TestUsageMe_ReturnsOnlyTheHolder requires the caller's own quota usage to
// count only the caller's reservations, for a session and for a token with
// an unrelated selector on either boundary.
func TestUsageMe_ReturnsOnlyTheHolder(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := tid("um-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: project, Name: "UM", Slug: "um-project"}))

	limit, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name: "um_holder_limit", ResourceType: "agent", Unit: "count", Description: "token test", DefaultValue: 9,
	})
	require.NoError(t, err)

	holder := hubConfigTokenUser(t, s, "um-holder", store.SystemRoleHubMember)
	createTestUserWithProjectRole(t, s, holder, holder+"@test.com", project, store.ProjectRoleMember)
	other := hubConfigTokenUser(t, s, "um-other", store.SystemRoleHubMember)
	for i, owner := range []string{holder, holder, other} {
		_, err := srv.quotaService.Reserve(ctx, limit.Name, owner, store.QuotaScopeSystem, "", tid("um-res-"+string(rune('a'+i))))
		require.NoError(t, err)
	}

	usageOf := func(t *testing.T, rec *httptest.ResponseRecorder) int64 {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp myUsageResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		for _, entry := range resp.Items {
			if entry.LimitDefinition.ID == limit.ID {
				return entry.Current
			}
		}
		t.Fatalf("limit %s missing from usage/me: %s", limit.ID, rec.Body.String())
		return 0
	}

	holderUser, err := s.GetUser(ctx, holder)
	require.NoError(t, err)
	otherUser, err := s.GetUser(ctx, other)
	require.NoError(t, err)
	assert.EqualValues(t, 2, usageOf(t, doRequestAsUser(t, srv, holderUser, http.MethodGet, "/api/v1/usage/me", nil)))
	assert.EqualValues(t, 1, usageOf(t, doRequestAsUser(t, srv, otherUser, http.MethodGet, "/api/v1/usage/me", nil)))

	hubKey := mintHubConfigToken(t, srv, holder, hubBoundary(), "user:read")
	assert.EqualValues(t, 2, usageOf(t, doRequestWithToken(t, srv, hubKey, http.MethodGet, "/api/v1/usage/me", nil)))
	projectKey := mintHubConfigToken(t, srv, holder, projectBoundary(project), "project:read")
	assert.EqualValues(t, 2, usageOf(t, doRequestWithToken(t, srv, projectKey, http.MethodGet, "/api/v1/usage/me", nil)))
	otherKey := mintHubConfigToken(t, srv, other, hubBoundary(), "group:read")
	assert.EqualValues(t, 1, usageOf(t, doRequestWithToken(t, srv, otherKey, http.MethodGet, "/api/v1/usage/me", nil)))
}
