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
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/testinfra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for offline member-tier workload issuer delegations
// (ptone/scion#4240, phase DL; dl-design.md). The delegation and the marker
// are written with the real offline writers (pkg/testinfra) on the test
// hub's own SQLite database; the workload token is x2FakeGoogle's.

var dlIssuers = []config.TestIdentityWorkloadIssuer{
	{Sub: x2Sub, MaxTier: config.WorkloadIssuerTierMember},
	{Sub: x2OtherSub, MaxTier: config.WorkloadIssuerTierMember},
}

// dlEnv is an x2Env at --test-mode=member with a marker for x2HubID and an
// ent client over the hub's database for the offline writers.
type dlEnv struct {
	*x2Env
	client *ent.Client
}

// dlEntClient returns an ent client over s's database. It shares s's
// connection pool and is never closed by the test (the store's cleanup
// closes the pool).
func dlEntClient(t *testing.T, s store.Store) *ent.Client {
	t.Helper()
	cs, ok := s.(*entadapter.CompositeStore)
	require.True(t, ok, "the test store is a CompositeStore")
	db := cs.DB()
	require.NotNil(t, db)
	return ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, db)))
}

func newDLEnv(t *testing.T) *dlEnv {
	t.Helper()
	e := newX2Env(t, true)
	e.srv.testMode = testModeState{level: TestModeMember}
	wi := e.srv.workloadIssuer
	wi.hubID = x2HubID
	wi.testModeLevel = e.srv.TestModeLevel
	srv := e.srv
	wi.writeAudit = func(ctx context.Context, st store.Store, record *store.MutationAuditRecord) error {
		return srv.testIdentities.hooks.writeAudit(ctx, st, record)
	}
	env := &dlEnv{x2Env: e, client: dlEntClient(t, e.s)}
	env.mark(t)
	return env
}

func (e *dlEnv) mark(t *testing.T) {
	t.Helper()
	_, err := testinfra.Mark(context.Background(), e.client, testinfra.MarkInput{HubID: x2HubID, Operator: "dl-test", Host: "dl-host", Now: time.Now()})
	require.NoError(t, err)
}

func (e *dlEnv) unmark(t *testing.T) {
	t.Helper()
	_, err := testinfra.Unmark(context.Background(), e.client, testinfra.UnmarkInput{Operator: "dl-test", Host: "dl-host"})
	require.NoError(t, err)
}

// offline creates an offline delegation for x2Sub with the offline writer.
func (e *dlEnv) offline(t *testing.T, ttl time.Duration) *store.TestIdentityDelegation {
	t.Helper()
	d, err := testinfra.Delegate(context.Background(), e.client, testinfra.DelegateInput{
		HubID: x2HubID, WorkloadSub: x2Sub, WorkloadIssuers: dlIssuers, TTL: ttl,
		Operator: "dl-op", Host: "dl-host", Now: time.Now(),
	})
	require.NoError(t, err)
	return d
}

func (e *dlEnv) redeemAudits(t *testing.T, delegationID string) []*store.MutationAuditRecord {
	t.Helper()
	audits, _, err := e.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: testIdentityDelegationRedeemOfflineMutation, TargetID: delegationID})
	require.NoError(t, err)
	return audits
}

// R1, R5, positive path: an offline delegation mints a member-tier fixture
// stamped with the reserved grantor and the delegation; the issue audit
// carries the offline facts; every admitted request writes one redeem row.
func TestOfflineDelegation_RedeemOK(t *testing.T) {
	e := newDLEnv(t)
	ctx := context.Background()
	d := e.offline(t, 0)
	reserved := store.TestInfraOfflineGrantorID(x2HubID)

	rec := e.workload(t, http.MethodPost, "/api/v1/test-identities", x2Token(t, x2TokenOpts{}), d.ID,
		map[string]string{"role": "member"}, map[string]string{testIssuerRunLabelHeader: "dl-run"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := tiDecode(t, rec)
	assert.Equal(t, reserved, resp.Identity.IssuedBy)
	assert.Equal(t, d.ID, resp.Identity.IssuerDelegationID)
	row, err := e.s.GetUser(ctx, resp.Identity.ID)
	require.NoError(t, err)
	assert.True(t, row.IsTestFixture(), "an ordinary member-tier fixture")
	assert.Equal(t, store.UserRoleMember, row.Role)

	// No user row for the reserved grantor or the workload.
	_, err = e.s.GetUser(ctx, reserved)
	assert.ErrorIs(t, err, store.ErrNotFound)

	audits, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityIssueMutation, TargetID: resp.Identity.ID})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	var summary struct {
		Workload testIdentityWorkloadAudit `json:"workload_delegation"`
	}
	require.NoError(t, json.Unmarshal([]byte(audits[0].AfterSummary), &summary))
	assert.Equal(t, store.DelegationGrantorTestInfraOffline, summary.Workload.GrantorKind)
	assert.Equal(t, x2HubID, summary.Workload.HubID)
	assert.Equal(t, string(TestModeMember), summary.Workload.TestMode)
	assert.NotEmpty(t, summary.Workload.MarkerCreatedAt)
	assert.Equal(t, reserved, summary.Workload.Grantor)

	redeems := e.redeemAudits(t, d.ID)
	require.Len(t, redeems, 1, "one redeem row per admitted request")
	r := redeems[0]
	assert.Equal(t, store.DelegationGrantorTestInfraOffline, r.ActorPrincipalKind)
	assert.Equal(t, reserved, r.ActorPrincipalID)
	assert.Equal(t, d.ID, r.ActorCredentialID)
	assert.Equal(t, string(CredentialKindWorkloadDelegation), r.ActorCredentialType)
	assert.Equal(t, x2Sub, r.ExecutorID)
	for _, want := range []string{
		"grantor_kind=test_infra_offline", "hub_id=" + x2HubID, "marker_id=" + testInfraMarkerAuditID,
		"marker_created_at=", "test_mode=member", "delegation_id=" + d.ID, "workload_sub=" + x2Sub,
		"route=POST /api/v1/test-identities", `run_label="dl-run"`,
	} {
		assert.Contains(t, r.AfterSummary, want)
	}

	// The list route is a redeem too.
	rec = e.workload(t, http.MethodGet, "/api/v1/test-identities", x2Token(t, x2TokenOpts{}), d.ID, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, e.redeemAudits(t, d.ID), 2)
}

