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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func enableCustomDefaultRoleExperiment(t *testing.T, srv *Server, ops *OperationalSettings, enabled bool) {
	t.Helper()
	val := "false"
	if enabled {
		val = "true"
	}
	doc := json.RawMessage(`{"overrides":{"` + experiments.CustomDefaultUserRole + `":` + val + `}}`)
	if ops != nil {
		if _, err := ops.store.UpsertHubSetting(context.Background(), "experiments", doc, "test", -1, "managed"); err != nil {
			t.Fatalf("seed experiments setting: %v", err)
		}
		if _, err := ops.Refresh(context.Background()); err != nil {
			t.Fatalf("refresh ops: %v", err)
		}
		return
	}
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", doc)
	expOps := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := expOps.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh expOps: %v", err)
	}
	srv.SetOperationalSettings(expOps)
}

func seedCustomRoleDef(t *testing.T, st store.Store, name, scopeType string, perms []string) *store.RoleDefinition {
	t.Helper()
	rd, err := st.CreateRoleDefinition(context.Background(), &store.RoleDefinition{
		Name:        name,
		Description: "test role " + name,
		ScopeType:   scopeType,
		Permissions: perms,
		System:      false,
	})
	require.NoError(t, err)
	return rd
}

func TestResolveDefaultUserRole_Matrix(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	eligible := seedCustomRoleDef(t, st, "custom-restricted", store.RoleScopeSystem, []string{
		"user.read", "broker.read", "skill.create_global", "template.update_global",
	})
	_ = seedCustomRoleDef(t, st, "custom-proj", store.RoleScopeProject, []string{"agent.read"})
	_ = seedCustomRoleDef(t, st, "custom-skill-read", store.RoleScopeSystem, []string{"skill.read"})

	// Built-in values
	res := resolveDefaultUserRole(ctx, st, "", false, true)
	assert.Equal(t, store.UserRoleMember, res.BuiltinRole)
	assert.Nil(t, res.CustomRole)
	assert.Nil(t, res.Warning)

	res = resolveDefaultUserRole(ctx, st, "member", false, true)
	assert.Equal(t, store.UserRoleMember, res.BuiltinRole)
	assert.Nil(t, res.CustomRole)
	assert.Nil(t, res.Warning)

	res = resolveDefaultUserRole(ctx, st, "viewer", false, true)
	assert.Equal(t, store.UserRoleViewer, res.BuiltinRole)
	assert.Nil(t, res.CustomRole)
	assert.Nil(t, res.Warning)

	// Experiment off -> fallback with experiment_disabled warning
	res = resolveDefaultUserRole(ctx, st, "custom-restricted", false, true)
	assert.Equal(t, store.UserRoleMember, res.BuiltinRole)
	assert.Nil(t, res.CustomRole)
	require.NotNil(t, res.Warning)
	assert.Equal(t, DefaultUserRoleWarningExperimentDisabled, res.Warning.Code)
	assert.Equal(t, "custom-restricted", res.Warning.ConfiguredRole)
	assert.Equal(t, store.UserRoleMember, res.Warning.EffectiveRole)

	// Experiment on + eligible custom role -> resolves cleanly
	res = resolveDefaultUserRole(ctx, st, "custom-restricted", true, true)
	assert.Equal(t, store.UserRoleMember, res.BuiltinRole)
	require.NotNil(t, res.CustomRole)
	assert.Equal(t, eligible.ID, res.CustomRole.ID)
	assert.Nil(t, res.Warning)

	// Experiment on + missing role -> role_not_found warning
	res = resolveDefaultUserRole(ctx, st, "does-not-exist", true, true)
	assert.Equal(t, store.UserRoleMember, res.BuiltinRole)
	assert.Nil(t, res.CustomRole)
	require.NotNil(t, res.Warning)
	assert.Equal(t, DefaultUserRoleWarningRoleNotFound, res.Warning.Code)

	// Experiment on + project-scope role -> role_not_found (lookup is system scope)
	res = resolveDefaultUserRole(ctx, st, "custom-proj", true, true)
	assert.Equal(t, store.UserRoleMember, res.BuiltinRole)
	assert.Nil(t, res.CustomRole)
	require.NotNil(t, res.Warning)
	assert.Equal(t, DefaultUserRoleWarningRoleNotFound, res.Warning.Code)

	// Experiment on + built-in system role -> role_ineligible
	res = resolveDefaultUserRole(ctx, st, store.SystemRoleHubAdmin, true, true)
	assert.Equal(t, store.UserRoleMember, res.BuiltinRole)
	assert.Nil(t, res.CustomRole)
	require.NotNil(t, res.Warning)
	assert.Equal(t, DefaultUserRoleWarningRoleIneligible, res.Warning.Code)

	// Experiment on + #7779 catalog permission -> role_ineligible
	res = resolveDefaultUserRole(ctx, st, "custom-skill-read", true, true)
	assert.Equal(t, store.UserRoleMember, res.BuiltinRole)
	assert.Nil(t, res.CustomRole)
	require.NotNil(t, res.Warning)
	assert.Equal(t, DefaultUserRoleWarningRoleIneligible, res.Warning.Code)

	_ = srv
}

