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
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	hostSAPatchOld = "old-host@host-project.iam.gserviceaccount.com"
	hostSAPatchNew = "new-host@host-project.iam.gserviceaccount.com"
)

// hostSAPatchFixture is a broker owned by a member, with a stored host
// service account, plus a hub admin who does not own it.
type hostSAPatchFixture struct {
	srv    *Server
	store  store.Store
	owner  *store.User
	admin  *store.User
	broker *store.RuntimeBroker
	audit  *mockAuditLogger
}

func setupHostSAPatch(t *testing.T, name string) *hostSAPatchFixture {
	t.Helper()
	return setupHostSAPatchFrom(t, name, hostSAPatchOld, "host-project")
}

// setupHostSAPatchFrom is setupHostSAPatch with the broker's stored host
// service account given (empty for a broker that has none).
func setupHostSAPatchFrom(t *testing.T, name, hostSAEmail, hostProjectID string) *hostSAPatchFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	f := &hostSAPatchFixture{srv: srv, store: s}
	f.owner = addExtraUser(t, s, tid(name+"-owner"), name+"-owner@test.com", store.UserRoleMember)
	f.admin = addExtraUser(t, s, tid(name+"-admin"), name+"-admin@test.com", store.UserRoleAdmin)
	// Admin access requires a role binding; the role field alone is not enough.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.admin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
	f.broker = &store.RuntimeBroker{
		ID:                         tid(name + "-broker"),
		Name:                       name + " broker",
		Slug:                       name + "-broker",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  f.owner.ID,
		GCPHostServiceAccountEmail: hostSAEmail,
		GCPHostProjectID:           hostProjectID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, f.broker))

	f.audit = &mockAuditLogger{}
	srv.SetAuditLogger(f.audit)
	return f
}

func (f *hostSAPatchFixture) patch(t *testing.T, as *store.User, body map[string]interface{}) (int, string) {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, as, http.MethodPatch, "/api/v1/runtime-brokers/"+f.broker.ID, body)
	return rec.Code, rec.Body.String()
}

func (f *hostSAPatchFixture) stored(t *testing.T) hostSAClaim {
	t.Helper()
	got, err := f.store.GetRuntimeBroker(context.Background(), f.broker.ID)
	require.NoError(t, err)
	return hostSAClaimOf(got)
}

// requireDeniedHostSAEvent asserts the single audit record of a refused
// change: the update surface, the requested email and a non-allow decision.
func requireDeniedHostSAEvent(t *testing.T, audit *mockAuditLogger, email string) {
	t.Helper()
	ev := onlySAEvent(t, audit)
	assert.Equal(t, SurfaceBrokerHostSAUpdate, ev.Surface)
	assert.Equal(t, email, ev.TargetSAEmail)
	require.NotNil(t, ev.Decision)
	assert.NotEqual(t, store.ActAsAllowed, *ev.Decision)
}

func TestBrokerUpdate_HostSA_OwnerLacksActAs_Denied(t *testing.T) {
	f := setupHostSAPatch(t, "hsa-owner-deny")
	checker := store.NewFakeCallerPermissionChecker().DenyTarget(hostSAPatchNew, "no actAs grant")
	enforceSAAssign(f.srv, checker)

	code, body := f.patch(t, f.owner, map[string]interface{}{"gcpHostServiceAccountEmail": hostSAPatchNew})

	assert.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, store.PermissionActAs)
	assert.Contains(t, body, hostSAPatchNew)
	assert.Equal(t, 1, checker.CallCount())
	assert.Equal(t, hostSAClaim{Email: hostSAPatchOld, ProjectID: "host-project"}, f.stored(t),
		"a refused change must leave the row unchanged")
	requireDeniedHostSAEvent(t, f.audit, hostSAPatchNew)
}

func TestBrokerUpdate_HostSA_AdminLacksActAs_Denied(t *testing.T) {
	f := setupHostSAPatch(t, "hsa-admin-deny")
	checker := store.NewFakeCallerPermissionChecker().DenyTarget(hostSAPatchNew, "no actAs grant")
	enforceSAAssign(f.srv, checker)

	code, body := f.patch(t, f.admin, map[string]interface{}{"gcpHostServiceAccountEmail": hostSAPatchNew})

	assert.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, store.PermissionActAs, "the refusal must come from the actAs check")
	assert.Equal(t, 1, checker.CallCount(), "the admin passes broker.update and reaches the actAs check")
	assert.Equal(t, hostSAPatchOld, f.stored(t).Email)
	requireDeniedHostSAEvent(t, f.audit, hostSAPatchNew)
}

