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
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestK8sWorkloadIdentitySetupExperiment_Dispatched(t *testing.T) {
	on := dispatchExperimentsServer(t, `{"overrides":{"hub.k8s_workload_identity_setup":true}}`)
	assert.True(t, slices.Contains(on.dispatchExperiments(), experiments.K8sWorkloadIdentitySetup))

	off := dispatchExperimentsServer(t, "")
	assert.False(t, slices.Contains(off.dispatchExperiments(), experiments.K8sWorkloadIdentitySetup))
}

func TestDispatcher_CreateKSA_MintedAndExperimentMatrix(t *testing.T) {
	ctx := context.Background()
	d, client, baseAgent := autoExposeDispatchFixture(t)

	now := time.Now()
	broker, err := d.store.GetRuntimeBroker(ctx, baseAgent.RuntimeBrokerID)
	require.NoError(t, err)
	broker.Profiles = []store.BrokerProfile{{
		Name:                  "gke",
		Type:                  "kubernetes",
		Available:             true,
		MappingsReported:      true,
		MappingsComplete:      true,
		MappingsReportVersion: api.BrokerSAReportVersion,
		MappingsReportedAt:    &now,
	}}
	broker.WorkloadIdentityTargets = []store.WorkloadIdentityTarget{{
		Profile:     "gke",
		Pool:        "cluster-project.svc.id.goog",
		Namespace:   "agents",
		KSACreation: true,
	}}
	require.NoError(t, d.store.UpdateRuntimeBroker(ctx, broker))

	mintedSA := &store.GCPServiceAccount{
		ID:                 "44444444-4444-4444-8444-444444444444",
		Scope:              store.ScopeProject,
		ScopeID:            baseAgent.ProjectID,
		Email:              "scion-minted@hub-proj.iam.gserviceaccount.com",
		ProjectID:          "hub-proj",
		Verified:           true,
		VerificationStatus: store.GCPVerificationVerified,
		Managed:            true,
		ManagedBy:          "scion-hub",
	}
	byoSA := &store.GCPServiceAccount{
		ID:                 "55555555-5555-4555-8555-555555555555",
		Scope:              store.ScopeProject,
		ScopeID:            baseAgent.ProjectID,
		Email:              "byo-worker@other-proj.iam.gserviceaccount.com",
		ProjectID:          "other-proj",
		Verified:           true,
		VerificationStatus: store.GCPVerificationVerified,
		Managed:            false,
	}
	require.NoError(t, d.store.CreateGCPServiceAccount(ctx, mintedSA))
	require.NoError(t, d.store.CreateGCPServiceAccount(ctx, byoSA))

	agentFor := func(sa *store.GCPServiceAccount) *store.Agent {
		cp := *baseAgent
		cp.AppliedConfig = &store.AgentAppliedConfig{
			Profile: "gke",
			GCPIdentity: &store.GCPIdentityConfig{
				MetadataMode:        store.GCPMetadataModeAssign,
				ServiceAccountID:    sa.ID,
				ServiceAccountEmail: sa.Email,
				ProjectID:           sa.ProjectID,
			},
		}
		return &cp
	}

	var enabled []string
	d.SetDispatchExperimentsProvider(func() []string { return enabled })

	t.Run("experiment ON + minted GSA passes precheck and sets CreateKSA on create, start, and restart", func(t *testing.T) {
		enabled = []string{experiments.K8sWorkloadIdentitySetup}
		ag := agentFor(mintedSA)

		_, err := d.gcpIdentityPrecheck(ctx, ag)
		require.NoError(t, err, "precheck must treat minted GSA on ksaCreation profile as creatable")

		req, err := d.buildCreateRequest(ctx, ag, "test")
		require.NoError(t, err)
		require.NotNil(t, req.Config)
		require.NotNil(t, req.Config.HubAgentDefaults)
		assert.True(t, req.Config.HubAgentDefaults.CreateKSA)

		require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
		require.NotNil(t, client.lastStartExtras.HubAgentDefaults)
		assert.True(t, client.lastStartExtras.HubAgentDefaults.CreateKSA)

		require.NoError(t, d.DispatchAgentRestart(ctx, ag))
		require.NotNil(t, client.lastRestartExtras.HubAgentDefaults)
		assert.True(t, client.lastRestartExtras.HubAgentDefaults.CreateKSA)
	})

	t.Run("experiment OFF + minted GSA refuses precheck and never sets CreateKSA", func(t *testing.T) {
		enabled = nil
		ag := agentFor(mintedSA)

		_, err := d.gcpIdentityPrecheck(ctx, ag)
		var pre *identityNotMappedPrecheck
		require.ErrorAs(t, err, &pre)

		req, err := d.buildCreateRequest(ctx, ag, "test")
		require.NoError(t, err)
		if req.Config != nil && req.Config.HubAgentDefaults != nil {
			assert.False(t, req.Config.HubAgentDefaults.CreateKSA)
		}
	})

	t.Run("experiment ON + BYO GSA refuses precheck and never sets CreateKSA", func(t *testing.T) {
		enabled = []string{experiments.K8sWorkloadIdentitySetup}
		ag := agentFor(byoSA)

		_, err := d.gcpIdentityPrecheck(ctx, ag)
		var pre *identityNotMappedPrecheck
		require.ErrorAs(t, err, &pre)

		req, err := d.buildCreateRequest(ctx, ag, "test")
		require.NoError(t, err)
		require.NotNil(t, req.Config.HubAgentDefaults)
		assert.False(t, req.Config.HubAgentDefaults.CreateKSA)
	})

	t.Run("experiment ON + minted GSA + broker ksaCreation false refuses precheck", func(t *testing.T) {
		enabled = []string{experiments.K8sWorkloadIdentitySetup}
		broker.WorkloadIdentityTargets = []store.WorkloadIdentityTarget{{
			Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents", KSACreation: false,
		}}
		require.NoError(t, d.store.UpdateRuntimeBroker(ctx, broker))
		ag := agentFor(mintedSA)

		_, err := d.gcpIdentityPrecheck(ctx, ag)
		var pre *identityNotMappedPrecheck
		require.ErrorAs(t, err, &pre)
	})
}