func TestValidateDefaultUserRoleSetting_All15UnsupportedCatalogPermsRejected(t *testing.T) {
	_, st := testServer(t)
	ctx := context.Background()

	unsupported := []string{
		"skill.read", "skill.list", "skill.create", "skill.update", "skill.delete",
		"template.read", "template.list", "template.create", "template.update", "template.delete",
		"harness_config.read", "harness_config.list", "harness_config.create", "harness_config.update", "harness_config.delete",
	}
	assert.Len(t, unsupportedDefaultUserRolePermissions, len(unsupported))

	for _, perm := range unsupported {
		t.Run("reject_"+perm, func(t *testing.T) {
			name := "bad-" + strings.ReplaceAll(perm, ".", "-")
			_ = seedCustomRoleDef(t, st, name, store.RoleScopeSystem, []string{"user.read", perm})
			rd, err := validateDefaultUserRoleSetting(ctx, st, name)
			assert.Nil(t, rd)
			require.Error(t, err)
			assert.Contains(t, err.Error(), perm)
		})
	}

	// Global write twins ARE permitted
	globalTwins := []string{
		"skill.create_global", "skill.update_global", "skill.delete_global",
		"template.create_global", "template.update_global", "template.delete_global",
	}
	_ = seedCustomRoleDef(t, st, "global-writer", store.RoleScopeSystem, globalTwins)
	rd, err := validateDefaultUserRoleSetting(ctx, st, "global-writer")
	require.NoError(t, err)
	require.NotNil(t, rd)
	assert.Equal(t, "global-writer", rd.Name)

	// Built-in system roles and "admin" are rejected
	for _, builtin := range []string{
		store.UserRoleAdmin,
		store.SystemRoleSuperAdmin,
		store.SystemRoleHubAdmin,
		store.SystemRoleHubMember,
		store.SystemRoleHubViewer,
		store.SystemRoleGlobalCatalogAuthor,
	} {
		t.Run("reject_builtin_"+builtin, func(t *testing.T) {
			rd, err := validateDefaultUserRoleSetting(ctx, st, builtin)
			assert.Nil(t, rd)
			require.Error(t, err)
		})
	}
}

