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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
	Warnings                 []string                      `json:"warnings"`
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

func mintHubForWI(t *testing.T, srv *Server, admin *store.User) (wiMintResponse, string) {
	t.Helper()
	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/gcp-service-accounts/mint?scope=hub",
		map[string]string{"account_id": "wi-hub"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp wiMintResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp, rec.Body.String()
}

func addStandaloneBroker(t *testing.T, s store.Store, name string, profiles ...store.BrokerProfile) *store.RuntimeBroker {
	t.Helper()
	b := &store.RuntimeBroker{
		ID:       tid("wi-hub-" + name + "-" + t.Name()),
		Name:     name,
		Slug:     "wi-hub-" + name + "-" + tidSlugSafe(t.Name()),
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
		Profiles: profiles,
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), b))
	return b
}

func wiAuditEvents(events []*GCPTokenEvent) []*GCPTokenEvent {
	var out []*GCPTokenEvent
	for _, e := range events {
		if e.EventType == GCPTokenEventWIBinding {
			out = append(out, e)
		}
	}
	return out
}

// Experiment on: the mint grants the Workload Identity role for every
// serving Kubernetes target in one write, reports each target, and leaves
// the required grants unchanged.
func TestMintGCPServiceAccount_WorkloadIdentityBindings(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	audit := &mockAuditLogger{}
	srv.auditLogger = audit
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

	wiEvents := wiAuditEvents(audit.gcpEvents)
	require.Len(t, wiEvents, 3)
	assert.True(t, wiEvents[0].Success)
	assert.Equal(t, projectID, wiEvents[0].ProjectID)
	assert.Equal(t, "serviceAccount:cluster-project.svc.id.goog[agents/scion-wi-test]", wiEvents[0].Member)
	assert.Empty(t, wiEvents[0].FailReason)
	assert.False(t, wiEvents[1].Success)
	assert.Equal(t, "no_workload_identity_pool a/gke-dev", wiEvents[1].FailReason)
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

// Hub-scoped mint with the experiment on: binds over every Runtime Broker on
// the hub (whether linked to a project or standalone, legacy or flat),
// merges duplicate (pool, namespace) pairs into one write, audits the bound
// member, and uses only brokerSelf-reported, validated targets.
func TestMintHubScopedGCPServiceAccount_WorkloadIdentityBindings(t *testing.T) {
	srv, s, mock, admin, _ := setupHubMintTest(t)
	audit := &mockAuditLogger{}
	srv.auditLogger = audit
	setWorkloadIdentitySetupExperiment(t, srv, true)
	grantDevUserRuntimeBrokerAccess(t, s)

	projectID := createTestProjectForSA(t, srv, nil)
	a := addProviderBroker(t, s, projectID, "a", k8sProfile("gke", false), k8sProfile("gke-dev", false))
	setWorkloadIdentityTargets(t, s, a.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-one.svc.id.goog", Namespace: "agents"},
	})

	// A broker linked to no project at all still receives bindings at hub
	// scope, and shares the (cluster-one, agents) entry with a/gke.
	standalone := addStandaloneBroker(t, s, "standalone", k8sProfile("prod", false), k8sProfile("staging", false))
	standaloneTargets := []store.WorkloadIdentityTarget{
		{Profile: "prod", Pool: "cluster-one.svc.id.goog", Namespace: "agents"},
		{Profile: "staging", Pool: "cluster-two.svc.id.goog", Namespace: "team"},
		{Profile: "staging", Pool: "$(evil).svc.id.goog", Namespace: "team"},
	}
	wiHeartbeatAsBroker(t, srv, standalone.ID, brokerHeartbeatRequest{
		Status:                  "online",
		Capabilities:            &store.BrokerCapabilities{WITargetsV1: true},
		WorkloadIdentityTargets: &standaloneTargets,
	})

	// A user-authenticated heartbeat cannot plant targets on a broker for a
	// hub-scoped mint either.
	untrusted := addStandaloneBroker(t, s, "old", k8sProfile("k8s", false))
	hostile := []store.WorkloadIdentityTarget{{Profile: "k8s", Pool: "attacker-proj.svc.id.goog", Namespace: "agents"}}
	postHeartbeat(t, srv, untrusted.ID, brokerHeartbeatRequest{Status: "online", WorkloadIdentityTargets: &hostile})

	// Non-Kubernetes brokers are ignored.
	addStandaloneBroker(t, s, "docker-only", store.BrokerProfile{Name: "local", Type: "docker"})

	resp, _ := mintHubForWI(t, srv, admin)
	assert.True(t, resp.Verified)
	assert.True(t, resp.Managed)
	assert.Equal(t, store.ScopeHub, resp.Scope)
	assert.Equal(t, "scion-wi-hub@test-hub-project.iam.gserviceaccount.com", resp.Email)

	require.Len(t, mock.addMembersCalls, 1, "one batched policy write across all brokers")
	assert.Equal(t, mockAddRoleMembersCall{
		SAEmail: resp.Email,
		Role:    "roles/iam.workloadIdentityUser",
		Members: []string{
			"serviceAccount:cluster-one.svc.id.goog[agents/scion-wi-hub]",
			"serviceAccount:cluster-two.svc.id.goog[team/scion-wi-hub]",
		},
	}, mock.addMembersCalls[0])
	require.Len(t, mock.iamPolicies, 2, "the required grants are unchanged")

	require.Len(t, resp.WorkloadIdentityBindings, 4)
	assert.Equal(t, api.WorkloadIdentityBinding{
		Pool: "cluster-one.svc.id.goog", Namespace: "agents", KSA: "scion-wi-hub",
		Member:   "serviceAccount:cluster-one.svc.id.goog[agents/scion-wi-hub]",
		Profiles: []string{"a/gke", "standalone/prod"}, State: api.WorkloadIdentityBindingBound,
	}, resp.WorkloadIdentityBindings[0])
	assert.Equal(t, api.WorkloadIdentityBinding{
		Pool: "cluster-two.svc.id.goog", Namespace: "team", KSA: "scion-wi-hub",
		Member:   "serviceAccount:cluster-two.svc.id.goog[team/scion-wi-hub]",
		Profiles: []string{"standalone/staging"}, State: api.WorkloadIdentityBindingBound,
	}, resp.WorkloadIdentityBindings[1])
	assert.Equal(t, []string{"a/gke-dev"}, resp.WorkloadIdentityBindings[2].Profiles)
	assert.Equal(t, api.WorkloadIdentityReasonNoPool, resp.WorkloadIdentityBindings[2].Reason)
	assert.Equal(t, []string{"old/k8s"}, resp.WorkloadIdentityBindings[3].Profiles)
	assert.Equal(t, api.WorkloadIdentityReasonNoPool, resp.WorkloadIdentityBindings[3].Reason)
	assert.Contains(t, resp.WorkloadIdentityBindings[3].Detail, "does not report")

	wiEvents := wiAuditEvents(audit.gcpEvents)
	require.Len(t, wiEvents, 4)
	assert.True(t, wiEvents[0].Success)
	assert.Empty(t, wiEvents[0].ProjectID, "hub-scoped mint carries empty projectId")
	assert.Equal(t, "serviceAccount:cluster-one.svc.id.goog[agents/scion-wi-hub]", wiEvents[0].Member)
	assert.True(t, wiEvents[1].Success)
	assert.Equal(t, "serviceAccount:cluster-two.svc.id.goog[team/scion-wi-hub]", wiEvents[1].Member)
	assert.False(t, wiEvents[2].Success)
	assert.Equal(t, "no_workload_identity_pool a/gke-dev", wiEvents[2].FailReason)
}