func TestBrokerUpdate_HostSA_CheckerNotConclusive_Denied(t *testing.T) {
	tests := []struct {
		name     string
		checker  *store.FakeCallerPermissionChecker
		wantBody string
	}{
		{
			name:     "checker error",
			checker:  store.NewFakeCallerPermissionChecker().FailTarget(hostSAPatchNew, errors.New("timeout")),
			wantBody: "check did not complete",
		},
		{
			name:     "indeterminate result",
			checker:  store.NewFakeCallerPermissionChecker().IndeterminateTarget(hostSAPatchNew, "no verdict"),
			wantBody: store.PermissionActAs,
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := setupHostSAPatch(t, "hsa-inconclusive-"+string(rune('a'+i)))
			enforceSAAssign(f.srv, tc.checker)

			code, body := f.patch(t, f.owner, map[string]interface{}{"gcpHostServiceAccountEmail": hostSAPatchNew})

			assert.Equal(t, http.StatusForbidden, code, "body: %s", body)
			assert.Contains(t, body, tc.wantBody)
			assert.Equal(t, 1, tc.checker.CallCount())
			assert.Equal(t, hostSAPatchOld, f.stored(t).Email, "a refused change must leave the row unchanged")
			requireDeniedHostSAEvent(t, f.audit, hostSAPatchNew)
		})
	}
}

func TestBrokerUpdate_HostSA_InvalidEmail_400BeforeCheck(t *testing.T) {
	f := setupHostSAPatch(t, "hsa-invalid")
	checker := store.NewFakeCallerPermissionChecker().AllowTarget("not-an-sa@example.com")
	enforceSAAssign(f.srv, checker)

	code, body := f.patch(t, f.owner, map[string]interface{}{"gcpHostServiceAccountEmail": "not-an-sa@example.com"})

	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Equal(t, 0, checker.CallCount(), "format validation runs before the actAs check")
	assert.Empty(t, f.audit.saEvents)
	assert.Equal(t, hostSAPatchOld, f.stored(t).Email)
}

func TestBrokerUpdate_HostSA_SetFromEmpty(t *testing.T) {
	t.Run("lacks actAs", func(t *testing.T) {
		f := setupHostSAPatchFrom(t, "hsa-from-empty-deny", "", "")
		checker := store.NewFakeCallerPermissionChecker().DenyTarget(hostSAPatchNew, "no actAs grant")
		enforceSAAssign(f.srv, checker)

		code, body := f.patch(t, f.owner, map[string]interface{}{
			"gcpHostServiceAccountEmail": hostSAPatchNew,
			"gcpHostProjectId":           "host-project",
		})

		assert.Equal(t, http.StatusForbidden, code, "body: %s", body)
		assert.Equal(t, 1, checker.CallCount())
		assert.Equal(t, hostSAClaim{}, f.stored(t), "a refused first value must not be stored")
		requireDeniedHostSAEvent(t, f.audit, hostSAPatchNew)
	})
	t.Run("with actAs", func(t *testing.T) {
		f := setupHostSAPatchFrom(t, "hsa-from-empty-allow", "", "")
		checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAPatchNew)
		enforceSAAssign(f.srv, checker)

		code, body := f.patch(t, f.owner, map[string]interface{}{
			"gcpHostServiceAccountEmail": hostSAPatchNew,
			"gcpHostProjectId":           "host-project",
		})

		require.Equal(t, http.StatusOK, code, "body: %s", body)
		assert.Equal(t, 1, checker.CallCount())
		assert.Equal(t, hostSAClaim{Email: hostSAPatchNew, ProjectID: "host-project"}, f.stored(t))
		ev := onlySAEvent(t, f.audit)
		assert.Equal(t, SurfaceBrokerHostSAUpdate, ev.Surface)
	})
}

func TestBrokerUpdate_HostSA_OwnerWithActAs_Allowed(t *testing.T) {
	f := setupHostSAPatch(t, "hsa-owner-allow")
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAPatchNew)
	enforceSAAssign(f.srv, checker)

	code, body := f.patch(t, f.owner, map[string]interface{}{
		"gcpHostServiceAccountEmail": hostSAPatchNew,
		"gcpHostProjectId":           "host-project",
	})

	require.Equal(t, http.StatusOK, code, "body: %s", body)
	assert.Equal(t, hostSAClaim{Email: hostSAPatchNew, ProjectID: "host-project"}, f.stored(t))
	calls := checker.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, f.owner.ID, calls[0].Caller.ID, "actAs must be evaluated for the requesting user")
	assert.Equal(t, hostSAPatchNew, calls[0].TargetSAEmail)

	ev := onlySAEvent(t, f.audit)
	assert.Equal(t, SurfaceBrokerHostSAUpdate, ev.Surface)
	assert.Equal(t, hostSAPatchNew, ev.TargetSAEmail)
}

