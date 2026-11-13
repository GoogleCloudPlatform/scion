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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const refusalTestDevToken = "scion_dev_refusal_test_token"

func flatUserAPITestHub(t *testing.T, s store.Store, enableBrokerAuth bool) *hub.Server {
	t.Helper()
	ctx := context.Background()
	_, err := s.UpsertHubSetting(ctx, "experiments",
		json.RawMessage(fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.FlatRuntimeBrokers)), "test", -1, "managed")
	require.NoError(t, err)
	hubCfg := hub.DefaultServerConfig()
	hubCfg.DisableCloudLogQuery = true
	hubCfg.DevAuthToken = refusalTestDevToken
	hubCfg.BrokerAuthConfig.Enabled = enableBrokerAuth
	srv, err := hub.New(hubCfg, s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	ops := hub.NewOperationalSettings(s, koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	return srv
}

func doHubUserJSON(t *testing.T, srv *hub.Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer "+refusalTestDevToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func prepareColocatedK8sHost(
	t *testing.T,
	srv *hub.Server,
	globalDir string,
	locks *colocatedFlatLocks,
	inst config.V1RuntimeBrokerInstanceConfig,
	newRT brokerhost.RuntimeFactory,
	probe brokerhost.ScopeProber,
) *brokerhost.Host {
	t.Helper()
	srv.ExpectEmbeddedBroker()
	srv.ExpectEmbeddedFlatInstances([]string{inst.Key})
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir:  globalDir,
		Instances:  []config.V1RuntimeBrokerInstanceConfig{inst},
		Mode:       brokerhost.ModeColocated,
		NewRuntime: newRT,
		ProbeScope: probe,
		Activator: &colocatedFlatActivator{
			hubSrv:    srv,
			globalDir: globalDir,
			locks:     locks,
			endpoint:  "http://localhost:9800/",
		},
		BuildServer: func(ic brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			cfg := runtimebroker.DefaultServerConfig()
			cfg.BrokerID = ic.Identity.RuntimeBrokerID
			cfg.StateDir = filepath.Join(globalDir, "state", ic.Identity.RuntimeBrokerID)
			cfg.FlatInstance = &runtimebroker.FlatInstanceConfig{Identity: ic.Identity, Instance: ic.Instance, HubInProcess: true}
			return runtimebroker.New(cfg, ic.Manager, ic.Runtime, config.RuntimeBuildRecord{}), nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))
	return h
}

func mockK8sFactory() brokerhost.RuntimeFactory {
	return func(context.Context, config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
		return &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}, nil
	}
}

func k8sScopeProber(clusterUID string) brokerhost.ScopeProber {
	return func(_ context.Context, inst config.V1RuntimeBrokerInstanceConfig, _ runtime.Runtime) (brokeridentity.ExecutionScope, error) {
		ns := "default"
		if inst.RuntimeTarget != nil && inst.RuntimeTarget.Namespace != "" {
			ns = inst.RuntimeTarget.Namespace
		}
		return brokeridentity.ExecutionScope{
			Type: brokeridentity.TargetTypeKubernetes,
			Kubernetes: &brokeridentity.KubernetesScope{
				ClusterUID: clusterUID,
				Namespace:  ns,
			},
		}, nil
	}
}