func TestMintHubScopedGCPServiceAccount_WorkloadIdentityGrantFailureKeepsMint(t *testing.T) {
	srv, s, mock, admin, _ := setupHubMintTest(t)
	setWorkloadIdentitySetupExperiment(t, srv, true)
	mock.addMembersErr = func(int, []string) error {
		return &googleapi.Error{Code: http.StatusForbidden, Message: "denied"}
	}
	b := addStandaloneBroker(t, s, "a", k8sProfile("gke", false))
	setWorkloadIdentityTargets(t, s, b.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
	})

	resp, _ := mintHubForWI(t, srv, admin)
	assert.True(t, resp.Verified)
	assert.True(t, resp.Managed)
	assert.Equal(t, store.ScopeHub, resp.Scope)
	assert.Empty(t, mock.deletedSAs, "the account is not cleaned up")
	stored, err := s.GetGCPServiceAccount(context.Background(), resp.ID)
	require.NoError(t, err)
	assert.True(t, stored.Verified)
	require.Len(t, resp.WorkloadIdentityBindings, 1)
	entry := resp.WorkloadIdentityBindings[0]
	assert.Equal(t, api.WorkloadIdentityBindingSkipped, entry.State)
	assert.Equal(t, api.WorkloadIdentityReasonPermissionDenied, entry.Reason)
	assert.Contains(t, entry.ManualStep, "gcloud iam service-accounts add-iam-policy-binding '"+resp.Email+"'")
}

func TestMintHubScopedGCPServiceAccount_WorkloadIdentityExperimentOff(t *testing.T) {
	srv, s, mock, admin, _ := setupHubMintTest(t)
	setWorkloadIdentitySetupExperiment(t, srv, false)
	b := addStandaloneBroker(t, s, "a", k8sProfile("gke", false))
	setWorkloadIdentityTargets(t, s, b.ID, true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
	})

	_, body := mintHubForWI(t, srv, admin)
	assert.Empty(t, mock.addMembersCalls)
	assert.NotContains(t, body, "workloadIdentityBindings")
}

