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

	proj := &store.Project{
		ID:      uuid.NewString(),
		Name:    "System One Authz Matrix",
		Slug:    "s1-authz-matrix",
		OwnerID: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	// Bind project roles.
	bindRole := func(u *store.User, roleName string) {
		role, rErr := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
		require.NoError(t, rErr)
		_, rbErr := s.CreateRoleBinding(ctx, &store.RoleBinding{
			ID:               uuid.NewString(),
			RoleDefinitionID: role.ID,
			PrincipalType:    "user",
			PrincipalID:      u.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          proj.ID,
		})
		require.NoError(t, rbErr)
	}
	bindRole(owner, store.ProjectRoleOwner)
	bindRole(projAdmin, store.ProjectRoleAdmin)
	bindRole(member, store.ProjectRoleMember)

	basePath := "/api/v1/projects/" + proj.ID + "/system-one/instances"

	// 1. Experiment OFF -> 404 on GET, PUT, DELETE even for super-admin.
	setSystemOneExperimentLive(t, srv, false)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, superAdmin, http.MethodGet, basePath, nil).Code)
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
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, member, http.MethodPut, basePath, SetProjectSystemOneInstancesRequest{Instances: []string{"inst-x"}}).Code)

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

	// 5. Agent JWT MUST be denied on GET, PUT, DELETE.
	agentIdent := dcAgentIdentity(uuid.NewString(), proj.ID, AgentRoleBaseline)
	doAsAgent := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		reqCtx := contextWithIdentity(req.Context(), agentIdent)
		reqCtx = contextWithCredentialContext(reqCtx, CredentialContext{Kind: CredentialKindAgentJWT, ID: "ajwt-s1"})
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req.WithContext(reqCtx))
		return rec
	}
	assert.Equal(t, http.StatusForbidden, doAsAgent(http.MethodGet, basePath).Code)
	assert.Equal(t, http.StatusForbidden, doAsAgent(http.MethodPut, basePath+"/inst-agent").Code)
	assert.Equal(t, http.StatusForbidden, doAsAgent(http.MethodDelete, basePath+"/inst-uat").Code)
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

	const rawRule = `{
	  "version": 1,
	  "binding": {"instance": "ts-allow"},
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Is this urgent?"}],
	  "output": {"question": "q1"}
	}`
	const rawRuleMissingSec = `{
	  "version": 1,
	  "binding": {"instance": "ts-missing-secret"},
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Is this urgent?"}],
	  "output": {"question": "q1"}
	}`

	// 1. Compile with project context refuses unlisted instance (0 secret reads).
	_, err = re.Compile(systemone.WithProjectID(ctx, proj.ID), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: testSystemOneConsumer,
		Name:     spec.ProbeEnvironmentName,
	})
	require.Error(t, err)
	ce, ok := spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategoryBinding, ce.Category)
	assert.Zero(t, countingSecrets.getCalls.Load())

	// Compile without project context (e.g. hub-level) to test evaluation refusal.
	compiled, err := re.Compile(ctx, systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: testSystemOneConsumer,
		Name:     spec.ProbeEnvironmentName,
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
		Name:     spec.ProbeEnvironmentName,
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
		Spec:    json.RawMessage(rawRule),
		Probe:   &ProbeSystemOneInput{Text: "probe with project"},
	})
	require.Equal(t, http.StatusOK, probeWithProjDenied.Code, probeWithProjDenied.Body.String())
	var probeDeniedResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(probeWithProjDenied.Body.Bytes(), &probeDeniedResp))
	assert.Equal(t, systemoneruntime.ClassFailed, probeDeniedResp.Class)
	assert.Equal(t, string(systemoneprovider.FailNoEligibleInstance), probeDeniedResp.Reason)
	assert.Zero(t, providerCalls.Load())
	assert.Zero(t, countingSecrets.getCalls.Load())

	// 5. Probe WITHOUT --project stays hub-admin-only and succeeds even with empty project allowlist.
	probeNoProj := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath+"/ts-allow/probe", ProbeSystemOneInstanceRequest{
		Kind:  string(spec.KindFilter),
		Spec:  json.RawMessage(rawRule),
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
		Name:     spec.ProbeEnvironmentName,
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
		Spec:    json.RawMessage(rawRule),
		Probe:   &ProbeSystemOneInput{Text: "probe with project allowed"},
	})
	require.Equal(t, http.StatusOK, probeWithProjOK.Code, probeWithProjOK.Body.String())
	var probeOKResp ProbeSystemOneInstanceResponse
	require.NoError(t, json.Unmarshal(probeWithProjOK.Body.Bytes(), &probeOKResp))
	assert.Equal(t, systemoneruntime.ClassSuccess, probeOKResp.Class)
	assert.Equal(t, int32(3), providerCalls.Load())

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
