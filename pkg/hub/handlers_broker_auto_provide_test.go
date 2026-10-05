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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Turning on a broker's auto-provide setting offers the broker to every
// project on the hub, so POST /api/v1/brokers requires broker.auto_provide
// (super-admin only) whenever a request turns the setting on. Keeping an
// existing setting, or turning it off, needs only registration authority.
// ============================================================================

// setBrokerAutoProvide writes the auto-provide setting of a broker directly
// in the store.
func setBrokerAutoProvide(t *testing.T, s store.Store, brokerID string, on bool) {
	t.Helper()
	ctx := context.Background()
	broker, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	broker.AutoProvide = on
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
}

func getBrokerAutoProvide(t *testing.T, s store.Store, brokerID string) bool {
	t.Helper()
	broker, err := s.GetRuntimeBroker(context.Background(), brokerID)
	require.NoError(t, err)
	return broker.AutoProvide
}

func TestBrokerAutoProvide_HubMemberSessionDeniedForNewBroker(t *testing.T) {
	srv, s := testServer(t)
	member := newHubMemberUser(t, s, "autoprovide-member-new")

	const name = "autoprovide-member-new-broker"
	rec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        name,
		AutoProvide: true,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a hub member must not turn on auto-provide; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "joinToken", "denied response must not carry a join token")
	_, err := s.GetRuntimeBrokerByName(context.Background(), name)
	assert.ErrorIs(t, err, store.ErrNotFound, "a denied registration must not create a broker record")

	// The same member registers without auto-provide.
	rec = doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: name})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestBrokerAutoProvide_HubMemberOwnerDeniedTurningOnForOwnBroker(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "autoprovide-owner-turnon")
	broker := createReregistrationTestBroker(t, s, "autoprovide-owner-turnon-broker", owner.ID)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        broker.Name,
		AutoProvide: true,
		Labels:      map[string]string{"env": "updated"},
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"the broker owner must not turn on auto-provide without broker.auto_provide; got: %s", rec.Body.String())
	assertBrokerUnchanged(t, s, broker.ID)
}

func TestBrokerAutoProvide_SuperAdminSessionAllowed(t *testing.T) {
	srv, s := testServer(t)
	admin := newSuperAdminUser(t, s, "autoprovide-admin-session")

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        "autoprovide-admin-session-broker",
		AutoProvide: true,
	})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, getBrokerAutoProvide(t, s, resp.BrokerID), "a super-admin may turn on auto-provide")
}

func TestBrokerAutoProvide_OwnerKeepsExistingSetting(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "autoprovide-owner-keep")
	broker := createReregistrationTestBroker(t, s, "autoprovide-owner-keep-broker", owner.ID)
	setBrokerAutoProvide(t, s, broker.ID, true)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        broker.Name,
		AutoProvide: true,
	})

	require.Equal(t, http.StatusCreated, rec.Code,
		"keeping an existing auto-provide setting needs no extra permission; got: %s", rec.Body.String())
	assert.True(t, getBrokerAutoProvide(t, s, broker.ID))
}

func TestBrokerAutoProvide_OwnerTurnsOff(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "autoprovide-owner-off")
	broker := createReregistrationTestBroker(t, s, "autoprovide-owner-off-broker", owner.ID)
	setBrokerAutoProvide(t, s, broker.ID, true)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: broker.Name,
	})

	require.Equal(t, http.StatusCreated, rec.Code,
		"turning auto-provide off needs no extra permission; got: %s", rec.Body.String())
	assert.False(t, getBrokerAutoProvide(t, s, broker.ID))
}

func TestBrokerAutoProvide_SuperAdminHubTokenDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	admin := newSuperAdminUser(t, s, "autoprovide-admin-uat")

	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(admin.ID), CreateTokenParams{
		UserID:   admin.ID,
		Name:     "autoprovide-admin-uat",
		Boundary: TokenBoundary{Kind: BoundaryKindHub},
		Scopes:   []string{"broker:create"},
	})
	require.NoError(t, err)

	const name = "autoprovide-admin-uat-broker"
	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        name,
		AutoProvide: true,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"no user access token selector carries broker.auto_provide; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "joinToken", "denied response must not carry a join token")
	_, err = s.GetRuntimeBrokerByName(ctx, name)
	assert.ErrorIs(t, err, store.ErrNotFound, "a denied registration must not create a broker record")
}

