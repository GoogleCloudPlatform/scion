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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ptone/scion#2138: the broker.create gate and scoped-credential
// restrictions on POST /api/v1/brokers and POST /api/v1/projects/register.
//
// broker.create is a hub-level permission (no UATScope yet: today's UATs are
// project-bound, and enforceUATConstraints already rejects any project-scoped
// UAT against a hub-level resource). These tests exercise that boundary
// through real middleware and handlers — no manually constructed identity
// stands in for the authorization decision itself — and confirm that neither
// the target-owner nor the super-admin shortcut in
// authorizedForBrokerOwnerAction admits a scoped UAT.
// ============================================================================

// TestBrokerCreateGate_ProjectUATLimitedToAgentReadDenied pins the primary
// acceptance criterion: a valid UAT scoped only to agent:read on an unrelated
// project must be denied on first registration, and must leave no broker or
// join-token state behind.
func TestBrokerCreateGate_ProjectUATLimitedToAgentReadDenied(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("gate-uat-proj")
	ownerID := tid("gate-uat-owner")
	createRS1Project(t, s, projectID, ownerID)

	uat := mintScopedUAT(t, srv, ownerID, projectID, []string{"agent:read"})

	const brokerName = "gate-uat-agentread-new-broker"
	rec := doRequestWithToken(t, srv, uat, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: brokerName,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a project UAT scoped only to agent:read must be denied broker registration; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "joinToken", "denied response must not carry a join token")

	_, err := s.GetRuntimeBrokerByName(context.Background(), brokerName)
	assert.ErrorIs(t, err, store.ErrNotFound, "a denied registration must not create a broker record")
}

// TestBrokerCreateGate_OwnerUATCannotRemint pins the end-to-end rule that a
// UAT belonging to the broker's own recorded creator, scoped to agent:read
// (not broker-related at all), must be denied re-registration of that
// broker. The broker.create gate is the first, and today the only, denier
// here: a project-scoped UAT is rejected against this hub-level resource
// before authorizedForBrokerOwnerAction ever runs. The scoped-credential
// exclusion in authorizedForBrokerOwnerAction (see
// TestBrokerCreateGate_OwnerUATCannotRotate) is what enforces the same rule
// on the owner shortcut once a caller reaches that check at all.
func TestBrokerCreateGate_OwnerUATCannotRemint(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("gate-uat-remint-proj")
	ownerID := tid("gate-uat-remint-owner")
	createRS1Project(t, s, projectID, ownerID)
	broker := createReregistrationTestBroker(t, s, "gate-uat-remint-broker", ownerID)

	uat := mintScopedUAT(t, srv, ownerID, projectID, []string{"agent:read"})

	rec := doRequestWithToken(t, srv, uat, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        broker.Name,
		AutoProvide: true,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"the broker owner's own UAT must not re-mint a join token when the UAT lacks broker.create; got: %s", rec.Body.String())
	assertBrokerUnchanged(t, s, broker.ID)
}

