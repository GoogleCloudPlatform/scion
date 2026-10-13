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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// Partition validation (ptone/scion#3274): restart and singleton replacement on one
// daemon, through the production pass-1 preflight and ownership keys.

type partitionDaemon struct {
	mu      sync.Mutex
	objects []api.AgentInfo
	deletes []string
	// name is the runtime name ("docker" when empty).
	name string
}

func (d *partitionDaemon) runtime() runtime.Runtime {
	name := d.name
	if name == "" {
		name = "docker"
	}
	return &runtime.MockRuntime{
		NameFunc: func() string { return name },
		ListFunc: func(_ context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			var out []api.AgentInfo
			for _, o := range d.objects {
				ok := true
				for k, v := range filter {
					ok = ok && o.Labels[k] == v
				}
				if ok {
					out = append(out, o)
				}
			}
			return out, nil
		},
		DeleteFunc: func(_ context.Context, ref runtime.RunRef) error {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.deletes = append(d.deletes, ref.ID)
			return nil
		},
		StopFunc: func(_ context.Context, ref runtime.RunRef) error {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.deletes = append(d.deletes, "stop:"+ref.ID)
			return nil
		},
	}
}

func partitionObject(owner, agentID, slug, id string) api.AgentInfo {
	return api.AgentInfo{ID: id, ContainerID: id, Name: slug, ProjectID: "proj-1", Phase: "running", Labels: map[string]string{
		"scion.agent": "true", api.LabelRuntimeBrokerID: owner, "scion.project_id": "proj-1",
		"agent_id": agentID, "scion.name": slug, api.LabelRunID: "run-" + id}}
}

// preparePartitionHost prepares a host for one instance on the shared
// daemon with the production preflight, ownership keys and a real
// owner-filtered manager.
func preparePartitionHost(t *testing.T, globalDir string, d *partitionDaemon, key string) *brokerhost.Host {
	t.Helper()
	return preparePartitionHostOn(t, globalDir, map[string]*partitionDaemon{key: d}, nil)
}

// preparePartitionHostOn prepares a host with one instance per key, each
// on its daemon, with the production preflight and ownership keys. scopes
// gives an instance's daemon ID (default "shared-daemon"); instances with
// one daemon ID share an execution scope.
func preparePartitionHostOn(t *testing.T, globalDir string, daemons map[string]*partitionDaemon, scopes map[string]string) *brokerhost.Host {
	t.Helper()
	return preparePartitionHostScoped(t, globalDir, daemons, &config.V1RuntimeTargetConfig{Type: "docker"},
		func(key string) brokeridentity.ExecutionScope {
			id := scopes[key]
			if id == "" {
				id = "shared-daemon"
			}
			return brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: id}}
		})
}

// preparePartitionHostScoped prepares a host whose instances (one per
// daemons key) have the runtime target target and the execution scope
// scopeOf(key), with the production ownership preflight and keys.
func preparePartitionHostScoped(t *testing.T, globalDir string, daemons map[string]*partitionDaemon, target *config.V1RuntimeTargetConfig,
	scopeOf func(key string) brokeridentity.ExecutionScope) *brokerhost.Host {
	t.Helper()
	var instances []config.V1RuntimeBrokerInstanceConfig
	for key := range daemons {
		tc := *target
		instances = append(instances, config.V1RuntimeBrokerInstanceConfig{Key: key, Name: key, RuntimeTarget: &tc})
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].Key < instances[j].Key })
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir: globalDir,
		Instances: instances,
		Mode:      brokerhost.ModeColocated,
		NewRuntime: func(_ context.Context, in config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
			return daemons[in.Key].runtime(), nil
		},
		ProbeScope: func(_ context.Context, in config.V1RuntimeBrokerInstanceConfig, _ runtime.Runtime) (brokeridentity.ExecutionScope, error) {
			return scopeOf(in.Key), nil
		},
		Activator: &recordingFlatActivator{},
		BuildServer: func(ic brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			cfg := runtimebroker.DefaultServerConfig()
			cfg.BrokerID = ic.Identity.RuntimeBrokerID
			cfg.HubEnabled = true
			cfg.HubEndpoint = "http://127.0.0.1:1"
			cfg.BrokerAuthStrictMode = false
			cfg.InMemoryCredentials = &brokercredentials.BrokerCredentials{BrokerID: ic.Identity.RuntimeBrokerID, SecretKey: "c2VjcmV0", HubEndpoint: cfg.HubEndpoint}
			cfg.FlatInstance = &runtimebroker.FlatInstanceConfig{Identity: ic.Identity, Instance: ic.Instance, HubInProcess: true,
				ConflictingOwnershipKeys: ic.ConflictingKeys}
			return runtimebroker.New(cfg, ic.Manager, ic.Runtime), nil
		},
		OwnershipPreflight: flatOwnershipPreflight,
		OwnershipKeys:      flatOwnershipKeys,
	})
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))
	return h
}

