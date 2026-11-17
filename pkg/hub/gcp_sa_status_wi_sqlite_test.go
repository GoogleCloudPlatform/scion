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
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func createStatusTestSA(t *testing.T, s store.Store, scope, scopeID, email string, managed bool) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:                 tid("wi-status-sa-" + scope + "-" + email + "-" + t.Name()),
		Scope:              scope,
		ScopeID:            scopeID,
		Email:              email,
		ProjectID:          "test-hub-project",
		Verified:           true,
		VerificationStatus: store.GCPVerificationVerified,
		VerifiedAt:         time.Now().UTC().Truncate(time.Second),
		CreatedAt:          time.Now().UTC().Truncate(time.Second),
		Managed:            managed,
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

// AC 1: With hub.k8s_workload_identity_setup off, the status view behaves
// exactly as on main for both minted and bring-your-own GSAs, with no IAM
// policy read.
func TestGCPSAStatus_WIBinding_ExperimentOff(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	setWorkloadIdentitySetupExperiment(t, srv, false)
	projectID := createTestProjectForSA(t, srv, nil)
	b := addProviderBroker(t, s, projectID, "a", authoritativeProfile("gke",
		"scion-minted@test-hub-project.iam.gserviceaccount.com",
		"byo@other-project.iam.gserviceaccount.com",
	))
	setWorkloadIdentityTargets(t, s, b.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
	})

	minted := createStatusTestSA(t, s, store.ScopeProject, projectID, "scion-minted@test-hub-project.iam.gserviceaccount.com", true)
	byo := createStatusTestSA(t, s, store.ScopeProject, projectID, "byo@other-project.iam.gserviceaccount.com", false)

	for _, sa := range []*store.GCPServiceAccount{minted, byo} {
		rec := doRequest(t, srv, http.MethodGet, statusPath(projectID, sa.ID), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var st GCPServiceAccountStatus
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
		assert.Equal(t, GCPServiceAccountBinding{State: GCPSABindingUnknown, Reason: "not checked"}, st.WorkloadIdentityBinding)
		assert.NotContains(t, rec.Body.String(), `"targets"`)
		require.Len(t, st.Mappings, 1)
		assert.Nil(t, st.Mappings[0].WorkloadIdentityBinding)
		assert.Equal(t, "Nothing missing that the hub can check. The Workload Identity binding is not checked.", st.NextStep.Message)
	}
	assert.Empty(t, mock.roleMembersCalls, "no IAM read while experiment is off")
}

// AC 9 & §3.7: Bring-your-own GSAs stay unknown with reason "not minted by
// this hub", and never trigger getIamPolicy.
func TestGCPSAStatus_WIBinding_BYOSAStaysUnknown(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	setWorkloadIdentitySetupExperiment(t, srv, true)
	projectID := createTestProjectForSA(t, srv, nil)
	b := addProviderBroker(t, s, projectID, "a", authoritativeProfile("gke", "byo@other-project.iam.gserviceaccount.com"))
	setWorkloadIdentityTargets(t, s, b.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
	})
	byo := createStatusTestSA(t, s, store.ScopeProject, projectID, "byo@other-project.iam.gserviceaccount.com", false)

	st := getSAStatus(t, srv, projectID, byo.ID)
	assert.Equal(t, GCPServiceAccountBinding{
		State:  GCPSABindingUnknown,
		Reason: "not minted by this hub",
	}, st.WorkloadIdentityBinding)
	require.Len(t, st.Mappings, 1)
	assert.Equal(t, &GCPServiceAccountProfileBinding{
		State:  GCPSABindingUnknown,
		Reason: "not minted by this hub",
	}, st.Mappings[0].WorkloadIdentityBinding)
	assert.Empty(t, mock.roleMembersCalls)
}