// TestBrokerCreateGate_SuperAdminUATCannotRemint pins the end-to-end rule
// that a super-admin's UAT, scoped to agent:read only, must be denied
// re-registering a broker it does not own. As with the owner case above, the
// broker.create gate denies this first; the scoped-credential exclusion in
// authorizedForBrokerOwnerAction (see
// TestBrokerCreateGate_SuperAdminUATCannotRotate) enforces the same rule on
// the admin shortcut once a caller reaches that check at all.
func TestBrokerCreateGate_SuperAdminUATCannotRemint(t *testing.T) {
	srv, s := testServer(t)
	owner := newPlainUser(t, s, "gate-uat-admin-remint-owner")
	broker := createReregistrationTestBroker(t, s, "gate-uat-admin-remint-broker", owner.ID)

	projectID := tid("gate-uat-admin-remint-proj")
	admin := newSuperAdminUser(t, s, "gate-uat-admin-remint-admin")
	rs4AddProjectRole(t, s, admin.ID, projectID, store.ProjectRoleOwner)
	adminUAT := mintScopedUAT(t, srv, admin.ID, projectID, []string{"agent:read"})

	rec := doRequestWithToken(t, srv, adminUAT, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        broker.Name,
		AutoProvide: true,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a super-admin's scoped UAT must not re-mint another user's broker via the admin shortcut; got: %s", rec.Body.String())
	assertBrokerUnchanged(t, s, broker.ID)
}

// TestBrokerCreateGate_OwnerUATCannotRotate confirms the owner shortcut in
// authorizedForBrokerOwnerAction does not admit a scoped UAT for
// rotate-secret. Rotate-secret is a distinct operation from registration and
// is not covered by the broker.create gate, so this exclusion is the only
// check standing between a scoped owner UAT and the broker's HMAC secret: a
// UAT minted by the broker's own recorded creator, scoped to agent:read
// only, must be denied.
func TestBrokerCreateGate_OwnerUATCannotRotate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("gate-uat-owner-rotate-proj")
	ownerID := tid("gate-uat-owner-rotate-owner")
	createRS1Project(t, s, projectID, ownerID)
	broker := &store.RuntimeBroker{ID: tid("gate-uat-owner-rotate-broker"), Name: "Gate UAT Owner Rotate Broker", Slug: "gate-uat-owner-rotate-broker", CreatedBy: ownerID}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	originalKey := seedBrokerSecret(t, s, broker.ID)

	uat := mintScopedUAT(t, srv, ownerID, projectID, []string{"agent:read"})

	rec := doRequestWithToken(t, srv, uat, http.MethodPost, "/api/v1/brokers/"+broker.ID+"/rotate-secret", map[string]interface{}{})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"the broker owner's own UAT must not rotate its secret via the owner shortcut; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secretKey", "denied response must not carry a secret")

	stored, err := s.GetBrokerSecret(ctx, broker.ID)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(originalKey, stored.SecretKey), "the stored secret must not change on a denied rotation")
}

// TestBrokerCreateGate_SuperAdminUATCannotRotate is the rotate-secret
// counterpart for the super-admin shortcut: rotate-secret is a distinct
// operation from registration and is not covered by the broker.create gate,
// so authorizedForBrokerOwnerAction is the only check standing between a
// scoped super-admin UAT and another user's HMAC secret.
func TestBrokerCreateGate_SuperAdminUATCannotRotate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newPlainUser(t, s, "gate-uat-admin-rotate-owner")
	broker := &store.RuntimeBroker{ID: tid("gate-uat-admin-rotate-broker"), Name: "Gate UAT Admin Rotate Broker", Slug: "gate-uat-admin-rotate-broker", CreatedBy: owner.ID}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	originalKey := seedBrokerSecret(t, s, broker.ID)

	projectID := tid("gate-uat-admin-rotate-proj")
	admin := newSuperAdminUser(t, s, "gate-uat-admin-rotate-admin")
	rs4AddProjectRole(t, s, admin.ID, projectID, store.ProjectRoleOwner)
	adminUAT := mintScopedUAT(t, srv, admin.ID, projectID, []string{"agent:read"})

	rec := doRequestWithToken(t, srv, adminUAT, http.MethodPost, "/api/v1/brokers/"+broker.ID+"/rotate-secret", map[string]interface{}{})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a super-admin's scoped UAT must not rotate another user's broker secret via the admin shortcut; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secretKey", "denied response must not carry a secret")

	stored, err := s.GetBrokerSecret(ctx, broker.ID)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(originalKey, stored.SecretKey), "the stored secret must not change on a denied rotation")
}

// TestBrokerRotateSecret_UnrelatedBrokerSelfDenied confirms that an
// authenticated broker's own HMAC identity only satisfies the self shortcut
// for ITS OWN id: broker A's credential must not rotate broker B's secret.
// Driven through real middleware — the request is HMAC-signed with broker
// A's actual stored secret via BrokerAuthService.SignRequest, and dispatched
// through srv.Handler().ServeHTTP, the same as a real broker's outbound
// call, rather than constructing the broker identity by hand.
func TestBrokerRotateSecret_UnrelatedBrokerSelfDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newPlainUser(t, s, "gate-unrelated-self-owner")
	target := &store.RuntimeBroker{ID: tid("gate-unrelated-self-target"), Name: "Gate Unrelated Self Target", Slug: "gate-unrelated-self-target", CreatedBy: owner.ID}
	require.NoError(t, s.CreateRuntimeBroker(ctx, target))
	originalKey := seedBrokerSecret(t, s, target.ID)

	caller := &store.RuntimeBroker{ID: tid("gate-unrelated-self-caller"), Name: "Gate Unrelated Self Caller", Slug: "gate-unrelated-self-caller"}
	require.NoError(t, s.CreateRuntimeBroker(ctx, caller))
	callerKey := seedBrokerSecret(t, s, caller.ID)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/brokers/"+target.ID+"/rotate-secret", nil)
	require.NoError(t, srv.brokerAuthService.SignRequest(req, caller.ID, callerKey))
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"an unrelated broker's own HMAC identity must not rotate another broker's secret; got: %s", rec.Body.String())

	stored, err := s.GetBrokerSecret(ctx, target.ID)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(originalKey, stored.SecretKey), "the stored secret must not change on a denied cross-broker rotation")
}

