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
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const hsrEmail = "shared-host@hsr-project.iam.gserviceaccount.com"

// probeCountingGenerator is a token generator whose impersonation probe is
// counted and can be made to fail.
type probeCountingGenerator struct {
	mockGCPTokenGenerator
	mu        sync.Mutex
	probes    []string
	verifyErr error
}

func (g *probeCountingGenerator) VerifyImpersonation(_ context.Context, email string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.probes = append(g.probes, email)
	return g.verifyErr
}

func (g *probeCountingGenerator) probeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.probes)
}

func hsrServer(t *testing.T, verifyErr error) (*Server, store.Store, *probeCountingGenerator) {
	t.Helper()
	srv, s := testServer(t)
	gen := &probeCountingGenerator{mockGCPTokenGenerator: mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"}, verifyErr: verifyErr}
	srv.SetGCPTokenGenerator(gen)
	return srv, s, gen
}

func hsrBroker(id string) *store.RuntimeBroker {
	return &store.RuntimeBroker{ID: tid(id), Name: id, GCPHostServiceAccountEmail: hsrEmail, GCPHostProjectID: "hsr-project"}
}

func hsrSeed(t *testing.T, s store.Store, id, scope, scopeID, createdBy string, managed bool) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID: tid(id), Scope: scope, ScopeID: scopeID, Email: hsrEmail, ProjectID: "hsr-project",
		Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: createdBy, CreatedAt: time.Now(), Managed: managed,
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

func hsrHubRecords(t *testing.T, s store.Store) []store.GCPServiceAccount {
	t.Helper()
	rows, err := s.ListGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{Email: hsrEmail, Scope: store.ScopeHub})
	require.NoError(t, err)
	return rows
}

func TestEnsureHostSARecord_ReusesOwnRecordForSameBroker(t *testing.T) {
	srv, _, gen := hsrServer(t, nil)
	b := hsrBroker("hsr-same")

	first, err := srv.ensureHostSARecord(context.Background(), b)
	require.NoError(t, err)
	second, err := srv.ensureHostSARecord(context.Background(), b)
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, b.ID, first.ScopeID)
	assert.Equal(t, hostSATranslationCreator, first.CreatedBy)
	assert.Equal(t, 1, gen.probeCount(), "only the create probes")
}

func TestEnsureHostSARecord_DoesNotReuseOtherBrokersRecord(t *testing.T) {
	srv, s, _ := hsrServer(t, nil)
	a, b := hsrBroker("hsr-broker-a"), hsrBroker("hsr-broker-b")

	ra, err := srv.ensureHostSARecord(context.Background(), a)
	require.NoError(t, err)
	rb, err := srv.ensureHostSARecord(context.Background(), b)
	require.NoError(t, err)

	assert.NotEqual(t, ra.ID, rb.ID, "a broker sharing the host account gets its own record")
	assert.Equal(t, b.ID, rb.ScopeID)
	assert.Len(t, hsrHubRecords(t, s), 2)
}

func TestEnsureHostSARecord_DoesNotReuseAdminRegisteredHubRecord(t *testing.T) {
	srv, s, gen := hsrServer(t, nil)
	b := hsrBroker("hsr-admin-rec")
	admin := hsrSeed(t, s, "hsr-admin-sa", store.ScopeHub, "hub-instance-1", tid("some-admin"), false)

	got, err := srv.ensureHostSARecord(context.Background(), b)
	require.NoError(t, err)

	assert.NotEqual(t, admin.ID, got.ID, "a record with other provenance is never reused")
	assert.Equal(t, hostSATranslationCreator, got.CreatedBy)
	assert.Equal(t, 1, gen.probeCount())
}

func TestEnsureHostSARecord_ProjectScopedEmail_RefusedNoTwin(t *testing.T) {
	srv, s, gen := hsrServer(t, nil)
	hsrSeed(t, s, "hsr-proj-sa", store.ScopeProject, tid("hsr-project-1"), tid("someone"), false)

	_, err := srv.ensureHostSARecord(context.Background(), hsrBroker("hsr-proj"))

	assert.True(t, errors.Is(err, errHostSARecordConflict), "got %v", err)
	assert.Empty(t, hsrHubRecords(t, s), "no hub-scoped twin is created")
	assert.Equal(t, 0, gen.probeCount())
}