// AC 9 & §3.7: For minted GSAs, buildGCPServiceAccountStatus reads the GSA's
// IAM policy, reports bound or not_bound per target (reusing the P1 target
// computation across legacy and flat rows), and caches the result for 60s.
func TestGCPSAStatus_WIBinding_MintedPerTargetAndCache(t *testing.T) {
	now := withWIBindingClock(t, time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	srv, s, mock := testServerWithMinting(t)
	setWorkloadIdentitySetupExperiment(t, srv, true)
	projectID := createTestProjectForSA(t, srv, nil)
	ctx := context.Background()

	const email = "scion-worker@test-hub-project.iam.gserviceaccount.com"
	a := addProviderBroker(t, s, projectID, "a",
		authoritativeProfile("gke-bound", email),
		authoritativeProfile("gke-unbound", email),
		authoritativeProfile("gke-nopool", email),
	)
	require.NoError(t, s.UpdateRuntimeBrokerHeartbeat(ctx, a.ID, store.BrokerStatusOnline))
	setWorkloadIdentityTargets(t, s, a.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke-bound", Pool: "pool-a.svc.id.goog", Namespace: "agents"},
		{Profile: "gke-unbound", Pool: "pool-b.svc.id.goog", Namespace: "team"},
	})

	// A flat provider sharing (pool-a.svc.id.goog, agents) merges into the same target.
	flat := &store.RuntimeBroker{
		ID: tid("wi-status-flat-" + t.Name()), Name: "flat-a", Slug: "flat-a-" + tidSlugSafe(t.Name()),
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
		LastHeartbeat:           *now,
		Capabilities:            &store.BrokerCapabilities{WITargetsV1: true},
		RuntimeTarget:           &api.RuntimeTargetDescriptor{ID: "rt-flat-a", Type: "kubernetes", DisplayName: "gke"},
		WorkloadIdentityTargets: []store.WorkloadIdentityTarget{{Pool: "pool-a.svc.id.goog", Namespace: "agents"}},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, flat))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: flat.ID, BrokerName: flat.Name, Status: flat.Status,
	}))

	sa := createStatusTestSA(t, s, store.ScopeProject, projectID, email, true)
	mock.setRoleMembers(email, roleWorkloadIdentityUser, []string{
		"serviceAccount:pool-a.svc.id.goog[agents/scion-worker]",
	})

	st := getSAStatus(t, srv, projectID, sa.ID)
	require.Len(t, mock.roleMembersCalls, 1)
	assert.Equal(t, mockRoleMembersCall{SAEmail: email, Role: roleWorkloadIdentityUser}, mock.roleMembersCalls[0])

	assert.Equal(t, GCPSABindingNotBound, st.WorkloadIdentityBinding.State)
	assert.Contains(t, st.WorkloadIdentityBinding.Reason, "1 of 2 targets bound")
	require.Len(t, st.WorkloadIdentityBinding.Targets, 3)

	// R1: NextStep has code "not_bound" and points at the unbound target's manual gcloud step.
	assert.Equal(t, "not_bound", st.NextStep.Code)
	assert.Contains(t, st.NextStep.Message, "a/gke-unbound")
	assert.Contains(t, st.NextStep.Message, workloadIdentityManualStep(email, "serviceAccount:pool-b.svc.id.goog[team/scion-worker]"))

	assert.Equal(t, GCPSABindingBound, st.WorkloadIdentityBinding.Targets[0].State)
	assert.Equal(t, "pool-a.svc.id.goog", st.WorkloadIdentityBinding.Targets[0].Pool)
	assert.Equal(t, "agents", st.WorkloadIdentityBinding.Targets[0].Namespace)
	assert.Equal(t, "scion-worker", st.WorkloadIdentityBinding.Targets[0].KSA)
	assert.Equal(t, "serviceAccount:pool-a.svc.id.goog[agents/scion-worker]", st.WorkloadIdentityBinding.Targets[0].Member)
	assert.Equal(t, []string{"a/gke-bound", "flat-a"}, st.WorkloadIdentityBinding.Targets[0].Profiles)
	assert.Empty(t, st.WorkloadIdentityBinding.Targets[0].ManualStep)

	assert.Equal(t, GCPSABindingNotBound, st.WorkloadIdentityBinding.Targets[1].State)
	assert.Equal(t, "serviceAccount:pool-b.svc.id.goog[team/scion-worker]", st.WorkloadIdentityBinding.Targets[1].Member)
	assert.Equal(t, []string{"a/gke-unbound"}, st.WorkloadIdentityBinding.Targets[1].Profiles)
	assert.Equal(t, workloadIdentityManualStep(email, "serviceAccount:pool-b.svc.id.goog[team/scion-worker]"),
		st.WorkloadIdentityBinding.Targets[1].ManualStep)

	assert.Equal(t, GCPSABindingUnknown, st.WorkloadIdentityBinding.Targets[2].State)
	assert.Equal(t, api.WorkloadIdentityReasonNoPool, st.WorkloadIdentityBinding.Targets[2].Reason)
	assert.Equal(t, []string{"a/gke-nopool"}, st.WorkloadIdentityBinding.Targets[2].Profiles)

	byProfile := map[string]*GCPServiceAccountProfileBinding{}
	for i := range st.Mappings {
		byProfile[st.Mappings[i].Profile] = st.Mappings[i].WorkloadIdentityBinding
	}
	require.NotNil(t, byProfile["gke-bound"])
	assert.Equal(t, GCPSABindingBound, byProfile["gke-bound"].State)
	require.NotNil(t, byProfile["gke-unbound"])
	assert.Equal(t, GCPSABindingNotBound, byProfile["gke-unbound"].State)
	require.NotNil(t, byProfile["gke-nopool"])
	assert.Equal(t, GCPSABindingUnknown, byProfile["gke-nopool"].State)
	assert.Equal(t, api.WorkloadIdentityReasonNoPool, byProfile["gke-nopool"].Reason)

	// Grant the second member on IAM: within 60s the cached answer stays as-is.
	mock.setRoleMembers(email, roleWorkloadIdentityUser, []string{
		"serviceAccount:pool-a.svc.id.goog[agents/scion-worker]",
		"serviceAccount:pool-b.svc.id.goog[team/scion-worker]",
	})
	*now = now.Add(30 * time.Second)
	stCached := getSAStatus(t, srv, projectID, sa.ID)
	assert.Equal(t, GCPSABindingNotBound, stCached.WorkloadIdentityBinding.State)
	assert.Len(t, mock.roleMembersCalls, 1, "60s cache avoids a second getIamPolicy")

	// Past 60s, the policy is re-read and both pool targets report bound;
	// because a/gke-nopool still reports no pool (O2), overall state is unknown
	// rather than bound and NextStep does not claim "Nothing missing".
	*now = now.Add(31 * time.Second)
	stFresh := getSAStatus(t, srv, projectID, sa.ID)
	assert.Len(t, mock.roleMembersCalls, 2)
	assert.Equal(t, GCPSABindingUnknown, stFresh.WorkloadIdentityBinding.State)
	assert.Contains(t, stFresh.WorkloadIdentityBinding.Reason, "2 reported targets bound")
	assert.Contains(t, stFresh.WorkloadIdentityBinding.Reason, "1 Kubernetes target reports no pool")
	assert.Equal(t, GCPSABindingBound, stFresh.WorkloadIdentityBinding.Targets[0].State)
	assert.Equal(t, GCPSABindingBound, stFresh.WorkloadIdentityBinding.Targets[1].State)
	assert.Equal(t, GCPSANextStepNone, stFresh.NextStep.Code)
	assert.NotContains(t, stFresh.NextStep.Message, "Nothing missing")
}

