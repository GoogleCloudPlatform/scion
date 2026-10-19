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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Workload issuer delegation for the test admin tiers (ptone/scion#4240,
// phase X4). The workload token is x2FakeGoogle's (see
// test_identity_workload_delegation_test.go); everything else (the auth
// arm, the delegation routes, admin-kind issuance, the caps, the sink)
// runs for real against the store. Every identity here is synthetic.

// x4AdminSub is an allow-listed workload subject with max_tier admin;
// x2Sub stays a max_tier member subject.
const x4AdminSub = "100000000000000000091"

// x4Env is a test hub running the member tier and the admin tier
// (super-admin too when asked), A2 in place, a captured test admin audit
// sink and a workload issuer allowlist with one member and one admin
// subject.
type x4Env struct {
	*x2Env
	sink *taSinkBuffer
}

// setupX4 turns srv (test identities on, sink captured in buf) into an X4
// hub.
func setupX4(t *testing.T, srv *Server, s store.Store, buf *taSinkBuffer, superAdmin bool) *x4Env {
	t.Helper()
	srv.testAdminGate = testAdminGate{hubAdmin: true, superAdmin: superAdmin, hubID: taHubID, hubClass: config.HubClassTest}
	require.NoError(t, TestAdminPolicyComplete(), "A2: the test-admin policy classification is complete")
	srv.testAdminPolicyClassified = true
	g := &x2FakeGoogle{now: func() time.Time { return srv.testIdentities.clock() }}
	wi := &workloadIssuerAuthenticator{
		audience:    testIssuerAudience(x2HubID),
		allowlist:   map[string]string{x2Sub: config.WorkloadIssuerTierMember, x4AdminSub: config.WorkloadIssuerTierAdmin},
		validator:   g,
		store:       s,
		now:         func() time.Time { return srv.testIdentities.clock() },
		adminScopes: srv.workloadAdminScopeAvailable,
	}
	srv.workloadIssuer = wi
	srv.authConfig.WorkloadIssuer = wi
	return &x4Env{x2Env: &x2Env{srv: srv, s: s, google: g}, sink: buf}
}

func newX4Env(t *testing.T, superAdmin bool) *x4Env {
	t.Helper()
	srv, s, buf := taServer(t)
	return setupX4(t, srv, s, buf, superAdmin)
}

// createDelegation posts a delegation create as user's session.
func (e *x4Env) createDelegation(t *testing.T, user *store.User, scope string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	if _, ok := body["workloadSub"]; !ok {
		body["workloadSub"] = x4AdminSub
	}
	body["scopes"] = []string{scope}
	return doRequestAsUser(t, e.srv, user, http.MethodPost, "/api/v1/test-identity-delegations", body)
}

// delegateScope creates a delegation of scope, requiring 201.
func (e *x4Env) delegateScope(t *testing.T, user *store.User, scope string, body map[string]any) TestIdentityDelegationView {
	t.Helper()
	rec := e.createDelegation(t, user, scope, body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var v TestIdentityDelegationView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	return v
}

// mintAdmin issues an admin kind of role through delegationID with a fresh
// workload token for the admin subject.
func (e *x4Env) mintAdmin(t *testing.T, delegationID, role string) *httptest.ResponseRecorder {
	t.Helper()
	return e.workload(t, http.MethodPost, taPath, x2Token(t, x2TokenOpts{sub: x4AdminSub}), delegationID, taBody(role), nil)
}

func (e *x4Env) liveAdmins(t *testing.T, kind string) int {
	t.Helper()
	n, err := e.s.CountLiveTestAdminUsers(context.Background(), kind, "", e.srv.testIdentities.clock())
	require.NoError(t, err)
	return n
}

func (e *x4Env) delegationCount(t *testing.T) int {
	t.Helper()
	rows, err := e.s.ListTestIdentityDelegations(context.Background(), "", 1000)
	require.NoError(t, err)
	return len(rows)
}

// x4JSONKeys returns the top-level keys of a JSON object body.
func x4JSONKeys(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &m), string(body))
	return m
}

// --- B1 (X4 AC1): super-admin delegation creation -----------------------------