// TestBrokerAutoProvide_RegistryRow pins the broker.auto_provide registry
// row and its tables: a hub-level scope permission with no user
// access token selector, not applicable to project targets, and not granted
// by the broker owner relationship.
func TestBrokerAutoProvide_RegistryRow(t *testing.T) {
	var row *permissions.Permission
	for i := range permissions.Registry {
		if permissions.Registry[i].ID == "broker.auto_provide" {
			row = &permissions.Registry[i]
		}
	}
	require.NotNil(t, row, "broker.auto_provide must be registered")
	assert.Equal(t, permissions.ResourceBroker, row.Resource)
	assert.Equal(t, permissions.ActionAutoProvide, row.Action)
	assert.Equal(t, string(ActionAutoProvide), row.Action, "hub and permissions action constants must agree")
	assert.Equal(t, permissions.CapabilityScope, row.CapabilityKind)
	assert.Empty(t, row.UATScope, "broker.auto_provide has no user access token selector")

	applies, known := permissions.AppliesToExistingProjectTarget("broker.auto_provide")
	assert.True(t, known)
	assert.False(t, applies)

	classes, known := permissions.CollectionTargetClassesFor("broker.auto_provide")
	assert.True(t, known)
	assert.Equal(t, []permissions.TargetClassKind{permissions.TargetClassKindHubResource}, classes)

	_, known = permissions.SelectorAllowedBoundaries("broker.auto_provide")
	assert.False(t, known, "broker.auto_provide has no selector, so no mint boundary entry")

	assert.False(t, permissions.RelationshipPolicyAllows("owner", "user", permissions.ResourceBroker, "broker.auto_provide"),
		"the broker owner relationship does not grant broker.auto_provide")
}

// TestBrokerAutoProvide_BuiltInRoles pins that only super-admin holds
// broker.auto_provide among the built-in roles.
func TestBrokerAutoProvide_BuiltInRoles(t *testing.T) {
	for _, role := range BuiltInRoles() {
		if role.Name == store.SystemRoleSuperAdmin {
			assert.Contains(t, role.Permissions, "broker.auto_provide", "super-admin holds broker.auto_provide")
			continue
		}
		assert.NotContains(t, role.Permissions, "broker.auto_provide", "%s must not hold broker.auto_provide", role.Name)
	}
}

// TestBrokerAutoProvide_SeedReconcileGrantsSuperAdmin pins that startup
// reconciliation adds broker.auto_provide to a stored super-admin role
// recorded at revision 1, and that a super-admin is then allowed the
// permission while a hub member is not.
func TestBrokerAutoProvide_SeedReconcileGrantsSuperAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	r1 := make([]string, 0, len(rd.Permissions))
	for _, p := range rd.Permissions {
		if p != "broker.auto_provide" {
			r1 = append(r1, p)
		}
	}
	require.Less(t, len(r1), len(rd.Permissions), "precondition: the seeded super-admin role holds broker.auto_provide")
	require.NoError(t, s.UpdateSystemRoleDefinitionPermissions(ctx, rd.ID, r1))
	recordBuiltInRoleMarker(ctx, s, store.SystemRoleSuperAdmin, builtInRoleMarker{Revision: 1, PermHash: permListHash(r1)})

	admin := newSuperAdminUser(t, s, "autoprovide-reconcile-admin")
	adminIdent := NewAuthenticatedUser(admin.ID, admin.Email, admin.DisplayName, "admin", "api")
	resource := Resource{Type: "broker"}
	d := srv.authzService.CheckAccess(ctx, adminIdent, resource, ActionAutoProvide)
	require.False(t, d.Allowed, "precondition: a revision 1 super-admin role lacks broker.auto_provide: %s", d.Reason)

	reconcileBuiltInRoles(ctx, s)

	rd, err = s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	assert.Contains(t, rd.Permissions, "broker.auto_provide")
	assert.Equal(t, 2, getAppliedBuiltInRoleMarker(ctx, s, store.SystemRoleSuperAdmin).Revision)

	d = srv.authzService.CheckAccess(ctx, adminIdent, resource, ActionAutoProvide)
	assert.True(t, d.Allowed, "a super-admin holds broker.auto_provide after reconciliation: %s", d.Reason)

	member := newHubMemberUser(t, s, "autoprovide-reconcile-member")
	memberIdent := NewAuthenticatedUser(member.ID, member.Email, member.DisplayName, "member", "api")
	d = srv.authzService.CheckAccess(ctx, memberIdent, resource, ActionAutoProvide)
	assert.False(t, d.Allowed, "a hub member does not hold broker.auto_provide: %s", d.Reason)
}