func TestDefaultUserRole_DBMode_ExperimentGate_Validation_Warning_And_Echo(t *testing.T) {
	srv, st := testServer(t)
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)

	// Seed an eligible custom role, an ineligible (#7779) custom role, and a project-scope role.
	_ = seedCustomRoleDef(t, st, "custom-basic", store.RoleScopeSystem, []string{"user.read", "broker.read"})
	_ = seedCustomRoleDef(t, st, "custom-unfiltered", store.RoleScopeSystem, []string{"template.read"})
	_ = seedCustomRoleDef(t, st, "custom-project", store.RoleScopeProject, []string{"agent.read"})

	// 1. Experiment OFF: saving a custom role returns 412 experiment_disabled.
	rr := putServerConfigDB(t, srv, ops, `{"server":{"auth":{"default_user_role":"custom-basic"}}}`)
	require.Equal(t, http.StatusPreconditionFailed, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), ErrCodeExperimentDisabled)

	// 2. Experiment OFF + pre-existing stored custom role:
	//    - GET reports experiment_disabled warning and omits eligible_default_roles.
	//    - Echoing the stored custom value passes (200).
	//    - Resetting to "member" passes (200).
	_, err = st.UpsertHubSetting(context.Background(), "access", json.RawMessage(`{"default_user_role":"custom-basic"}`), "test", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)

	getRec := httptest.NewRecorder()
	srv.handleGetServerConfigDB(getRec, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var getResp ServerConfigDBResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &getResp))
	require.NotNil(t, getResp.DefaultUserRoleWarning)
	assert.Equal(t, DefaultUserRoleWarningExperimentDisabled, getResp.DefaultUserRoleWarning.Code)
	assert.Empty(t, getResp.EligibleDefaultRoles)

	echoRec := putServerConfigDB(t, srv, ops, `{"server":{"auth":{"default_user_role":"custom-basic","user_access_mode":"open"}}}`)
	require.Equal(t, http.StatusOK, echoRec.Code, echoRec.Body.String())

	resetRec := putServerConfigDB(t, srv, ops, `{"server":{"auth":{"default_user_role":"member"}}}`)
	require.Equal(t, http.StatusOK, resetRec.Code, resetRec.Body.String())

	// 3. Enable experiment:
	enableCustomDefaultRoleExperiment(t, srv, ops, true)

	// Rejects nonexistent, built-in, project-scope, and #7779 roles with 422.
	for _, bad := range []string{"nonexistent", "admin", "hub-admin", "hub-member", "custom-project", "custom-unfiltered"} {
		t.Run("db_reject_"+bad, func(t *testing.T) {
			rec := putServerConfigDB(t, srv, ops, `{"server":{"auth":{"default_user_role":"`+bad+`"}}}`)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "server.auth.default_user_role")
		})
	}

	// Accepts eligible custom role (200) and updates live snapshot.
	okRec := putServerConfigDB(t, srv, ops, `{"server":{"auth":{"default_user_role":"custom-basic"}}}`)
	require.Equal(t, http.StatusOK, okRec.Code, okRec.Body.String())
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "custom-basic", ops.Snapshot().DefaultUserRole)

	// GET returns no warning and lists only eligible custom system-scope roles.
	getRec = httptest.NewRecorder()
	srv.handleGetServerConfigDB(getRec, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	getResp = ServerConfigDBResponse{}
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &getResp))
	assert.Nil(t, getResp.DefaultUserRoleWarning)
	require.Len(t, getResp.EligibleDefaultRoles, 1)
	assert.Equal(t, "custom-basic", getResp.EligibleDefaultRoles[0].Name)

	// Echoing the full GET body (including eligible_default_roles) passes echo validation.
	fullEchoRec := putServerConfigDB(t, srv, ops, getRec.Body.String())
	assert.Equal(t, http.StatusOK, fullEchoRec.Code, fullEchoRec.Body.String())

	// Delete the custom role -> GET reports role_not_found warning.
	rd, err := st.GetRoleDefinitionByName(context.Background(), "custom-basic", store.RoleScopeSystem)
	require.NoError(t, err)
	require.NoError(t, st.DeleteRoleDefinition(context.Background(), rd.ID))

	getRec = httptest.NewRecorder()
	srv.handleGetServerConfigDB(getRec, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	getResp = ServerConfigDBResponse{}
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &getResp))
	require.NotNil(t, getResp.DefaultUserRoleWarning)
	assert.Equal(t, DefaultUserRoleWarningRoleNotFound, getResp.DefaultUserRoleWarning.Code)
}

