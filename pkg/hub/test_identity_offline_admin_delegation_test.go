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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/testidentitydelegation"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/testinfra"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Offline hub-admin test identity delegations
// (test_identity_offline_admin_delegation.go). The marker and delegations
// are written with the real offline writers (pkg/testinfra) on the test
// hub's own database; the workload token is x2FakeGoogle's; issuance,
// caps, audit and sink run for real.

// oadIssuers is the allowlist the offline writer checks: x4AdminSub at the
// admin tier, x2Sub at the member tier.
var oadIssuers = []config.TestIdentityWorkloadIssuer{
	{Sub: x2Sub, MaxTier: config.WorkloadIssuerTierMember},
	{Sub: x4AdminSub, MaxTier: config.WorkloadIssuerTierAdmin},
}

// superAdminVariants are spellings of the test super-admin permission or
// scope; every offline hub-admin check refuses each one.
var superAdminVariants = []string{
	permissionTestIdentityIssueSuperAdmin,
	testIdentityDelegationSuperAdminScope,
	"TEST_IDENTITY.ISSUE_SUPER_ADMIN",
	"Test_Identity:Issue_Super_Admin",
	" test_identity.issue_super_admin",
	"test_identity.issue_super_admin ",
	"test_identity.issue_super-admin",
	"test_identity.issue_superadmin",
}

// oadEnv is an x4Env (admin tier, A2, captured sink) at
// --test-mode=admin with a marker for x2HubID, the offline gate wired as
// New wires it, and the offline hub-admin switch as given.
type oadEnv struct {
	*x4Env
	client *ent.Client
}

func newOADEnv(t *testing.T, on bool) *oadEnv {
	t.Helper()
	return oadEnvFrom(t, newX4Env(t, false), on)
}

// oadEnvFrom turns an x4Env into an oadEnv (see newOADEnv).
func oadEnvFrom(t *testing.T, e *x4Env, on bool) *oadEnv {
	t.Helper()
	e.srv.testMode = testModeState{level: TestModeAdmin}
	wi := e.srv.workloadIssuer
	wi.hubID = x2HubID
	wi.testModeLevel = e.srv.TestModeLevel
	srv := e.srv
	wi.writeAudit = func(ctx context.Context, st store.Store, record *store.MutationAuditRecord) error {
		return srv.testIdentities.hooks.writeAudit(ctx, st, record)
	}
	env := &oadEnv{x4Env: e, client: dlEntClient(t, e.s)}
	env.setSwitch(on)
	env.mark(t)
	return env
}

// setSwitch sets the offline hub-admin switch as New would.
func (e *oadEnv) setSwitch(on bool) {
	e.srv.offlineAdmin = offlineAdminState{on: on}
	e.srv.workloadIssuer.offlineAdmin = on
}

func (e *oadEnv) mark(t *testing.T) {
	t.Helper()
	_, err := testinfra.Mark(context.Background(), e.client, testinfra.MarkInput{HubID: x2HubID, Operator: "oad-test", Host: "oad-host", Now: time.Now()})
	require.NoError(t, err)
}

func (e *oadEnv) unmark(t *testing.T) {
	t.Helper()
	_, err := testinfra.Unmark(context.Background(), e.client, testinfra.UnmarkInput{Operator: "oad-test", Host: "oad-host"})
	require.NoError(t, err)
}

// offlineAdmin creates an offline hub-admin delegation for x4AdminSub with
// the offline writer.
func (e *oadEnv) offlineAdmin(t *testing.T, adminCap int) *store.TestIdentityDelegation {
	t.Helper()
	d, err := testinfra.Delegate(context.Background(), e.client, testinfra.DelegateInput{
		HubID: x2HubID, WorkloadSub: x4AdminSub, WorkloadIssuers: oadIssuers,
		Scope: store.TestInfraOfflineAdminDelegationScope, AdminOptIn: x2HubID, AdminCap: adminCap,
		Operator: "oad-op", Host: "oad-host", Now: time.Now(),
	})
	require.NoError(t, err)
	return d
}

// offlineMember creates an offline member delegation for x2Sub.
func (e *oadEnv) offlineMember(t *testing.T) *store.TestIdentityDelegation {
	t.Helper()
	d, err := testinfra.Delegate(context.Background(), e.client, testinfra.DelegateInput{
		HubID: x2HubID, WorkloadSub: x2Sub, WorkloadIssuers: oadIssuers,
		Operator: "oad-op", Host: "oad-host", Now: time.Now(),
	})
	require.NoError(t, err)
	return d
}

// forge writes an offline delegation row directly (bypassing the writer's
// checks), bound to the current marker, with the given scope.
func (e *oadEnv) forge(t *testing.T, scope string) *store.TestIdentityDelegation {
	t.Helper()
	ctx := context.Background()
	marker, err := e.client.TestInfraMarker.Get(ctx, testinfra.MarkerID)
	require.NoError(t, err)
	now := time.Now().UTC()
	row, err := e.client.TestIdentityDelegation.Create().
		SetID(uuid.New()).
		SetGrantorID(store.TestInfraOfflineGrantorID(x2HubID)).
		SetWorkloadSub(x4AdminSub).
		SetScope(scope).
		SetMemberCap(1).
		SetCreatedAt(now).
		SetExpiresAt(now.Add(time.Hour)).
		SetGrantorKind(testidentitydelegation.GrantorKindTestInfraOffline).
		SetHubID(x2HubID).
		SetMarkerCreatedAt(marker.CreatedAt).
		Save(ctx)
	require.NoError(t, err)
	d, err := e.s.GetTestIdentityDelegation(ctx, row.ID.String())
	require.NoError(t, err)
	return d
}

func (e *oadEnv) issueAudits(t *testing.T) []*store.MutationAuditRecord {
	t.Helper()
	audits, _, err := e.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: testAdminIssueMutation})
	require.NoError(t, err)
	return audits
}

// authReason runs the workload arm alone on the request a mint sends and
// returns its refusal reason, or "" when the arm admits (the client only
// ever sees a generic refusal message).
func (e *oadEnv) authReason(t *testing.T, delegationID string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, taPath, nil)
	req.Header.Set("Authorization", "Bearer "+x2Token(t, x2TokenOpts{sub: x4AdminSub}))
	req.Header.Set(testIssuerDelegationHeader, delegationID)
	_, refusal := e.srv.workloadIssuer.authenticate(req.Context(), req, true)
	if refusal == nil {
		return ""
	}
	return refusal.reason
}

// redeems counts the per-redeem audit rows of a delegation: one for every
// request the workload arm admitted.
func (e *oadEnv) redeems(t *testing.T, delegationID string) int {
	t.Helper()
	rows, _, err := e.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: testIdentityDelegationRedeemOfflineMutation, TargetID: delegationID})
	require.NoError(t, err)
	return len(rows)
}

// workloadRefusalBody is the generic body of a workload arm refusal.
const workloadRefusalBody = "workload delegation does not grant this request"