// TestColocatedFlatStartupRefusal_MarksOfflineImmediatelyAndCreateReturns503
// verifies ptone/scion#7820 across all startup refusal paths:
//  1. Scope change after restart (e.g. Kubernetes namespace edited in settings.yaml)
//  2. Activate-phase refusal (Candidate.Identity already resolved, Activate fails)
//  3. Pass-1 refusal before Activate (ErrExecutionScopeUnidentified / runtime build failure)
//
// In every case, without waiting 5 minutes for the reaper:
//   - GET /api/v1/runtime-brokers/{id} and GET /api/v1/runtime-brokers report
//     status: "offline", connectionState: "disconnected", hostedByThisServer: false,
//     and non-healthy health.
//   - POST /api/v1/agents on a hub-managed project (explicit runtimeBrokerId and
//     project defaultRuntimeBrokerId) fails with 503 broker-unavailable rather than
//     the misleading 409 remote shared-workspace error.
//   - Restoring valid configuration on a subsequent restart re-activates the row cleanly.
func TestColocatedFlatStartupRefusal_MarksOfflineImmediatelyAndCreateReturns503(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	globalDir := t.TempDir()

	instOrig := config.V1RuntimeBrokerInstanceConfig{
		Key:  "k8s-staging",
		Name: "k8s-staging",
		RuntimeTarget: &config.V1RuntimeTargetConfig{
			Type:        "kubernetes",
			Namespace:   "scion-agents",
			DisplayName: "Staging GKE",
		},
	}

	// Boot 1: instance activates cleanly, connects, reports healthy, and is
	// linked as default provider of a hub-managed (local-workspace) project.
	srv1 := flatUserAPITestHub(t, s, true)
	h1 := prepareColocatedK8sHost(t, srv1, globalDir, nil, instOrig, mockK8sFactory(), k8sScopeProber("cluster-uid-1"))
	require.Len(t, h1.Active(), 1)
	brokerID := h1.Active()[0].Context.Identity.RuntimeBrokerID

	require.NoError(t, s.ClaimRuntimeBrokerConnection(ctx, brokerID, srv1.InstanceID(), "sess-boot-1"))
	row1, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	row1.Health = &api.BrokerHealthReport{
		Status: api.BrokerHealthHealthy,
		Checks: map[string]string{"runtime": api.BrokerHealthAvailable},
	}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, row1))

	project := &store.Project{
		ID:                     api.NewUUID(),
		Name:                   "Hub Managed Project",
		Slug:                   "hub-managed-proj",
		DefaultRuntimeBrokerID: brokerID,
		CreatedBy:              hub.DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   brokerID,
		BrokerName: instOrig.Name,
		Status:     store.BrokerStatusOnline,
	}))

	assertRefusedOfflineAndCreate503 := func(t *testing.T, srv *hub.Server, step string) {
		t.Helper()

		getRec := doHubUserJSON(t, srv, http.MethodGet, "/api/v1/runtime-brokers/"+brokerID, nil)
		require.Equal(t, http.StatusOK, getRec.Code, "%s: %s", step, getRec.Body.String())
		var detail struct {
			Status             string                  `json:"status"`
			ConnectionState    string                  `json:"connectionState"`
			HostedByThisServer bool                    `json:"hostedByThisServer"`
			Health             *api.BrokerHealthReport `json:"health"`
		}
		require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &detail))
		assert.Equal(t, store.BrokerStatusOffline, detail.Status, "%s: detail status", step)
		assert.Equal(t, "disconnected", detail.ConnectionState, "%s: detail connectionState", step)
		assert.False(t, detail.HostedByThisServer, "%s: hostedByThisServer", step)
		require.NotNil(t, detail.Health, "%s: detail health", step)
		assert.NotEqual(t, api.BrokerHealthHealthy, detail.Health.Status, "%s: detail health must not stay healthy", step)

		listRec := doHubUserJSON(t, srv, http.MethodGet, "/api/v1/runtime-brokers", nil)
		require.Equal(t, http.StatusOK, listRec.Code, "%s: %s", step, listRec.Body.String())
		var list struct {
			Brokers []struct {
				ID              string                  `json:"id"`
				Status          string                  `json:"status"`
				ConnectionState string                  `json:"connectionState"`
				Health          *api.BrokerHealthReport `json:"health"`
			} `json:"brokers"`
		}
		require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &list))
		require.Len(t, list.Brokers, 1)
		assert.Equal(t, store.BrokerStatusOffline, list.Brokers[0].Status, "%s: list status", step)
		assert.Equal(t, "disconnected", list.Brokers[0].ConnectionState, "%s: list connectionState", step)
		require.NotNil(t, list.Brokers[0].Health, "%s: list health", step)
		assert.NotEqual(t, api.BrokerHealthHealthy, list.Brokers[0].Health.Status, "%s: list health", step)

		prov, err := s.GetProjectProvider(ctx, project.ID, brokerID)
		require.NoError(t, err)
		assert.Equal(t, store.BrokerStatusOffline, prov.Status, "%s: project provider marked offline", step)

		// Explicit runtimeBrokerId: 503 runtime_broker_unavailable, never 409 remote shared-workspace.
		explicitRec := doHubUserJSON(t, srv, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":            "agent-explicit-" + step,
			"projectId":       project.ID,
			"runtimeBrokerId": brokerID,
		})
		assert.Equal(t, http.StatusServiceUnavailable, explicitRec.Code, "%s explicit create: %s", step, explicitRec.Body.String())
		assert.Contains(t, explicitRec.Body.String(), "runtime_broker_unavailable", "%s explicit create error code", step)
		assert.NotContains(t, explicitRec.Body.String(), "shared workspace lives on the hub's local disk", "%s explicit create must not hit remote-workspace 409", step)

		// Project defaultRuntimeBrokerId: 422 no_runtime_broker ("Default runtime broker is unavailable..."),
		// never 409 remote shared-workspace.
		defaultRec := doHubUserJSON(t, srv, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":      "agent-default-" + step,
			"projectId": project.ID,
		})
		assert.Equal(t, http.StatusUnprocessableEntity, defaultRec.Code, "%s default create: %s", step, defaultRec.Body.String())
		assert.Contains(t, defaultRec.Body.String(), "no_runtime_broker", "%s default create error code", step)
		assert.NotContains(t, defaultRec.Body.String(), "shared workspace lives on the hub's local disk", "%s default create must not hit remote-workspace 409", step)
	}

	reseedOnline := func(t *testing.T, prevSrv *hub.Server) {
		t.Helper()
		h := prepareColocatedK8sHost(t, prevSrv, globalDir, nil, instOrig, mockK8sFactory(), k8sScopeProber("cluster-uid-1"))
		require.Len(t, h.Active(), 1)
		require.NoError(t, s.ClaimRuntimeBrokerConnection(ctx, brokerID, prevSrv.InstanceID(), "sess-reseed"))
		r, err := s.GetRuntimeBroker(ctx, brokerID)
		require.NoError(t, err)
		r.Health = &api.BrokerHealthReport{
			Status: api.BrokerHealthHealthy,
			Checks: map[string]string{"runtime": api.BrokerHealthAvailable},
		}
		require.NoError(t, s.UpdateRuntimeBroker(ctx, r))
	}

	// Case 1: Kubernetes namespace changed in settings.yaml -> scope mismatch refusal.
	instRenamedNS := instOrig
	instRenamedNS.RuntimeTarget = &config.V1RuntimeTargetConfig{
		Type:        "kubernetes",
		Namespace:   "scion-agents-renamed",
		DisplayName: "Staging GKE",
	}
	srv2 := flatUserAPITestHub(t, s, true)
	h2 := prepareColocatedK8sHost(t, srv2, globalDir, nil, instRenamedNS, mockK8sFactory(), k8sScopeProber("cluster-uid-1"))
	assert.Empty(t, h2.Active())
	assert.Equal(t, brokerhost.StateRefused, h2.Status()[0].State)
	assertRefusedOfflineAndCreate503(t, srv2, "namespace-changed")

	// Reseed online, then Case 2 (Path A): Activate fails after Candidate.Identity is resolved
	// (e.g. credentials_unavailable when broker auth is unavailable).
	srvReseedA := flatUserAPITestHub(t, s, true)
	reseedOnline(t, srvReseedA)

	srv3 := flatUserAPITestHub(t, s, false) // broker auth disabled -> Activate fails after RegisterEmbeddedFlatRuntimeBroker
	h3 := prepareColocatedK8sHost(t, srv3, globalDir, nil, instOrig, mockK8sFactory(), k8sScopeProber("cluster-uid-1"))
	assert.Empty(t, h3.Active())
	assert.Equal(t, "credentials_unavailable", h3.Status()[0].Reason)
	assertRefusedOfflineAndCreate503(t, srv3, "activate-failed")

	// Reseed online, then Case 3 (Path B): Pass-1 scope probe fails with ErrExecutionScopeUnidentified
	// before Activate ever receives Candidate.Identity.
	srvReseedB := flatUserAPITestHub(t, s, true)
	reseedOnline(t, srvReseedB)

	srv4 := flatUserAPITestHub(t, s, true)
	failingProbe := func(context.Context, config.V1RuntimeBrokerInstanceConfig, runtime.Runtime) (brokeridentity.ExecutionScope, error) {
		return brokeridentity.ExecutionScope{}, fmt.Errorf("%w: API server unreachable", brokeridentity.ErrExecutionScopeUnidentified)
	}
	h4 := prepareColocatedK8sHost(t, srv4, globalDir, nil, instOrig, mockK8sFactory(), failingProbe)
	assert.Empty(t, h4.Active())
	assert.Equal(t, brokerhost.StateRefused, h4.Status()[0].State)
	assertRefusedOfflineAndCreate503(t, srv4, "probe-unidentified")

	// Recovery: restoring valid config & reachable scope re-activates the instance,
	// restores online/connected, clears synthetic refusal health, and restores provider status.
	srv5 := flatUserAPITestHub(t, s, true)
	h5 := prepareColocatedK8sHost(t, srv5, globalDir, nil, instOrig, mockK8sFactory(), k8sScopeProber("cluster-uid-1"))
	require.Len(t, h5.Active(), 1)
	recRecovered := doHubUserJSON(t, srv5, http.MethodGet, "/api/v1/runtime-brokers/"+brokerID, nil)
	require.Equal(t, http.StatusOK, recRecovered.Code)
	var recovered struct {
		Status             string                  `json:"status"`
		ConnectionState    string                  `json:"connectionState"`
		HostedByThisServer bool                    `json:"hostedByThisServer"`
		Health             *api.BrokerHealthReport `json:"health"`
	}
	require.NoError(t, json.Unmarshal(recRecovered.Body.Bytes(), &recovered))
	assert.Equal(t, store.BrokerStatusOnline, recovered.Status)
	assert.Equal(t, "connected", recovered.ConnectionState)
	assert.True(t, recovered.HostedByThisServer)
	assert.Nil(t, recovered.Health, "synthetic activation refusal health is cleared once re-activated")

	provRecovered, err := s.GetProjectProvider(ctx, project.ID, brokerID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOnline, provRecovered.Status)
}