// End-to-end: minting a GSA with the experiment on records the grant, and the
// per-account status view reports bound. Also covers a hub-scoped minted GSA
// viewed from a project.
func TestGCPSAStatus_WIBinding_EndToEndFromMintAndHubScope(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	setWorkloadIdentitySetupExperiment(t, srv, true)
	projectID := createTestProjectForSA(t, srv, nil)
	a := addProviderBroker(t, s, projectID, "a", authoritativeProfile("gke", "scion-wi-test@test-hub-project.iam.gserviceaccount.com"))
	require.NoError(t, s.UpdateRuntimeBrokerHeartbeat(context.Background(), a.ID, store.BrokerStatusOnline))
	setWorkloadIdentityTargets(t, s, a.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
	})

	minted, _ := mintForWI(t, srv, projectID)
	st := getSAStatus(t, srv, projectID, minted.ID)
	assert.Equal(t, GCPSABindingBound, st.WorkloadIdentityBinding.State)
	assert.Empty(t, st.WorkloadIdentityBinding.Reason)
	require.Len(t, st.WorkloadIdentityBinding.Targets, 1)
	assert.Equal(t, GCPSABindingBound, st.WorkloadIdentityBinding.Targets[0].State)
	require.Len(t, st.Mappings, 1)
	require.NotNil(t, st.Mappings[0].WorkloadIdentityBinding)
	assert.Equal(t, GCPSABindingBound, st.Mappings[0].WorkloadIdentityBinding.State)
	assert.Equal(t, GCPSANextStepNone, st.NextStep.Code)
	assert.NotContains(t, st.NextStep.Message, "not checked")

	// Hub-scoped minted account viewed in this project checks the same project targets.
	const hubEmail = "scion-shared@test-hub-project.iam.gserviceaccount.com"
	hubSA := createStatusTestSA(t, s, store.ScopeHub, "hub", hubEmail, true)
	mock.setRoleMembers(hubEmail, roleWorkloadIdentityUser, []string{
		"serviceAccount:cluster-project.svc.id.goog[agents/scion-shared]",
	})
	hubSt := getSAStatus(t, srv, projectID, hubSA.ID)
	assert.Equal(t, GCPSABindingBound, hubSt.WorkloadIdentityBinding.State)
}