// TestEnsureHostSARecord_ProjectScopedEmailOtherCase_Refused: a
// project-scoped record whose email differs only in letter case is the same
// account, so no hub-scoped twin is created for the lowercased broker email.
func TestEnsureHostSARecord_ProjectScopedEmailOtherCase_Refused(t *testing.T) {
	srv, s, gen := hsrServer(t, nil)
	mixed := &store.GCPServiceAccount{
		ID: tid("hsr-mixed-case"), Scope: store.ScopeProject, ScopeID: tid("hsr-project-3"),
		Email: "Shared-Host@HSR-Project.iam.gserviceaccount.com", ProjectID: "hsr-project",
		CreatedBy: tid("someone"), CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), mixed))

	_, err := srv.ensureHostSARecord(context.Background(), hsrBroker("hsr-mixed"))

	assert.True(t, errors.Is(err, errHostSARecordConflict), "got %v", err)
	rows, err := s.ListGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{
		EmailFold: hsrEmail, Scope: store.ScopeHub,
	})
	require.NoError(t, err)
	assert.Empty(t, rows, "no hub-scoped twin is created")
	assert.Equal(t, 0, gen.probeCount(), "no probe runs")
}

func TestEnsureHostSARecord_ManagedEmail_Refused(t *testing.T) {
	srv, s, _ := hsrServer(t, nil)
	managed := hsrSeed(t, s, "hsr-managed-sa", store.ScopeHub, "hub", "system:provisioner", true)

	_, err := srv.ensureHostSARecord(context.Background(), hsrBroker("hsr-managed"))

	assert.True(t, errors.Is(err, errHostSARecordConflict), "got %v", err)
	rows := hsrHubRecords(t, s)
	require.Len(t, rows, 1)
	assert.Equal(t, managed.ID, rows[0].ID)
}

// TestEnsureHostSARecord_LegacyTwinNotReused: a hub-scoped translation record
// that coexists with a project-scoped record of the same email (a twin made
// before records were broker-bound) is refused, not reused, even for its own
// broker.
func TestEnsureHostSARecord_LegacyTwinNotReused(t *testing.T) {
	srv, s, _ := hsrServer(t, nil)
	b := hsrBroker("hsr-twin")
	hsrSeed(t, s, "hsr-twin-proj", store.ScopeProject, tid("hsr-project-2"), tid("someone"), false)
	hsrSeed(t, s, "hsr-twin-hub", store.ScopeHub, b.ID, hostSATranslationCreator, false)

	_, err := srv.ensureHostSARecord(context.Background(), b)

	assert.True(t, errors.Is(err, errHostSARecordConflict), "got %v", err)
}

func TestEnsureHostSARecord_ProbesImpersonationOnCreate(t *testing.T) {
	srv, _, gen := hsrServer(t, nil)

	got, err := srv.ensureHostSARecord(context.Background(), hsrBroker("hsr-probe"))

	require.NoError(t, err)
	assert.Equal(t, []string{hsrEmail}, gen.probes)
	assert.True(t, got.Verified)
	assert.Equal(t, store.GCPVerificationVerified, got.VerificationStatus)
}

func TestEnsureHostSARecord_ProbeFailure_Refused(t *testing.T) {
	srv, s, gen := hsrServer(t, errors.New("permission denied"))

	_, err := srv.ensureHostSARecord(context.Background(), hsrBroker("hsr-probe-fail"))

	assert.True(t, errors.Is(err, errHostSARecordUnverifiable), "got %v", err)
	assert.Equal(t, 1, gen.probeCount())
	assert.Empty(t, hsrHubRecords(t, s), "no record is created without a successful probe")
}

func TestEnsureHostSARecord_NoGenerator_Refused(t *testing.T) {
	srv, s := testServer(t)

	_, err := srv.ensureHostSARecord(context.Background(), hsrBroker("hsr-no-gen"))

	assert.True(t, errors.Is(err, errHostSARecordUnverifiable), "got %v", err)
	assert.Empty(t, hsrHubRecords(t, s))
}