// R4: unmarking the database ends the delegation at the next request, on
// the same running hub.
func TestOfflineDelegation_DiesOnUnmark(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	require.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code)
	before := e.liveFixtures(t)

	e.unmark(t)
	rec := e.mint(t, d.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, before, e.liveFixtures(t))
	rec = e.workload(t, http.MethodGet, "/api/v1/test-identities", x2Token(t, x2TokenOpts{}), d.ID, nil, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Len(t, e.redeemAudits(t, d.ID), 1, "a refused request writes no redeem row")
}

// R4: a new marker (unmark, then mark again) does not revive a delegation
// bound to the old marker instance.
func TestOfflineDelegation_DiesOnRemark(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	require.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code)
	e.unmark(t)
	time.Sleep(2 * time.Millisecond) // a distinct marker creation instant
	e.mark(t)
	rec := e.mint(t, d.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A delegation written against the new marker works.
	d2 := e.offline(t, 0)
	assert.Equal(t, http.StatusCreated, e.mint(t, d2.ID).Code)
}

// R4: an offline revoke ends the delegation at the next request.
func TestOfflineDelegation_DiesOnOfflineRevoke(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	require.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code)
	_, changed, err := testinfra.RevokeDelegation(context.Background(), e.client, testinfra.RevokeDelegationInput{HubID: x2HubID, ID: d.ID, Operator: "dl-op"})
	require.NoError(t, err)
	require.True(t, changed)
	rec := e.mint(t, d.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// R4: an unscoped platform admin can revoke an offline delegation through
// the API (no human's id equals the reserved grantor id, so no other user
// can see or revoke it), and the next request is refused.
func TestOfflineDelegation_DiesOnAPIRevokeByPlatformAdmin(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	require.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code)

	member, _ := e.bindingGrantor(t, "dl-binding-grantor")
	rec := doRequestAsUser(t, e.srv, member, http.MethodDelete, "/api/v1/test-identity-delegations/"+d.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code, "a refused revoke changes nothing")

	admin := e.grantor(t, "dl-platform-admin")
	rec = doRequestAsUser(t, e.srv, admin, http.MethodGet, "/api/v1/test-identity-delegations", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list ListTestIdentityDelegationsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	var view *TestIdentityDelegationView
	for i := range list.Items {
		if list.Items[i].ID == d.ID {
			view = &list.Items[i]
		}
	}
	require.NotNil(t, view, "the platform admin sees the offline delegation")
	assert.Equal(t, store.DelegationGrantorTestInfraOffline, view.GrantorKind)
	assert.Equal(t, x2HubID, view.HubID)
	assert.True(t, view.Live)

	rec = doRequestAsUser(t, e.srv, admin, http.MethodDelete, "/api/v1/test-identity-delegations/"+d.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = e.mint(t, d.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// R4: expiry and the offline maximum age, by the hub clock.
func TestOfflineDelegation_DiesOnExpiry(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, time.Minute)
	require.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code)
	later := time.Now().Add(2 * time.Minute)
	e.srv.testIdentities.now = func() time.Time { return later }
	rec := e.mint(t, d.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// R4: the running test-mode level is read at every request. Below member
// (off) the gate refuses even with test identities still on.
func TestOfflineDelegation_DeniedBelowMember(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	require.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code)
	e.srv.testMode = testModeState{level: TestModeOff}
	rec := e.mint(t, d.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// The admin levels are "member or above".
	for _, level := range []TestMode{TestModeAdmin, TestModeSuperAdmin, TestModeMember} {
		e.srv.testMode = testModeState{level: level}
		assert.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code, "level %s", level)
	}
}

// R4: with test identities off the routes are gone and the arm refuses.
func TestOfflineDelegation_DeniedAtTestModeOff(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	e.srv.testMode = testModeState{level: TestModeOff}
	e.srv.testIdentities = newTestIdentityState(false)
	e.srv.authConfig.TestIdentitiesEnabled = false
	rec := e.mint(t, d.ID)
	assert.NotEqual(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Zero(t, e.liveFixtures(t))
}

// dlBindingStore overrides the marker binding read (and optionally the
// delegation read) of an embedded store.
type dlBindingStore struct {
	store.Store
	bindingErr error
	binding    *store.TestInfraBinding
	userErr    error
	kind       string
}

func (s dlBindingStore) GetTestInfraBinding(ctx context.Context) (*store.TestInfraBinding, error) {
	if s.bindingErr != nil {
		return nil, s.bindingErr
	}
	if s.binding != nil {
		b := *s.binding
		return &b, nil
	}
	return s.Store.GetTestInfraBinding(ctx)
}

func (s dlBindingStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.userErr != nil {
		return nil, s.userErr
	}
	return s.Store.GetUser(ctx, id)
}

func (s dlBindingStore) GetTestIdentityDelegation(ctx context.Context, id string) (*store.TestIdentityDelegation, error) {
	d, err := s.Store.GetTestIdentityDelegation(ctx, id)
	if err == nil && s.kind != "" {
		d.GrantorKind = s.kind
	}
	return d, err
}

// R1: any read error denies (fail closed), and nothing is minted.
func TestOfflineDelegation_ReadErrorsDeny(t *testing.T) {
	for name, st := range map[string]func(store.Store) store.Store{
		"marker read error": func(s store.Store) store.Store {
			return dlBindingStore{Store: s, bindingErr: errors.New("db down")}
		},
		"reserved grantor check error": func(s store.Store) store.Store {
			return dlBindingStore{Store: s, userErr: errors.New("db down")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newDLEnv(t)
			d := e.offline(t, 0)
			e.srv.workloadIssuer.store = st(e.s)
			rec := e.mint(t, d.ID)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Zero(t, e.liveFixtures(t))
			assert.Empty(t, e.redeemAudits(t, d.ID))
		})
	}
}

// R1: the gate runs again inside the mint transaction: a marker removed
// after the request authenticated mints nothing.
func TestOfflineDelegation_GateRunsInsideMintTx(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	orig := e.srv.testIdentities.hooks.writeAudit
	e.srv.testIdentities.hooks.writeAudit = func(ctx context.Context, tx store.Store, record *store.MutationAuditRecord) error {
		if err := orig(ctx, tx, record); err != nil {
			return err
		}
		if record.MutationType == testIdentityDelegationRedeemOfflineMutation {
			// Between authentication and the mint transaction.
			e.unmark(t)
		}
		return nil
	}
	rec := e.mint(t, d.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Zero(t, e.liveFixtures(t), "nothing was minted")
}

// R5: a redeem audit write failure denies.
func TestOfflineDelegation_RedeemAuditWriteFailureDenies(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	e.srv.testIdentities.hooks.writeAudit = func(ctx context.Context, tx store.Store, record *store.MutationAuditRecord) error {
		if record.MutationType == testIdentityDelegationRedeemOfflineMutation {
			return errors.New("audit sink down")
		}
		return tx.CreateMutationAudit(ctx, record)
	}
	rec := e.mint(t, d.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Zero(t, e.liveFixtures(t))
}

// R3: an unknown grantor kind is refused at the arm (no fallthrough to the
// human path), even for a delegation that would otherwise be live.
func TestOfflineDelegation_UnknownGrantorKindDenied(t *testing.T) {
	e := newDLEnv(t)
	grantor := e.grantor(t, "dl-unknown-kind-grantor")
	human := e.delegate(t, grantor, nil)
	require.Equal(t, http.StatusCreated, e.mint(t, human.ID).Code)

	e.srv.workloadIssuer.store = dlBindingStore{Store: e.s, kind: "robot"}
	rec := e.mint(t, human.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A human delegation relabelled offline fails the offline gate (no hub
	// or marker binding, not the reserved grantor).
	e.srv.workloadIssuer.store = dlBindingStore{Store: e.s, kind: store.DelegationGrantorTestInfraOffline}
	rec = e.mint(t, human.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// dlGateAuthenticator is a workload issuer authenticator for gate unit
// tests: member allowlist, hub x2HubID, the given level.
func dlGateAuthenticator(level TestMode) *workloadIssuerAuthenticator {
	return &workloadIssuerAuthenticator{
		audience:      testIssuerAudience(x2HubID),
		allowlist:     map[string]string{x2Sub: config.WorkloadIssuerTierMember},
		now:           time.Now,
		hubID:         x2HubID,
		testModeLevel: func() TestMode { return level },
	}
}

func dlGateDelegation(markerAt time.Time) *store.TestIdentityDelegation {
	now := time.Now()
	return &store.TestIdentityDelegation{
		ID: generateID(), GrantorID: store.TestInfraOfflineGrantorID(x2HubID), WorkloadSub: x2Sub,
		Scope: store.TestInfraOfflineDelegationScope, MemberCap: 1, Created: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		GrantorKind: store.DelegationGrantorTestInfraOffline, HubID: x2HubID, MarkerCreatedAt: &markerAt,
	}
}

// dlNoUserStore answers the two reads the gate makes without a database.
type dlNoUserStore struct {
	store.Store
	binding *store.TestInfraBinding
	err     error
}

func (s dlNoUserStore) GetTestInfraBinding(context.Context) (*store.TestInfraBinding, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.binding == nil {
		return nil, store.ErrNotFound
	}
	b := *s.binding
	return &b, nil
}

func (s dlNoUserStore) GetUser(context.Context, string) (*store.User, error) {
	return nil, store.ErrNotFound
}

// R2, R3, R1 (unit): the gate's three-way hub id binding, member-scope
// structure and fail-closed reads.
func TestOfflineGrantorGate_Unit(t *testing.T) {
	ctx := context.Background()
	markerAt := time.Date(2026, 10, 19, 12, 0, 0, 123456000, time.UTC)
	good := &store.TestInfraBinding{HubID: x2HubID, Class: config.HubClassTest, CreatedAt: markerAt}
	st := dlNoUserStore{binding: good}

	facts, refusal := dlGateAuthenticator(TestModeMember).offlineGrantorGate(ctx, st, dlGateDelegation(markerAt))
	require.Nil(t, refusal)
	assert.Equal(t, x2HubID, facts.hubID)
	assert.Equal(t, TestModeMember, facts.level)

	cases := map[string]struct {
		a   func() *workloadIssuerAuthenticator
		st  store.Store
		mut func(*store.TestIdentityDelegation)
	}{
		"live hub id differs": {a: func() *workloadIssuerAuthenticator {
			a := dlGateAuthenticator(TestModeMember)
			a.hubID = "x2-other-hub"
			return a
		}, st: st},
		"live hub id unresolved": {a: func() *workloadIssuerAuthenticator {
			a := dlGateAuthenticator(TestModeMember)
			a.hubID = ""
			return a
		}, st: st},
		"delegation hub id differs": {st: st, mut: func(d *store.TestIdentityDelegation) { d.HubID = "x2-other-hub" }},
		"grantor is another hub's reserved id": {st: st, mut: func(d *store.TestIdentityDelegation) {
			d.GrantorID = store.TestInfraOfflineGrantorID("x2-other-hub")
		}},
		"marker hub id differs": {st: dlNoUserStore{binding: &store.TestInfraBinding{HubID: "x2-other-hub", Class: config.HubClassTest, CreatedAt: markerAt}}},
		"marker class not test": {st: dlNoUserStore{binding: &store.TestInfraBinding{HubID: x2HubID, Class: "prod", CreatedAt: markerAt}}},
		"marker absent":         {st: dlNoUserStore{}},
		"marker read error":     {st: dlNoUserStore{err: errors.New("db down")}},
		"marker re-created":     {st: dlNoUserStore{binding: &store.TestInfraBinding{HubID: x2HubID, Class: config.HubClassTest, CreatedAt: markerAt.Add(time.Second)}}},
		"no marker binding":     {st: st, mut: func(d *store.TestIdentityDelegation) { d.MarkerCreatedAt = nil }},
		"admin scope":           {st: st, mut: func(d *store.TestIdentityDelegation) { d.Scope = testIdentityDelegationAdminScope }},
		"super-admin scope":     {st: st, mut: func(d *store.TestIdentityDelegation) { d.Scope = testIdentityDelegationSuperAdminScope }},
		"human kind":            {st: st, mut: func(d *store.TestIdentityDelegation) { d.GrantorKind = store.DelegationGrantorHuman }},
		"unknown kind":          {st: st, mut: func(d *store.TestIdentityDelegation) { d.GrantorKind = "robot" }},
		"revoked": {st: st, mut: func(d *store.TestIdentityDelegation) {
			at := time.Now()
			d.RevokedAt = &at
		}},
		"expired":              {st: st, mut: func(d *store.TestIdentityDelegation) { d.ExpiresAt = time.Now().Add(-time.Second) }},
		"past offline max age": {st: st, mut: func(d *store.TestIdentityDelegation) { d.Created = time.Now().Add(-25 * time.Hour) }},
		"subject off allowlist": {st: st, mut: func(d *store.TestIdentityDelegation) {
			d.WorkloadSub = "100000000000000000099"
		}},
		"test-mode off":     {a: func() *workloadIssuerAuthenticator { return dlGateAuthenticator(TestModeOff) }, st: st},
		"test-mode unknown": {a: func() *workloadIssuerAuthenticator { return dlGateAuthenticator(TestMode("")) }, st: st},
		"no test-mode reader": {a: func() *workloadIssuerAuthenticator {
			a := dlGateAuthenticator(TestModeMember)
			a.testModeLevel = nil
			return a
		}, st: st},
		"reserved id names a user": {st: dlUserExistsStore{dlNoUserStore{binding: good}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := dlGateAuthenticator(TestModeMember)
			if tc.a != nil {
				a = tc.a()
			}
			d := dlGateDelegation(markerAt)
			if tc.mut != nil {
				tc.mut(d)
			}
			facts, refusal := a.offlineGrantorGate(ctx, tc.st, d)
			require.NotNil(t, refusal, "the gate must refuse")
			assert.Nil(t, facts)
			assert.Equal(t, http.StatusForbidden, refusal.status)
		})
	}

	// The same instant in a different text format (timestamp
	// normalization) is the same marker.
	same := &store.TestInfraBinding{HubID: x2HubID, Class: config.HubClassTest, CreatedAt: markerAt.In(time.FixedZone("x", 3600))}
	_, refusal = dlGateAuthenticator(TestModeMember).offlineGrantorGate(ctx, dlNoUserStore{binding: same}, dlGateDelegation(markerAt))
	assert.Nil(t, refusal)
}

type dlUserExistsStore struct{ dlNoUserStore }

func (dlUserExistsStore) GetUser(_ context.Context, id string) (*store.User, error) {
	return &store.User{ID: id, Kind: store.UserKindHuman, Status: store.UserStatusActive}, nil
}

// R3: the human path rejects an offline (or any non-human) grantor kind
// explicitly, before reading any user row.
func TestLiveHumanGrantor_RejectsNonHumanKind(t *testing.T) {
	a := dlGateAuthenticator(TestModeMember)
	a.store = dlUserExistsStore{}
	for _, kind := range []string{store.DelegationGrantorTestInfraOffline, "robot"} {
		d := dlGateDelegation(time.Now())
		d.GrantorKind = kind
		u, refusal := a.liveHumanGrantor(context.Background(), d)
		assert.Nil(t, u, kind)
		require.NotNil(t, refusal, kind)
		assert.Equal(t, http.StatusForbidden, refusal.status)
	}
}

// R3: checkDelegationLive refuses an offline row holding an admin scope,
// whatever else it says.
func TestCheckDelegationLive_OfflineAdminScope(t *testing.T) {
	a := dlGateAuthenticator(TestModeMember)
	a.allowlist = map[string]string{x2Sub: config.WorkloadIssuerTierAdmin}
	a.adminScopes = func(testAdminKind) bool { return true }
	d := dlGateDelegation(time.Now())
	d.Scope = testIdentityDelegationAdminScope
	require.NotNil(t, a.checkDelegationLive(d, time.Now()))
	d.GrantorKind = store.DelegationGrantorHuman
	assert.Nil(t, a.checkDelegationLive(d, time.Now()), "the same row with a human grantor passes the delegation check (X4 unchanged)")
}

// dlOfflineCtx builds a request context as the workload arm would for an
// offline delegation, with offlineGatePassed as given.
func dlOfflineCtx(t *testing.T, d *store.TestIdentityDelegation, passed bool) (context.Context, *ScopedUserIdentity) {
	t.Helper()
	ceiling, ok := permissions.BuildCeilingFromSelectors([]string{d.Scope})
	require.True(t, ok)
	base := NewAuthenticatedUser(d.GrantorID, "", offlineGrantorDisplayName, store.UserRoleMember, string(ClientTypeAPI))
	identity := NewScopedUserIdentityWithBoundary(base, TokenBoundary{Kind: BoundaryKindHub}, []string{d.Scope}, d.ID, ceiling)
	cred := credentialContextForIdentity(identity)
	cred.Kind = CredentialKindWorkloadDelegation
	cred.Type = string(CredentialKindWorkloadDelegation)
	st := &workloadDelegationState{delegation: d, sub: d.WorkloadSub, offlineGatePassed: passed}
	if passed {
		st.offline = &offlineGateFacts{hubID: x2HubID, level: TestModeMember}
	}
	ctx := context.WithValue(context.Background(), userContextKey{}, identity)
	ctx = contextWithIdentity(ctx, identity)
	ctx = contextWithCredentialContext(ctx, cred)
	ctx = contextWithWorkloadDelegationState(ctx, st)
	return ctx, identity
}

func dlDecide(e *dlEnv, ctx context.Context, identity *ScopedUserIdentity, permission string) Decision {
	return e.srv.authzService.Decide(ctx, AuthzRequest{
		Principal:      principalContextForIdentity(identity),
		Credential:     credentialContextForIdentity(identity),
		Resource:       Resource{Type: permissions.ResourceTestIdentity},
		Action:         Action(permissionAction(permission)),
		Permission:     permission,
		TargetEvidence: hubCollectionEvidence(permission),
	})
}

// Advisor Q4 pin test: an offline delegation whose state does NOT say the
// gate passed gets no synthetic binding, and Decide denies; with the gate
// passed it gets exactly test_identity.issue.
func TestOfflineDelegationBinding_RequiresGatePassed(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)

	ctx, identity := dlOfflineCtx(t, d, false)
	cb, role := offlineDelegationBinding(ctx, principalContextForIdentity(identity), credentialContextForIdentity(identity), permissionTestIdentityIssue)
	assert.Nil(t, cb)
	assert.Nil(t, role)
	assert.False(t, dlDecide(e, ctx, identity, permissionTestIdentityIssue).Allowed, "offline delegation without a passed gate: deny")

	ctx, identity = dlOfflineCtx(t, d, true)
	cb, role = offlineDelegationBinding(ctx, principalContextForIdentity(identity), credentialContextForIdentity(identity), permissionTestIdentityIssue)
	require.NotNil(t, cb)
	require.NotNil(t, role)
	assert.Equal(t, map[string]struct{}{permissionTestIdentityIssue: {}}, role.Permissions)
	assert.Equal(t, ScopeTypeSystem, cb.ScopeType)
	assert.True(t, dlDecide(e, ctx, identity, permissionTestIdentityIssue).Allowed, "gate passed: test_identity.issue only")
}

// R6 / Q4 (a)-(c): the synthetic grant covers exactly test_identity.issue,
// only for the delegation the state names, and only for an offline kind.
func TestOfflineDelegationBinding_OnlyIssueOnlyThisRequest(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	ctx, identity := dlOfflineCtx(t, d, true)
	principal := principalContextForIdentity(identity)
	cred := credentialContextForIdentity(identity)

	for _, p := range []string{permissionTestIdentityIssueAdmin, permissionTestIdentityIssueSuperAdmin, "user.read", ""} {
		cb, _ := offlineDelegationBinding(ctx, principal, cred, p)
		assert.Nil(t, cb, "permission %q", p)
	}
	assert.False(t, dlDecide(e, ctx, identity, permissionTestIdentityIssueAdmin).Allowed)

	// A context without the arm's state (another request) gets nothing.
	bare := contextWithCredentialContext(context.Background(), GetCredentialContextFromContext(ctx))
	cb, _ := offlineDelegationBinding(bare, principal, cred, permissionTestIdentityIssue)
	assert.Nil(t, cb)

	// Mismatched ids or a forged human-kind state get nothing.
	other := cred
	other.ID = generateID()
	cb, _ = offlineDelegationBinding(ctx, principal, other, permissionTestIdentityIssue)
	assert.Nil(t, cb, "credential id differs")
	wrongPrincipal := principal
	wrongPrincipal.ID = generateID()
	cb, _ = offlineDelegationBinding(ctx, wrongPrincipal, cred, permissionTestIdentityIssue)
	assert.Nil(t, cb, "principal differs")
	human := *d
	human.GrantorKind = store.DelegationGrantorHuman
	hctx, hidentity := dlOfflineCtx(t, &human, true)
	cb, _ = offlineDelegationBinding(hctx, principalContextForIdentity(hidentity), credentialContextForIdentity(hidentity), permissionTestIdentityIssue)
	assert.Nil(t, cb, "human kind")
	adminScope := *d
	adminScope.Scope = testIdentityDelegationAdminScope
	actx, aidentity := dlOfflineCtx(t, &adminScope, true)
	cb, _ = offlineDelegationBinding(actx, principalContextForIdentity(aidentity), credentialContextForIdentity(aidentity), permissionTestIdentityIssue)
	assert.Nil(t, cb, "admin scope")
}

// R6: no chaining. The workload reaches only the issuance routes, cannot
// create delegations, and the fixtures it mints cannot issue or delegate.
func TestOfflineDelegation_NoChaining(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	rec := e.mint(t, d.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	fixture := tiDecode(t, rec)

	tok := func() string { return x2Token(t, x2TokenOpts{}) }
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identity-delegations", tok(), d.ID,
		map[string]any{"workloadSub": x2OtherSub, "scopes": []string{"test_identity:issue"}}, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "a workload cannot create a delegation")
	for _, path := range []string{"/api/v1/users", "/api/v1/test-identity-delegations", "/api/v1/auth/me"} {
		rec = e.workload(t, http.MethodGet, path, tok(), d.ID, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s is outside the issuance routes", path)
	}
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities/admin", tok(), d.ID, map[string]string{"adminRole": "hub-admin"}, nil)
	assert.NotEqual(t, http.StatusCreated, rec.Code, "never an admin kind")

	rec = doRequestWithToken(t, e.srv, fixture.AccessToken, http.MethodPost, "/api/v1/test-identities", map[string]string{"role": "member"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a fixture cannot issue")
	rec = doRequestWithToken(t, e.srv, fixture.AccessToken, http.MethodPost, "/api/v1/test-identity-delegations",
		map[string]any{"workloadSub": x2OtherSub, "scopes": []string{"test_identity:issue"}})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a fixture cannot delegate")
}

// Regression: the human-grantor path is unchanged. Its delegation is
// "human", its mint carries no offline facts, and it writes no offline
// redeem row.
func TestHumanDelegation_UnchangedRegression(t *testing.T) {
	e := newDLEnv(t)
	ctx := context.Background()
	grantor := e.grantor(t, "dl-human-grantor")
	d := e.delegate(t, grantor, nil)
	assert.Equal(t, store.DelegationGrantorHuman, d.GrantorKind)
	assert.Empty(t, d.HubID)

	rec := e.mint(t, d.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := tiDecode(t, rec)
	assert.Equal(t, grantor.ID, resp.Identity.IssuedBy)
	audits, _, err := e.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityIssueMutation, TargetID: resp.Identity.ID})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	assert.NotContains(t, audits[0].AfterSummary, "grantor_kind")
	assert.NotContains(t, audits[0].AfterSummary, "test_mode")
	assert.Empty(t, e.redeemAudits(t, d.ID))

	// The human path still needs no marker or test-mode reader: its live
	// check is the grantor.
	e.unmark(t)
	assert.Equal(t, http.StatusCreated, e.mint(t, d.ID).Code)
}

// OPT-1 (ti-dl-rev round 1): testAdminIssuer refuses a workload delegation
// whose grantor kind is not human even when its scope covers the admin
// kind, and even with gate-passed offline state (defence in depth: the
// arm and checkDelegationLive already refuse such a row). The control (a
// human-kind state of the same scope) passes this branch and is refused
// later, by the caller-kind check, with its distinct detail reason.
func TestTestAdminIssuer_RefusesNonHumanGrantorKind(t *testing.T) {
	e := newDLEnv(t)
	spec := testAdminKindSpecs[testAdminRoleHubAdmin]
	call := func(d *store.TestIdentityDelegation, passed bool) *httptest.ResponseRecorder {
		ctx, _ := dlOfflineCtx(t, d, passed)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/test-identities/admin", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		user, _, st, ok := e.srv.testAdminIssuer(rr, req, spec)
		assert.False(t, ok)
		assert.Nil(t, user)
		assert.Nil(t, st)
		return rr
	}
	offline := dlGateDelegation(time.Now())
	offline.Scope = testIdentityDelegationAdminScope
	for _, passed := range []bool{true, false} {
		rr := call(offline, passed)
		assert.Equal(t, http.StatusForbidden, rr.Code)
		assert.NotContains(t, rr.Body.String(), testAdminReasonCallerNotHuman, "refused at the grantor-kind branch, before the caller-kind check")
	}
	unknown := *offline
	unknown.GrantorKind = "robot"
	rr := call(&unknown, false)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.NotContains(t, rr.Body.String(), testAdminReasonCallerNotHuman)

	// A forged human-kind state carrying offline gate state is refused too.
	human := *offline
	human.GrantorKind = store.DelegationGrantorHuman
	rr = call(&human, true)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.NotContains(t, rr.Body.String(), testAdminReasonCallerNotHuman)

	// Control: a clean human-kind state passes the grantor-kind branch and
	// is refused by the caller-kind check (the reserved id names no user).
	rr = call(&human, false)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.Contains(t, rr.Body.String(), testAdminReasonCallerNotHuman)
}

// OPT-5 (ti-dl-rev round 1): token re-issue and the web-login code for an
// identity issued through an offline delegation share the mint-transaction
// recheck: they work while the gate passes and are refused once the marker
// is gone.
func TestOfflineDelegation_ReissueAndWebCode(t *testing.T) {
	e := newDLEnv(t)
	x2EnableWebSessions(t, e.x2Env)
	d := e.offline(t, 0)
	rec := e.mint(t, d.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := tiDecode(t, rec).Identity.ID
	tok := func() string { return x2Token(t, x2TokenOpts{}) }

	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities/"+id+"/token", tok(), d.ID, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities/"+id+"/web-login-code", tok(), d.ID, nil, nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	webAudits := x2WebCodeAudits(t, e.x2Env, id)
	require.Len(t, webAudits, 1)
	assert.Equal(t, store.TestInfraOfflineGrantorID(x2HubID), webAudits[0].ActorPrincipalID)

	// Refused inside the mint transaction: the marker goes away between
	// authentication and the re-issue/web-code transaction.
	orig := e.srv.testIdentities.hooks.writeAudit
	unmarkNext := false
	e.srv.testIdentities.hooks.writeAudit = func(ctx context.Context, tx store.Store, record *store.MutationAuditRecord) error {
		if err := orig(ctx, tx, record); err != nil {
			return err
		}
		if unmarkNext && record.MutationType == testIdentityDelegationRedeemOfflineMutation {
			unmarkNext = false
			e.unmark(t)
		}
		return nil
	}
	unmarkNext = true
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities/"+id+"/token", tok(), d.ID, nil, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	e.mark(t)
	// A new marker does not revive the delegation (marker instance binding).
	rec = e.workload(t, http.MethodPost, "/api/v1/test-identities/"+id+"/web-login-code", tok(), d.ID, nil, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Len(t, x2WebCodeAudits(t, e.x2Env, id), 1, "no second web-login code was minted")
}

// X4b merge (phase DL): testAdminCredentialIssuer, the admission of an
// admin-kind credential (token re-issue, web-login code) through a
// workload delegation, refuses a non-human grantor kind first, before any
// grantor read; a forged human-kind state carrying offline gate state is
// refused too. The control (a clean human-kind state) passes this branch
// and is refused later, by the live super-admin check, with its distinct
// detail reason.
func TestTestAdminCredentialIssuer_RefusesNonHumanGrantorKind(t *testing.T) {
	e := newDLEnv(t)
	spec := testAdminKindSpecs[testAdminRoleHubAdmin]
	exp := time.Now().Add(time.Hour)
	call := func(d *store.TestIdentityDelegation, passed bool) *httptest.ResponseRecorder {
		ctx, identity := dlOfflineCtx(t, d, passed)
		delegationID, grantor := d.ID, d.GrantorID
		u := &store.User{
			ID: generateID(), Kind: store.UserKindTestHubAdmin, Status: store.UserStatusActive,
			ExpiresAt: &exp, Created: time.Now(), IssuedBy: &grantor, IssuerDelegationID: &delegationID,
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/test-identities/"+u.ID+"/token", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		ok := e.srv.testAdminCredentialIssuer(rr, req, identity, u, map[string]bool{spec.permission: true})
		assert.False(t, ok)
		return rr
	}
	offline := dlGateDelegation(time.Now())
	offline.Scope = testIdentityDelegationAdminScope
	for _, passed := range []bool{true, false} {
		rr := call(offline, passed)
		assert.Equal(t, http.StatusForbidden, rr.Code)
		assert.NotContains(t, rr.Body.String(), testAdminReasonNotLiveAdmin, "refused at the grantor-kind branch, before the grantor read")
	}
	unknown := *offline
	unknown.GrantorKind = "robot"
	rr := call(&unknown, false)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.NotContains(t, rr.Body.String(), testAdminReasonNotLiveAdmin)

	human := *offline
	human.GrantorKind = store.DelegationGrantorHuman
	rr = call(&human, true)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.NotContains(t, rr.Body.String(), testAdminReasonNotLiveAdmin)

	// Control: a clean human-kind state reaches the live super-admin check
	// (the reserved id names no user, so it is refused there).
	rr = call(&human, false)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.Contains(t, rr.Body.String(), testAdminReasonNotLiveAdmin)
}

// X4b merge (phase DL): the in-transaction grantor recheck of the admin
// token re-issue and web-code paths passes the delegation's grantor kind,
// so an offline kind is refused there as well (unit: the recheck itself).
func TestRecheckTestAdminDelegationGrantor_OfflineKindRefused(t *testing.T) {
	e := newDLEnv(t)
	ctx := context.Background()
	err := e.s.WithTx(ctx, func(tx store.Store) error {
		return e.srv.recheckTestAdminDelegationGrantor(ctx, tx, store.DelegationGrantorTestInfraOffline, store.TestInfraOfflineGrantorID(x2HubID), permissionTestIdentityIssueAdmin)
	})
	require.ErrorIs(t, err, errTestAdminIssuerNotLive)
}

// The account-status gate admits the offline delegation's reserved grantor,
// which has no users row, on a request whose offline gate passed, for
// test_identity.issue. The allow comes from decide Step 5b4's synthetic
// binding.
func TestDecide_AccountStatusGate_OfflineGrantorPassesWithPassedGate(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	require.Equal(t, store.TestInfraOfflineGrantorID(x2HubID), d.GrantorID, "precondition: the reserved grantor id")
	_, err := e.s.GetUser(context.Background(), d.GrantorID)
	require.ErrorIs(t, err, store.ErrNotFound, "precondition: no users row for the reserved grantor")

	ctx, identity := dlOfflineCtx(t, d, true)
	dec := dlDecide(e, ctx, identity, permissionTestIdentityIssue)
	assert.True(t, dec.Allowed, "reason %q", dec.Reason)
	assert.NotEqual(t, principalNotActiveReason, dec.Reason)
	assert.Equal(t, "synthetic:test-infra-offline-delegation:"+d.ID, dec.BindingID,
		"the allow comes from the Step 5b4 synthetic binding")
}

// The account-status gate refuses a principal with no users row unless the
// request is one that decide Step 5b4 grants and the principal is the
// reserved grantor of the delegation's hub.
func TestDecide_AccountStatusGate_OfflineGrantorRefusedWithoutProof(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	require.Equal(t, store.TestInfraOfflineGrantorID(x2HubID), d.GrantorID, "precondition: the reserved grantor id")

	other := *d
	other.GrantorID = generateID()
	require.NotEqual(t, store.TestInfraOfflineGrantorID(other.HubID), other.GrantorID)

	cases := []struct {
		name       string
		delegation *store.TestIdentityDelegation
		passed     bool
		bareCtx    bool
		permission string
	}{
		{name: "a_gate_not_passed", delegation: d, passed: false, permission: permissionTestIdentityIssue},
		{name: "b_other_permission", delegation: d, passed: true, permission: "user.read"},
		{name: "c_issue_admin", delegation: d, passed: true, permission: "test_identity.issue_admin"},
		{name: "d_grantor_not_reserved_id", delegation: &other, passed: true, permission: permissionTestIdentityIssue},
		{name: "e_no_delegation_state", delegation: d, passed: true, bareCtx: true, permission: permissionTestIdentityIssue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, identity := dlOfflineCtx(t, tc.delegation, tc.passed)
			if tc.bareCtx {
				ctx = context.Background()
			}
			dec := dlDecide(e, ctx, identity, tc.permission)
			assert.False(t, dec.Allowed)
			assert.Equal(t, principalNotActiveReason, dec.Reason)
		})
	}
}

// A users row with the reserved grantor id goes through the account-status
// rule like any other row: an invited row is refused, also on a request
// whose offline gate passed.
func TestDecide_AccountStatusGate_OfflineGrantorRowStillChecked(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	require.Equal(t, store.TestInfraOfflineGrantorID(x2HubID), d.GrantorID, "precondition: the reserved grantor id")
	require.NoError(t, e.s.CreateUser(context.Background(), &store.User{ID: d.GrantorID, Email: "dl-reserved-row@test.com",
		DisplayName: "reserved row", Role: store.UserRoleMember, Status: store.UserStatusInvited, Created: time.Now()}))

	ctx, identity := dlOfflineCtx(t, d, true)
	dec := dlDecide(e, ctx, identity, permissionTestIdentityIssue)
	assert.False(t, dec.Allowed)
	assert.Equal(t, principalNotActiveReason, dec.Reason)
}

// A users-row lookup fault denies the reserved grantor too, tagged as a
// resolution error, on a request whose offline gate passed.
func TestDecide_AccountStatusGate_OfflineGrantorLookupFaultDenies(t *testing.T) {
	e := newDLEnv(t)
	d := e.offline(t, 0)
	e.srv.authzService.store = dlUserReadFaultStore{Store: e.srv.authzService.store}

	ctx, identity := dlOfflineCtx(t, d, true)
	dec := dlDecide(e, ctx, identity, permissionTestIdentityIssue)
	assert.False(t, dec.Allowed)
	assert.Equal(t, principalStatusLookupReason, dec.Reason)
	assert.Equal(t, DenyCauseResolutionError, dec.DenyCause)
}

// dlUserReadFaultStore fails every users-row read with a store error.
type dlUserReadFaultStore struct{ store.Store }

func (dlUserReadFaultStore) GetUser(context.Context, string) (*store.User, error) {
	return nil, errors.New("injected user read failure")
}

// Source pin: offlineGrantorStatusExempt is referenced once in this
// package's non-test code, inside principalStatusGate, and the workload
// delegation state's offlineGatePassed field is assigned once, in
// test_identity_workload_issuer.go.
func TestPrincipalStatusGate_OfflineExemptionSingleSite(t *testing.T) {
	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, names)
	fset := token.NewFileSet()
	var exemptRefs, gateAssigns []string
	// scan records, under where, every reference to the exemption helper,
	// every assignment to a field named offlineGatePassed, and every
	// workloadDelegationState literal that sets it (keyed) or sets fields
	// positionally.
	scan := func(node ast.Node, where string) {
		ast.Inspect(node, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if n.Name == "offlineGrantorStatusExempt" {
					exemptRefs = append(exemptRefs, where)
				}
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "offlineGatePassed" {
						gateAssigns = append(gateAssigns, where)
					}
				}
			case *ast.KeyValueExpr:
				if k, ok := n.Key.(*ast.Ident); ok && k.Name == "offlineGatePassed" {
					gateAssigns = append(gateAssigns, where+" (keyed literal)")
				}
			case *ast.CompositeLit:
				if id, ok := n.Type.(*ast.Ident); ok && id.Name == "workloadDelegationState" {
					for _, el := range n.Elts {
						if _, keyed := el.(*ast.KeyValueExpr); !keyed {
							gateAssigns = append(gateAssigns, where+" (positional literal)")
							break
						}
					}
				}
			}
			return true
		})
	}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err, name)
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				if fn.Body != nil {
					scan(fn.Body, name+":"+fn.Name.Name)
				}
				continue
			}
			scan(decl, name+":<package level>")
		}
	}
	assert.Equal(t, []string{"authz_principal_status.go:principalStatusGate"}, exemptRefs,
		"offlineGrantorStatusExempt has exactly one caller")
	require.Len(t, gateAssigns, 1, "offlineGatePassed is assigned once: %v", gateAssigns)
	assert.True(t, strings.HasPrefix(gateAssigns[0], "test_identity_workload_issuer.go:"), "offlineGatePassed is assigned in %s", gateAssigns[0])
}
