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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	hostSARegOld = "reg-old@host-project.iam.gserviceaccount.com"
	hostSARegNew = "reg-new@host-project.iam.gserviceaccount.com"
)

// createBrokerRegistrationWithHostSA is CreateBrokerRegistration with an
// allowed host service account decision for exactly the value the request
// asks for, for service-level tests that set one.
func createBrokerRegistrationWithHostSA(ctx context.Context, svc *BrokerAuthService, req CreateBrokerRegistrationRequest, createdBy string) (*CreateBrokerRegistrationResponse, error) {
	v := hostSAClaimVerdict{
		Claim:   hostSAClaim{Email: strings.ToLower(req.GCPHostServiceAccountEmail), ProjectID: req.GCPHostProjectID},
		Allowed: true,
		Checked: true,
	}
	return svc.createBrokerRegistration(ctx, req, createdBy, "", false, true, v)
}

// requireDeniedRegisterEvent asserts the single audit record of a refused
// registration: the register surface, the requested email and a non-allow
// decision.
func requireDeniedRegisterEvent(t *testing.T, audit *mockAuditLogger, email string) {
	t.Helper()
	ev := onlySAEvent(t, audit)
	assert.Equal(t, SurfaceBrokerHostSARegister, ev.Surface)
	assert.Equal(t, email, ev.TargetSAEmail)
	require.NotNil(t, ev.Decision)
	assert.NotEqual(t, store.ActAsAllowed, *ev.Decision)
}

// hostSARegFixture is a test server with a hub member who registers brokers.
type hostSARegFixture struct {
	srv   *Server
	store store.Store
	user  *store.User
	audit *mockAuditLogger
}

func setupHostSAReg(t *testing.T, name string, checker store.CallerPermissionChecker) *hostSARegFixture {
	t.Helper()
	srv, s := testServer(t)
	f := &hostSARegFixture{srv: srv, store: s, user: newHubMemberUser(t, s, name+"-user")}
	if checker != nil {
		enforceSAAssign(srv, checker)
	}
	f.audit = &mockAuditLogger{}
	srv.SetAuditLogger(f.audit)
	return f
}

func (f *hostSARegFixture) register(t *testing.T, req CreateBrokerRegistrationRequest) (int, string) {
	t.Helper()
	rec := doRequestAsUserLoopback(t, f.srv, f.user, http.MethodPost, "/api/v1/brokers", req)
	return rec.Code, rec.Body.String()
}

// seedLegacyBroker stores a legacy broker owned by the fixture user with the
// given host service account. An empty email stores no host pair.
func (f *hostSARegFixture) seedLegacyBroker(t *testing.T, name, email string) *store.RuntimeBroker {
	t.Helper()
	b := &store.RuntimeBroker{
		ID:                         tid(name),
		Name:                       name,
		Slug:                       name,
		Status:                     store.BrokerStatusOffline,
		GCPHostServiceAccountEmail: email,
		Created:                    time.Now(),
		Updated:                    time.Now(),
		CreatedBy:                  f.user.ID,
	}
	if email != "" {
		b.GCPHostProjectID = "host-project"
		b.GCPHostSource = store.GCPHostSourceRegistration
	}
	require.NoError(t, f.store.CreateRuntimeBroker(context.Background(), b))
	return b
}

func (f *hostSARegFixture) stored(t *testing.T, id string) hostSAClaim {
	t.Helper()
	b, err := f.store.GetRuntimeBroker(context.Background(), id)
	require.NoError(t, err)
	return hostSAClaimOf(b)
}

