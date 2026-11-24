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
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	systemoneprovider "github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

func doProjectSystemOneBearerReq(t *testing.T, srv *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestProjectSystemOneInstances_EmptyByDefaultInHostedAndWorkstationModes(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name        string
		workstation bool
	}{
		{name: "hosted", workstation: false},
		{name: "workstation", workstation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _ := newSystemOneTestServer(t, true)
			srv.config.Workstation = tc.workstation

			admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-def-admin-"+tc.name, store.SystemRoleSuperAdmin))
			require.NoError(t, err)

			proj := &store.Project{
				ID:      uuid.NewString(),
				Name:    "Default Empty " + tc.name,
				Slug:    "default-empty-" + tc.name,
				OwnerID: admin.ID,
			}
			require.NoError(t, s.CreateProject(ctx, proj))

			// Store returns empty list by default.
			list, err := s.GetProjectSystemOneInstances(ctx, proj.ID)
			require.NoError(t, err)
			assert.Empty(t, list)

			// API returns empty JSON array [] by default.
			rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/system-one/instances", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp ProjectSystemOneInstancesResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, proj.ID, resp.ProjectID)
			assert.Empty(t, resp.Instances)
		})
	}
}

func TestProjectSystemOneInstances_AuthzMatrixAndExperimentGate(t *testing.T) {
	ctx := context.Background()
	srv, s, _ := newSystemOneTestServer(t, true)

	// Create users for every role in the authz matrix.
	superAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-authz-super", store.SystemRoleSuperAdmin))
	require.NoError(t, err)
	hubAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-authz-hubadmin", store.SystemRoleHubAdmin))
	require.NoError(t, err)
	owner, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-authz-owner", store.SystemRoleHubMember))
	require.NoError(t, err)
	projAdmin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-authz-projadmin", store.SystemRoleHubMember))
	require.NoError(t, err)
	member, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-authz-member", store.SystemRoleHubMember))
	require.NoError(t, err)
	otherOwner, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-authz-other-owner", store.SystemRoleHubMember))
	require.NoError(t, err)
	customRoleUser, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-authz-custom", store.SystemRoleHubMember))
	require.NoError(t, err)

	proj := &store.Project{
		ID:      uuid.NewString(),
		Name:    "System One Authz Matrix",
		Slug:    "s1-authz-matrix",
		OwnerID: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	otherProj := &store.Project{
		ID:      uuid.NewString(),
		Name:    "Other Project",
		Slug:    "s1-authz-other",
		OwnerID: otherOwner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, otherProj))

	// Seed hub System One instances so allowlist mutations pass existence validation.
	for _, instName := range []string{
		"inst-owner", "inst-projadmin", "inst-hubadmin", "inst-superadmin",
		"inst-uat", "inst-uat-hub", "inst-uat-denied", "inst-x", "inst-agent",
	} {
		require.NoError(t, s.CreateSystemOneInstance(ctx, &store.SystemOneInstance{
			ID:       uuid.NewString(),
			Name:     instName,
			Adapter:  "typesafe",
			Model:    "jev-1.14.0",
			Endpoint: "https://api.typesafe.ai",
			Auth:     store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindAPIKey, SecretName: "s1-sec-" + instName},
			Enabled:  true,
		}))
	}

	// Bind project roles.
	bindRole := func(targetProjID string, u *store.User, roleName string) {
		role, rErr := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
		require.NoError(t, rErr)
		_, rbErr := s.CreateRoleBinding(ctx, &store.RoleBinding{
			ID:               uuid.NewString(),
			RoleDefinitionID: role.ID,
			PrincipalType:    "user",
			PrincipalID:      u.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          targetProjID,
		})
		require.NoError(t, rbErr)
	}
	bindRole(proj.ID, owner, store.ProjectRoleOwner)
	bindRole(proj.ID, projAdmin, store.ProjectRoleAdmin)
	bindRole(proj.ID, member, store.ProjectRoleMember)
	bindRole(otherProj.ID, otherOwner, store.ProjectRoleOwner)

	// Create a custom project role that grants project.manage_system_one_instances
	// without being project-owner or project-admin, to verify the owner/admin rule.
	customRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		ID:          uuid.NewString(),
		Name:        "custom-s1-manager",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read", "project.manage_system_one_instances"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               uuid.NewString(),
		RoleDefinitionID: customRole.ID,
		PrincipalType:    "user",
		PrincipalID:      customRoleUser.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
	})
	require.NoError(t, err)

	basePath := "/api/v1/projects/" + proj.ID + "/system-one/instances"

	// 1. Experiment OFF -> 404 on GET, collection PUT, item PUT, DELETE even for super-admin.
	setSystemOneExperimentLive(t, srv, false)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, superAdmin, http.MethodGet, basePath, nil).Code)
	emptyList := []string{}
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, superAdmin, http.MethodPut, basePath, SetProjectSystemOneInstancesRequest{Instances: &emptyList}).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, superAdmin, http.MethodPut, basePath+"/inst-1", nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, superAdmin, http.MethodDelete, basePath+"/inst-1", nil).Code)
	setSystemOneExperimentLive(t, srv, true)

	// 2. Owner, project-admin, hub-admin, super-admin CAN add/remove/set.
	for _, actor := range []struct {
		name string
		user *store.User
		inst string
	}{
		{"owner", owner, "inst-owner"},
		{"project-admin", projAdmin, "inst-projadmin"},
		{"hub-admin", hubAdmin, "inst-hubadmin"},
		{"super-admin", superAdmin, "inst-superadmin"},
	} {
		t.Run("allowed_"+actor.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, actor.user, http.MethodPut, basePath+"/"+actor.inst, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp ProjectSystemOneInstancesResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Contains(t, resp.Instances, actor.inst)

			delRec := doRequestAsUser(t, srv, actor.user, http.MethodDelete, basePath+"/"+actor.inst, nil)
			require.Equal(t, http.StatusOK, delRec.Code, delRec.Body.String())
		})
	}

	// 3. Project member CAN read (project.read), CANNOT add/remove/set.
	assert.Equal(t, http.StatusOK, doRequestAsUser(t, srv, member, http.MethodGet, basePath, nil).Code)
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, member, http.MethodPut, basePath+"/inst-x", nil).Code)
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, member, http.MethodDelete, basePath+"/inst-x", nil).Code)
	instX := []string{"inst-x"}
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, member, http.MethodPut, basePath, SetProjectSystemOneInstancesRequest{Instances: &instX}).Code)

	// 4. Scoped UAT: WITH project:manage_system_one_instances allowed; WITHOUT scope denied.
	uatWithScope := projectOpsMint(t, srv, owner.ID, projectBoundary(proj.ID), "project:manage_system_one_instances")
	rec := doProjectSystemOneBearerReq(t, srv, http.MethodPut, basePath+"/inst-uat", uatWithScope, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	uatWithoutScope := projectOpsMint(t, srv, owner.ID, projectBoundary(proj.ID), "project:read")
	rec = doProjectSystemOneBearerReq(t, srv, http.MethodPut, basePath+"/inst-uat-denied", uatWithoutScope, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doProjectSystemOneBearerReq(t, srv, http.MethodDelete, basePath+"/inst-uat", uatWithoutScope, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	// UAT with project:read CAN GET.
	rec = doProjectSystemOneBearerReq(t, srv, http.MethodGet, basePath, uatWithoutScope, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 4b. Other-project owner and other-project UAT MUST be denied (403).
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, otherOwner, http.MethodGet, basePath, nil).Code)
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, otherOwner, http.MethodPut, basePath+"/inst-x", nil).Code)
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, otherOwner, http.MethodDelete, basePath+"/inst-uat", nil).Code)

	otherProjUAT := projectOpsMint(t, srv, otherOwner.ID, projectBoundary(otherProj.ID), "project:manage_system_one_instances")
	assert.Equal(t, http.StatusForbidden, doProjectSystemOneBearerReq(t, srv, http.MethodGet, basePath, otherProjUAT, nil).Code)
	assert.Equal(t, http.StatusForbidden, doProjectSystemOneBearerReq(t, srv, http.MethodPut, basePath+"/inst-x", otherProjUAT, nil).Code)
	assert.Equal(t, http.StatusForbidden, doProjectSystemOneBearerReq(t, srv, http.MethodDelete, basePath+"/inst-uat", otherProjUAT, nil).Code)

	// 4c. Hub-boundary UAT minted by project owner with project:manage_system_one_instances IS allowed (200).
	hubBoundaryOwnerUAT := projectOpsMint(t, srv, owner.ID, hubBoundary(), "project:manage_system_one_instances")
	rec = doProjectSystemOneBearerReq(t, srv, http.MethodPut, basePath+"/inst-uat-hub", hubBoundaryOwnerUAT, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doProjectSystemOneBearerReq(t, srv, http.MethodDelete, basePath+"/inst-uat-hub", hubBoundaryOwnerUAT, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 4d. Scoped UAT minted by a hub admin who is NOT an active member of proj MUST be denied (403).
	// Grant project-admin transiently to mint the token, then revoke the project binding
	// (matching TestProjectUAT_HubAdminScheduledEventGrantsDoNotUnlockAgents).
	projAdminRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	tempBinding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               uuid.NewString(),
		RoleDefinitionID: projAdminRole.ID,
		PrincipalType:    "user",
		PrincipalID:      hubAdmin.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
	})
	require.NoError(t, err)
	hubAdminNonMemberUAT := projectOpsMint(t, srv, hubAdmin.ID, projectBoundary(proj.ID), "project:manage_system_one_instances")
	require.NoError(t, s.DeleteRoleBinding(ctx, tempBinding.ID))
	assert.Equal(t, http.StatusForbidden, doProjectSystemOneBearerReq(t, srv, http.MethodPut, basePath+"/inst-x", hubAdminNonMemberUAT, nil).Code)
	assert.Equal(t, http.StatusForbidden, doProjectSystemOneBearerReq(t, srv, http.MethodDelete, basePath+"/inst-uat", hubAdminNonMemberUAT, nil).Code)

	// 4e. Non-owner/admin holding a custom role granting project.manage_system_one_instances
	// (and a UAT minted by them) MUST be denied (403) by the owner/admin rule.
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, customRoleUser, http.MethodPut, basePath+"/inst-x", nil).Code)
	customRoleUAT := projectOpsMint(t, srv, customRoleUser.ID, projectBoundary(proj.ID), "project:manage_system_one_instances")
	assert.Equal(t, http.StatusForbidden, doProjectSystemOneBearerReq(t, srv, http.MethodPut, basePath+"/inst-x", customRoleUAT, nil).Code)

	// 5. Non-user / non-interactive credentials MUST be denied (403) on GET, PUT, DELETE:
	//    agent JWT, delegated agent, broker direct, broker on-behalf-of owner,
	//    hub-delivery, and federated user (Hub O1).
	doWithCred := func(ident Identity, cc CredentialContext, method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		reqCtx := contextWithIdentity(req.Context(), ident)
		reqCtx = contextWithCredentialContext(reqCtx, cc)
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req.WithContext(reqCtx))
		return rec
	}
	ownerUserIdent := NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, string(ClientTypeAPI))
	deniedCreds := []struct {
		name  string
		ident Identity
		cc    CredentialContext
	}{
		{"agent-jwt", dcAgentIdentity(uuid.NewString(), proj.ID, AgentRoleBaseline), CredentialContext{Kind: CredentialKindAgentJWT, ID: "ajwt-s1"}},
		{"delegated-agent", dcAgentIdentity(uuid.NewString(), proj.ID, AgentRoleBaseline), CredentialContext{Kind: CredentialKindDelegatedAgent, ID: "dcred-s1"}},
		{"broker-direct", NewBrokerIdentity(uuid.NewString()), CredentialContext{Kind: CredentialKindBroker, ID: "brk-s1"}},
		{"broker-obo-owner", ownerUserIdent, CredentialContext{Kind: CredentialKindBroker, ID: "brk-obo-s1"}},
		{"hub-delivery", ownerUserIdent, CredentialContext{Kind: CredentialKindHubDelivery, ID: "hdel-s1"}},
		{"federated-user", NewFederatedUserIdentity("https://issuer.example", "fed-sub-s1", owner.Email, "Fed Owner", "admin", nil), CredentialContext{Kind: CredentialKindFederation, ID: "fed-s1"}},
	}
	for _, tc := range deniedCreds {
		t.Run("denied_cred_"+tc.name, func(t *testing.T) {
			assert.Equal(t, http.StatusForbidden, doWithCred(tc.ident, tc.cc, http.MethodGet, basePath).Code)
			assert.Equal(t, http.StatusForbidden, doWithCred(tc.ident, tc.cc, http.MethodPut, basePath+"/inst-agent").Code)
			assert.Equal(t, http.StatusForbidden, doWithCred(tc.ident, tc.cc, http.MethodDelete, basePath+"/inst-uat").Code)
		})
	}
}