// A super-admin delegation is created only from a live human super-admin
// interactive session. A hub token, a human holding the capabilities
// through a custom role but no super-admin binding, a demoted super-admin
// and a member test identity are refused, and nothing is written.
func TestWorkloadAdminDelegation_SuperAdminCreateOnlyFromLiveHumanSuperAdminSession(t *testing.T) {
	e := newX4Env(t, true)
	ctx := context.Background()
	super := taUser(t, e.s, "x4-b1-super", store.SystemRoleSuperAdmin)

	d := e.delegateScope(t, super, testIdentityDelegationSuperAdminScope, nil)
	assert.Equal(t, testIdentityDelegationSuperAdminScope, d.Scope)
	assert.Equal(t, super.ID, d.GrantorID)
	assert.True(t, d.Live)
	before := e.delegationCount(t)

	refused := map[string]func() *httptest.ResponseRecorder{}

	// A hub-boundary access token of the same super-admin (not a session).
	pat := mintHubConfigToken(t, e.srv, super.ID, hubBoundary(), "test_identity:issue")
	refused["hub token"] = func() *httptest.ResponseRecorder {
		return doRequestWithToken(t, e.srv, pat, http.MethodPost, "/api/v1/test-identity-delegations",
			map[string]any{"workloadSub": x4AdminSub, "scopes": []string{testIdentityDelegationSuperAdminScope}})
	}

	// A human member holding test_identity.issue and the super-admin
	// issuance capability through a custom system role, with no
	// super-admin binding.
	custom := &store.User{ID: tid("x4-b1-custom"), Email: tid("x4-b1-custom") + "@test.com", DisplayName: "custom", Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, e.s.CreateUser(ctx, custom))
	ensureHubMembership(ctx, e.s, custom.ID)
	rd, err := e.s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		ID: api.NewUUID(), Name: tid("x4-b1-role"), Description: "x4 custom issuer",
		ScopeType: store.RoleScopeSystem, Permissions: []string{permissionTestIdentityIssue, permissionTestIdentityIssueSuperAdmin},
	})
	require.NoError(t, err)
	_, err = e.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: custom.ID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	refused["custom role, not super-admin"] = func() *httptest.ResponseRecorder {
		return e.createDelegation(t, custom, testIdentityDelegationSuperAdminScope, nil)
	}

	// A super-admin whose super-admin binding was removed.
	demoted := taUser(t, e.s, "x4-b1-demoted", store.SystemRoleSuperAdmin)
	taDeleteSystemRole(t, e.s, demoted.ID, store.SystemRoleSuperAdmin)
	refused["demoted super-admin"] = func() *httptest.ResponseRecorder {
		return e.createDelegation(t, demoted, testIdentityDelegationSuperAdminScope, nil)
	}

	// A member test identity (issued through a member delegation).
	member := e.delegateScope(t, super, testIdentityDelegationScope, map[string]any{"workloadSub": x2Sub})
	rec := e.mint(t, member.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	fixtureToken := tiDecode(t, rec).AccessToken
	refused["member test identity"] = func() *httptest.ResponseRecorder {
		return doRequestWithToken(t, e.srv, fixtureToken, http.MethodPost, "/api/v1/test-identity-delegations",
			map[string]any{"workloadSub": x4AdminSub, "scopes": []string{testIdentityDelegationSuperAdminScope}})
	}
	before++ // the member delegation above

	for name, call := range refused {
		rec := call()
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", name, rec.Body.String())
		assert.NotContains(t, x4JSONKeys(t, rec.Body.Bytes()), "id", name)
	}
	assert.Equal(t, before, e.delegationCount(t), "no refused create wrote a delegation")

	// A super-admin binding holder whose stored role is no longer admin
	// passes the route and the capability decision but is not a live
	// super-admin (canIssueTestAdmin): 403 issuer_not_live_super_admin,
	// for both admin scopes.
	notAdmin := taUser(t, e.s, "x4-b1-not-admin", store.SystemRoleSuperAdmin)
	notAdmin.Role = store.UserRoleMember
	require.NoError(t, e.s.UpdateUser(ctx, notAdmin))
	for _, scope := range []string{testIdentityDelegationSuperAdminScope, testIdentityDelegationAdminScope} {
		rec = e.createDelegation(t, notAdmin, scope, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", scope, rec.Body.String())
		assert.Equal(t, testAdminReasonNotLiveAdmin, taReason(t, rec), scope)
	}
	assert.Equal(t, before, e.delegationCount(t))
}

// --- B2 (X4 AC2): C1 on the grantor ---------------------------------------------

// After the grantor is demoted, the next admin mint through its delegation
// gets 403 and creates no identity, no audit record and no sink line.
func TestWorkloadAdminDelegation_GrantorDemotedNextMint403(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		scope, role string
		super       bool
	}{
		{testIdentityDelegationAdminScope, testAdminRoleHubAdmin, false},
		{testIdentityDelegationSuperAdminScope, testAdminRoleSuperAdmin, true},
	} {
		t.Run(tc.role, func(t *testing.T) {
			e := newX4Env(t, tc.super)
			grantor := taUser(t, e.s, "x4-b2-"+tc.role, store.SystemRoleSuperAdmin)
			d := e.delegateScope(t, grantor, tc.scope, nil)
			if tc.scope == testIdentityDelegationAdminScope {
				rec := e.mintAdmin(t, d.ID, tc.role)
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			}

			taDeleteSystemRole(t, e.s, grantor.ID, store.SystemRoleSuperAdmin)
			admins, users := e.liveAdmins(t, ""), e.userCount(t)
			audits, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testAdminIssueMutation})
			require.NoError(t, err)
			lines := len(tsaSinkLines(e.sink, testAdminAuditEventIssue))

			rec := e.mintAdmin(t, d.ID, tc.role)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.NotContains(t, x4JSONKeys(t, rec.Body.Bytes()), "accessToken")
			assert.Equal(t, admins, e.liveAdmins(t, ""), "no admin-kind identity")
			assert.Equal(t, users, e.userCount(t), "no user row")
			after, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testAdminIssueMutation})
			require.NoError(t, err)
			assert.Len(t, after, len(audits), "no issuance audit")
			assert.Len(t, tsaSinkLines(e.sink, testAdminAuditEventIssue), lines, "no issuance sink line")
		})
	}
}

// C1 inside the issuance transaction: recheckTestAdminDelegationGrantor
// reads the grantor through the transaction (on the one-connection SQLite
// store this also shows it never reads outside it) and refuses a demoted,
// suspended or non-admin grantor, a grantor that lost the capability, a
// missing grantor and a test identity.
func TestWorkloadAdminDelegation_InTransactionGrantorRecheck(t *testing.T) {
	e := newX4Env(t, true)
	ctx := context.Background()
	inTx := func(grantorID, permission string) error {
		return e.s.WithTx(ctx, func(tx store.Store) error {
			return e.srv.recheckTestAdminDelegationGrantor(ctx, tx, store.DelegationGrantorHuman, grantorID, permission)
		})
	}

	live := taUser(t, e.s, "x4-c1-live", store.SystemRoleSuperAdmin)
	require.NoError(t, inTx(live.ID, permissionTestIdentityIssueAdmin))
	require.NoError(t, inTx(live.ID, permissionTestIdentityIssueSuperAdmin))
	// Phase DL: a non-human grantor kind is refused before any read, even
	// for a live super-admin's id.
	for _, kind := range []string{store.DelegationGrantorTestInfraOffline, "robot", ""} {
		err := e.s.WithTx(ctx, func(tx store.Store) error {
			return e.srv.recheckTestAdminDelegationGrantor(ctx, tx, kind, live.ID, permissionTestIdentityIssueAdmin)
		})
		require.ErrorIs(t, err, errTestAdminIssuerNotLive, "grantor kind %q", kind)
	}
	assert.ErrorIs(t, inTx(live.ID, "test_identity.no_such_capability"), errTestAdminIssuerNotLive, "capability not held")

	demoted := taUser(t, e.s, "x4-c1-demoted", store.SystemRoleSuperAdmin)
	taDeleteSystemRole(t, e.s, demoted.ID, store.SystemRoleSuperAdmin)
	assert.ErrorIs(t, inTx(demoted.ID, permissionTestIdentityIssueAdmin), errTestAdminIssuerNotLive, "no super-admin binding")

	suspended := taUser(t, e.s, "x4-c1-suspended", store.SystemRoleSuperAdmin)
	suspended.Status = store.UserStatusSuspended
	require.NoError(t, e.s.UpdateUser(ctx, suspended))
	assert.ErrorIs(t, inTx(suspended.ID, permissionTestIdentityIssueAdmin), errTestAdminIssuerNotLive, "suspended")

	member := taUser(t, e.s, "x4-c1-role", store.SystemRoleSuperAdmin)
	member.Role = store.UserRoleMember
	require.NoError(t, e.s.UpdateUser(ctx, member))
	assert.ErrorIs(t, inTx(member.ID, permissionTestIdentityIssueAdmin), errTestAdminIssuerNotLive, "stored role not admin")

	assert.ErrorIs(t, inTx(generateID(), permissionTestIdentityIssueAdmin), errTestAdminIssuerNotLive, "missing grantor")

	issuer := taUser(t, e.s, "x4-c1-issuer", store.SystemRoleSuperAdmin)
	testSuper := tsaIssue(t, e.srv, issuer, testAdminRoleSuperAdmin)
	assert.ErrorIs(t, inTx(testSuper.Identity.ID, permissionTestIdentityIssueAdmin), errTestAdminIssuerNotLive, "a test super-admin is not a human grantor")
}