// snapshot records the admin-kind identities, issue audits and issue sink
// lines; the returned func checks that none was added since.
func (e *oadEnv) snapshot(t *testing.T) func() {
	t.Helper()
	admins, audits, lines := e.liveAdmins(t, ""), len(e.issueAudits(t)), len(tsaSinkLines(e.sink, testAdminAuditEventIssue))
	return func() {
		t.Helper()
		assert.Equal(t, admins, e.liveAdmins(t, ""), "no admin-kind identity was minted")
		assert.Len(t, e.issueAudits(t), audits, "no issuance audit")
		assert.Len(t, tsaSinkLines(e.sink, testAdminAuditEventIssue), lines, "no issuance sink line")
	}
}

// --- Positive path ----------------------------------------------------------

// Test 11: with the setting on at --test-mode=admin, an offline
// hub-admin delegation mints one test hub-admin. The identity carries the
// reserved grantor and the delegation; the issue audit, the per-redeem
// audit (with the scope) and the sink line (delegation id, subject, cap,
// identity id, grantor kind) are written.
func TestOfflineAdminDelegation_MintsTestHubAdmin(t *testing.T) {
	e := newOADEnv(t, true)
	d := e.offlineAdmin(t, 1)
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp TestAdminIdentityTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, store.UserKindTestHubAdmin, resp.Identity.Kind)
	assert.NotEmpty(t, resp.AccessToken)

	u, err := e.s.GetUser(context.Background(), resp.Identity.ID)
	require.NoError(t, err)
	require.NotNil(t, u.IssuedBy)
	assert.Equal(t, store.TestInfraOfflineGrantorID(x2HubID), *u.IssuedBy)
	require.NotNil(t, u.IssuerDelegationID)
	assert.Equal(t, d.ID, *u.IssuerDelegationID)

	audits := e.issueAudits(t)
	require.Len(t, audits, 1)
	assert.Equal(t, u.ID, audits[0].TargetID)
	var summary struct {
		Workload *testIdentityWorkloadAudit `json:"workload_delegation"`
	}
	require.NoError(t, json.Unmarshal([]byte(audits[0].AfterSummary), &summary), audits[0].AfterSummary)
	require.NotNil(t, summary.Workload, audits[0].AfterSummary)
	assert.Equal(t, d.ID, summary.Workload.DelegationID)
	assert.Equal(t, x4AdminSub, summary.Workload.WorkloadSub)
	assert.Equal(t, store.DelegationGrantorTestInfraOffline, summary.Workload.GrantorKind)
	assert.Equal(t, store.TestInfraOfflineAdminDelegationScope, summary.Workload.Scope)
	assert.Equal(t, 1, summary.Workload.DelegationCap)

	redeems, _, err := e.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: testIdentityDelegationRedeemOfflineMutation, TargetID: d.ID})
	require.NoError(t, err)
	require.NotEmpty(t, redeems)
	assert.Contains(t, redeems[0].AfterSummary, "scope="+store.TestInfraOfflineAdminDelegationScope+" admin_cap=1 ")

	lines := tsaSinkLines(e.sink, testAdminAuditEventIssue)
	require.Len(t, lines, 1)
	assert.Equal(t, u.ID, lines[0]["test_identity_id"])
	assert.Equal(t, d.ID, lines[0]["issuer_delegation_id"])
	assert.Equal(t, x4AdminSub, lines[0]["workload_sub"])
	assert.Equal(t, store.DelegationGrantorTestInfraOffline, lines[0]["grantor_kind"])
	assert.Equal(t, store.TestInfraOfflineAdminDelegationScope, lines[0]["scope"])
	assert.EqualValues(t, 1, lines[0]["delegation_cap"])
}

// the sink line is written inside the mint transaction: a failing sink
// mints nothing.
func TestOfflineAdminDelegation_SinkFailureMintsNothing(t *testing.T) {
	e := newOADEnv(t, true)
	d := e.offlineAdmin(t, 1)
	check := e.snapshot(t)
	users := e.userCount(t)
	e.sink.mu.Lock()
	e.sink.err = errors.New("disk full")
	e.sink.mu.Unlock()
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.NotContains(t, x4JSONKeys(t, rec.Body.Bytes()), "accessToken")
	check()
	assert.Equal(t, users, e.userCount(t), "no user row")
}

// Test 19: the delegation's admin cap applies (cap 1: the second mint is
// refused).
func TestOfflineAdminDelegation_DelegationCap(t *testing.T) {
	e := newOADEnv(t, true)
	d := e.offlineAdmin(t, 1)
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	check := e.snapshot(t)
	rec = e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), testAdminReasonDelegationCap)
	check()
}

// Test 19: the hub-wide cap of live admin-kind identities applies to an
// offline delegation too: with the hub already holding the maximum, an
// offline delegation with room in its own cap mints nothing.
func TestOfflineAdminDelegation_HubWideAdminCap(t *testing.T) {
	e := newOADEnv(t, true)
	for i := 0; i < testAdminHubCap; i++ {
		grantor := taUser(t, e.s, "oad-hubcap-grantor-"+string(rune('a'+i)), store.SystemRoleSuperAdmin)
		hd := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, map[string]any{"adminCap": 1})
		rec := e.mintAdmin(t, hd.ID, testAdminRoleHubAdmin)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}
	require.Equal(t, testAdminHubCap, e.liveAdmins(t, ""))
	d := e.offlineAdmin(t, 2)
	check := e.snapshot(t)
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), testAdminReasonHubCap)
	check()
}

// --- Refusals end to end --------------------------------------------------

// Test 12: with the setting off the same delegation mints nothing.
func TestOfflineAdminDelegation_RefusedWithSwitchOff(t *testing.T) {
	e := newOADEnv(t, false)
	d := e.offlineAdmin(t, 1)
	check := e.snapshot(t)
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), workloadRefusalBody)
	assert.Zero(t, e.redeems(t, d.ID), "refused by the workload arm, before any redeem")
	assert.Contains(t, e.authReason(t, d.ID), "an enabled hub-admin scope")
	check()
}

// Test 13: below --test-mode=admin the delegation mints nothing, even
// while the admin gate captured at startup still reports the admin kind.
func TestOfflineAdminDelegation_RefusedBelowAdminLevel(t *testing.T) {
	e := newOADEnv(t, true)
	d := e.offlineAdmin(t, 1)
	e.srv.testMode = testModeState{level: TestModeMember}
	check := e.snapshot(t)
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), workloadRefusalBody)
	assert.Zero(t, e.redeems(t, d.ID), "refused by the workload arm, before any redeem")
	assert.Contains(t, e.authReason(t, d.ID), "needs --test-mode at admin or above")
	check()
}

// Test 14: unmarking, or unmarking and marking again, ends the delegation.
func TestOfflineAdminDelegation_RefusedAfterUnmarkAndRemark(t *testing.T) {
	e := newOADEnv(t, true)
	d := e.offlineAdmin(t, 2)
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	e.unmark(t)
	check := e.snapshot(t)
	redeemed := e.redeems(t, d.ID)
	rec = e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), workloadRefusalBody)
	assert.Contains(t, e.authReason(t, d.ID), "test infrastructure marker absent")
	e.mark(t)
	rec = e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String(), "a new marker does not revive the delegation")
	assert.Contains(t, rec.Body.String(), workloadRefusalBody)
	assert.Contains(t, e.authReason(t, d.ID), "not the one the delegation was bound to")
	assert.Equal(t, redeemed, e.redeems(t, d.ID), "both refused by the workload arm")
	check()
}