func TestDefaultUserRole_CanDelegate_DBAndFileModes(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Create a custom role with a permission ("broker.create") that our limited admin does NOT hold.
	_ = seedCustomRoleDef(t, st, "custom-with-broker", store.RoleScopeSystem, []string{"broker.create"})

	// Create a limited admin user who holds hub.config.read and hub.config.update, but NOT broker.create.
	limitedRole := seedCustomRoleDef(t, st, "config-only-admin", store.RoleScopeSystem, []string{
		"hub.config.read", "hub.config.update",
	})
	limitedUser := &store.User{
		ID:          "00000000-0000-0000-0000-000000000042",
		Email:       "config-only@example.com",
		DisplayName: "Config Only",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, st.CreateUser(ctx, limitedUser))
	_, err := st.CreateRoleBinding(ctx, &store.RoleBinding{
		PrincipalType:    "user",
		PrincipalID:      limitedUser.ID,
		RoleDefinitionID: limitedRole.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.AdminAPICreatedBy,
	})
	require.NoError(t, err)
	limitedIdentity := NewAuthenticatedUser(limitedUser.ID, limitedUser.Email, limitedUser.DisplayName, limitedUser.Role, "web")

	// DB mode: enable experiment and test CanDelegate denial for limitedIdentity.
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	enableCustomDefaultRoleExperiment(t, srv, ops, true)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/server-config", strings.NewReader(`{"server":{"auth":{"default_user_role":"custom-with-broker"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithCredentialContext(contextWithIdentity(req.Context(), limitedIdentity), CredentialContext{Kind: CredentialKindInteractive}))
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "cannot set server.auth.default_user_role")

	// File mode: same CanDelegate denial for limitedIdentity.
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))

	fileReq := httptest.NewRequest(http.MethodPut, "/api/v1/admin/server-config", strings.NewReader(`{"server":{"auth":{"default_user_role":"custom-with-broker"}}}`))
	fileReq.Header.Set("Content-Type", "application/json")
	fileReq = fileReq.WithContext(contextWithCredentialContext(contextWithIdentity(fileReq.Context(), limitedIdentity), CredentialContext{Kind: CredentialKindInteractive}))
	fileRec := httptest.NewRecorder()
	srv.handlePutServerConfig(fileRec, fileReq)
	require.Equal(t, http.StatusForbidden, fileRec.Code, fileRec.Body.String())
	assert.Contains(t, fileRec.Body.String(), "cannot set server.auth.default_user_role")
}

func TestDefaultUserRole_FileMode_CustomRole_Save_Get_And_Echo(t *testing.T) {
	srv, st := testServer(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))

	_ = seedCustomRoleDef(t, st, "custom-file-role", store.RoleScopeSystem, []string{"user.read"})
	enableCustomDefaultRoleExperiment(t, srv, nil, true)

	// Save eligible custom role in file mode.
	putReq := adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server":{"auth":{"default_user_role":"custom-file-role"}}}`)
	putRec := httptest.NewRecorder()
	srv.handlePutServerConfig(putRec, putReq)
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	// GET returns custom role, no warning, and eligible_default_roles.
	getRec := httptest.NewRecorder()
	srv.handleGetServerConfig(getRec)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var resp ServerConfigResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Server)
	require.NotNil(t, resp.Server.Auth)
	assert.Equal(t, "custom-file-role", resp.Server.Auth.DefaultUserRole)
	assert.Nil(t, resp.DefaultUserRoleWarning)
	require.Len(t, resp.EligibleDefaultRoles, 1)
	assert.Equal(t, "custom-file-role", resp.EligibleDefaultRoles[0].Name)

	// Turn experiment off -> GET shows experiment_disabled warning, and echoing the GET body still passes!
	enableCustomDefaultRoleExperiment(t, srv, nil, false)
	getRec2 := httptest.NewRecorder()
	srv.handleGetServerConfig(getRec2)
	require.Equal(t, http.StatusOK, getRec2.Code, getRec2.Body.String())
	var resp2 ServerConfigResponse
	require.NoError(t, json.Unmarshal(getRec2.Body.Bytes(), &resp2))
	require.NotNil(t, resp2.DefaultUserRoleWarning)
	assert.Equal(t, DefaultUserRoleWarningExperimentDisabled, resp2.DefaultUserRoleWarning.Code)
	assert.Empty(t, resp2.EligibleDefaultRoles)

	echoReq := adminRequest(http.MethodPut, "/api/v1/admin/server-config", getRec2.Body.String())
	echoRec := httptest.NewRecorder()
	srv.handlePutServerConfig(echoRec, echoReq)
	require.Equal(t, http.StatusOK, echoRec.Code, echoRec.Body.String())
}