// x4LockHookStore runs a hook, once and only when armed, inside the
// issuance transaction right after LockTestFixtureIssuance: after the auth
// arm and testAdminIssuer accepted the request, before the handler's
// in-transaction re-checks. The hook writes through the transaction, so a
// refusal rolls its change back with the rest.
type x4LockHookStore struct {
	store.Store
	hook *x4LockHook
}

type x4LockHook struct {
	armed atomic.Bool
	fn    func(ctx context.Context, tx store.Store) error
}

func (s *x4LockHookStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&x4LockHookTx{Store: tx, hook: s.hook})
	})
}

type x4LockHookTx struct {
	store.Store
	hook *x4LockHook
}

func (s *x4LockHookTx) LockTestFixtureIssuance(ctx context.Context) error {
	if err := s.Store.LockTestFixtureIssuance(ctx); err != nil {
		return err
	}
	if s.hook.armed.CompareAndSwap(true, false) && s.hook.fn != nil {
		return s.hook.fn(ctx, s.Store)
	}
	return nil
}

// The handler wires both in-transaction re-checks: a grantor demoted, or a
// delegation revoked, inside the issuance transaction (after the request
// passed the auth arm and every check outside the transaction) mints
// nothing. Without recheckTestAdminDelegationGrantor or
// recheckWorkloadDelegation in the transaction, these mints would succeed.
func TestWorkloadAdminDelegation_InTransactionRechecksWired(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		reason string
		change func(t *testing.T, grantor *store.User, delegationID string) func(ctx context.Context, tx store.Store) error
	}{
		{
			name:   "grantor demoted inside the transaction",
			reason: testAdminReasonNotLiveAdmin,
			change: func(t *testing.T, grantor *store.User, _ string) func(ctx context.Context, tx store.Store) error {
				return func(ctx context.Context, tx store.Store) error {
					rd, err := tx.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
					if err != nil {
						return err
					}
					bindings, err := tx.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, grantor.ID)
					if err != nil {
						return err
					}
					n := 0
					for _, b := range bindings {
						if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
							if err := tx.DeleteRoleBinding(ctx, b.ID); err != nil {
								return err
							}
							n++
						}
					}
					assert.Positive(t, n, "the grantor's super-admin binding was removed")
					return nil
				}
			},
		},
		{
			name: "delegation revoked inside the transaction",
			change: func(t *testing.T, grantor *store.User, delegationID string) func(ctx context.Context, tx store.Store) error {
				return func(ctx context.Context, tx store.Store) error {
					changed, err := tx.RevokeTestIdentityDelegation(ctx, delegationID, grantor.ID, time.Now())
					assert.True(t, changed, "the delegation was revoked")
					return err
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, buf := taServer(t)
			hook := &x4LockHook{}
			installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *x4LockHookStore {
				return &x4LockHookStore{Store: inner, hook: hook}
			})
			e := setupX4(t, srv, s, buf, false)
			grantor := taUser(t, e.s, "x4-wired-grantor", store.SystemRoleSuperAdmin)
			d := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, nil)

			// Positive control: the unarmed hook changes nothing.
			rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			admins, users := e.liveAdmins(t, ""), e.userCount(t)
			audits, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testAdminIssueMutation})
			require.NoError(t, err)
			lines := len(tsaSinkLines(e.sink, testAdminAuditEventIssue))

			hook.fn = tc.change(t, grantor, d.ID)
			hook.armed.Store(true)
			rec = e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
			assert.False(t, hook.armed.Load(), "the hook ran inside the issuance transaction")
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			keys := x4JSONKeys(t, rec.Body.Bytes())
			assert.NotContains(t, keys, "accessToken")
			if tc.reason != "" {
				assert.Equal(t, tc.reason, taReason(t, rec))
			}
			assert.Equal(t, admins, e.liveAdmins(t, ""), "no admin-kind identity")
			assert.Equal(t, users, e.userCount(t), "no user row")
			after, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testAdminIssueMutation})
			require.NoError(t, err)
			assert.Len(t, after, len(audits), "no issuance audit")
			assert.Len(t, tsaSinkLines(e.sink, testAdminAuditEventIssue), lines, "no issuance sink line")
		})
	}
}

// --- B3 (X4 AC3): caps --------------------------------------------------------