func TestBrokerRegistration_HostSA_CallerLacksActAs_Denied(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker().DenyTarget(hostSARegNew, "no actAs grant")
	f := setupHostSAReg(t, "hsa-reg-deny", checker)

	code, body := f.register(t, CreateBrokerRegistrationRequest{
		Name:                       "hsa-reg-deny-broker",
		GCPHostServiceAccountEmail: hostSARegNew,
		GCPHostProjectID:           "host-project",
	})

	assert.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, store.PermissionActAs)
	assert.NotContains(t, body, "joinToken", "no join token is issued")
	assert.Equal(t, 1, checker.CallCount())
	brokers, err := f.store.ListRuntimeBrokers(context.Background(), store.RuntimeBrokerFilter{}, store.ListOptions{})
	require.NoError(t, err)
	for _, b := range brokers.Items {
		assert.NotEqual(t, "hsa-reg-deny-broker", b.Name, "no row is created")
	}
	requireDeniedRegisterEvent(t, f.audit, hostSARegNew)
}

func TestBrokerRegistration_HostSA_CallerWithActAs_Allowed(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSARegNew)
	f := setupHostSAReg(t, "hsa-reg-allow", checker)

	rec := doRequestAsUserLoopback(t, f.srv, f.user, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:                       "hsa-reg-allow-broker",
		GCPHostServiceAccountEmail: "Reg-New@host-project.iam.gserviceaccount.com",
		GCPHostProjectID:           "host-project",
	})

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	resp := decodeRegistration(t, rec)
	assert.NotEmpty(t, resp.JoinToken)
	assert.Equal(t, hostSAClaim{Email: hostSARegNew, ProjectID: "host-project"}, f.stored(t, resp.BrokerID))
	calls := checker.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, f.user.ID, calls[0].Caller.ID, "actAs is evaluated for the registering user")
	ev := onlySAEvent(t, f.audit)
	assert.Equal(t, SurfaceBrokerHostSARegister, ev.Surface)
}

// TestBrokerReregistration_HostSA_FillLacksActAs_Denied: a re-registration
// that would fill an unset host pair needs actAs on the requested value.
func TestBrokerReregistration_HostSA_FillLacksActAs_Denied(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker().DenyTarget(hostSARegNew, "no actAs grant")
	f := setupHostSAReg(t, "hsa-rereg-deny", checker)
	b := f.seedLegacyBroker(t, "hsa-rereg-deny-broker", "")

	code, body := f.register(t, CreateBrokerRegistrationRequest{
		Name:                       b.Name,
		GCPHostServiceAccountEmail: hostSARegNew,
		GCPHostProjectID:           "host-project",
		Labels:                     map[string]string{"env": "changed"},
	})

	assert.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.NotContains(t, body, "joinToken")
	assert.Equal(t, 1, checker.CallCount())
	got, err := f.store.GetRuntimeBroker(context.Background(), b.ID)
	require.NoError(t, err)
	assert.Empty(t, got.GCPHostServiceAccountEmail, "the pair stays unset")
	assert.NotEqual(t, "changed", got.Labels["env"], "nothing else is written either")
	requireDeniedRegisterEvent(t, f.audit, hostSARegNew)
}

func TestBrokerReregistration_HostSA_UnchangedValue_NoCheck(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker() // denies anything it is asked
	f := setupHostSAReg(t, "hsa-rereg-same", checker)
	b := f.seedLegacyBroker(t, "hsa-rereg-same-broker", hostSARegOld)

	code, body := f.register(t, CreateBrokerRegistrationRequest{
		Name:                       b.Name,
		GCPHostServiceAccountEmail: "Reg-Old@host-project.iam.gserviceaccount.com",
		GCPHostProjectID:           "host-project",
	})

	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	assert.Equal(t, 0, checker.CallCount(), "re-sending the stored value makes no check")
	assert.Empty(t, f.audit.saEvents)
	assert.Equal(t, hostSARegOld, f.stored(t, b.ID).Email)
}

// TestBrokerReregistration_HostSA_Omitted_NoCheck: a re-registration
// without a host pair writes none (an empty value never clears the stored
// pair) and makes no check.
func TestBrokerReregistration_HostSA_Omitted_NoCheck(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker()
	f := setupHostSAReg(t, "hsa-rereg-clear", checker)
	b := f.seedLegacyBroker(t, "hsa-rereg-clear-broker", hostSARegOld)

	code, body := f.register(t, CreateBrokerRegistrationRequest{Name: b.Name})

	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	assert.Equal(t, 0, checker.CallCount())
	assert.Equal(t, hostSARegOld, f.stored(t, b.ID).Email)
}