// TestColocatedFlatStartupRefusal_LivePeerOwnerNeverMarkedOffline verifies the
// ptone/scion#7626 safety invariant: when another live server process holds the
// local instance lock OR has a fresh heartbeat on a shared Hub DB, neither a
// Pass-2 reasonColocatedInstanceInUse refusal nor a Pass-1 refusal in a second
// process may mark the live owner's RuntimeBroker or provider rows offline.
func TestColocatedFlatStartupRefusal_LivePeerOwnerNeverMarkedOffline(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	inst := config.V1RuntimeBrokerInstanceConfig{
		Key:  "k8s-shared",
		Name: "k8s-shared",
		RuntimeTarget: &config.V1RuntimeTargetConfig{
			Type:      "kubernetes",
			Namespace: "scion-agents",
		},
	}

	dir1 := t.TempDir()
	locks1 := &colocatedFlatLocks{}
	t.Cleanup(func() { locks1.Stop(ctx) })

	srv1 := flatUserAPITestHub(t, s, true)
	h1 := prepareColocatedK8sHost(t, srv1, dir1, locks1, inst, mockK8sFactory(), k8sScopeProber("cluster-uid-1"))
	require.Len(t, h1.Active(), 1)
	brokerID := h1.Active()[0].Context.Identity.RuntimeBrokerID
	require.NoError(t, s.ClaimRuntimeBrokerConnection(ctx, brokerID, srv1.InstanceID(), "sess-live"))
	liveRow, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	liveRow.Health = &api.BrokerHealthReport{Status: api.BrokerHealthHealthy, Checks: map[string]string{"runtime": api.BrokerHealthAvailable}}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, liveRow))

	assertStillOnline := func(t *testing.T, label string) {
		t.Helper()
		got, err := s.GetRuntimeBroker(ctx, brokerID)
		require.NoError(t, err)
		assert.Equal(t, store.BrokerStatusOnline, got.Status, label)
		assert.Equal(t, "connected", got.ConnectionState, label)
		require.NotNil(t, got.Health, label)
		assert.Equal(t, api.BrokerHealthHealthy, got.Health.Status, label)
	}

	// 1. Same globalDir while locks1 is held: both Pass-2 (instance_in_use) and
	// Pass-1 (probe failure) in a second process must leave srv1's row online.
	srv2 := flatUserAPITestHub(t, s, true)
	locks2 := &colocatedFlatLocks{}
	t.Cleanup(func() { locks2.Stop(ctx) })
	h2 := prepareColocatedK8sHost(t, srv2, dir1, locks2, inst, mockK8sFactory(), k8sScopeProber("cluster-uid-1"))
	assert.Equal(t, reasonColocatedInstanceInUse, h2.Status()[0].Reason)
	assertStillOnline(t, "same-dir lock held (Pass 2)")

	srv3 := flatUserAPITestHub(t, s, true)
	h3 := prepareColocatedK8sHost(t, srv3, dir1, locks2, inst,
		func(context.Context, config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
			return nil, errors.New("broken kubeconfig in contender")
		},
		k8sScopeProber("cluster-uid-1"))
	assert.Equal(t, brokerhost.StateRefused, h3.Status()[0].State)
	assertStillOnline(t, "same-dir lock held (Pass 1)")

	// 2. Separate globalDir sharing the Hub DB with copied identity.json while
	// srv1's heartbeat is fresh: both Pass-2 and Pass-1 refusals leave srv1 online.
	dir2 := t.TempDir()
	idBytes, err := os.ReadFile(filepath.Join(brokeridentity.InstanceDir(dir1, inst.Key), brokeridentity.IdentityFileName))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(brokeridentity.InstanceDir(dir2, inst.Key), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(brokeridentity.InstanceDir(dir2, inst.Key), brokeridentity.IdentityFileName), idBytes, 0o600))

	srv4 := flatUserAPITestHub(t, s, true)
	h4 := prepareColocatedK8sHost(t, srv4, dir2, nil, inst, mockK8sFactory(), k8sScopeProber("cluster-uid-1"))
	assert.Equal(t, reasonColocatedInstanceInUse, h4.Status()[0].Reason)
	assertStillOnline(t, "shared-DB foreign heartbeat fresh (Pass 2)")

	srv5 := flatUserAPITestHub(t, s, true)
	h5 := prepareColocatedK8sHost(t, srv5, dir2, nil, inst,
		mockK8sFactory(),
		func(context.Context, config.V1RuntimeBrokerInstanceConfig, runtime.Runtime) (brokeridentity.ExecutionScope, error) {
			return brokeridentity.ExecutionScope{}, brokeridentity.ErrExecutionScopeUnidentified
		})
	assert.Equal(t, brokerhost.StateRefused, h5.Status()[0].State)
	assertStillOnline(t, "shared-DB foreign heartbeat fresh (Pass 1)")
}