// When a profile maps a minted GSA to a non-conventional KSA name, that
// profile's row checks the mapped KSA's member rather than claiming bound from
// the conventional KSA's member.
func TestGCPSAStatus_WIBinding_ProfileUsesMappedKSA(t *testing.T) {
	now := withWIBindingClock(t, time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	srv, s, mock := testServerWithMinting(t)
	setWorkloadIdentitySetupExperiment(t, srv, true)
	projectID := createTestProjectForSA(t, srv, nil)

	const email = "scion-worker@test-hub-project.iam.gserviceaccount.com"
	reportedAt := now.Add(-time.Minute)
	prof := store.BrokerProfile{
		Name: "gke", Type: "kubernetes", Available: true,
		MappingsReported: true, MappingsComplete: true, MappingsReportedAt: &reportedAt,
		MappingsReportVersion: api.BrokerSAReportVersion,
		ServiceAccountMappings: []store.BrokerProfileSAMapping{
			{GSA: email, KSA: "custom-ksa", Namespace: "agents", Source: "mapped"},
		},
	}
	b := addProviderBroker(t, s, projectID, "a", prof)
	require.NoError(t, s.UpdateRuntimeBrokerHeartbeat(context.Background(), b.ID, store.BrokerStatusOnline))
	setWorkloadIdentityTargets(t, s, b.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
	})
	sa := createStatusTestSA(t, s, store.ScopeProject, projectID, email, true)

	// Direction 1: Conventional KSA is bound, mapped custom-ksa is NOT bound.
	// Both the profile row, the dispatch target, the summary state, and nextStep
	// must report not_bound for custom-ksa (never "bound / Nothing missing").
	mock.setRoleMembers(email, roleWorkloadIdentityUser, []string{
		"serviceAccount:cluster-project.svc.id.goog[agents/scion-worker]",
	})
	st := getSAStatus(t, srv, projectID, sa.ID)
	customMember := "serviceAccount:cluster-project.svc.id.goog[agents/custom-ksa]"
	customManual := workloadIdentityManualStep(email, customMember)
	require.Len(t, st.Mappings, 1)
	require.NotNil(t, st.Mappings[0].WorkloadIdentityBinding)
	assert.Equal(t, GCPSABindingNotBound, st.Mappings[0].WorkloadIdentityBinding.State)
	assert.Equal(t, customMember, st.Mappings[0].WorkloadIdentityBinding.Member)
	assert.Equal(t, customManual, st.Mappings[0].WorkloadIdentityBinding.ManualStep)
	assert.Equal(t, GCPSABindingNotBound, st.WorkloadIdentityBinding.State)
	require.Len(t, st.WorkloadIdentityBinding.Targets, 1)
	assert.Equal(t, GCPSABindingNotBound, st.WorkloadIdentityBinding.Targets[0].State)
	assert.Equal(t, "custom-ksa", st.WorkloadIdentityBinding.Targets[0].KSA)
	assert.Equal(t, customMember, st.WorkloadIdentityBinding.Targets[0].Member)
	assert.Equal(t, customManual, st.WorkloadIdentityBinding.Targets[0].ManualStep)
	assert.Equal(t, GCPSANextStepNotBound, st.NextStep.Code)
	assert.Contains(t, st.NextStep.Message, customManual)
	assert.NotContains(t, st.NextStep.Message, "Nothing missing")

	// Direction 2 (reverse): Mapped custom-ksa IS bound, while the unused
	// conventional KSA member is NOT bound. Because custom-ksa is the only
	// dispatch identity in this project, the summary is bound and nextStep
	// does not ask for an unneeded grant on scion-worker.
	mock.setRoleMembers(email, roleWorkloadIdentityUser, []string{
		customMember,
	})
	*now = now.Add(gcpSAWIBindingCacheTTL + time.Second)
	st = getSAStatus(t, srv, projectID, sa.ID)
	require.NotNil(t, st.Mappings[0].WorkloadIdentityBinding)
	assert.Equal(t, GCPSABindingBound, st.Mappings[0].WorkloadIdentityBinding.State)
	assert.Empty(t, st.Mappings[0].WorkloadIdentityBinding.ManualStep)
	assert.Equal(t, GCPSABindingBound, st.WorkloadIdentityBinding.State)
	assert.Empty(t, st.WorkloadIdentityBinding.Reason)
	require.Len(t, st.WorkloadIdentityBinding.Targets, 1)
	assert.Equal(t, GCPSABindingBound, st.WorkloadIdentityBinding.Targets[0].State)
	assert.Equal(t, customMember, st.WorkloadIdentityBinding.Targets[0].Member)
	assert.Empty(t, st.WorkloadIdentityBinding.Targets[0].ManualStep)
	assert.Equal(t, GCPSANextStepNone, st.NextStep.Code)
	assert.Equal(t, "Nothing missing that the hub can check.", st.NextStep.Message)
}