func TestBrokerReregistration_PreserveSettings_NoCheck(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker()
	f := setupHostSAReg(t, "hsa-rereg-preserve", checker)
	b := f.seedLegacyBroker(t, "hsa-rereg-preserve-broker", hostSARegOld)

	code, body := f.register(t, CreateBrokerRegistrationRequest{
		Name:                       b.Name,
		PreserveSettings:           true,
		GCPHostServiceAccountEmail: hostSARegNew,
	})

	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	assert.Equal(t, 0, checker.CallCount())
	assert.Empty(t, f.audit.saEvents)
	assert.Equal(t, hostSARegOld, f.stored(t, b.ID).Email)
}

func TestBrokerRegistration_HostSA_CheckModeOff_AllowedAndAudited(t *testing.T) {
	f := setupHostSAReg(t, "hsa-reg-off", nil)
	require.Equal(t, SAAssignCheckOff, f.srv.saAssignCheckModeValue())

	rec := doRequestAsUserLoopback(t, f.srv, f.user, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:                       "hsa-reg-off-broker",
		GCPHostServiceAccountEmail: hostSARegNew,
	})

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	resp := decodeRegistration(t, rec)
	assert.Equal(t, hostSARegNew, f.stored(t, resp.BrokerID).Email)
	ev := onlySAEvent(t, f.audit)
	assert.Equal(t, SurfaceBrokerHostSARegister, ev.Surface)
	assert.Equal(t, store.MechanismCheckDisabled, ev.Mechanism)
}

func TestFlatRegistration_HostSA_CallerLacksActAs_Denied(t *testing.T) {
	f := newFlatRegFixture(t, true)
	checker := store.NewFakeCallerPermissionChecker().DenyTarget(hostSARegNew, "no actAs grant")
	enforceSAAssign(f.srv, checker)
	audit := &mockAuditLogger{}
	f.srv.SetAuditLogger(audit)
	id := tid("hsa-flat-deny")

	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{
		BrokerID: id, Name: "hsa-flat-deny", RuntimeTarget: f.targetFor(id),
		GCPHostServiceAccountEmail: hostSARegNew, GCPHostProjectID: "host-project",
	})

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "joinToken")
	_, err := f.s.GetRuntimeBroker(context.Background(), id)
	assert.ErrorIs(t, err, store.ErrNotFound, "no row is created")
	assert.Equal(t, 1, checker.CallCount())
	requireDeniedRegisterEvent(t, audit, hostSARegNew)
}

// TestFlatReregistration_HostSA_FillLacksActAs_Denied: a flat
// re-registration that would fill an unset host pair needs actAs on the
// requested value; a refusal leaves the row unchanged.
func TestFlatReregistration_HostSA_FillLacksActAs_Denied(t *testing.T) {
	f := newFlatRegFixture(t, true)
	id := tid("hsa-flat-rereg")
	target := f.targetFor(id)
	// Created without a host pair.
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{
		BrokerID: id, Name: "hsa-flat-rereg", RuntimeTarget: target,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	checker := store.NewFakeCallerPermissionChecker().DenyTarget(hostSARegNew, "no actAs grant")
	enforceSAAssign(f.srv, checker)
	audit := &mockAuditLogger{}
	f.srv.SetAuditLogger(audit)
	renamed := *target
	renamed.DisplayName = "Renamed Docker"

	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{
		BrokerID: id, Name: "hsa-flat-rereg", RuntimeTarget: &renamed,
		GCPHostServiceAccountEmail: hostSARegNew, GCPHostProjectID: "host-project",
		Labels: map[string]string{"env": "changed"},
	})

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, checker.CallCount())
	row := f.flatRow(t, id)
	assert.Empty(t, row.GCPHostServiceAccountEmail, "the pair stays unset")
	assert.NotEqual(t, "changed", row.Labels["env"])
	assert.Equal(t, target.DisplayName, row.RuntimeTarget.DisplayName,
		"a refusal leaves the row unchanged, including the target display name")
	requireDeniedRegisterEvent(t, audit, hostSARegNew)
}