func TestProjectSystemOneInstances_UnknownInstanceAndDeletePrune(t *testing.T) {
	ctx := context.Background()
	srv, s, _ := newSystemOneTestServer(t, true)
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-o1-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	proj := &store.Project{
		ID:      uuid.NewString(),
		Name:    "System One Existence & Prune Project",
		Slug:    "s1-exist-prune",
		OwnerID: admin.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	basePath := "/api/v1/projects/" + proj.ID + "/system-one/instances"

	// 1. Adding or setting a non-existent instance name returns 422 unknown_instance.
	addMissing := doRequestAsUser(t, srv, admin, http.MethodPut, basePath+"/does-not-exist", nil)
	require.Equal(t, http.StatusUnprocessableEntity, addMissing.Code, addMissing.Body.String())
	assert.Contains(t, addMissing.Body.String(), "unknown_instance")

	missingInsts := []string{"does-not-exist"}
	setMissing := doRequestAsUser(t, srv, admin, http.MethodPut, basePath, SetProjectSystemOneInstancesRequest{
		Instances: &missingInsts,
	})
	require.Equal(t, http.StatusUnprocessableEntity, setMissing.Code, setMissing.Body.String())
	assert.Contains(t, setMissing.Body.String(), "unknown_instance")

	// PUT {} without the "instances" key returns 400 validation_error and never
	// silently clears the allowlist; explicit {"instances": []} clears it (Hub N2).
	emptyBodyRec := doRequestAsUser(t, srv, admin, http.MethodPut, basePath, map[string]any{})
	require.Equal(t, http.StatusBadRequest, emptyBodyRec.Code, emptyBodyRec.Body.String())
	assert.Contains(t, emptyBodyRec.Body.String(), "instances field is required")

	// Instance names > 63 characters return 400 validation_error, not 422 (rev3-hub nit 5).
	longSlug := "a123456789012345678901234567890123456789012345678901234567890123"
	longRec := doRequestAsUser(t, srv, admin, http.MethodPut, basePath+"/"+longSlug, nil)
	require.Equal(t, http.StatusBadRequest, longRec.Code, longRec.Body.String())

	// Removing a non-existent valid slug stays idempotent (200).
	delMissing := doRequestAsUser(t, srv, admin, http.MethodDelete, basePath+"/does-not-exist", nil)
	require.Equal(t, http.StatusOK, delMissing.Code, delMissing.Body.String())

	// 2. Tiny positive limits.ratePerSec (< 1e-6) is rejected at instance create time (400).
	tinyRateRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "tiny-rate-inst",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "sk-offline-tiny-rate-0001"},
		"limits":  map[string]any{"ratePerSec": 1e-11},
	})
	require.Equal(t, http.StatusBadRequest, tinyRateRec.Code, tinyRateRec.Body.String())

	// 3. Create instance, allowlist it on proj, delete the hub instance ->
	// proj's allowlist is pruned immediately, and recreating an instance with
	// the same name does NOT silently re-authorize proj.
	createRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "reusable-inst",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "sk-offline-reusable-0001"},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())

	addRec := doRequestAsUser(t, srv, admin, http.MethodPut, basePath+"/reusable-inst", nil)
	require.Equal(t, http.StatusOK, addRec.Code, addRec.Body.String())

	// Handler-level immutability check (rev3-hub O3 addendum): PUT/PATCH on the
	// admin instance endpoint are refused (405 Method Not Allowed) and leave
	// both the instance and project allowlist intact.
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		renameRec := doRequestAsUser(t, srv, admin, method, systemOneAdminPath+"/reusable-inst", map[string]any{
			"name": "reusable-inst-renamed",
		})
		assert.Equal(t, http.StatusMethodNotAllowed, renameRec.Code, "method=%s body=%s", method, renameRec.Body.String())
	}
	gotBeforeDelete, err := s.GetProjectSystemOneInstances(ctx, proj.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"reusable-inst"}, gotBeforeDelete)

	delInstRec := doRequestAsUser(t, srv, admin, http.MethodDelete, systemOneAdminPath+"/reusable-inst", nil)
	require.Equal(t, http.StatusNoContent, delInstRec.Code, delInstRec.Body.String())

	gotAfterDelete, err := s.GetProjectSystemOneInstances(ctx, proj.ID)
	require.NoError(t, err)
	assert.Empty(t, gotAfterDelete, "deleting hub instance must prune it from project allowlists")

	recreateRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "reusable-inst",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "sk-offline-reusable-0002"},
	})
	require.Equal(t, http.StatusCreated, recreateRec.Code, recreateRec.Body.String())

	gotAfterRecreate, err := s.GetProjectSystemOneInstances(ctx, proj.ID)
	require.NoError(t, err)
	assert.Empty(t, gotAfterRecreate, "recreating an instance with the same name must not silently re-authorize proj")
}