func TestUpdateRoleDefinition_GuardsActiveDefaultUserRole(t *testing.T) {
	srv, st := testServer(t)
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	enableCustomDefaultRoleExperiment(t, srv, ops, true)

	activeRole := seedCustomRoleDef(t, st, "active-default", store.RoleScopeSystem, []string{"user.read"})
	otherRole := seedCustomRoleDef(t, st, "other-custom", store.RoleScopeSystem, []string{"user.read"})
	srv.config.DefaultUserRole = "active-default"

	// Adding an unsupported #7779 permission to the active default_user_role is rejected with 400.
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/roles/"+activeRole.ID, updateRoleDefinitionRequest{
		Name:        "active-default",
		Description: "now tries to add skill.read",
		Permissions: []string{"user.read", "skill.read"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "server.auth.default_user_role")
	assert.Contains(t, rec.Body.String(), "skill.read")

	// Updating the active default_user_role with eligible permissions (including *_global) succeeds.
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/admin/roles/"+activeRole.ID, updateRoleDefinitionRequest{
		Name:        "active-default",
		Description: "eligible update",
		Permissions: []string{"user.read", "template.create_global"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Adding skill.read to an unrelated custom role still succeeds.
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/admin/roles/"+otherRole.ID, updateRoleDefinitionRequest{
		Name:        "other-custom",
		Description: "not the default role",
		Permissions: []string{"user.read", "skill.read"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestApplySnapshot_DefaultUserRole_PreservedWhenExperimentEnabled(t *testing.T) {
	srv := &Server{
		config:      ServerConfig{DefaultUserRole: "viewer"},
		maintenance: NewMaintenanceState(false, ""),
	}
	enableCustomDefaultRoleExperiment(t, srv, nil, true)

	ApplySnapshot(srv, Layer1Snapshot{DefaultUserRole: "custom-default"})
	assert.Equal(t, "custom-default", srv.config.DefaultUserRole)
	assert.Equal(t, "custom-default", srv.DefaultUserRole())

	// Clearing still clears to "" (DefaultUserRole() falls back to "member").
	ApplySnapshot(srv, Layer1Snapshot{DefaultUserRole: ""})
	assert.Equal(t, "", srv.config.DefaultUserRole)
	assert.Equal(t, store.UserRoleMember, srv.DefaultUserRole())
}

func TestValidateDefaultUserRoleSave_NilIdentityFailsClosed(t *testing.T) {
	srv, st := testServer(t)
	require.NotNil(t, srv.authzService)

	_ = seedCustomRoleDef(t, st, "custom-nil-actor", store.RoleScopeSystem, []string{"user.read"})
	enableCustomDefaultRoleExperiment(t, srv, nil, true)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/server-config", nil)
	rr := httptest.NewRecorder()

	ok := srv.validateDefaultUserRoleSave(rr, req, "custom-nil-actor", "member")
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "unauthenticated")
}

type roleLookupErrorStore struct {
	store.Store
	err error
}

func (s *roleLookupErrorStore) GetRoleDefinitionByName(_ context.Context, _, _ string) (*store.RoleDefinition, error) {
	return nil, s.err
}

func TestResolveDefaultUserRole_LookupErrorDistinctFromNotFound(t *testing.T) {
	_, st := testServer(t)
	faultStore := &roleLookupErrorStore{
		Store: st,
		err:   errors.New("transient database timeout"),
	}

	res := resolveDefaultUserRole(context.Background(), faultStore, "custom-role", true, true)
	assert.Equal(t, store.UserRoleMember, res.BuiltinRole)
	assert.Nil(t, res.CustomRole)
	require.NotNil(t, res.Warning)
	assert.Equal(t, DefaultUserRoleWarningLookupError, res.Warning.Code)
	assert.NotEqual(t, DefaultUserRoleWarningRoleNotFound, res.Warning.Code)
	assert.Equal(t, "custom-role", res.Warning.ConfiguredRole)
	assert.Equal(t, store.UserRoleMember, res.Warning.EffectiveRole)
}