func TestCreateBrokerRegistration_StaleHostSAVerdict_Refused(t *testing.T) {
	svc, _ := setupTestBrokerAuthService(t)
	ctx := context.Background()
	req := CreateBrokerRegistrationRequest{Name: "hsa-stale", GCPHostServiceAccountEmail: hostSARegNew}

	// A decision for a different value does not authorize this one.
	stale := hostSAClaimVerdict{Claim: hostSAClaim{Email: hostSARegOld}, Allowed: true}
	_, err := svc.createBrokerRegistration(ctx, req, "owner", "", true, true, stale)
	assert.True(t, errors.Is(err, ErrHostSAClaimNotAuthorized), "got %v", err)

	// The plain entry point carries no decision.
	_, err = svc.CreateBrokerRegistration(ctx, req, "owner")
	assert.True(t, errors.Is(err, ErrHostSAClaimNotAuthorized), "got %v", err)

	existing, err := svc.FindExistingBroker(ctx, req.Name, "", nil)
	require.NoError(t, err)
	assert.Nil(t, existing, "no row is created")
}

func TestRegisterFlatRuntimeBroker_ZeroVerdictWithoutClaimChange_Allowed(t *testing.T) {
	f := newFlatRegFixture(t, true)
	id := tid("hsa-flat-zero")
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{
		BrokerID: id, Name: "hsa-flat-zero", RuntimeTarget: f.targetFor(id),
		GCPHostServiceAccountEmail: hostSARegOld,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	existing := f.flatRow(t, id)

	// A write that does not touch the host service account (as the embedded
	// registration does) passes with the zero decision.
	row, created, err := f.srv.registerFlatRuntimeBroker(context.Background(), flatRegistration{
		BrokerID: id, Name: "hsa-flat-zero", Target: *existing.RuntimeTarget, Existing: existing,
		Apply: func(b *store.RuntimeBroker, _ bool) { b.Endpoint = "http://in-process" },
	})
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, "http://in-process", row.Endpoint)
	assert.Equal(t, hostSARegOld, row.GCPHostServiceAccountEmail)

	// The same zero decision does not let a write change it.
	_, _, err = f.srv.registerFlatRuntimeBroker(context.Background(), flatRegistration{
		BrokerID: id, Name: "hsa-flat-zero", Target: *existing.RuntimeTarget, Existing: row,
		Apply: func(b *store.RuntimeBroker, _ bool) { b.GCPHostServiceAccountEmail = hostSARegNew },
	})
	assert.True(t, errors.Is(err, ErrHostSAClaimNotAuthorized), "got %v", err)
	assert.Equal(t, hostSARegOld, f.flatRow(t, id).GCPHostServiceAccountEmail)
}

// TestCreateBrokerRegistration_UncheckedVerdictAfterRowChanged_Refused: a
// decision made without an actAs check, because the request matched the
// stored value X, does not let the write site set X back after the row
// changed to Y.
func TestCreateBrokerRegistration_UncheckedVerdictAfterRowChanged_Refused(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	ctx := context.Background()
	existing := &store.RuntimeBroker{
		ID: tid("hsa-unchecked"), Name: "hsa-unchecked", Slug: "hsa-unchecked",
		Status: store.BrokerStatusOffline, CreatedBy: "owner",
		GCPHostServiceAccountEmail: hostSARegNew, GCPHostProjectID: "host-project", // Y
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, existing))

	x := hostSAClaim{Email: hostSARegOld, ProjectID: "host-project"}
	unchecked := hostSAClaimVerdict{Claim: x, Prior: x, Allowed: true}
	req := CreateBrokerRegistrationRequest{
		Name: existing.Name, GCPHostServiceAccountEmail: x.Email, GCPHostProjectID: x.ProjectID,
	}

	_, err := svc.createBrokerRegistration(ctx, req, "owner", existing.ID, false, false, unchecked)

	assert.True(t, errors.Is(err, ErrHostSAClaimNotAuthorized), "got %v", err)
	got, err := s.GetRuntimeBroker(ctx, existing.ID)
	require.NoError(t, err)
	assert.Equal(t, hostSARegNew, got.GCPHostServiceAccountEmail, "the row still holds Y")
}