// §3.7: The list view never reads IAM policies, even with assignStatus=true
// and the experiment enabled.
func TestGCPSAStatus_WIBinding_ListRoutesDoNotReadPolicy(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	setWorkloadIdentitySetupExperiment(t, srv, true)
	projectID := createTestProjectForSA(t, srv, nil)
	b := addProviderBroker(t, s, projectID, "a", authoritativeProfile("gke", "scion-worker@test-hub-project.iam.gserviceaccount.com"))
	setWorkloadIdentityTargets(t, s, b.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
	})
	createStatusTestSA(t, s, store.ScopeProject, projectID, "scion-worker@test-hub-project.iam.gserviceaccount.com", true)

	for _, path := range []string{
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID),
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts?assignStatus=true&profile=gke&broker=a", projectID),
		fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s", projectID),
		"/api/v1/gcp-service-accounts?scope=hub",
	} {
		rec := doRequest(t, srv, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
	}
	assert.Empty(t, mock.roleMembersCalls, "list endpoints never call getIamPolicy")
}

func TestGCPSAStatus_WIBinding_IAMReadErrorIsUnknownAndNotCached(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	setWorkloadIdentitySetupExperiment(t, srv, true)
	projectID := createTestProjectForSA(t, srv, nil)
	const email = "scion-worker@test-hub-project.iam.gserviceaccount.com"
	b := addProviderBroker(t, s, projectID, "a", authoritativeProfile("gke", email))
	setWorkloadIdentityTargets(t, s, b.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
	})
	sa := createStatusTestSA(t, s, store.ScopeProject, projectID, email, true)

	mock.roleMembersErr = func(int, string, string) error {
		return &googleapi.Error{Code: http.StatusForbidden, Message: "denied"}
	}
	st := getSAStatus(t, srv, projectID, sa.ID)
	assert.Equal(t, GCPSABindingUnknown, st.WorkloadIdentityBinding.State)
	assert.Contains(t, st.WorkloadIdentityBinding.Reason, "permission denied")
	assert.Equal(t, GCPSANextStepNone, st.NextStep.Code)
	assert.Contains(t, st.NextStep.Message, "could not be checked")
	assert.NotContains(t, st.NextStep.Message, "is not checked")
	require.Len(t, st.Mappings, 1)
	require.NotNil(t, st.Mappings[0].WorkloadIdentityBinding)
	assert.Equal(t, GCPSABindingUnknown, st.Mappings[0].WorkloadIdentityBinding.State)

	// Clear the error: the next request succeeds without waiting 60s.
	mock.roleMembersErr = nil
	mock.setRoleMembers(email, roleWorkloadIdentityUser, []string{
		"serviceAccount:cluster-project.svc.id.goog[agents/scion-worker]",
	})
	st = getSAStatus(t, srv, projectID, sa.ID)
	assert.Equal(t, GCPSABindingBound, st.WorkloadIdentityBinding.State)
	assert.Len(t, mock.roleMembersCalls, 2)
}