func TestBrokerUpdate_HostSA_UnchangedValue_NoCheck(t *testing.T) {
	f := setupHostSAPatch(t, "hsa-unchanged")
	checker := store.NewFakeCallerPermissionChecker() // denies anything it is asked
	enforceSAAssign(f.srv, checker)

	// Same value in a different case, plus an unrelated field.
	code, body := f.patch(t, f.owner, map[string]interface{}{
		"gcpHostServiceAccountEmail": "Old-Host@host-project.iam.gserviceaccount.com",
		"gcpHostProjectId":           "host-project",
		"labels":                     map[string]string{"team": "a"},
	})

	require.Equal(t, http.StatusOK, code, "body: %s", body)
	assert.Equal(t, 0, checker.CallCount(), "re-sending the stored value must not call the checker")
	assert.Empty(t, f.audit.saEvents)
}

func TestBrokerUpdate_HostProjectOnlyChange_RequiresActAs(t *testing.T) {
	f := setupHostSAPatch(t, "hsa-project-only")
	checker := store.NewFakeCallerPermissionChecker().DenyTarget(hostSAPatchOld, "no actAs grant")
	enforceSAAssign(f.srv, checker)

	code, body := f.patch(t, f.owner, map[string]interface{}{"gcpHostProjectId": "other-project"})

	assert.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Equal(t, 1, checker.CallCount(), "a project-only change is a change to the claim")
	assert.Equal(t, "host-project", f.stored(t).ProjectID)
	requireDeniedHostSAEvent(t, f.audit, hostSAPatchOld)
}

func TestBrokerUpdate_HostSA_Clear_NoActAsRequired(t *testing.T) {
	f := setupHostSAPatch(t, "hsa-clear")
	checker := store.NewFakeCallerPermissionChecker()
	enforceSAAssign(f.srv, checker)

	code, body := f.patch(t, f.owner, map[string]interface{}{
		"gcpHostServiceAccountEmail": "",
		"gcpHostProjectId":           "",
	})

	require.Equal(t, http.StatusOK, code, "body: %s", body)
	assert.Equal(t, 0, checker.CallCount())
	assert.Equal(t, hostSAClaim{}, f.stored(t))
}

func TestBrokerUpdate_HostSA_CheckModeOff_AllowedAndAudited(t *testing.T) {
	f := setupHostSAPatch(t, "hsa-off")
	// gcpIamCheckMode defaults to off on the test server.
	require.Equal(t, SAAssignCheckOff, f.srv.saAssignCheckModeValue())

	code, body := f.patch(t, f.owner, map[string]interface{}{"gcpHostServiceAccountEmail": hostSAPatchNew})

	require.Equal(t, http.StatusOK, code, "body: %s", body)
	assert.Equal(t, hostSAPatchNew, f.stored(t).Email)
	ev := onlySAEvent(t, f.audit)
	assert.Equal(t, SurfaceBrokerHostSAUpdate, ev.Surface)
	assert.Equal(t, store.MechanismCheckDisabled, ev.Mechanism)
	assert.Equal(t, hostSAPatchNew, ev.TargetSAEmail)
}

func TestBrokerUpdate_HostSA_EnforceNoGenerator_Denied(t *testing.T) {
	f := setupHostSAPatch(t, "hsa-no-generator")
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAPatchNew)
	f.srv.mu.Lock()
	f.srv.saAssignCheckMode = SAAssignCheckEnforce
	f.srv.saAssignChecker = checker
	f.srv.mu.Unlock()

	code, body := f.patch(t, f.owner, map[string]interface{}{"gcpHostServiceAccountEmail": hostSAPatchNew})

	assert.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "not available")
	assert.Equal(t, 0, checker.CallCount(), "the unavailable checker is substituted; the configured one is not consulted")
	assert.Equal(t, hostSAPatchOld, f.stored(t).Email)
}