func listedIDs(t *testing.T, h *brokerhost.Host) []string {
	t.Helper()
	active := h.Active()
	require.Len(t, active, 1)
	rec := httptest.NewRecorder()
	active[0].Server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Agents []api.AgentInfo `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	var ids []string
	for _, a := range resp.Agents {
		ids = append(ids, a.ContainerID)
	}
	return ids
}

func TestFlatPartition_RestartAndReplacementThroughProductionPreflight(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	globalDir := t.TempDir()
	scope := brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "shared-daemon"}}
	idA, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(globalDir, "docker-a"), "docker-a", "docker", scope, nil)
	require.NoError(t, err)
	d := &partitionDaemon{objects: []api.AgentInfo{
		partitionObject(idA.RuntimeBrokerID, "agent-a1", "worker", "cid-a1"),
		partitionObject("rb-another-instance", "agent-b1", "helper", "cid-b1"),
	}}

	// Start: A activates; the preflight records A's object only.
	h := preparePartitionHost(t, globalDir, d, "docker-a")
	require.Equal(t, brokerhost.StateActive, h.Status()[0].State, h.Status()[0].Error)
	assert.Equal(t, []string{"cid-a1"}, listedIDs(t, h))
	dirA, err := runtimebroker.DefaultStateDir(idA.RuntimeBrokerID)
	require.NoError(t, err)
	storeA := runtimebroker.NewOwnershipStore(dirA, idA.RuntimeBrokerID)
	recA, ok, err := storeA.Get("proj-1", "agent-a1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, recA.OwnsUID("cid-a1"))
	_, ok, _ = storeA.Get("proj-1", "agent-b1")
	assert.False(t, ok, "another instance's object is never recorded")

	// Restart: same identity, same records, same partition.
	h = preparePartitionHost(t, globalDir, d, "docker-a")
	require.Equal(t, idA.RuntimeBrokerID, h.Status()[0].RuntimeBrokerID)
	assert.Equal(t, []string{"cid-a1"}, listedIDs(t, h))

	// Replacement: C replaces A on the same daemon. A's and the other
	// instance's objects are theirs: C activates, lists and records none,
	// and its delete of A's agent touches nothing.
	h = preparePartitionHost(t, globalDir, d, "docker-c")
	st := h.Status()[0]
	require.Equal(t, brokerhost.StateActive, st.State, st.Error)
	assert.Empty(t, listedIDs(t, h))
	dirC, err := runtimebroker.DefaultStateDir(st.RuntimeBrokerID)
	require.NoError(t, err)
	recs, err := runtimebroker.NewOwnershipStore(dirC, st.RuntimeBrokerID).List()
	require.NoError(t, err)
	assert.Empty(t, recs, "C adopts nothing")
	rec := httptest.NewRecorder()
	h.Active()[0].Server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/worker?projectId=proj-1&deleteFiles=true", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	d.mu.Lock()
	assert.Empty(t, d.deletes, "nothing on the daemon was stopped or deleted")
	d.mu.Unlock()
}