// clearOnUpdateStore clears a broker's GCP host pair and hands it back to
// registration (source "") right after the next record update, once armed:
// the row the handler read had a pair, and the row the fill sees has none.
type clearOnUpdateStore struct {
	store.Store
	fault *storeFaultSwitch
	once  sync.Once
}

func (s *clearOnUpdateStore) UpdateRuntimeBroker(ctx context.Context, b *store.RuntimeBroker) error {
	if err := s.Store.UpdateRuntimeBroker(ctx, b); err != nil {
		return err
	}
	if !s.fault.Active() {
		return nil
	}
	var err error
	s.once.Do(func() {
		if _, err = s.Store.SetRuntimeBrokerGCPHost(ctx, b.ID, "", ""); err != nil {
			return
		}
		_, err = s.Store.SetRuntimeBrokerGCPHostSource(ctx, b.ID, "")
	})
	return err
}

// TestReregistration_FillOfUnsetRow_ChecksActAs: on an existing row whose
// host pair is unset and not set through the update API, a re-registration
// that requests a pair writes it, so it needs actAs. A denial is a 403 that
// writes nothing; an allowed request fills the pair.
func TestReregistration_FillOfUnsetRow_ChecksActAs(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		t.Run("denied", func(t *testing.T) {
			var spy *gcpHostWriteSpyStore
			var fault *storeFaultSwitch
			f := newOwnedBrokerFixtureWithStore(t, flat, "fill-denied", func(srv *Server) {
				spy, fault = installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *gcpHostWriteSpyStore {
					return &gcpHostWriteSpyStore{Store: inner, fault: sw}
				})
				srv.brokerAuthService.store = spy
			})
			checker := store.NewFakeCallerPermissionChecker().DenyTarget(ownedEmailA, "no grant")
			enforceSAAssign(f.srv, checker)
			fault.Arm()

			rec := doRequestAsUserLoopback(t, f.srv, f.owner, http.MethodPost, "/api/v1/brokers", f.req(func(r *CreateBrokerRegistrationRequest) {
				withGCPHost(ownedEmailA, ownedProjectA)(r)
				r.Labels = map[string]string{"zone": "x"}
			}))

			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Equal(t, 1, checker.CallCount())
			assert.Empty(t, spy.calls, "no GCP host writer is reached")
			f.requireGCPHost(t, "", "")
			assert.NotContains(t, f.row(t).Labels, "zone", "the refusal comes before any write")
		})
		t.Run("allowed", func(t *testing.T) {
			f := newOwnedBrokerFixture(t, flat, "fill-allowed")
			checker := store.NewFakeCallerPermissionChecker().AllowTarget(ownedEmailA)
			enforceSAAssign(f.srv, checker)
			audit := installBrokerAuditCapture(f.srv)

			resp := f.reregister(t, withGCPHost(ownedEmailA, ownedProjectA))

			assert.Empty(t, resp.KeptSettings)
			assert.Equal(t, 1, checker.CallCount())
			f.requireGCPHost(t, ownedEmailA, ownedProjectA)
			assert.Equal(t, store.GCPHostSourceRegistration, f.row(t).GCPHostSource)
			events := brokerAuditEventsOfType(audit, BrokerAuthEventRegister)
			require.Len(t, events, 1)
			assert.Equal(t, "true", events[0].Details["gcp_host_set"])
		})
	})
}