// Per-delegation and hub-wide caps, sequentially: a super-admin
// delegation's second test super-admin, a second test super-admin through
// another delegation, and a fifth admin-kind identity across delegations
// each get 429 with their reason; a hub-admin delegation stops at its
// admin cap.
func TestWorkloadAdminDelegation_Caps(t *testing.T) {
	e := newX4Env(t, true)
	grantor := taUser(t, e.s, "x4-b3-grantor", store.SystemRoleSuperAdmin)

	s1 := e.delegateScope(t, grantor, testIdentityDelegationSuperAdminScope, nil)
	assert.Equal(t, 1, s1.AdminCap)
	require.Equal(t, http.StatusCreated, e.mintAdmin(t, s1.ID, testAdminRoleSuperAdmin).Code)
	rec := e.mintAdmin(t, s1.ID, testAdminRoleSuperAdmin)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, testAdminReasonDelegationCap, taReason(t, rec), "per-delegation second super-admin")

	s2 := e.delegateScope(t, grantor, testIdentityDelegationSuperAdminScope, nil)
	rec = e.mintAdmin(t, s2.ID, testAdminRoleSuperAdmin)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, testAdminReasonSuperAdminCap, taReason(t, rec), "hub-wide second super-admin")
	assert.Equal(t, 1, e.liveAdmins(t, store.UserKindTestSuperAdmin))

	h1 := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, nil)
	assert.Equal(t, testHubAdminDelegationMaxIdentityCap, h1.AdminCap)
	for i := 0; i < testHubAdminDelegationMaxIdentityCap; i++ {
		rec := e.mintAdmin(t, h1.ID, testAdminRoleHubAdmin)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}
	rec = e.mintAdmin(t, h1.ID, testAdminRoleHubAdmin)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, testAdminReasonDelegationCap, taReason(t, rec), "hub-admin delegation at its cap")

	h2 := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, map[string]any{"adminCap": 1})
	assert.Equal(t, 1, h2.AdminCap)
	require.Equal(t, http.StatusCreated, e.mintAdmin(t, h2.ID, testAdminRoleHubAdmin).Code)
	assert.Equal(t, testAdminHubCap, e.liveAdmins(t, ""), "1 super-admin + 3 hub-admins")

	h3 := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, nil)
	rec = e.mintAdmin(t, h3.ID, testAdminRoleHubAdmin)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, testAdminReasonHubCap, taReason(t, rec), "hub-wide fifth admin-kind identity")
	assert.Equal(t, testAdminHubCap, e.liveAdmins(t, ""))
}

// runX4ParallelAdminMints fires parallel admin-kind mints across three
// hub-admin delegations (cap 2 each) and two super-admin delegations, all
// released at once, and checks that the hub-wide caps hold (exactly 4
// admin-kind identities, at most 1 test super-admin), that no delegation
// exceeds its cap, and that every other answer is 429.
func runX4ParallelAdminMints(t *testing.T, e *x4Env) {
	t.Helper()
	ctx := context.Background()
	grantor := taUser(t, e.s, "x4-par-grantor", store.SystemRoleSuperAdmin)
	type attempt struct{ delegation, role string }
	var attempts []attempt
	caps := map[string]int{}
	for i := 0; i < 3; i++ {
		d := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, nil)
		caps[d.ID] = d.AdminCap
		for j := 0; j < 3; j++ {
			attempts = append(attempts, attempt{d.ID, testAdminRoleHubAdmin})
		}
	}
	for i := 0; i < 2; i++ {
		d := e.delegateScope(t, grantor, testIdentityDelegationSuperAdminScope, nil)
		caps[d.ID] = d.AdminCap
		for j := 0; j < 2; j++ {
			attempts = append(attempts, attempt{d.ID, testAdminRoleSuperAdmin})
		}
	}
	codes := make([]int, len(attempts))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, a := range attempts {
		wg.Add(1)
		go func(i int, a attempt) {
			defer wg.Done()
			<-start
			codes[i] = e.mintAdmin(t, a.delegation, a.role).Code
		}(i, a)
	}
	close(start)
	wg.Wait()

	created := 0
	for i, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusTooManyRequests:
		default:
			t.Errorf("attempt %d (%s): unexpected status %d", i, attempts[i].role, c)
		}
	}
	now := e.srv.testIdentities.clock()
	assert.Equal(t, testAdminHubCap, created, "exactly the hub-wide admin cap is issued")
	assert.Equal(t, testAdminHubCap, e.liveAdmins(t, ""))
	assert.LessOrEqual(t, e.liveAdmins(t, store.UserKindTestSuperAdmin), testSuperAdminHubCap)
	for id, c := range caps {
		n, err := e.s.CountLiveTestAdminUsersByDelegation(ctx, id, now)
		require.NoError(t, err)
		assert.LessOrEqual(t, n, c, "delegation %s", id)
	}
}

func TestWorkloadAdminDelegation_ParallelMintsHoldCaps(t *testing.T) {
	runX4ParallelAdminMints(t, newX4Env(t, true))
}

// The same parallel mints on Postgres (T1 allow-list:
// TEST_IDENTITY_POSTGRES_TESTS), where the issuance advisory lock is what
// serializes them.
func TestWorkloadAdminDelegation_ParallelMintsHoldCaps_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	srv, s := testServerWithStore(t, entadapter.NewCompositeStore(enttest.NewClient(t)))
	enableTestIdentitiesForTest(srv)
	buf := &taSinkBuffer{}
	srv.testAdminAudit = newTestAdminAuditSink(buf)
	runX4ParallelAdminMints(t, setupX4(t, srv, s, buf, true))
}

// --- B4 (X4 AC4): the cage ------------------------------------------------------