// Test 15 (end to end): a forged offline row with the super-admin
// scope mints nothing, whatever role it asks for, with the setting on.
func TestOfflineAdminDelegation_ForgedSuperAdminRowMintsNothing(t *testing.T) {
	e := newOADEnv(t, true)
	e.srv.testAdminGate.superAdmin = true
	d := e.forge(t, testIdentityDelegationSuperAdminScope)
	check := e.snapshot(t)
	for _, role := range []string{testAdminRoleSuperAdmin, testAdminRoleHubAdmin} {
		rec := e.mintAdmin(t, d.ID, role)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", role, rec.Body.String())
		assert.Contains(t, rec.Body.String(), workloadRefusalBody, role)
	}
	assert.Zero(t, e.redeems(t, d.ID), "refused by the workload arm, before any redeem")
	assert.Contains(t, e.authReason(t, d.ID), "an enabled hub-admin scope")
	check()
}

// Issuance only: an offline hub-admin delegation never re-issues the token
// of, or mints a web-login code for, an identity it issued.
func TestOfflineAdminDelegation_NoReissueNoWebCode(t *testing.T) {
	e := newOADEnv(t, true)
	x2EnableWebSessions(t, e.x2Env)
	e.srv.testAdminWeb = testAdminWebState{on: true}
	d := e.offlineAdmin(t, 1)
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp TestAdminIdentityTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	id := resp.Identity.ID
	tok := func() string { return x2Token(t, x2TokenOpts{sub: x4AdminSub}) }

	tokenAudits := func() int {
		rows, _, err := e.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: testIdentityTokenIssueMutation, TargetID: id})
		require.NoError(t, err)
		return len(rows)
	}
	// Both requests pass the workload arm (a redeem row each) and are then
	// refused by authorization: 403 forbidden, nothing written.
	redeemed := e.redeems(t, d.ID)
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities/"+id+"/token", tok(), d.ID, nil, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"forbidden"`)
	assert.NotContains(t, rec.Body.String(), "accessToken")
	assert.Zero(t, tokenAudits(), "no token re-issue")
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities/"+id+"/web-login-code", tok(), d.ID, nil, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"forbidden"`)
	assert.Empty(t, x2WebCodeAudits(t, e.x2Env, id), "no web-login code was minted")
	assert.Equal(t, redeemed+2, e.redeems(t, d.ID), "both passed the workload arm and were refused after it")
}

// Test 16: a human-grantor admin delegation is unchanged with the
// offline setting on: a demoted grantor mints nothing.
func TestOfflineAdminDelegation_HumanGrantorStillNeedsLiveSuperAdmin(t *testing.T) {
	e := newOADEnv(t, true)
	grantor := taUser(t, e.s, "oad-human-grantor", store.SystemRoleSuperAdmin)
	d := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, nil)
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	taDeleteSystemRole(t, e.s, grantor.ID, store.SystemRoleSuperAdmin)
	check := e.snapshot(t)
	rec = e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	// Refused by the authorization decision for the hub-admin issue
	// action (the demoted grantor no longer holds it), on the human path.
	assert.Contains(t, rec.Body.String(), `"code":"forbidden"`)
	assert.Contains(t, rec.Body.String(), `"denied_action":"issue_admin"`)
	assert.NotContains(t, rec.Body.String(), testAdminReasonOfflineDelegation, "refused on the human path, not the offline one")
	check()
}

// --- Gate and liveness (unit) ---------------------------------------------

// oadGateAuthenticator is dlGateAuthenticator with x2Sub at the admin
// tier, every admin scope available, and the switch on.
func oadGateAuthenticator(level TestMode) *workloadIssuerAuthenticator {
	a := dlGateAuthenticator(level)
	a.allowlist = map[string]string{x2Sub: config.WorkloadIssuerTierAdmin}
	a.adminScopes = func(testAdminKind) bool { return true }
	a.offlineAdmin = true
	return a
}

func oadGateDelegation(markerAt time.Time) *store.TestIdentityDelegation {
	d := dlGateDelegation(markerAt)
	d.Scope = store.TestInfraOfflineAdminDelegationScope
	return d
}

// Each gate condition for the hub-admin scope refuses on its own, and the
// test pins which check refused: the gate's own scope check (switch off,
// non-canonical scope), its super-admin check, its level check, and the
// liveness check's scope availability, admin-tier allowlist entry and
// hub-admin maximum age.
func TestOfflineGrantorGate_HubAdminScope(t *testing.T) {
	ctx := context.Background()
	markerAt := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	st := dlNoUserStore{binding: &store.TestInfraBinding{HubID: x2HubID, Class: config.HubClassTest, CreatedAt: markerAt}}

	facts, refusal := oadGateAuthenticator(TestModeAdmin).offlineGrantorGate(ctx, st, oadGateDelegation(markerAt))
	require.Nil(t, refusal)
	assert.True(t, facts.adminScope)
	assert.Equal(t, TestModeAdmin, facts.level)

	// The member scope still passes at member level, and is not marked as
	// the admin scope.
	facts, refusal = oadGateAuthenticator(TestModeMember).offlineGrantorGate(ctx, st, dlGateDelegation(markerAt))
	require.Nil(t, refusal)
	assert.False(t, facts.adminScope)

	type gateCase struct {
		a      func(*workloadIssuerAuthenticator)
		mut    func(*store.TestIdentityDelegation)
		reason string
	}
	const gateScope = "with the offline admin delegation setting on"
	cases := map[string]gateCase{
		"switch off":              {a: func(a *workloadIssuerAuthenticator) { a.offlineAdmin = false }, reason: gateScope},
		"level member":            {a: func(a *workloadIssuerAuthenticator) { a.testModeLevel = func() TestMode { return TestModeMember } }, reason: "needs --test-mode at admin or above"},
		"admin scope unavailable": {a: func(a *workloadIssuerAuthenticator) { a.adminScopes = func(testAdminKind) bool { return false } }, reason: "delegation scope not available on this hub"},
		"member-tier subject": {a: func(a *workloadIssuerAuthenticator) {
			a.allowlist = map[string]string{x2Sub: config.WorkloadIssuerTierMember}
		}, reason: "workload subject not allow-listed"},
		"past hub-admin max age": {mut: func(d *store.TestIdentityDelegation) {
			d.Created = time.Now().Add(-testHubAdminDelegationMaxAge - time.Second)
		}, reason: "delegation past its maximum age"},
		"padded scope": {mut: func(d *store.TestIdentityDelegation) { d.Scope = store.TestInfraOfflineAdminDelegationScope + " " }, reason: gateScope},
	}
	for _, v := range superAdminVariants {
		v := v
		cases["super-admin variant "+v] = gateCase{mut: func(d *store.TestIdentityDelegation) { d.Scope = v }, reason: "never holds the super-admin scope"}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := oadGateAuthenticator(TestModeAdmin)
			if tc.a != nil {
				tc.a(a)
			}
			d := oadGateDelegation(markerAt)
			if tc.mut != nil {
				tc.mut(d)
			}
			facts, refusal := a.offlineGrantorGate(ctx, st, d)
			require.NotNil(t, refusal, "the gate must refuse")
			assert.Nil(t, facts)
			assert.Contains(t, refusal.reason, tc.reason)
		})
	}
}