// TestPassthroughTranslation_ProjectScopedHostSA_Create409: the HTTP mapping
// of a record conflict on the create path.
func TestPassthroughTranslation_ProjectScopedHostSA_Create409(t *testing.T) {
	owner := ptUser(tid("hsr-409-owner"), "hsr-409-owner@test.com", store.UserRoleMember)
	srv, s, project, _ := setupPassthroughSandboxServer(t, owner, hsrEmail, "hsr-project")
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget(hsrEmail))
	hsrSeed(t, s, "hsr-409-proj", store.ScopeProject, tid("hsr-other-project"), tid("someone"), false)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "hsr-409-agent", ProjectID: project.ID, Task: "test",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: "passthrough"},
	})

	assert.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	assert.Empty(t, hsrHubRecords(t, s))
}

// TestTranslatedHostSARecord_ExplicitAssign_Refused: a translation record
// cannot be assigned explicitly, even by a hub member who holds actAs on its
// account (which the hub-member assign rule would otherwise admit).
func TestTranslatedHostSARecord_ExplicitAssign_Refused(t *testing.T) {
	owner := ptUser(tid("hsr-assign-owner"), "hsr-assign-owner@test.com", store.UserRoleMember)
	srv, s, project, _ := setupPassthroughSandboxServer(t, owner, "", "")
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget(hsrEmail))
	translated := hsrSeed(t, s, "hsr-translated", store.ScopeHub, tid("some-other-broker"), hostSATranslationCreator, false)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "hsr-assign-agent", ProjectID: project.ID, Task: "test",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: "assign", ServiceAccountID: translated.ID},
	})

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "cannot be assigned directly")
	_, err := s.GetAgentBySlug(context.Background(), project.ID, "hsr-assign-agent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// hsrCreateRaceStore simulates a concurrent create: while armed, its first
// CreateGCPServiceAccount stores racer instead and reports ErrAlreadyExists.
type hsrCreateRaceStore struct {
	store.Store
	fault *storeFaultSwitch
	mu    sync.Mutex
	racer *store.GCPServiceAccount
}

func (r *hsrCreateRaceStore) CreateGCPServiceAccount(ctx context.Context, sa *store.GCPServiceAccount) error {
	r.mu.Lock()
	racer := r.racer
	if r.fault.Active() && racer != nil {
		r.racer = nil
	} else {
		racer = nil
	}
	r.mu.Unlock()
	if racer == nil {
		return r.Store.CreateGCPServiceAccount(ctx, sa)
	}
	if err := r.Store.CreateGCPServiceAccount(ctx, racer); err != nil {
		return err
	}
	return fmt.Errorf("create gcp service account: %w", store.ErrAlreadyExists)
}

func hsrRaceServer(t *testing.T) (*Server, store.Store, *hsrCreateRaceStore) {
	t.Helper()
	srv, s, race, fault := testServerWithStoreFault(t, func(inner store.Store, fault *storeFaultSwitch) *hsrCreateRaceStore {
		return &hsrCreateRaceStore{Store: inner, fault: fault}
	})
	srv.SetGCPTokenGenerator(&probeCountingGenerator{mockGCPTokenGenerator: mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"}})
	fault.Arm()
	return srv, s, race
}

func TestEnsureHostSARecord_CreateRace_ReusesOwnRecord(t *testing.T) {
	srv, _, race := hsrRaceServer(t)
	b := hsrBroker("hsr-race-own")
	race.racer = &store.GCPServiceAccount{
		ID: tid("hsr-race-own-sa"), Scope: store.ScopeHub, ScopeID: b.ID, Email: hsrEmail, ProjectID: "hsr-project",
		Verified: true, VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: hostSATranslationCreator, CreatedAt: time.Now(),
	}

	got, err := srv.ensureHostSARecord(context.Background(), b)

	require.NoError(t, err)
	assert.Equal(t, tid("hsr-race-own-sa"), got.ID, "the concurrently created record for this broker is reused")
}

func TestEnsureHostSARecord_CreateRace_ForeignRecordRefused(t *testing.T) {
	srv, _, race := hsrRaceServer(t)
	b := hsrBroker("hsr-race-foreign")
	race.racer = &store.GCPServiceAccount{
		ID: tid("hsr-race-foreign-sa"), Scope: store.ScopeHub, ScopeID: b.ID, Email: hsrEmail, ProjectID: "hsr-project",
		Verified: true, VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: tid("some-admin"), CreatedAt: time.Now(),
	}

	got, err := srv.ensureHostSARecord(context.Background(), b)

	require.Error(t, err, "a record with other provenance is never returned")
	assert.Nil(t, got)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}