// A test super-admin (and a test hub-admin) calling delegation create on its
// interactive credential gets 403, and nothing is written. For the test
// super-admin, which holds every permission through its super-admin
// binding, the refusal is the test-admin policy's: the route's
// test_identity.issue is deny-classified for the admin kinds, and the sink
// has a test_admin_policy_denial line for that permission.
func TestWorkloadAdminDelegation_TestSuperAdminCreateRefusedByCage(t *testing.T) {
	e := newX4Env(t, true)
	issuer := taUser(t, e.s, "x4-b4-issuer", store.SystemRoleSuperAdmin)
	issuer2 := taUser(t, e.s, "x4-b4-issuer2", store.SystemRoleSuperAdmin)
	superKind := tsaIssue(t, e.srv, issuer, testAdminRoleSuperAdmin)
	hubKind := tsaIssue(t, e.srv, issuer2, testAdminRoleHubAdmin)
	before := e.delegationCount(t)

	for _, who := range []TestAdminIdentityTokenResponse{superKind, hubKind} {
		for _, scope := range []string{testIdentityDelegationSuperAdminScope, testIdentityDelegationAdminScope, testIdentityDelegationScope} {
			rec := doRequestWithToken(t, e.srv, who.AccessToken, http.MethodPost, "/api/v1/test-identity-delegations",
				map[string]any{"workloadSub": x4AdminSub, "scopes": []string{scope}})
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", who.Identity.Kind, scope, rec.Body.String())
		}
		rec := doRequestWithToken(t, e.srv, who.AccessToken, http.MethodGet, "/api/v1/test-identity-delegations", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s list: %s", who.Identity.Kind, rec.Body.String())
	}
	assert.Equal(t, before, e.delegationCount(t), "nothing written")

	found := false
	for _, l := range tsaSinkLines(e.sink, testAdminAuditEventPolicyDenial) {
		if l["issuance_id"] == superKind.Identity.IssuanceID && l["class"] == "permission" && l["subject"] == permissionTestIdentityIssue {
			found = true
		}
	}
	assert.True(t, found, "the test super-admin's refusal is the test-admin policy's (sink policy denial of %s)", permissionTestIdentityIssue)
}

// --- B5 (X4 AC5): the A2 sink line and the audit ------------------------------

// Every admin-kind issuance through a delegation writes a test_admin_issue
// sink line carrying the delegation id (and the workload subject), and a
// test_admin_issue audit record whose issuer credential is the delegation
// and whose workload part names it; the identity is stamped with it.
func TestWorkloadAdminDelegation_IssuanceSinkLineAndAudit(t *testing.T) {
	ctx := context.Background()
	e := newX4Env(t, true)
	grantor := taUser(t, e.s, "x4-b5-grantor", store.SystemRoleSuperAdmin)
	for _, tc := range []struct{ scope, role, kind string }{
		{testIdentityDelegationAdminScope, testAdminRoleHubAdmin, store.UserKindTestHubAdmin},
		{testIdentityDelegationSuperAdminScope, testAdminRoleSuperAdmin, store.UserKindTestSuperAdmin},
	} {
		d := e.delegateScope(t, grantor, tc.scope, nil)
		rec := e.workload(t, http.MethodPost, taPath, x2Token(t, x2TokenOpts{sub: x4AdminSub}), d.ID, taBody(tc.role),
			map[string]string{testIssuerRunLabelHeader: "x4-run"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		resp := taDecode(t, rec)
		assert.Equal(t, tc.kind, resp.Identity.Kind)
		assert.Equal(t, d.ID, resp.Identity.IssuerDelegationID, "the response view names the delegation")
		assert.NotEmpty(t, resp.AccessToken)

		row, err := e.s.GetUser(ctx, resp.Identity.ID)
		require.NoError(t, err)
		require.NotNil(t, row.IssuerDelegationID)
		assert.Equal(t, d.ID, *row.IssuerDelegationID)
		require.NotNil(t, row.IssuerCredentialID)
		assert.Equal(t, d.ID, *row.IssuerCredentialID, "C4: the credential is the delegation")
		require.NotNil(t, row.IssuedBy)
		assert.Equal(t, grantor.ID, *row.IssuedBy)

		var line map[string]any
		for _, l := range tsaSinkLines(e.sink, testAdminAuditEventIssue) {
			if l["test_identity_id"] == resp.Identity.ID {
				line = l
			}
		}
		require.NotNil(t, line, "a test_admin_issue sink line for %s", resp.Identity.ID)
		assert.Equal(t, d.ID, line["issuer_delegation_id"])
		assert.Equal(t, d.ID, line["issuer_credential_id"])
		assert.Equal(t, x4AdminSub, line["workload_sub"])
		assert.Equal(t, grantor.ID, line["issuer_id"])
		assert.Equal(t, tc.kind, line["kind"])

		audits, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testAdminIssueMutation, TargetID: resp.Identity.ID})
		require.NoError(t, err)
		require.Len(t, audits, 1)
		var summary struct {
			IssuerCredentialID string `json:"issuer_credential_id"`
			Workload           *struct {
				DelegationID string `json:"delegation_id"`
				Grantor      string `json:"grantor"`
				WorkloadSub  string `json:"workload_sub"`
				RunLabel     string `json:"run_label"`
			} `json:"workload_delegation"`
		}
		require.NoError(t, json.Unmarshal([]byte(audits[0].AfterSummary), &summary))
		assert.Equal(t, d.ID, summary.IssuerCredentialID)
		require.NotNil(t, summary.Workload)
		assert.Equal(t, d.ID, summary.Workload.DelegationID)
		assert.Equal(t, grantor.ID, summary.Workload.Grantor)
		assert.Equal(t, x4AdminSub, summary.Workload.WorkloadSub)
		assert.Equal(t, "x4-run", summary.Workload.RunLabel)
	}

	// A session issuance's line carries no delegation fields.
	issuer := taUser(t, e.s, "x4-b5-session", store.SystemRoleSuperAdmin)
	resp := taIssue(t, e.srv, issuer)
	for _, l := range tsaSinkLines(e.sink, testAdminAuditEventIssue) {
		if l["test_identity_id"] == resp.Identity.ID {
			assert.NotContains(t, l, "issuer_delegation_id")
			assert.NotContains(t, l, "workload_sub")
		}
	}
}

// --- B7: scope gating and the member tier --------------------------------------

// Admin scopes are refused below their --test-mode level and without A2,
// at create (422) and, for an existing delegation, at mint (403); a
// max_tier member subject never takes an admin scope.
func TestWorkloadAdminDelegation_ScopesGatedByLevelAndA2(t *testing.T) {
	// Member level only (no admin tier): both admin scopes 422.
	{
		e := newX4Env(t, false)
		e.srv.testAdminGate = testAdminGate{}
		super := taUser(t, e.s, "x4-b7-member-level", store.SystemRoleSuperAdmin)
		for _, scope := range []string{testIdentityDelegationAdminScope, testIdentityDelegationSuperAdminScope} {
			rec := e.createDelegation(t, super, scope, nil)
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "%s: %s", scope, rec.Body.String())
			assert.Equal(t, testIdentityDelegationReasonAdminTier, taReason(t, rec), scope)
		}
		e.delegateScope(t, super, testIdentityDelegationScope, map[string]any{"workloadSub": x2Sub})
	}

	// Admin level: hub-admin yes, super-admin 422.
	e := newX4Env(t, false)
	super := taUser(t, e.s, "x4-b7-admin-level", store.SystemRoleSuperAdmin)
	hd := e.delegateScope(t, super, testIdentityDelegationAdminScope, nil)
	rec := e.createDelegation(t, super, testIdentityDelegationSuperAdminScope, nil)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, testIdentityDelegationReasonAdminTier, taReason(t, rec))

	// A max_tier member subject: 422 for an admin scope.
	rec = e.createDelegation(t, super, testIdentityDelegationAdminScope, map[string]any{"workloadSub": x2Sub})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, testIdentityDelegationReasonSub, taReason(t, rec))

	// Without A2 (the test-admin policy classification not complete): 422
	// at create, 403 at mint on the existing delegation, and it lists as
	// not live.
	e.srv.testAdminPolicyClassified = false
	rec = e.createDelegation(t, super, testIdentityDelegationAdminScope, nil)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, testIdentityDelegationReasonAdminTier, taReason(t, rec))
	before := e.liveAdmins(t, "")
	rec = e.mintAdmin(t, hd.ID, testAdminRoleHubAdmin)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, before, e.liveAdmins(t, ""))
	rec = doRequestAsUser(t, e.srv, super, http.MethodGet, "/api/v1/test-identity-delegations", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list ListTestIdentityDelegationsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	for _, it := range list.Items {
		if it.ID == hd.ID {
			assert.False(t, it.Live, "an unavailable admin scope is not live")
		}
	}
	e.srv.testAdminPolicyClassified = true
	require.Equal(t, http.StatusCreated, e.mintAdmin(t, hd.ID, testAdminRoleHubAdmin).Code, "available again")

	// A super-admin delegation created at the super-admin level mints
	// nothing once the hub runs at the admin level (a restart below it).
	e2 := newX4Env(t, true)
	super2 := taUser(t, e2.s, "x4-b7-restart", store.SystemRoleSuperAdmin)
	sd := e2.delegateScope(t, super2, testIdentityDelegationSuperAdminScope, nil)
	e2.srv.testAdminGate = testAdminGate{hubAdmin: true, hubID: taHubID, hubClass: config.HubClassTest}
	rec = e2.mintAdmin(t, sd.ID, testAdminRoleSuperAdmin)
	assert.NotEqual(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Zero(t, e2.liveAdmins(t, store.UserKindTestSuperAdmin))
}

// A delegation's scope bounds the kind: a hub-admin delegation cannot mint
// a test super-admin, and a member delegation cannot reach admin issuance.
func TestWorkloadAdminDelegation_ScopeBoundsTheKind(t *testing.T) {
	e := newX4Env(t, true)
	super := taUser(t, e.s, "x4-kind-grantor", store.SystemRoleSuperAdmin)
	hd := e.delegateScope(t, super, testIdentityDelegationAdminScope, nil)
	md := e.delegateScope(t, super, testIdentityDelegationScope, map[string]any{"workloadSub": x2Sub})

	rec := e.mintAdmin(t, hd.ID, testAdminRoleSuperAdmin)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	for _, role := range []string{testAdminRoleHubAdmin, testAdminRoleSuperAdmin} {
		rec = e.workload(t, http.MethodPost, taPath, x2Token(t, x2TokenOpts{}), md.ID, taBody(role), nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "member delegation, %s: %s", role, rec.Body.String())
	}
	// The admin subject's token on the member delegation: wrong subject.
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities", x2Token(t, x2TokenOpts{sub: x4AdminSub}), md.ID, map[string]string{"role": "member"}, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	// revoke-all is never a workload route.
	rec = e.workload(t, http.MethodPost, taRevokeAllPath, x2Token(t, x2TokenOpts{sub: x4AdminSub}), hd.ID, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Zero(t, e.liveAdmins(t, ""))
}

// Admin-scope delegations are issue-only in X4: member issuance, list,
// token re-issue, delete and the web-login code refuse them (403), even for
// the identities they issued.
func TestWorkloadAdminDelegation_IssueOnly(t *testing.T) {
	e := newX4Env(t, true)
	x2EnableWebSessions(t, e.x2Env)
	ctx := context.Background()
	super := taUser(t, e.s, "x4-io-grantor", store.SystemRoleSuperAdmin)
	hd := e.delegateScope(t, super, testIdentityDelegationAdminScope, nil)
	rec := e.mintAdmin(t, hd.ID, testAdminRoleHubAdmin)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	adminID := taDecode(t, rec).Identity.ID
	tok := func() string { return x2Token(t, x2TokenOpts{sub: x4AdminSub}) }
	fixtures := e.liveFixtures(t)

	for name, call := range map[string]func() *httptest.ResponseRecorder{
		"member issue": func() *httptest.ResponseRecorder {
			return e.workload(t, http.MethodPost, "/api/v1/test-identities", tok(), hd.ID, map[string]string{"role": "member"}, nil)
		},
		"list": func() *httptest.ResponseRecorder {
			return e.workload(t, http.MethodGet, "/api/v1/test-identities", tok(), hd.ID, nil, nil)
		},
		"token": func() *httptest.ResponseRecorder {
			return e.workload(t, http.MethodPost, "/api/v1/test-identities/"+adminID+"/token", tok(), hd.ID, nil, nil)
		},
		"delete": func() *httptest.ResponseRecorder {
			return e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+adminID, tok(), hd.ID, nil, nil)
		},
		"web-code": func() *httptest.ResponseRecorder {
			return e.workload(t, http.MethodPost, "/api/v1/test-identities/"+adminID+"/web-login-code", tok(), hd.ID, nil, nil)
		},
	} {
		got := call()
		assert.Equal(t, http.StatusForbidden, got.Code, "%s: %s", name, got.Body.String())
		keys := x4JSONKeys(t, got.Body.Bytes())
		assert.NotContains(t, keys, "accessToken", name)
		assert.NotContains(t, keys, "items", name)
		assert.NotContains(t, keys, "code", name)
	}
	_, err := e.s.GetUser(ctx, adminID)
	require.NoError(t, err, "nothing deleted")
	assert.Equal(t, fixtures, e.liveFixtures(t), "nothing issued")

	// The handler's own check, independent of the route guard: an
	// admin-scope workload delegation is refused by testIdentityCaller; a
	// member-scope one is admitted.
	callerFor := func(d TestIdentityDelegationView, sub string) bool {
		dl, err := e.s.GetTestIdentityDelegation(ctx, d.ID)
		require.NoError(t, err)
		base := NewAuthenticatedUser(super.ID, super.Email, super.DisplayName, super.Role, string(ClientTypeAPI))
		ceiling, ok := permissions.BuildCeilingFromSelectors([]string{dl.Scope})
		require.True(t, ok)
		ident := NewScopedUserIdentityWithBoundary(base, TokenBoundary{Kind: BoundaryKindHub}, []string{dl.Scope}, dl.ID, ceiling)
		cred := credentialContextForIdentity(ident)
		cred.Kind = CredentialKindWorkloadDelegation
		cred.Type = string(CredentialKindWorkloadDelegation)
		c := context.WithValue(context.Background(), userContextKey{}, ident)
		c = contextWithIdentity(c, ident)
		c = contextWithCredentialContext(c, cred)
		c = contextWithWorkloadDelegationState(c, &workloadDelegationState{delegation: dl, sub: sub})
		r := httptest.NewRequest(http.MethodGet, "/api/v1/test-identities", nil).WithContext(c)
		w := httptest.NewRecorder()
		_, admitted := e.srv.testIdentityCaller(w, r)
		if !admitted {
			assert.Equal(t, http.StatusForbidden, w.Code)
		}
		return admitted
	}
	md := e.delegateScope(t, super, testIdentityDelegationScope, map[string]any{"workloadSub": x2Sub})
	assert.True(t, callerFor(md, x2Sub), "member-scope delegation admitted")
	assert.False(t, callerFor(hd, x4AdminSub), "admin-scope delegation refused")

	// Purge of the admin-kind identity (admin tier A3 admits
	// DELETE ?purge=true for admin kinds, for its issuer or a super-admin):
	// the admin-scope delegation is refused (403); the member-scope
	// delegation of the same grantor gets the empty report (the A3 purge
	// check refuses every workload delegation caller) and purges nothing.
	purges := func() int {
		audits, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityPurgeMutation})
		require.NoError(t, err)
		return len(audits)
	}
	before := purges()
	rec = e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+adminID+"?purge=true", tok(), hd.ID, nil, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "admin-scope delegation purge: %s", rec.Body.String())
	assert.NotContains(t, x4JSONKeys(t, rec.Body.Bytes()), "identityDeleted")
	rec = e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+adminID+"?purge=true", x2Token(t, x2TokenOpts{}), md.ID, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "member-scope delegation purge: %s", rec.Body.String())
	var report TestIdentityPurgeReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report))
	assert.Equal(t, *emptyTestIdentityPurgeReport(adminID), report, "the empty report")
	assert.False(t, report.IdentityDeleted, "the member-scope delegation deletes nothing")
	_, err = e.s.GetUser(ctx, adminID)
	require.NoError(t, err, "the admin-kind identity still exists")
	assert.Equal(t, before, purges(), "no purge audit")
	// Positive control: the grantor's own session purges it.
	rec = doRequestAsUser(t, e.srv, super, http.MethodDelete, "/api/v1/test-identities/"+adminID+"?purge=true", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	report = TestIdentityPurgeReport{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report))
	assert.True(t, report.IdentityDeleted, "the grantor's session purges its admin-kind identity")
}