// checkDelegationLive, alone: an offline hub-admin row is live only with
// the switch on, the scope available and the subject at the admin tier;
// an offline super-admin row never. Each refusal names its check.
func TestCheckDelegationLive_OfflineHubAdmin(t *testing.T) {
	now := time.Now()
	a := oadGateAuthenticator(TestModeAdmin)
	d := oadGateDelegation(now)
	require.Nil(t, a.checkDelegationLive(d, now))

	reason := func() string {
		r := a.checkDelegationLive(d, now)
		if r == nil {
			return ""
		}
		return r.reason
	}
	a.offlineAdmin = false
	assert.Contains(t, reason(), "an enabled hub-admin scope", "switch off")
	a.offlineAdmin = true

	a.adminScopes = func(testAdminKind) bool { return false }
	assert.Contains(t, reason(), "delegation scope not available on this hub", "scope unavailable")
	a.adminScopes = func(testAdminKind) bool { return true }

	a.allowlist = map[string]string{x2Sub: config.WorkloadIssuerTierMember}
	assert.Contains(t, reason(), "workload subject not allow-listed", "member-tier subject")
	a.allowlist = map[string]string{x2Sub: config.WorkloadIssuerTierAdmin}

	d.Scope = testIdentityDelegationSuperAdminScope
	assert.Contains(t, reason(), "an enabled hub-admin scope", "super-admin scope")
}

// --- Request-local grant (unit) -------------------------------------------

// oadCtx builds a request context as the workload arm would, with the
// offline gate state as given.
func oadCtx(t *testing.T, d *store.TestIdentityDelegation, passed, adminScope bool) (context.Context, *ScopedUserIdentity) {
	t.Helper()
	ctx, identity := dlOfflineCtx(t, d, passed)
	st := workloadDelegationFromContext(ctx)
	if passed {
		st.offline = &offlineGateFacts{hubID: x2HubID, level: TestModeAdmin, adminScope: adminScope}
	}
	return ctx, identity
}

// Test 17: the grant of an offline hub-admin delegation is exactly
// test_identity.issue_admin, byte for byte, and only with the hub-admin
// gate state; never member issuance, never any super-admin spelling.
func TestOfflineDelegationBinding_HubAdminExactlyOnePermission(t *testing.T) {
	d := oadGateDelegation(time.Now())
	ctx, identity := oadCtx(t, d, true, true)
	principal := principalContextForIdentity(identity)
	cred := credentialContextForIdentity(identity)

	cb, role := offlineDelegationBinding(ctx, principal, cred, permissionTestIdentityIssueAdmin)
	require.NotNil(t, cb)
	assert.Equal(t, map[string]struct{}{permissionTestIdentityIssueAdmin: {}}, role.Permissions)

	refused := append([]string{permissionTestIdentityIssue, "TEST_IDENTITY.ISSUE_ADMIN", permissionTestIdentityIssueAdmin + " ", "test_identity:issue_admin"}, superAdminVariants...)
	for _, p := range refused {
		cb, _ := offlineDelegationBinding(ctx, principal, cred, p)
		assert.Nil(t, cb, "permission %q", p)
	}

	// Without the hub-admin gate state, no grant.
	ctx, identity = oadCtx(t, d, true, false)
	cb, _ = offlineDelegationBinding(ctx, principalContextForIdentity(identity), credentialContextForIdentity(identity), permissionTestIdentityIssueAdmin)
	assert.Nil(t, cb, "gate passed for the member scope only")
	ctx, identity = oadCtx(t, d, false, false)
	cb, _ = offlineDelegationBinding(ctx, principalContextForIdentity(identity), credentialContextForIdentity(identity), permissionTestIdentityIssueAdmin)
	assert.Nil(t, cb, "gate not passed")

	// A super-admin-scope row with hub-admin gate state gets nothing.
	forged := oadGateDelegation(time.Now())
	forged.Scope = testIdentityDelegationSuperAdminScope
	ctx, identity = oadCtx(t, forged, true, true)
	for _, p := range append([]string{permissionTestIdentityIssueAdmin}, superAdminVariants...) {
		cb, _ := offlineDelegationBinding(ctx, principalContextForIdentity(identity), credentialContextForIdentity(identity), p)
		assert.Nil(t, cb, "forged super-admin row, permission %q", p)
	}
}

// the exact-permission and scope checks, with the super-admin refusal
// first.
func TestOfflineAdminPermissionAndScopeAdmitted(t *testing.T) {
	assert.True(t, offlineAdminPermissionAdmitted(permissionTestIdentityIssueAdmin))
	assert.True(t, offlineAdminScopeAdmitted(store.TestInfraOfflineAdminDelegationScope))
	for _, v := range append([]string{"", permissionTestIdentityIssue, "TEST_IDENTITY.ISSUE_ADMIN", " test_identity.issue_admin", "test_identity:issue_admin"}, superAdminVariants...) {
		assert.False(t, offlineAdminPermissionAdmitted(v), "permission %q", v)
	}
	for _, v := range append([]string{"", store.TestInfraOfflineDelegationScope, "TEST_IDENTITY:ISSUE_ADMIN", "test_identity.issue_admin"}, superAdminVariants...) {
		assert.False(t, offlineAdminScopeAdmitted(v), "scope %q", v)
	}
	for _, v := range superAdminVariants {
		assert.True(t, store.IsSuperAdminIssueVariant(v), v)
	}
}

// --- Issuer step (unit, per-site conditions) --------------------------------