// ----------------------------------------------------------------------------
// Embedded-broker path: POST /api/v1/projects/register
// ----------------------------------------------------------------------------

// TestProjectRegisterEmbeddedBroker_NoBrokerCreateDenied confirms the
// embedded-broker path enforces the identical broker.create gate as POST
// /brokers: a caller who could pass the project.create gate but lacks
// broker.create must still be denied, and must leave no partial state (no
// project, no broker, no quota reservation). In today's built-in roles this
// is a defense-in-depth case (project.create and broker.create are granted
// together by the hub-member role), pinned with a synthetic role so a future
// role split cannot silently drop the check.
func TestProjectRegisterEmbeddedBroker_NoBrokerCreateDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// A user with project.create but deliberately NOT broker.create — a
	// role shape the built-in roles do not produce today, but the gate must
	// hold even if a future custom role separates the two.
	userID := tid("gate-embedded-no-brokercreate")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: userID + "@test.com", DisplayName: "No Broker Create", Role: "member", Status: "active",
	}))
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "project-create-only-compat",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"project.create"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	user, err := s.GetUser(ctx, userID)
	require.NoError(t, err)

	setUserProjectQuotaCeiling(t, s, 5)
	quotaBefore := countProjectQuotaReservations(t, s, userID)

	const brokerName = "gate-embedded-no-brokercreate-broker"
	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "gate-embedded-no-brokercreate-project",
		Broker: &RegisterProjectBrokerInfo{
			Name:    brokerName,
			Version: "1.0.0",
		},
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"project.create alone must not authorize the embedded broker.create path; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secretKey", "denied response must not carry a secret")

	_, err = s.GetRuntimeBrokerByName(ctx, brokerName)
	assert.ErrorIs(t, err, store.ErrNotFound, "a denied embedded registration must not create a broker record")

	_, err = s.GetProjectBySlugCaseInsensitive(ctx, api.Slugify("gate-embedded-no-brokercreate-project"))
	assert.ErrorIs(t, err, store.ErrNotFound, "a denied embedded registration must not create a project")

	quotaAfter := countProjectQuotaReservations(t, s, userID)
	assert.Equal(t, quotaBefore, quotaAfter, "a denied embedded registration must not consume a quota slot")
}

// TestBrokerCreateGate_RegistryRowHasNoUATScopeYet pins the current
// decision: broker.create has no UATScope yet, so it cannot appear in a
// token-create request or a UAT's effective scopes. ptone/scion#2123
// introduces the hub-bound UAT boundary that broker.create needs before a
// "broker:create" selector can be safely offered.
func TestBrokerCreateGate_RegistryRowHasNoUATScopeYet(t *testing.T) {
	found := false
	for _, p := range permissions.Registry {
		if p.ID != "broker.create" {
			continue
		}
		found = true
		assert.Empty(t, p.UATScope,
			"broker.create must not have a UATScope yet (ptone/scion#2123 introduces the hub-bound UAT boundary it needs first)")
	}
	require.True(t, found, "broker.create must exist in the registry")
}

// TestBrokerCreateGate_ProjectUATCannotMintBrokerCreateScope confirms that
// "broker:create" is rejected outright as an unknown UAT scope: a project
// UAT cannot be minted with it today, independent of any issuer-authority
// check.
func TestBrokerCreateGate_ProjectUATCannotMintBrokerCreateScope(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("gate-mint-brokercreate-proj")
	ownerID := tid("gate-mint-brokercreate-owner")
	createRS1Project(t, s, projectID, ownerID)

	_, _, err := srv.uatService.CreateToken(rs4MintContext(ownerID), ownerID, "gate-mint-test", projectID, []string{"broker:create"}, nil)
	require.Error(t, err, "minting a UAT with scope broker:create must fail")
	assert.ErrorIs(t, err, ErrInvalidUATScope)
}