// Member-tier X2 behaviour is unchanged on an X4 hub: a member delegation
// mints a member, its view carries memberCap and no adminCap, and the
// admin subject (max_tier admin) also covers the member scope.
func TestWorkloadAdminDelegation_MemberTierUnchanged(t *testing.T) {
	e := newX4Env(t, true)
	super := taUser(t, e.s, "x4-member-grantor", store.SystemRoleSuperAdmin)
	rec := e.createDelegation(t, super, testIdentityDelegationScope, map[string]any{"workloadSub": x2Sub, "memberCap": 3})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	keys := x4JSONKeys(t, rec.Body.Bytes())
	assert.Contains(t, keys, "memberCap")
	assert.NotContains(t, keys, "adminCap")
	var md TestIdentityDelegationView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &md))
	assert.Equal(t, 3, md.MemberCap)
	assert.WithinDuration(t, time.Now().Add(testIdentityDelegationDefaultTTL), md.ExpiresAt, time.Minute)

	rec = e.mint(t, md.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, store.UserRoleMember, tiDecode(t, rec).Identity.Role)

	ad := e.delegateScope(t, super, testIdentityDelegationScope, nil) // admin subject, member scope
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities", x2Token(t, x2TokenOpts{sub: x4AdminSub}), ad.ID, map[string]string{"role": "viewer"}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Zero(t, e.liveAdmins(t, ""))
}