// TestReregistration_SetRow_KeptWithoutActAs: on an existing row with a host
// pair, a re-registration requesting another pair writes nothing: the
// request is kept and reported, and no actAs check runs.
func TestReregistration_SetRow_KeptWithoutActAs(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		f := newOwnedBrokerFixture(t, flat, "set-kept")
		applied, err := f.s.SetRuntimeBrokerGCPHostIfUnset(context.Background(), f.id, ownedEmailA, ownedProjectA, store.GCPHostSourceRegistration)
		require.NoError(t, err)
		require.True(t, applied)
		checker := store.NewFakeCallerPermissionChecker().DenyTarget(ownedEmailB, "no grant")
		enforceSAAssign(f.srv, checker)
		audit := installBrokerAuditCapture(f.srv)

		resp := f.reregister(t, withGCPHost(ownedEmailB, ownedProjectB))

		assert.Zero(t, checker.CallCount())
		kept := keptByField(resp.KeptSettings)
		assert.Equal(t, KeptReasonSet, kept[keptFieldGCPHostEmail].Reason)
		f.requireGCPHost(t, ownedEmailA, ownedProjectA)
		events := brokerAuditEventsOfType(audit, BrokerAuthEventRegister)
		require.Len(t, events, 1)
		assert.NotContains(t, events[0].Details, "gcp_host_set")
		assert.Equal(t, "true", events[0].Details["gcp_host_kept"])
	})
}

// TestReregistration_AdminSourcedRow_KeptWithoutActAs: on an existing row
// whose pair was cleared through the update API, a re-registration
// requesting a pair writes nothing: the request is kept as pinned, and no
// actAs check runs.
func TestReregistration_AdminSourcedRow_KeptWithoutActAs(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		f := newOwnedBrokerFixture(t, flat, "admin-kept")
		_, err := f.s.SetRuntimeBrokerGCPHost(context.Background(), f.id, "", "")
		require.NoError(t, err)
		checker := store.NewFakeCallerPermissionChecker().DenyTarget(ownedEmailB, "no grant")
		enforceSAAssign(f.srv, checker)
		audit := installBrokerAuditCapture(f.srv)

		resp := f.reregister(t, withGCPHost(ownedEmailB, ownedProjectB))

		assert.Zero(t, checker.CallCount())
		kept := keptByField(resp.KeptSettings)
		assert.Equal(t, KeptReasonPinned, kept[keptFieldGCPHostEmail].Reason)
		f.requireGCPHost(t, "", "")
		events := brokerAuditEventsOfType(audit, BrokerAuthEventRegister)
		require.Len(t, events, 1)
		assert.NotContains(t, events[0].Details, "gcp_host_set")
		assert.Equal(t, "true", events[0].Details["gcp_host_kept"])
	})
}

// TestReregistration_RowBecameFillableAfterDecision_Conflict: the handler
// read a row with a host pair, so it made no actAs decision. When the pair is
// cleared and handed back to registration before the fill, the fill has no
// decision for the requested pair: the request gets 409 and the pair is not
// written.
func TestReregistration_RowBecameFillableAfterDecision_Conflict(t *testing.T) {
	t.Parallel()
	forFlatAndLegacy(t, func(t *testing.T, flat bool) {
		var fault *storeFaultSwitch
		f := newOwnedBrokerFixtureWithStore(t, flat, "fill-stale", func(srv *Server) {
			var w *clearOnUpdateStore
			w, fault = installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *clearOnUpdateStore {
				return &clearOnUpdateStore{Store: inner, fault: sw}
			})
			srv.brokerAuthService.store = w
		})
		applied, err := f.s.SetRuntimeBrokerGCPHostIfUnset(context.Background(), f.id, ownedEmailA, ownedProjectA, store.GCPHostSourceRegistration)
		require.NoError(t, err)
		require.True(t, applied)
		checker := store.NewFakeCallerPermissionChecker().AllowTarget(ownedEmailB)
		enforceSAAssign(f.srv, checker)
		fault.Arm()

		rec := doRequestAsUserLoopback(t, f.srv, f.owner, http.MethodPost, "/api/v1/brokers", f.req(withGCPHost(ownedEmailB, ownedProjectB)))

		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Zero(t, checker.CallCount(), "the row read for the decision would not be filled")
		f.requireGCPHost(t, "", "")
	})
}