func TestMintHubScopedGCPServiceAccount_WorkloadIdentityNoKubernetesTargets(t *testing.T) {
	srv, s, mock, admin, _ := setupHubMintTest(t)
	setWorkloadIdentitySetupExperiment(t, srv, true)
	addStandaloneBroker(t, s, "a", store.BrokerProfile{Name: "local", Type: "docker"})

	_, body := mintHubForWI(t, srv, admin)
	assert.Empty(t, mock.addMembersCalls)
	assert.NotContains(t, body, "workloadIdentityBindings")
}

type wiBrokerReadCountStore struct {
	store.Store
	fault           *storeFaultSwitch
	failListBrokers atomic.Bool
	providersReads  atomic.Int32
	brokerGets      atomic.Int32
	brokerLists     atomic.Int32
}

func (c *wiBrokerReadCountStore) GetProjectProviders(ctx context.Context, projectID string) ([]store.ProjectProvider, error) {
	if c.fault.Active() {
		c.providersReads.Add(1)
	}
	return c.Store.GetProjectProviders(ctx, projectID)
}

func (c *wiBrokerReadCountStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	if c.fault.Active() {
		c.brokerGets.Add(1)
	}
	return c.Store.GetRuntimeBroker(ctx, id)
}

func (c *wiBrokerReadCountStore) ListRuntimeBrokers(ctx context.Context, filter store.RuntimeBrokerFilter, opts store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
	if c.fault.Active() {
		c.brokerLists.Add(1)
		if c.failListBrokers.Load() {
			return nil, errors.New("broker listing failed")
		}
	}
	return c.Store.ListRuntimeBrokers(ctx, filter, opts)
}

// Each mint reads its broker set once and reuses it for both the Workload
// Identity plan and the SA mapping warnings.
func TestMintGCPServiceAccount_BrokerListReusedForWarnings(t *testing.T) {
	t.Run("project scope", func(t *testing.T) {
		srv, s, mock := testServerWithMinting(t)
		counter, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *wiBrokerReadCountStore {
			return &wiBrokerReadCountStore{Store: inner, fault: f}
		})
		setWorkloadIdentitySetupExperiment(t, srv, true)
		projectID := createTestProjectForSA(t, srv, nil)
		b := addProviderBroker(t, s, projectID, "a", authoritativeProfile("gke", mappedGSA))
		setWorkloadIdentityTargets(t, s, b.ID, true, []store.WorkloadIdentityTarget{
			{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
		})

		fault.Arm()
		resp, _ := mintForWI(t, srv, projectID)
		assert.Len(t, mock.addMembersCalls, 1)
		require.Len(t, resp.WorkloadIdentityBindings, 1)
		assert.Equal(t, api.WorkloadIdentityBindingBound, resp.WorkloadIdentityBindings[0].State)
		require.Len(t, resp.Warnings, 1, "unmapped warning still produced from the reused broker list")
		assert.Contains(t, resp.Warnings[0], resp.Email)
		assert.Equal(t, int32(1), counter.providersReads.Load(), "GetProjectProviders called once")
		assert.Equal(t, int32(1), counter.brokerGets.Load(), "GetRuntimeBroker called once")
	})

	t.Run("hub scope", func(t *testing.T) {
		srv, s, mock, admin, _ := setupHubMintTest(t)
		counter, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *wiBrokerReadCountStore {
			return &wiBrokerReadCountStore{Store: inner, fault: f}
		})
		setWorkloadIdentitySetupExperiment(t, srv, true)
		b := addStandaloneBroker(t, s, "a", authoritativeProfile("gke", mappedGSA))
		setWorkloadIdentityTargets(t, s, b.ID, true, []store.WorkloadIdentityTarget{
			{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"},
		})

		fault.Arm()
		resp, _ := mintHubForWI(t, srv, admin)
		assert.Len(t, mock.addMembersCalls, 1)
		require.Len(t, resp.WorkloadIdentityBindings, 1)
		assert.Equal(t, api.WorkloadIdentityBindingBound, resp.WorkloadIdentityBindings[0].State)
		require.Len(t, resp.Warnings, 1, "unmapped hub warning still produced from the reused broker list")
		assert.Contains(t, resp.Warnings[0], "of this hub")
		assert.Equal(t, int32(1), counter.brokerLists.Load(), "ListRuntimeBrokers called once")
	})

	t.Run("hub scope broker list failure still mints", func(t *testing.T) {
		srv, s, _, admin, _ := setupHubMintTest(t)
		counter, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *wiBrokerReadCountStore {
			return &wiBrokerReadCountStore{Store: inner, fault: f}
		})
		setWorkloadIdentitySetupExperiment(t, srv, true)
		addStandaloneBroker(t, s, "a", authoritativeProfile("gke", mappedGSA))

		counter.failListBrokers.Store(true)
		fault.Arm()
		resp, _ := mintHubForWI(t, srv, admin)
		assert.True(t, resp.Verified)
	})
}