// --- B8: lifetimes ------------------------------------------------------------

// Admin delegation lifetimes and caps are validated at create: hub-admin
// at most 8 h (default 4 h), super-admin at most 2 h (default 1 h); the
// admin cap at most 2 (hub-admin) or 1 (super-admin), and the cap of the
// other tier is refused.
func TestWorkloadAdminDelegation_CreateLifetimesAndCaps(t *testing.T) {
	e := newX4Env(t, true)
	super := taUser(t, e.s, "x4-b8-create", store.SystemRoleSuperAdmin)

	hd := e.delegateScope(t, super, testIdentityDelegationAdminScope, nil)
	assert.WithinDuration(t, time.Now().Add(4*time.Hour), hd.ExpiresAt, time.Minute)
	sd := e.delegateScope(t, super, testIdentityDelegationSuperAdminScope, nil)
	assert.WithinDuration(t, time.Now().Add(time.Hour), sd.ExpiresAt, time.Minute)
	assert.Equal(t, 1, sd.AdminCap)
	longest := e.delegateScope(t, super, testIdentityDelegationAdminScope, map[string]any{"ttlSeconds": int64(8 * 3600)})
	assert.WithinDuration(t, time.Now().Add(8*time.Hour), longest.ExpiresAt, time.Minute)
	e.delegateScope(t, super, testIdentityDelegationSuperAdminScope, map[string]any{"ttlSeconds": int64(2 * 3600)})

	for name, tc := range map[string]struct {
		scope  string
		body   map[string]any
		field  string
		reason string
	}{
		"hub-admin ttl above 8 h":    {testIdentityDelegationAdminScope, map[string]any{"ttlSeconds": int64(8*3600 + 1)}, "ttlSeconds", testIdentityDelegationReasonTTL},
		"super-admin ttl above 2 h":  {testIdentityDelegationSuperAdminScope, map[string]any{"ttlSeconds": int64(2*3600 + 1)}, "ttlSeconds", testIdentityDelegationReasonTTL},
		"ttl below a minute":         {testIdentityDelegationAdminScope, map[string]any{"ttlSeconds": 59}, "ttlSeconds", testIdentityDelegationReasonTTL},
		"hub-admin cap 3":            {testIdentityDelegationAdminScope, map[string]any{"adminCap": 3}, "adminCap", testIdentityDelegationReasonAdminCap},
		"super-admin cap 2":          {testIdentityDelegationSuperAdminScope, map[string]any{"adminCap": 2}, "adminCap", testIdentityDelegationReasonAdminCap},
		"negative admin cap":         {testIdentityDelegationAdminScope, map[string]any{"adminCap": -1}, "adminCap", testIdentityDelegationReasonAdminCap},
		"member cap on admin scope":  {testIdentityDelegationAdminScope, map[string]any{"memberCap": 1}, "memberCap", testIdentityDelegationReasonAdminCap},
		"admin cap on member scope":  {testIdentityDelegationScope, map[string]any{"workloadSub": x2Sub, "adminCap": 1}, "adminCap", testIdentityDelegationReasonCap},
		"unknown scope":              {"test_identity:issue_root", nil, "scopes", testIdentityDelegationReasonScope},
		"member ttl above 7 d stays": {testIdentityDelegationScope, map[string]any{"workloadSub": x2Sub, "ttlSeconds": int64(7*24*3600 + 1)}, "ttlSeconds", testIdentityDelegationReasonTTL},
	} {
		rec := e.createDelegation(t, super, tc.scope, tc.body)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "%s: %s", name, rec.Body.String())
		var body struct {
			Error struct {
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), name)
		assert.Equal(t, tc.field, body.Error.Details["field"], name)
		assert.Equal(t, tc.reason, body.Error.Details["reason"], name)
	}

	// The admin view names the cap adminCap, never memberCap.
	rec := e.createDelegation(t, super, testIdentityDelegationAdminScope, map[string]any{"adminCap": 1})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	keys := x4JSONKeys(t, rec.Body.Bytes())
	assert.Contains(t, keys, "adminCap")
	assert.NotContains(t, keys, "memberCap")
}