// each condition of the issuer step's offline case refuses on its own
// with the offline detail reason; the control passes the step.
func TestTestAdminIssuer_OfflineHubAdminConditions(t *testing.T) {
	e := newOADEnv(t, true)
	spec := testAdminKindSpecs[testAdminRoleHubAdmin]
	call := func(d *store.TestIdentityDelegation, passed, adminScope bool) (*httptest.ResponseRecorder, bool) {
		ctx, _ := oadCtx(t, d, passed, adminScope)
		req := httptest.NewRequest(http.MethodPost, taPath, nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		_, _, _, ok := e.srv.testAdminIssuer(rr, req, spec)
		return rr, ok
	}
	d := oadGateDelegation(time.Now())

	// Control: every condition holds.
	rr, ok := call(d, true, true)
	require.True(t, ok, rr.Body.String())

	rr, ok = call(d, false, false)
	assert.False(t, ok, "gate not passed")
	assert.Contains(t, rr.Body.String(), testAdminReasonOfflineDelegation)

	rr, ok = call(d, true, false)
	assert.False(t, ok, "gate passed for the member scope only")
	assert.Contains(t, rr.Body.String(), testAdminReasonOfflineDelegation)

	e.setSwitch(false)
	rr, ok = call(d, true, true)
	assert.False(t, ok, "switch off")
	assert.Contains(t, rr.Body.String(), testAdminReasonOfflineDelegation)
	e.setSwitch(true)

	// A non-canonical spelling of the hub-admin scope never reaches this
	// step: it resolves to no ceiling, so the workload arm refuses it (and
	// TestOfflineGrantorGate_HubAdminScope "padded scope" refuses it at the
	// gate).

	// A super-admin request through an offline delegation is refused.
	forged := *d
	forged.Scope = testIdentityDelegationSuperAdminScope
	ctx, _ := oadCtx(t, &forged, true, true)
	req := httptest.NewRequest(http.MethodPost, taPath, nil).WithContext(ctx)
	rr = httptest.NewRecorder()
	_, _, _, ok = e.srv.testAdminIssuer(rr, req, testAdminKindSpecs[testAdminRoleSuperAdmin])
	assert.False(t, ok, "super-admin through an offline delegation")
}

// --- In-transaction re-check (unit) ---------------------------------------

// Tests 15 and 18: recheckOfflineAdminDelegationGrantor admits a live
// offline hub-admin delegation and refuses, on its own, each failed
// condition: a permission other than test_identity.issue_admin (every
// super-admin spelling), no passed hub-admin gate state, the switch off, a
// revoked or unmarked delegation, a level below admin, and a stored row of
// another scope.
func TestRecheckOfflineAdminDelegationGrantor(t *testing.T) {
	e := newOADEnv(t, true)
	ctx := context.Background()
	d := e.offlineAdmin(t, 1)
	state := func(passed, adminScope bool) *workloadDelegationState {
		st := &workloadDelegationState{delegation: d, sub: x4AdminSub, offlineGatePassed: passed}
		if passed {
			st.offline = &offlineGateFacts{hubID: x2HubID, level: TestModeAdmin, adminScope: adminScope}
		}
		return st
	}
	recheck := func(st *workloadDelegationState, permission string) error {
		return e.s.WithTx(ctx, func(tx store.Store) error {
			return e.srv.recheckOfflineAdminDelegationGrantor(ctx, tx, st, permission)
		})
	}
	require.NoError(t, recheck(state(true, true), permissionTestIdentityIssueAdmin))

	for _, p := range append([]string{permissionTestIdentityIssue, "TEST_IDENTITY.ISSUE_ADMIN"}, superAdminVariants...) {
		err := recheck(state(true, true), p)
		require.ErrorIs(t, err, errTestAdminIssuerNotLive, p)
		assert.Contains(t, err.Error(), "not delegable offline", p)
	}
	err := recheck(state(false, false), permissionTestIdentityIssueAdmin)
	require.ErrorIs(t, err, errTestAdminIssuerNotLive)
	assert.Contains(t, err.Error(), "no passed offline hub-admin gate")
	err = recheck(state(true, false), permissionTestIdentityIssueAdmin)
	require.ErrorIs(t, err, errTestAdminIssuerNotLive)
	assert.Contains(t, err.Error(), "no passed offline hub-admin gate")

	e.setSwitch(false)
	err = recheck(state(true, true), permissionTestIdentityIssueAdmin)
	require.ErrorIs(t, err, errTestAdminIssuerNotLive)
	assert.Contains(t, err.Error(), "offline hub-admin delegations are off")
	e.setSwitch(true)

	e.srv.testMode = testModeState{level: TestModeMember}
	err = recheck(state(true, true), permissionTestIdentityIssueAdmin)
	require.ErrorIs(t, err, errTestAdminIssuerNotLive, "level member")
	assert.Contains(t, err.Error(), "needs --test-mode at admin or above")
	e.srv.testMode = testModeState{level: TestModeAdmin}

	e.unmark(t)
	err = recheck(state(true, true), permissionTestIdentityIssueAdmin)
	require.ErrorIs(t, err, errTestAdminIssuerNotLive, "unmarked")
	assert.Contains(t, err.Error(), "test infrastructure marker absent")
	e.mark(t)
	err = recheck(state(true, true), permissionTestIdentityIssueAdmin)
	require.ErrorIs(t, err, errTestAdminIssuerNotLive, "re-marked")
	assert.Contains(t, err.Error(), "not the one the delegation was bound to")

	// Revoked: a fresh delegation on the new marker passes, then is
	// revoked and refused.
	_, _, err = testinfra.RevokeDelegation(ctx, e.client, testinfra.RevokeDelegationInput{HubID: x2HubID, ID: d.ID, Operator: "oad-op"})
	require.NoError(t, err)
	d2 := e.offlineAdmin(t, 1)
	st2 := &workloadDelegationState{delegation: d2, sub: x4AdminSub, offlineGatePassed: true,
		offline: &offlineGateFacts{hubID: x2HubID, level: TestModeAdmin, adminScope: true}}
	require.NoError(t, recheck(st2, permissionTestIdentityIssueAdmin))
	_, _, err = testinfra.RevokeDelegation(ctx, e.client, testinfra.RevokeDelegationInput{HubID: x2HubID, ID: d2.ID, Operator: "oad-op"})
	require.NoError(t, err)
	err = recheck(st2, permissionTestIdentityIssueAdmin)
	require.ErrorIs(t, err, errTestAdminIssuerNotLive, "revoked")
	assert.Contains(t, err.Error(), "delegation revoked")

	// A forged super-admin-scope row with hub-admin gate state.
	forged := e.forge(t, testIdentityDelegationSuperAdminScope)
	st := &workloadDelegationState{delegation: forged, sub: x4AdminSub, offlineGatePassed: true,
		offline: &offlineGateFacts{hubID: x2HubID, level: TestModeAdmin, adminScope: true}}
	err = recheck(st, permissionTestIdentityIssueAdmin)
	require.ErrorIs(t, err, errTestAdminIssuerNotLive)
	assert.Contains(t, err.Error(), "not the offline hub-admin scope")
}

// Test 18: a revocation between authentication and the mint transaction
// mints nothing.
func TestOfflineAdminDelegation_RevokedBeforeMintMintsNothing(t *testing.T) {
	e := newOADEnv(t, true)
	d := e.offlineAdmin(t, 1)
	orig := e.srv.testIdentities.hooks.writeAudit
	revokeNext := true
	e.srv.testIdentities.hooks.writeAudit = func(ctx context.Context, tx store.Store, record *store.MutationAuditRecord) error {
		if err := orig(ctx, tx, record); err != nil {
			return err
		}
		if revokeNext && record.MutationType == testIdentityDelegationRedeemOfflineMutation {
			revokeNext = false
			_, _, err := testinfra.RevokeDelegation(context.Background(), e.client, testinfra.RevokeDelegationInput{HubID: x2HubID, ID: d.ID, Operator: "oad-op"})
			require.NoError(t, err)
		}
		return nil
	}
	check := e.snapshot(t)
	redeemed := e.redeems(t, d.ID)
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), workloadRefusalBody)
	// The workload arm admitted the request (its redeem row was written,
	// and the revoke ran from that write), so the refusal came from the
	// mint transaction's re-check.
	assert.Equal(t, redeemed+1, e.redeems(t, d.ID), "the request passed the workload arm")
	assert.False(t, revokeNext, "the revoke ran between authentication and the mint")
	assert.Contains(t, e.authReason(t, d.ID), "delegation revoked")
	check()
}