func TestEvaluateHostSAClaim_UnchangedOrEmptySkipsChecker(t *testing.T) {
	srv, _ := testServer(t)
	checker := store.NewFakeCallerPermissionChecker()
	enforceSAAssign(srv, checker)
	ctx := context.Background() // no caller identity: any evaluation would deny

	prior := hostSAClaim{Email: hostSAPatchOld, ProjectID: "host-project"}

	v := srv.evaluateHostSAClaim(ctx, prior, prior, SurfaceBrokerHostSAUpdate)
	assert.True(t, v.Allowed)
	assert.Equal(t, prior, v.Claim)

	empty := hostSAClaim{ProjectID: "host-project"}
	v = srv.evaluateHostSAClaim(ctx, prior, empty, SurfaceBrokerHostSAUpdate)
	assert.True(t, v.Allowed)
	assert.Equal(t, empty, v.Claim)

	assert.Equal(t, 0, checker.CallCount())

	// A real change with no user on ctx is denied without reaching the checker.
	v = srv.evaluateHostSAClaim(ctx, prior, hostSAClaim{Email: hostSAPatchNew}, SurfaceBrokerHostSAUpdate)
	assert.False(t, v.Allowed)
	assert.Equal(t, 0, checker.CallCount())
}

func TestEvaluateHostSAClaim_NonUserCallerDenied(t *testing.T) {
	prior := hostSAClaim{Email: hostSAPatchOld, ProjectID: "host-project"}
	requested := hostSAClaim{Email: hostSAPatchNew, ProjectID: "host-project"}

	t.Run("agent acting as the requested account", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()
		audit := &mockAuditLogger{}
		srv.SetAuditLogger(audit)
		checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAPatchNew)
		enforceSAAssign(srv, checker)

		owner := addExtraUser(t, s, tid("hsa-agent-owner"), "hsa-agent-owner@test.com", store.UserRoleMember)
		project := &store.Project{
			ID: tid("hsa-agent-project"), Name: "hsa agent project", Slug: "hsa-agent-project",
			OwnerID: owner.ID, CreatedBy: owner.ID, Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, project))
		agentID := tid("hsa-agent")
		// The agent holds the requested account in assign mode, so an actAs
		// evaluation would allow it by the same-account rule.
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID: agentID, Slug: "hsa-agent", Name: "hsa-agent", ProjectID: project.ID, Phase: "running",
			AppliedConfig: &store.AgentAppliedConfig{
				GCPIdentity: &store.GCPIdentityConfig{
					MetadataMode:        store.GCPMetadataModeAssign,
					ServiceAccountID:    tid("hsa-agent-sa"),
					ServiceAccountEmail: hostSAPatchNew,
				},
			},
		}))
		agentCtx := contextWithIdentity(ctx, &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: agentID},
			ProjectID: project.ID,
		}})

		v := srv.evaluateHostSAClaim(agentCtx, prior, requested, SurfaceBrokerHostSAUpdate)

		assert.False(t, v.Allowed)
		assert.Equal(t, requested, v.Claim)
		assert.Equal(t, MechanismHostSAClaimNonUser, v.Result.Mechanism)
		assert.Equal(t, 0, checker.CallCount())
		ev := onlySAEvent(t, audit)
		assert.Equal(t, SurfaceBrokerHostSAUpdate, ev.Surface)
		assert.Equal(t, MechanismHostSAClaimNonUser, ev.Mechanism)
		assert.Equal(t, store.PrincipalAgent, ev.Caller.Kind)
		assert.Equal(t, hostSAPatchNew, ev.TargetSAEmail)
		require.NotNil(t, ev.Decision)
		assert.Equal(t, store.ActAsDenied, *ev.Decision)
		assert.Error(t, hostSAClaimWriteAllowed(prior, requested, v))
	})

	t.Run("broker", func(t *testing.T) {
		srv, _ := testServer(t)
		audit := &mockAuditLogger{}
		srv.SetAuditLogger(audit)
		checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAPatchNew)
		enforceSAAssign(srv, checker)
		brokerCtx := contextWithIdentity(context.Background(), NewBrokerIdentity(tid("hsa-broker")))

		v := srv.evaluateHostSAClaim(brokerCtx, prior, requested, SurfaceBrokerHostSAUpdate)

		assert.False(t, v.Allowed)
		assert.Equal(t, MechanismHostSAClaimNonUser, v.Result.Mechanism)
		assert.Equal(t, 0, checker.CallCount())
		ev := onlySAEvent(t, audit)
		assert.Equal(t, SurfaceBrokerHostSAUpdate, ev.Surface)
		assert.Equal(t, MechanismHostSAClaimNonUser, ev.Mechanism)
		require.NotNil(t, ev.Decision)
		assert.Equal(t, store.ActAsDenied, *ev.Decision)
	})
}