// Admin delegation lifetimes are enforced at every mint: past the stored
// expiry, and past the scope's maximum age from creation even when the
// stored expiry is later (only the store could write such a row).
func TestWorkloadAdminDelegation_MintLifetimes(t *testing.T) {
	e := newX4Env(t, true)
	ctx := context.Background()
	super := taUser(t, e.s, "x4-b8-mint", store.SystemRoleSuperAdmin)

	d := e.delegateScope(t, super, testIdentityDelegationAdminScope, map[string]any{"ttlSeconds": 120})
	require.Equal(t, http.StatusCreated, e.mintAdmin(t, d.ID, testAdminRoleHubAdmin).Code)
	e.srv.testIdentities.now = func() time.Time { return time.Now().Add(3 * time.Minute) }
	e.google.now = time.Now // the workload token itself stays valid
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	assert.Equal(t, http.StatusForbidden, rec.Code, "past the stored expiry: %s", rec.Body.String())
	e.srv.testIdentities.now = time.Now

	now := time.Now()
	for _, tc := range []struct {
		name, scope, role string
		age               time.Duration
		want              int
	}{
		{"hub-admin just under 8 h", testIdentityDelegationAdminScope, testAdminRoleHubAdmin, testHubAdminDelegationMaxAge - time.Minute, http.StatusCreated},
		{"hub-admin past 8 h", testIdentityDelegationAdminScope, testAdminRoleHubAdmin, testHubAdminDelegationMaxAge + time.Minute, http.StatusForbidden},
		{"super-admin past 2 h", testIdentityDelegationSuperAdminScope, testAdminRoleSuperAdmin, testSuperAdminDelegationMaxAge + time.Minute, http.StatusForbidden},
		{"super-admin just under 2 h", testIdentityDelegationSuperAdminScope, testAdminRoleSuperAdmin, testSuperAdminDelegationMaxAge - time.Minute, http.StatusCreated},
	} {
		row := &store.TestIdentityDelegation{GrantorID: super.ID, WorkloadSub: x4AdminSub, Scope: tc.scope, MemberCap: 1,
			Created: now.Add(-tc.age), ExpiresAt: now.Add(time.Hour)}
		require.NoError(t, e.s.CreateTestIdentityDelegation(ctx, row))
		rec := e.mintAdmin(t, row.ID, tc.role)
		assert.Equal(t, tc.want, rec.Code, "%s: %s", tc.name, rec.Body.String())
	}
}