type s1CountingSecretBackend struct {
	secret.SecretBackend
	getCalls atomic.Int32
}

func (c *s1CountingSecretBackend) Get(ctx context.Context, key, scope, scopeID string) (*secret.SecretWithValue, error) {
	c.getCalls.Add(1)
	return c.SecretBackend.Get(ctx, key, scope, scopeID)
}

func TestProjectSystemOneInstances_CompileEvaluateRemovalSecretIndependenceAndProbe(t *testing.T) {
	ctx := context.Background()
	srv, s, _ := newSystemOneTestServer(t, true)
	countingSecrets := &s1CountingSecretBackend{SecretBackend: srv.secretBackend}
	srv.secretBackend = countingSecrets

	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-e2e-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	proj := &store.Project{
		ID:      uuid.NewString(),
		Name:    "System One E2E Project",
		Slug:    "s1-e2e-proj",
		OwnerID: admin.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	const apiKey = "sk-offline-allowlist-e2e-1234"
	var providerCalls atomic.Int32
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"models": [{"name": "jev-1.14.0", "description": "Pinned Jev 1.14.0 release.", "release_date": "2026-09-15"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{
		  "model": "jev-1.14.0",
		  "answers": {"q1": {"type": "noul", "noul": 0.92}},
		  "usage": {"input_tokens": 12, "output_tokens": 2}
		}`))
	}))
	t.Cleanup(tlsSrv.Close)

	srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			egress.WithLoopbackServerForTesting(tlsSrv.URL, tlsSrv.Client()),
		),
	)

	// Create instance "ts-allow" with an owned API key.
	createRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "ts-allow",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": apiKey},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	instRow, err := s.GetSystemOneInstanceByName(ctx, "ts-allow")
	require.NoError(t, err)
	instRow.Endpoint = tlsSrv.URL
	require.NoError(t, s.UpdateSystemOneInstance(ctx, instRow))

	// Create a second instance "ts-missing-secret" whose secret we delete to prove
	// the allowlist gate is independent of secret existence.
	createRec2 := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "ts-missing-secret",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "sk-temp-delete-me"},
	})
	require.Equal(t, http.StatusCreated, createRec2.Code, createRec2.Body.String())
	instRow2, err := s.GetSystemOneInstanceByName(ctx, "ts-missing-secret")
	require.NoError(t, err)
	instRow2.Endpoint = tlsSrv.URL
	require.NoError(t, s.UpdateSystemOneInstance(ctx, instRow2))
	require.NoError(t, s.DeleteSecret(ctx, instRow2.Auth.SecretName, store.ScopeHub, srv.hubID))

	re := srv.RuleEngine()
	require.NoError(t, re.AllowConsumerDialects(testSystemOneConsumer, ruleengine.GrantIO(systemone.DialectName)))
	const testEnvName = "system_one_e2e_env"
	require.NoError(t, systemone.RegisterConsumerEnvironment(re, testSystemOneConsumer, spec.ContentEnvironment{
		Name:    testEnvName,
		Version: 1,
		Fields: []spec.FieldDecl{
			{Name: spec.ProbeFieldText, Kind: spec.FieldKindText, MaxBytes: spec.ProbeMaxBytes, Trust: spec.TrustUntrusted},
		},
		MaxTotalBytes: spec.ProbeMaxBytes,
	}))

	const rawSpec = `{
	  "version": 1,
	  "binding": {"instance": "ts-allow"},
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Is this urgent?"}],
	  "output": {"question": "q1"}
	}`
	const rawRule = `{"inputs": ["probe.text"], "spec": ` + rawSpec + `}`
	const rawRuleMissingSec = `{
	  "inputs": ["probe.text"],
	  "spec": {
	    "version": 1,
	    "binding": {"instance": "ts-missing-secret"},
	    "questions": [{"id": "q1", "type": "binary", "instructions": "Is this urgent?"}],
	    "output": {"question": "q1"}
	  }
	}`

	// 1. Compile with project context refuses unlisted instance (0 secret reads).
	_, err = re.Compile(systemone.WithProjectID(ctx, proj.ID), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: testSystemOneConsumer,
		Name:     testEnvName,
	})
	require.Error(t, err)
	ce, ok := spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategoryBinding, ce.Category)
	assert.Zero(t, countingSecrets.getCalls.Load())

	// Compile without project context (e.g. hub-level) to test evaluation refusal.
	compiled, err := re.Compile(ctx, systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: testSystemOneConsumer,
		Name:     testEnvName,
	})
	require.NoError(t, err)

	// 2. Evaluation refuses unlisted instance with no_eligible_instance, 0 requests, 0 secret reads.
	_, err = re.Evaluate(ctx, compiled, ruleengine.Input{
		Payload: systemone.Request{
			Input:     systemone.ProbeInput("urgent outage"),
			ProjectID: proj.ID,
		},
	}, ruleengine.Budget{})
	require.Error(t, err)
	var failedErr *ruleengine.FailedError
	require.ErrorAs(t, err, &failedErr)
	assert.Equal(t, string(systemoneprovider.FailNoEligibleInstance), failedErr.Reason)
	assert.Zero(t, providerCalls.Load())
	assert.Zero(t, countingSecrets.getCalls.Load())

	// 3. Gate is independent of secret existence: unlisted instance with deleted secret
	// still returns no_eligible_instance with 0 secret reads (not auth).
	compiledMissingSec, err := re.Compile(ctx, systemone.DialectName, rawRuleMissingSec, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: testSystemOneConsumer,
		Name:     testEnvName,
	})
	require.NoError(t, err)
	_, err = re.Evaluate(ctx, compiledMissingSec, ruleengine.Input{
		Payload: systemone.Request{
			Input:     systemone.ProbeInput("urgent outage"),
			ProjectID: proj.ID,
		},
	}, ruleengine.Budget{})
	require.Error(t, err)
	failedErr = nil
	require.ErrorAs(t, err, &failedErr)
	assert.Equal(t, string(systemoneprovider.FailNoEligibleInstance), failedErr.Reason)
	assert.Zero(t, countingSecrets.getCalls.Load())

	// 4. Probe with --project P respects P's allowlist (refused when unlisted, 0 requests, 0 secret reads).
	probeWithProjDenied := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/ts-allow/probe", ProbeSystemOneInstanceRequest{
		Project: proj.ID,
		Kind:    string(spec.KindFilter),
		Spec:    json.RawMessage(rawSpec),
		Probe:   &ProbeSystemOneInput{Text: "probe with project"},
	})
	require.Equal(t, http.StatusOK, probeWithProjDenied.Code, probeWithProjDenied.Body.String())
	var probeDeniedResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(probeWithProjDenied.Body.Bytes(), &probeDeniedResp))
	assert.Equal(t, systemoneruntime.ClassFailed, probeDeniedResp.Class)
	assert.Equal(t, string(systemoneprovider.FailNoEligibleInstance), probeDeniedResp.Reason)
	assert.Zero(t, providerCalls.Load())
	assert.Zero(t, countingSecrets.getCalls.Load())

	// 4b. Models probe with --project P also respects P's allowlist (Hub O2):
	//     unlisted -> no_eligible_instance with 0 provider hits and 0 secret reads.
	modelsDenied := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/ts-allow/probe", ProbeSystemOneInstanceRequest{
		Models:  true,
		Project: proj.ID,
	})
	require.Equal(t, http.StatusOK, modelsDenied.Code, modelsDenied.Body.String())
	var modelsDeniedResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(modelsDenied.Body.Bytes(), &modelsDeniedResp))
	assert.Equal(t, systemoneruntime.ClassFailed, modelsDeniedResp.Class)
	assert.Equal(t, string(systemoneprovider.FailNoEligibleInstance), modelsDeniedResp.Reason)
	assert.Zero(t, providerCalls.Load())
	assert.Zero(t, countingSecrets.getCalls.Load())

	// 5. Probe WITHOUT --project stays hub-admin-only and succeeds even with empty project allowlist.
	probeNoProj := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/ts-allow/probe", ProbeSystemOneInstanceRequest{
		Kind:  string(spec.KindFilter),
		Spec:  json.RawMessage(rawSpec),
		Probe: &ProbeSystemOneInput{Text: "probe without project"},
	})
	require.Equal(t, http.StatusOK, probeNoProj.Code, probeNoProj.Body.String())
	var probeNoProjResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(probeNoProj.Body.Bytes(), &probeNoProjResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, probeNoProjResp.Class)
	assert.Equal(t, int32(1), providerCalls.Load())

	// 6. Add "ts-allow" to proj's allowlist via HTTP API -> Compile, Evaluate, and Probe-with-project all succeed.
	addRec := doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/projects/"+proj.ID+"/system-one/instances/ts-allow", nil)
	require.Equal(t, http.StatusOK, addRec.Code, addRec.Body.String())

	_, err = re.Compile(systemone.WithProjectID(ctx, proj.ID), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: testSystemOneConsumer,
		Name:     testEnvName,
	})
	require.NoError(t, err)

	dec, err := re.Evaluate(ctx, compiled, ruleengine.Input{
		Payload: systemone.Request{
			Input:     systemone.ProbeInput("urgent outage"),
			ProjectID: proj.ID,
		},
	}, ruleengine.Budget{})
	require.NoError(t, err)
	assert.True(t, dec.Bool)
	assert.Equal(t, int32(2), providerCalls.Load())

	probeWithProjOK := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/ts-allow/probe", ProbeSystemOneInstanceRequest{
		Project: proj.ID,
		Kind:    string(spec.KindFilter),
		Spec:    json.RawMessage(rawSpec),
		Probe:   &ProbeSystemOneInput{Text: "probe with project allowed"},
	})
	require.Equal(t, http.StatusOK, probeWithProjOK.Code, probeWithProjOK.Body.String())
	var probeOKResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(probeWithProjOK.Body.Bytes(), &probeOKResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, probeOKResp.Class)
	assert.Equal(t, int32(3), providerCalls.Load())

	// Models probe with --project P succeeds once ts-allow is allowlisted.
	modelsOK := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/ts-allow/probe", ProbeSystemOneInstanceRequest{
		Models:  true,
		Project: proj.ID,
	})
	require.Equal(t, http.StatusOK, modelsOK.Code, modelsOK.Body.String())
	var modelsOKResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(modelsOK.Body.Bytes(), &modelsOKResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, modelsOKResp.Class)
	assert.Contains(t, modelsOKResp.Models, "jev-1.14.0")
	assert.Equal(t, int32(4), providerCalls.Load())

	// 7. Remove "ts-allow" via HTTP API -> takes effect immediately on the next evaluation (0 new requests).
	delRec := doRequestAsUser(t, srv, admin, http.MethodDelete, "/api/v1/projects/"+proj.ID+"/system-one/instances/ts-allow", nil)
	require.Equal(t, http.StatusOK, delRec.Code, delRec.Body.String())

	callsBefore := providerCalls.Load()
	secretsBefore := countingSecrets.getCalls.Load()
	_, err = re.Evaluate(ctx, compiled, ruleengine.Input{
		Payload: systemone.Request{
			Input:     systemone.ProbeInput("urgent outage"),
			ProjectID: proj.ID,
		},
	}, ruleengine.Budget{})
	require.Error(t, err)
	failedErr = nil
	require.ErrorAs(t, err, &failedErr)
	assert.Equal(t, string(systemoneprovider.FailNoEligibleInstance), failedErr.Reason)
	assert.Equal(t, callsBefore, providerCalls.Load())
	assert.Equal(t, secretsBefore, countingSecrets.getCalls.Load())
}

func TestProjectSystemOneInstances_TestAdminDenied(t *testing.T) {
	ctx := context.Background()
	srv, s, _ := tsaServer(t)
	setSystemOneExperimentLive(t, srv, true)

	issuer1 := taUser(t, s, "s1-ta-iss1", store.SystemRoleSuperAdmin)
	issuer2 := taUser(t, s, "s1-ta-iss2", store.SystemRoleSuperAdmin)
	superKind := tsaIssue(t, srv, issuer1, testAdminRoleSuperAdmin)
	hubKind := tsaIssue(t, srv, issuer2, testAdminRoleHubAdmin)
	humanHubAdmin := taUser(t, s, "s1-ta-human-hubadmin", store.SystemRoleHubAdmin)
	humanTok := taTokenFor(t, srv, humanHubAdmin)

	proj := &store.Project{
		ID:      uuid.NewString(),
		Name:    "System One Test Admin Gate",
		Slug:    "s1-ta-gate",
		OwnerID: humanHubAdmin.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	require.NoError(t, s.CreateSystemOneInstance(ctx, &store.SystemOneInstance{
		ID:       uuid.NewString(),
		Name:     "ta-inst",
		Adapter:  "typesafe",
		Model:    "jev-1.14.0",
		Endpoint: "https://api.typesafe.ai",
		Auth:     store.SystemOneInstanceAuth{Kind: store.SystemOneAuthKindAPIKey, SecretName: "sec-ta-inst"},
		Enabled:  true,
	}))

	basePath := "/api/v1/projects/" + proj.ID + "/system-one/instances"
	instList := []string{"ta-inst"}

	// Both test_hub_admin and test_super_admin MUST receive 403 on collection PUT,
	// item PUT (add), and item DELETE (remove).
	for who, tok := range map[string]string{
		"test_super_admin": superKind.AccessToken,
		"test_hub_admin":   hubKind.AccessToken,
	} {
		recSet := doRequestWithToken(t, srv, tok, http.MethodPut, basePath, SetProjectSystemOneInstancesRequest{Instances: &instList})
		assert.Equal(t, http.StatusForbidden, recSet.Code, "%s collection PUT: %s", who, recSet.Body.String())

		recAdd := doRequestWithToken(t, srv, tok, http.MethodPut, basePath+"/ta-inst", nil)
		assert.Equal(t, http.StatusForbidden, recAdd.Code, "%s item PUT: %s", who, recAdd.Body.String())

		recDel := doRequestWithToken(t, srv, tok, http.MethodDelete, basePath+"/ta-inst", nil)
		assert.Equal(t, http.StatusForbidden, recDel.Code, "%s item DELETE: %s", who, recDel.Body.String())
	}

	// Direct role-gate check: projectSystemOneOwnerOrAdminRule itself refuses test-admin
	// identities before the IsUnscopedLocalPlatformAdmin / IsHubAdmin bypass.
	testAdminUserIdent := tsaIdentity(t, s, hubKind.Identity.ID)
	taCtx := contextWithIdentity(ctx, testAdminUserIdent)
	taCtx = contextWithCredentialContext(taCtx, CredentialContext{Kind: CredentialKindInteractive, ID: "ta-web"})
	assert.False(t, srv.projectSystemOneOwnerOrAdminRule(taCtx, testAdminUserIdent, proj.ID),
		"projectSystemOneOwnerOrAdminRule must refuse test-admin identity directly")

	// Control: human hub admin succeeds (200) on add, set, and remove.
	recAdd := doRequestWithToken(t, srv, humanTok, http.MethodPut, basePath+"/ta-inst", nil)
	require.Equal(t, http.StatusOK, recAdd.Code, recAdd.Body.String())

	recSet := doRequestWithToken(t, srv, humanTok, http.MethodPut, basePath, SetProjectSystemOneInstancesRequest{Instances: &instList})
	require.Equal(t, http.StatusOK, recSet.Code, recSet.Body.String())

	recDel := doRequestWithToken(t, srv, humanTok, http.MethodDelete, basePath+"/ta-inst", nil)
	require.Equal(t, http.StatusOK, recDel.Code, recDel.Body.String())
}