// --- Switch-off cleanup at startup -----------------------------------------

// With the setting off at startup, every offline hub-admin delegation is
// revoked (with an audit record) and every identity it issued is revoked
// (with a test_admin_revoke audit record); offline member delegations,
// human delegations and other admin identities are untouched. A second
// run changes nothing. With the setting on it does nothing.
func TestEndOfflineAdminDelegationsWhenOff(t *testing.T) {
	ctx := context.Background()
	e := newOADEnv(t, true)
	d := e.offlineAdmin(t, 2)
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp TestAdminIdentityTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	offlineAdminID := resp.Identity.ID

	member := e.offlineMember(t)
	grantor := taUser(t, e.s, "oad-cleanup-grantor", store.SystemRoleSuperAdmin)
	human := e.delegateScope(t, grantor, testIdentityDelegationAdminScope, nil)
	rec = e.mintAdmin(t, human.ID, testAdminRoleHubAdmin)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	humanAdminID := resp.Identity.ID

	// On: nothing changes.
	require.NoError(t, e.srv.EndOfflineAdminDelegationsWhenOff(ctx))
	got, err := e.s.GetTestIdentityDelegation(ctx, d.ID)
	require.NoError(t, err)
	assert.Nil(t, got.RevokedAt)

	// Off (a restart without the setting).
	e.setSwitch(false)
	require.NoError(t, e.srv.EndOfflineAdminDelegationsWhenOff(ctx))

	got, err = e.s.GetTestIdentityDelegation(ctx, d.ID)
	require.NoError(t, err)
	require.NotNil(t, got.RevokedAt, "the offline hub-admin delegation is revoked")
	require.NotNil(t, got.RevokedBy)
	assert.Equal(t, "system:"+offlineAdminCleanupActorID, *got.RevokedBy)
	revokes, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityDelegationRevokeMutation, TargetID: d.ID})
	require.NoError(t, err)
	require.Len(t, revokes, 1)
	assert.Equal(t, "system", revokes[0].ActorPrincipalKind)

	u, err := e.s.GetUser(ctx, offlineAdminID)
	if err == nil {
		assert.Equal(t, store.UserStatusSuspended, u.Status, "kept only as suspended")
	} else {
		assert.ErrorIs(t, err, store.ErrNotFound, "the identity is deleted")
	}
	adminRevokes, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testAdminRevokeMutation, TargetID: offlineAdminID})
	require.NoError(t, err)
	assert.Len(t, adminRevokes, 1)

	for _, id := range []string{member.ID, human.ID} {
		other, err := e.s.GetTestIdentityDelegation(ctx, id)
		require.NoError(t, err)
		assert.Nil(t, other.RevokedAt, "delegation %s is untouched", id)
	}
	hu, err := e.s.GetUser(ctx, humanAdminID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, hu.Status, "an identity of a human delegation is untouched")

	// Idempotent.
	require.NoError(t, e.srv.EndOfflineAdminDelegationsWhenOff(ctx))
	revokes, _, err = e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityDelegationRevokeMutation, TargetID: d.ID})
	require.NoError(t, err)
	assert.Len(t, revokes, 1)
	adminRevokes, _, err = e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testAdminRevokeMutation, TargetID: offlineAdminID})
	require.NoError(t, err)
	assert.Len(t, adminRevokes, 1)
}

// the cleanup also ends a forged offline row of any non-member scope,
// and runs on a hub without the test admin audit sink (a restart below the
// admin level).
func TestEndOfflineAdminDelegationsWhenOff_ForgedRowNoSink(t *testing.T) {
	ctx := context.Background()
	e := newOADEnv(t, false)
	forged := e.forge(t, testIdentityDelegationSuperAdminScope)
	e.srv.testAdminAudit = nil
	require.NoError(t, e.srv.EndOfflineAdminDelegationsWhenOff(ctx))
	got, err := e.s.GetTestIdentityDelegation(ctx, forged.ID)
	require.NoError(t, err)
	assert.NotNil(t, got.RevokedAt)
}

// oadCleanupFaultStore fails chosen store calls of the startup cleanup
// while its switch is armed and the matching flag is set.
type oadCleanupFaultStore struct {
	store.Store
	fault       *storeFaultSwitch
	failList    atomic.Bool
	failSession atomic.Bool
}

var errOADCleanupFault = errors.New("injected cleanup fault")

func (w *oadCleanupFaultStore) ListTestIdentityDelegations(ctx context.Context, grantorID string, limit int) ([]store.TestIdentityDelegation, error) {
	if w.fault.Active() && w.failList.Load() {
		return nil, errOADCleanupFault
	}
	return w.Store.ListTestIdentityDelegations(ctx, grantorID, limit)
}

func (w *oadCleanupFaultStore) IncrementSessionGeneration(ctx context.Context, userID string) error {
	if w.fault.Active() && w.failSession.Load() {
		return errOADCleanupFault
	}
	return w.Store.IncrementSessionGeneration(ctx, userID)
}

// newOADEnvWithCleanupFault is newOADEnv with the cleanup fault wrapper
// installed on the server's store before any audited setup.
func newOADEnvWithCleanupFault(t *testing.T) (*oadEnv, *oadCleanupFaultStore, *storeFaultSwitch) {
	t.Helper()
	srv, s, buf := taServer(t)
	wrapped, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *oadCleanupFaultStore {
		return &oadCleanupFaultStore{Store: inner, fault: f}
	})
	return oadEnvFrom(t, setupX4(t, srv, s, buf, false), true), wrapped, fault
}

// The cleanup fails closed: a failed delegation list refuses startup with
// ErrOfflineAdminDelegationsCleanup and changes nothing.
func TestEndOfflineAdminDelegationsWhenOff_ListFailureRefusesStartup(t *testing.T) {
	ctx := context.Background()
	e, w, fault := newOADEnvWithCleanupFault(t)
	d := e.offlineAdmin(t, 1)
	e.setSwitch(false)
	w.failList.Store(true)
	fault.Arm()
	err := e.srv.EndOfflineAdminDelegationsWhenOff(ctx)
	require.ErrorIs(t, err, ErrOfflineAdminDelegationsCleanup)
	assert.Contains(t, err.Error(), "list delegations")
	got, err := e.s.GetTestIdentityDelegation(ctx, d.ID)
	require.NoError(t, err)
	assert.Nil(t, got.RevokedAt, "nothing was changed")
}