func TestHandleListAndStatusGCPServiceAccounts_CreatableHTTP(t *testing.T) {
	ctx := context.Background()
	srv, s, _ := testServerWithMinting(t)
	projectID := createTestProjectForSA(t, srv, nil)

	const (
		brokerID = "11111111-1111-4111-8111-111111111111"
		mintedID = "22222222-2222-4222-8222-222222222222"
		byoID    = "33333333-3333-4333-8333-333333333333"
	)
	now := time.Now()
	broker := &store.RuntimeBroker{
		ID:            brokerID,
		Name:          "broker-p3",
		Slug:          "broker-p3",
		Status:        store.BrokerStatusOnline,
		LastHeartbeat: now,
		Capabilities:  &store.BrokerCapabilities{WITargetsV1: true},
		Profiles: []store.BrokerProfile{{
			Name:                  "gke",
			Type:                  "kubernetes",
			Available:             true,
			MappingsReported:      true,
			MappingsComplete:      true,
			MappingsReportVersion: api.BrokerSAReportVersion,
			MappingsReportedAt:    &now,
		}},
		WorkloadIdentityTargets: []store.WorkloadIdentityTarget{{
			Profile:     "gke",
			Pool:        "cluster-project.svc.id.goog",
			Namespace:   "agents",
			KSACreation: true,
		}},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
	}))
	proj, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)
	proj.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, proj))

	minted := &store.GCPServiceAccount{
		ID:                 mintedID,
		Scope:              store.ScopeProject,
		ScopeID:            projectID,
		Email:              "scion-minted@hub-proj.iam.gserviceaccount.com",
		ProjectID:          "hub-proj",
		Verified:           true,
		VerificationStatus: store.GCPVerificationVerified,
		Managed:            true,
	}
	byo := &store.GCPServiceAccount{
		ID:                 byoID,
		Scope:              store.ScopeProject,
		ScopeID:            projectID,
		Email:              "byo@other-proj.iam.gserviceaccount.com",
		ProjectID:          "other-proj",
		Verified:           true,
		VerificationStatus: store.GCPVerificationVerified,
		Managed:            false,
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, minted))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, byo))

	// Experiment OFF: both minted and BYO report not_mapped.
	setWorkloadIdentitySetupExperiment(t, srv, false)
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/gcp-service-accounts?assignStatus=true&profile=gke", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listOff struct {
		Items    []GCPServiceAccountWithCapabilities `json:"items"`
		Warnings []string                            `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listOff))
	for _, it := range listOff.Items {
		require.NotNil(t, it.AssignStatus)
		assert.Equal(t, SAMappingStateNotMapped, it.AssignStatus.State, it.ID)
	}

	// Experiment ON: minted reports creatable, BYO still reports not_mapped.
	setWorkloadIdentitySetupExperiment(t, srv, true)
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/gcp-service-accounts?assignStatus=true&profile=gke", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listOn struct {
		Items    []GCPServiceAccountWithCapabilities `json:"items"`
		Warnings []string                            `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listOn))
	byID := map[string]GCPServiceAccountWithCapabilities{}
	for _, it := range listOn.Items {
		byID[it.ID] = it
	}
	assert.Equal(t, SAMappingStateCreatable, byID[mintedID].AssignStatus.State)
	assert.Equal(t, 1, byID[mintedID].Mapping.MappedProfiles)
	assert.Equal(t, SAMappingStateNotMapped, byID[byoID].AssignStatus.State)
	assert.Equal(t, 0, byID[byoID].Mapping.MappedProfiles)

	// Per-account status view for minted SA before WI binding grant:
	// mapping stays creatable ("KSA created at start") with conventional KSA,
	// while Workload Identity binding checks the conventional member and
	// surfaces the not_bound grant step.
	const expectedMember = "serviceAccount:cluster-project.svc.id.goog[agents/scion-minted]"
	statusRec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/gcp-service-accounts/"+mintedID+"/status", nil)
	require.Equal(t, http.StatusOK, statusRec.Code, statusRec.Body.String())
	var st GCPServiceAccountStatus
	require.NoError(t, json.Unmarshal(statusRec.Body.Bytes(), &st))
	require.Len(t, st.Mappings, 1)
	assert.Equal(t, GCPSAMappingCreatable, st.Mappings[0].State)
	assert.Equal(t, "scion-minted", st.Mappings[0].KubernetesServiceAccount)
	assert.Equal(t, "agents", st.Mappings[0].Namespace)
	require.NotNil(t, st.Mappings[0].WorkloadIdentityBinding)
	assert.Equal(t, GCPSABindingNotBound, st.Mappings[0].WorkloadIdentityBinding.State)
	assert.Equal(t, expectedMember, st.Mappings[0].WorkloadIdentityBinding.Member)
	assert.Equal(t, GCPSABindingNotBound, st.WorkloadIdentityBinding.State)
	assert.Equal(t, GCPSANextStepNotBound, st.NextStep.Code)
	assert.Contains(t, st.NextStep.Message, expectedMember)

	// Once roles/iam.workloadIdentityUser is granted to the conventional KSA
	// member, the status view reports bound and nextStep becomes none.
	require.NoError(t, srv.addWorkloadIdentityMembers(ctx, minted.Email, []string{expectedMember}))
	statusRec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/gcp-service-accounts/"+mintedID+"/status", nil)
	require.Equal(t, http.StatusOK, statusRec.Code, statusRec.Body.String())
	st = GCPServiceAccountStatus{}
	require.NoError(t, json.Unmarshal(statusRec.Body.Bytes(), &st))
	require.Len(t, st.Mappings, 1)
	assert.Equal(t, GCPSAMappingCreatable, st.Mappings[0].State)
	require.NotNil(t, st.Mappings[0].WorkloadIdentityBinding)
	assert.Equal(t, GCPSABindingBound, st.Mappings[0].WorkloadIdentityBinding.State)
	assert.Equal(t, expectedMember, st.Mappings[0].WorkloadIdentityBinding.Member)
	assert.Equal(t, GCPSABindingBound, st.WorkloadIdentityBinding.State)
	assert.Equal(t, GCPSANextStepNone, st.NextStep.Code)
}