// End-to-end FYI-4 through heartbeat downgrade: a broker reports a pool with
// wiTargetsV1, then downgrades and heartbeats without wiTargetsV1. The hub
// keeps the stored target and checks the binding, flagging the target as stale.
func TestGCPSAStatus_WIBinding_DowngradedBrokerHeartbeatMarksStale(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	setWorkloadIdentitySetupExperiment(t, srv, true)
	projectID := createTestProjectForSA(t, srv, nil)
	const email = "scion-worker@test-hub-project.iam.gserviceaccount.com"
	b := addProviderBroker(t, s, projectID, "a", authoritativeProfile("gke", email))
	sa := createStatusTestSA(t, s, store.ScopeProject, projectID, email, true)
	mock.setRoleMembers(email, roleWorkloadIdentityUser, []string{
		"serviceAccount:cluster-project.svc.id.goog[agents/scion-worker]",
	})

	targets := []store.WorkloadIdentityTarget{{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"}}
	wiHeartbeatAsBroker(t, srv, b.ID, brokerHeartbeatRequest{
		Status:                  "online",
		Capabilities:            &store.BrokerCapabilities{WITargetsV1: true},
		WorkloadIdentityTargets: &targets,
	})
	st := getSAStatus(t, srv, projectID, sa.ID)
	require.Len(t, st.WorkloadIdentityBinding.Targets, 1)
	assert.False(t, st.WorkloadIdentityBinding.Targets[0].Stale)
	assert.NotNil(t, st.WorkloadIdentityBinding.Targets[0].LastHeartbeat)

	// Downgraded broker sends Capabilities without WITargetsV1 and omits WorkloadIdentityTargets.
	wiHeartbeatAsBroker(t, srv, b.ID, brokerHeartbeatRequest{
		Status:       "online",
		Capabilities: &store.BrokerCapabilities{WITargetsV1: false},
	})
	st = getSAStatus(t, srv, projectID, sa.ID)
	assert.Equal(t, GCPSABindingBound, st.WorkloadIdentityBinding.State)
	assert.Equal(t, "1 target from stale broker report", st.WorkloadIdentityBinding.Reason)
	assert.NotContains(t, st.NextStep.Message, "Nothing missing")
	assert.Contains(t, st.NextStep.Message, "stale broker reports")
	require.Len(t, st.WorkloadIdentityBinding.Targets, 1)
	assert.Equal(t, GCPSABindingBound, st.WorkloadIdentityBinding.Targets[0].State)
	assert.True(t, st.WorkloadIdentityBinding.Targets[0].Stale)
	assert.Equal(t, GCPSABindingTargetStaleBrokerNoLongerReports, st.WorkloadIdentityBinding.Targets[0].StaleReason)
	assert.Nil(t, st.WorkloadIdentityBinding.Targets[0].LastHeartbeat)
	require.NotNil(t, st.Mappings[0].WorkloadIdentityBinding)
	assert.True(t, st.Mappings[0].WorkloadIdentityBinding.Stale)
}

// O4: Deleting a minted GSA registration invalidates its cached Workload
// Identity role-members entry so re-registering/re-minting the same email
// within 60s re-reads IAM immediately.
func TestGCPSAStatus_WIBinding_DeleteSAInvalidatesCache(t *testing.T) {
	withWIBindingClock(t, time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	srv, s, mock := testServerWithMinting(t)
	setWorkloadIdentitySetupExperiment(t, srv, true)
	projectID := createTestProjectForSA(t, srv, nil)
	const email = "scion-worker@test-hub-project.iam.gserviceaccount.com"
	b := addProviderBroker(t, s, projectID, "a", authoritativeProfile("gke", email))
	require.NoError(t, s.UpdateRuntimeBrokerHeartbeat(context.Background(), b.ID, store.BrokerStatusOnline))
	setWorkloadIdentityTargets(t, s, b.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
	})

	sa := createStatusTestSA(t, s, store.ScopeProject, projectID, email, true)
	mock.setRoleMembers(email, roleWorkloadIdentityUser, []string{
		"serviceAccount:cluster-project.svc.id.goog[agents/scion-worker]",
	})
	st := getSAStatus(t, srv, projectID, sa.ID)
	assert.Equal(t, GCPSABindingBound, st.WorkloadIdentityBinding.State)
	require.Len(t, mock.roleMembersCalls, 1)

	// Delete the GSA registration through the HTTP handler and clear IAM bindings.
	rec := doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s", projectID, sa.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	mock.setRoleMembers(email, roleWorkloadIdentityUser, nil)

	// Re-create a minted GSA with the same email within the 60s window: status
	// must NOT serve the deleted account's cached "bound" set.
	reminted := &store.GCPServiceAccount{
		ID:                 tid("wi-status-sa-reminted-" + t.Name()),
		Scope:              store.ScopeProject,
		ScopeID:            projectID,
		Email:              email,
		ProjectID:          "test-hub-project",
		Verified:           true,
		VerificationStatus: store.GCPVerificationVerified,
		VerifiedAt:         time.Now().UTC().Truncate(time.Second),
		CreatedAt:          time.Now().UTC().Truncate(time.Second),
		Managed:            true,
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), reminted))

	stAfter := getSAStatus(t, srv, projectID, reminted.ID)
	assert.Len(t, mock.roleMembersCalls, 2, "delete invalidated the cached entry")
	assert.Equal(t, GCPSABindingNotBound, stAfter.WorkloadIdentityBinding.State)
	assert.Equal(t, "not_bound", stAfter.NextStep.Code)
}