// The cleanup fails closed after the delegation revoke when an identity
// cannot be ended, and the next start completes: the already-revoked
// delegation still counts as ended, so its identity is revoked then.
func TestEndOfflineAdminDelegationsWhenOff_IdentityFailureThenRetry(t *testing.T) {
	ctx := context.Background()
	e, w, fault := newOADEnvWithCleanupFault(t)
	d := e.offlineAdmin(t, 1)
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp TestAdminIdentityTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	id := resp.Identity.ID

	e.setSwitch(false)
	w.failSession.Store(true)
	fault.Arm()
	err := e.srv.EndOfflineAdminDelegationsWhenOff(ctx)
	require.ErrorIs(t, err, ErrOfflineAdminDelegationsCleanup)
	assert.Contains(t, err.Error(), "end web sessions of "+id)
	got, err := e.s.GetTestIdentityDelegation(ctx, d.ID)
	require.NoError(t, err)
	require.NotNil(t, got.RevokedAt, "the delegation was revoked before the identity step failed")
	u, err := e.s.GetUser(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, u.Status, "the identity is not yet revoked")

	// The next start, with the fault cleared, completes.
	w.failSession.Store(false)
	require.NoError(t, e.srv.EndOfflineAdminDelegationsWhenOff(ctx))
	if u, err := e.s.GetUser(ctx, id); err == nil {
		assert.Equal(t, store.UserStatusSuspended, u.Status)
	} else {
		assert.ErrorIs(t, err, store.ErrNotFound)
	}
	revokes, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testAdminRevokeMutation, TargetID: id})
	require.NoError(t, err)
	assert.Len(t, revokes, 1)
}

// The cleanup revokes an identity of an offline hub-admin delegation
// whatever its status: a Suspended one is revoked too and holds no role
// bindings afterwards.
func TestEndOfflineAdminDelegationsWhenOff_RevokesSuspendedIdentity(t *testing.T) {
	ctx := context.Background()
	e := newOADEnv(t, true)
	d := e.offlineAdmin(t, 1)
	rec := e.mintAdmin(t, d.ID, testAdminRoleHubAdmin)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp TestAdminIdentityTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	id := resp.Identity.ID

	u, err := e.s.GetUser(ctx, id)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, e.s.UpdateUser(ctx, u))
	before, err := e.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, id)
	require.NoError(t, err)
	require.NotEmpty(t, before, "the suspended identity still holds its hub-admin binding")

	e.setSwitch(false)
	require.NoError(t, e.srv.EndOfflineAdminDelegationsWhenOff(ctx))
	revokes, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testAdminRevokeMutation, TargetID: id})
	require.NoError(t, err)
	assert.Len(t, revokes, 1, "the suspended identity was revoked")
	after, err := e.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, id)
	require.NoError(t, err)
	assert.Empty(t, after, "no role bindings remain")
}

// --- The reserved grantor id is never a caller ------------------------------

// No credential authenticates as the reserved grantor id: a hub access
// token (what a web session presents to the API) is refused, a workload
// token whose subject is the reserved id is refused, and the kind read
// classifies it as neither human nor a test identity. A platform admin
// cannot mint a web-login code for it.
func TestReservedOfflineGrantorID_NeverACaller(t *testing.T) {
	e := newOADEnv(t, true)
	ctx := context.Background()
	reserved := store.TestInfraOfflineGrantorID(x2HubID)

	k, err := e.srv.kindForPrincipal(ctx, reserved)
	require.NoError(t, err)
	assert.False(t, k.Found)
	assert.False(t, k.isHuman())
	assert.False(t, k.isTestIdentity())
	assert.False(t, k.isTestAdmin())

	token, _, err := e.srv.userTokenService.GenerateAccessTokenWithTTL(reserved, "", "reserved", store.UserRoleAdmin, ClientTypeAPI, 10*time.Minute)
	require.NoError(t, err)
	for _, path := range []string{"/api/v1/test-identities", "/api/v1/test-identity-delegations", "/api/v1/users"} {
		rec := doRequestWithToken(t, e.srv, token, http.MethodGet, path, nil)
		assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code, "%s: %s", path, rec.Body.String())
	}
	rec := doRequestWithToken(t, e.srv, token, http.MethodPost, taPath, taBody(testAdminRoleHubAdmin))
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code, rec.Body.String())

	d := e.offlineAdmin(t, 1)
	check := e.snapshot(t)
	rec = e.workload(t, http.MethodPost, taPath, x2Token(t, x2TokenOpts{sub: reserved}), d.ID, taBody(testAdminRoleHubAdmin), nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	check()

	// With test identity web sessions available, the 404 comes from the
	// lookup of the reserved id (no such user), not from a closed gate.
	x2EnableWebSessions(t, e.x2Env)
	admin := taUser(t, e.s, "oad-platform-admin", store.SystemRoleSuperAdmin)
	rec = doRequestAsUser(t, e.srv, admin, http.MethodPost, "/api/v1/test-identities/"+reserved+"/web-login-code", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "not available on this hub")
}

// --- Test 21: delegation view -----------------------------------------------

// The delegation view shows an offline hub-admin delegation as live only
// with the switch on and the hub at an admin level.
func TestOfflineAdminDelegation_ViewLiveness(t *testing.T) {
	e := newOADEnv(t, true)
	ctx := context.Background()
	d := e.offlineAdmin(t, 1)
	now := e.srv.testIdentities.clock()
	view := func() TestIdentityDelegationView {
		return testIdentityDelegationViewOf(ctx, e.s, d, now, e.srv.workloadIssuer)
	}
	assert.True(t, view().Live)
	assert.Equal(t, 1, view().AdminCap)
	e.srv.testMode = testModeState{level: TestModeMember}
	assert.False(t, view().Live, "below admin level")
	e.srv.testMode = testModeState{level: TestModeAdmin}
	e.setSwitch(false)
	assert.False(t, view().Live, "switch off")
}

// --- Startup validation -------------------------------------------------------

func TestValidateOfflineAdminDelegationsConfig(t *testing.T) {
	admin := ServerConfig{
		HubID:                   "oad-hub",
		TestMode:                TestModeConfig{Level: TestModeAdmin},
		TestAdminGate:           TestAdminGateConfig{HubAdmin: true},
		OfflineAdminDelegations: OfflineAdminDelegationsConfig{HubID: "oad-hub"},
	}
	require.NoError(t, validateOfflineAdminDelegationsConfig(admin))

	off := admin
	off.OfflineAdminDelegations = OfflineAdminDelegationsConfig{}
	require.NoError(t, validateOfflineAdminDelegationsConfig(off))

	member := admin
	member.TestMode = TestModeConfig{Level: TestModeMember}
	member.TestAdminGate = TestAdminGateConfig{}
	require.ErrorIs(t, validateOfflineAdminDelegationsConfig(member), ErrOfflineAdminDelegationsBelowAdmin)

	other := admin
	other.OfflineAdminDelegations = OfflineAdminDelegationsConfig{HubID: "another-hub"}
	require.ErrorIs(t, validateOfflineAdminDelegationsConfig(other), ErrOfflineAdminDelegationsHubIDMismatch)
}

// The permission constants this file relies on stay what the store and
// the catalog name.
func TestOfflineAdminDelegation_Constants(t *testing.T) {
	assert.Equal(t, testIdentityDelegationAdminScope, store.TestInfraOfflineAdminDelegationScope)
	assert.Equal(t, "test_identity.issue_admin", permissionTestIdentityIssueAdmin)
	ceiling, ok := permissions.BuildCeilingFromSelectors([]string{store.TestInfraOfflineAdminDelegationScope})
	require.True(t, ok)
	assert.Equal(t, []string{permissionTestIdentityIssueAdmin}, ceiling.PermissionIDs)
}

// The reaper's purge audit names the offline grantor by its kind, not as a
// user; any other issuer stays "user".
func TestReaperIssuerPrincipalKind(t *testing.T) {
	reserved := store.TestInfraOfflineGrantorID(x2HubID)
	assert.Equal(t, store.DelegationGrantorTestInfraOffline, reaperIssuerPrincipalKind(reserved, x2HubID))
	assert.Equal(t, "user", reaperIssuerPrincipalKind(reserved, "another-hub"), "another hub's reserved id is not this hub's grantor")
	assert.Equal(t, "user", reaperIssuerPrincipalKind(generateID(), x2HubID))
	assert.Equal(t, "user", reaperIssuerPrincipalKind("", ""))
}

// The gate's own hub-admin conditions refuse on their own: with the
// liveness check replaced by a stub that admits everything, the gate still
// refuses the hub-admin scope with the switch off, below the admin level,
// and for a non-canonical or super-admin scope.
func TestOfflineGrantorGate_HubAdminOwnChecksWithAdmittingLiveness(t *testing.T) {
	ctx := context.Background()
	markerAt := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	st := dlNoUserStore{binding: &store.TestInfraBinding{HubID: x2HubID, Class: config.HubClassTest, CreatedAt: markerAt}}
	admit := func(*store.TestIdentityDelegation, time.Time) *workloadDelegationRefusal { return nil }
	gate := func(mut func(*workloadIssuerAuthenticator), scope string) (*offlineGateFacts, *workloadDelegationRefusal) {
		a := oadGateAuthenticator(TestModeAdmin)
		a.liveCheckForTest = admit
		if mut != nil {
			mut(a)
		}
		d := oadGateDelegation(markerAt)
		d.Scope = scope
		return a.offlineGrantorGate(ctx, st, d)
	}

	facts, refusal := gate(nil, store.TestInfraOfflineAdminDelegationScope)
	require.Nil(t, refusal, "control")
	assert.True(t, facts.adminScope)

	_, refusal = gate(func(a *workloadIssuerAuthenticator) { a.offlineAdmin = false }, store.TestInfraOfflineAdminDelegationScope)
	assert.NotNil(t, refusal, "switch off: the gate's own check refuses")

	_, refusal = gate(func(a *workloadIssuerAuthenticator) { a.testModeLevel = func() TestMode { return TestModeMember } }, store.TestInfraOfflineAdminDelegationScope)
	assert.NotNil(t, refusal, "below admin level: the gate's own check refuses")

	for _, scope := range append([]string{store.TestInfraOfflineAdminDelegationScope + " ", "TEST_IDENTITY:ISSUE_ADMIN", "test_identity:issue_owner"}, superAdminVariants...) {
		_, refusal = gate(nil, scope)
		assert.NotNil(t, refusal, "scope %q: the gate's own check refuses", scope)
	}
}

// liveCheckForTest is set only by tests: no non-test file in pkg/hub or
// below assigns it, so production always runs checkDelegationLive in the
// gate.
func TestLiveCheckForTest_NeverSetOutsideTests(t *testing.T) {
	fset := token.NewFileSet()
	var names []string
	// pkgmove:scan-covers pkg/hub/...
	// recursive walk of pkg/hub with no SkipDir, so it reads every directory below pkg/hub.
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") {
			names = append(names, path)
		}
		return nil
	})
	require.NoError(t, err)
	var sets, parsed []string
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err, name)
		parsed = append(parsed, name)
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "liveCheckForTest" {
						sets = append(sets, fset.Position(n.Pos()).String())
					}
				}
			case *ast.KeyValueExpr:
				if k, ok := n.Key.(*ast.Ident); ok && k.Name == "liveCheckForTest" {
					sets = append(sets, fset.Position(n.Pos()).String())
				}
			}
			return true
		})
	}
	// The walk read pkg/hub itself: the file declaring the field was parsed.
	require.NotZero(t, len(parsed), "no non-test .go file was parsed")
	require.Contains(t, parsed, "test_identity_workload_issuer.go", "the walk must start in pkg/hub")
	assert.Empty(t, sets, "liveCheckForTest must never be set outside tests")
}

// New refuses the offline hub-admin delegation setting below an admin
// level, with another hub id, and without the test admin audit sink; at an
// admin level it starts with the switch on and the gate-start sink line
// carries offline_admin_delegations=true (false when the setting is off).
func TestOfflineAdminDelegations_NewRefusesMisconfiguration(t *testing.T) {
	t.Run("below admin level", func(t *testing.T) {
		tiaCaptureAudit(t)
		cfg := tiaGateServerConfig()
		cfg.TestAdminGate = TestAdminGateConfig{}
		cfg.TestMode.Level = TestModeMember
		cfg.OfflineAdminDelegations = OfflineAdminDelegationsConfig{HubID: tiaHubID}
		_, _, err := tiaNewServer(t, cfg)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrOfflineAdminDelegationsBelowAdmin)
		assert.Contains(t, err.Error(), config.TestIdentityOfflineAdminDelegationsKey)
	})
	t.Run("another hub id", func(t *testing.T) {
		tiaCaptureAudit(t)
		cfg := tiaGateServerConfig()
		cfg.OfflineAdminDelegations = OfflineAdminDelegationsConfig{HubID: "another-hub"}
		_, _, err := tiaNewServer(t, cfg)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrOfflineAdminDelegationsHubIDMismatch)
	})
	t.Run("no audit sink", func(t *testing.T) {
		prev := testAdminAuditOutput
		testAdminAuditOutput = nil
		t.Cleanup(func() { testAdminAuditOutput = prev })
		cfg := tiaGateServerConfig()
		cfg.OfflineAdminDelegations = OfflineAdminDelegationsConfig{HubID: tiaHubID}
		_, _, err := tiaNewServer(t, cfg)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrOfflineAdminDelegationsRequiresAuditSink)
	})
	t.Run("admin level", func(t *testing.T) {
		buf := tiaCaptureAudit(t)
		cfg := tiaGateServerConfig()
		cfg.OfflineAdminDelegations = OfflineAdminDelegationsConfig{HubID: tiaHubID}
		srv, _, err := tiaNewServer(t, cfg)
		require.NoError(t, err)
		assert.True(t, srv.offlineAdmin.on)
		assert.True(t, srv.offlineAdminDelegationsOn())
		assert.Equal(t, true, ws4GateStartLine(t, buf.String())["offline_admin_delegations"])
	})
	t.Run("admin level, setting off", func(t *testing.T) {
		buf := tiaCaptureAudit(t)
		srv, _, err := tiaNewServer(t, tiaGateServerConfig())
		require.NoError(t, err)
		assert.False(t, srv.offlineAdmin.on)
		assert.Equal(t, false, ws4GateStartLine(t, buf.String())["offline_admin_delegations"])
	})
}
