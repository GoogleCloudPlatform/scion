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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Shared execution scopes (ptone/scion#3274): instances on one
// execution scope are hosted together; unresolved ownership on a scope
// refuses every instance of that scope before any activates.

func sharedScopeIdentity(t *testing.T, globalDir, key, daemonID string) *brokeridentity.Identity {
	t.Helper()
	id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(globalDir, key), key, "docker",
		brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: daemonID}}, nil)
	require.NoError(t, err)
	return id
}

func sharedScopeIdentityFor(t *testing.T, globalDir, key string, tc sharedScopeCase) *brokeridentity.Identity {
	t.Helper()
	id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(globalDir, key), key, tc.target.Type, tc.scope, nil)
	require.NoError(t, err)
	return id
}

func serverOf(t *testing.T, h *brokerhost.Host, key string) *runtimebroker.Server {
	t.Helper()
	for _, a := range h.Active() {
		if a.Context.Instance.Key == key {
			return a.Server
		}
	}
	t.Fatalf("instance %s is not active", key)
	return nil
}

func listedOn(t *testing.T, srv *runtimebroker.Server) []string {
	t.Helper()
	ids, err := listOn(srv)
	require.NoError(t, err)
	return ids
}

// listOn lists the container IDs the instance's server reports. It makes no
// test assertion, so it is safe to call from any goroutine.
func listOn(srv *runtimebroker.Server) ([]string, error) {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil))
	if rec.Code != http.StatusOK {
		return nil, fmt.Errorf("list: status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Agents []api.AgentInfo `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return nil, err
	}
	var ids []string
	for _, a := range resp.Agents {
		ids = append(ids, a.ContainerID)
	}
	sort.Strings(ids)
	return ids, nil
}

// sharedScopeCase is one kind of shared execution scope.
type sharedScopeCase struct {
	name   string
	target *config.V1RuntimeTargetConfig
	scope  brokeridentity.ExecutionScope
}

func sharedScopeCases() []sharedScopeCase {
	return []sharedScopeCase{
		{"docker daemon", &config.V1RuntimeTargetConfig{Type: "docker"},
			brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "shared-daemon"}}},
		{"kubernetes namespace", &config.V1RuntimeTargetConfig{Type: "kubernetes", Namespace: "agents"},
			brokeridentity.ExecutionScope{Type: "kubernetes", Kubernetes: &brokeridentity.KubernetesScope{ClusterUID: "uid-shared", Namespace: "agents"}}},
	}
}

// TestFlatSharedScope_ConcurrentOperationsStayPartitioned: two instances on
// one execution scope (one Docker daemon; one Kubernetes cluster and
// namespace) both activate through the production preflight; each lists
// only its own agents, and concurrent deletes and lists from both instances
// act only on each instance's own objects (one instance's delete of the
// other's agent is 404 and touches nothing). After the host restarts on the
// same scope, both instances keep their identities, and their lists stay
// partitioned.
func TestFlatSharedScope_ConcurrentOperationsStayPartitioned(t *testing.T) {
	for _, tc := range sharedScopeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			globalDir := t.TempDir()
			idA := sharedScopeIdentityFor(t, globalDir, "inst-a", tc)
			idB := sharedScopeIdentityFor(t, globalDir, "inst-b", tc)
			d := &partitionDaemon{name: tc.target.Type, objects: []api.AgentInfo{
				partitionObject(idA.RuntimeBrokerID, "agent-a1", "worker", "cid-a1"),
				partitionObject(idA.RuntimeBrokerID, "agent-a2", "helper", "cid-a2"),
				partitionObject(idB.RuntimeBrokerID, "agent-b1", "coder", "cid-b1"),
				partitionObject(idB.RuntimeBrokerID, "agent-b2", "tester", "cid-b2"),
			}}
			daemons := map[string]*partitionDaemon{"inst-a": d, "inst-b": d}
			prepare := func() *brokerhost.Host {
				return preparePartitionHostScoped(t, globalDir, daemons, tc.target, func(string) brokeridentity.ExecutionScope { return tc.scope })
			}
			h := prepare()
			for _, st := range h.Status() {
				require.Equal(t, brokerhost.StateActive, st.State, "%s: %s", st.Key, st.Error)
			}
			a, b := serverOf(t, h, "inst-a"), serverOf(t, h, "inst-b")
			assert.Equal(t, []string{"cid-a1", "cid-a2"}, listedOn(t, a))
			assert.Equal(t, []string{"cid-b1", "cid-b2"}, listedOn(t, b))

			del := func(srv *runtimebroker.Server, slug string) int {
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+slug+"?projectId=proj-1", nil))
				return rec.Code
			}
			var wg sync.WaitGroup
			problems := make(chan string, 32)
			for i := 0; i < 4; i++ {
				wg.Add(4)
				go func() { defer wg.Done(); del(a, "worker") }()
				go func() { defer wg.Done(); del(b, "coder") }()
				go func() {
					defer wg.Done()
					if c := del(a, "tester"); c != http.StatusNotFound {
						problems <- fmt.Sprintf("inst-a's delete of inst-b's tester answered %d, want 404", c)
					}
				}()
				go func() {
					defer wg.Done()
					ids, err := listOn(b)
					if err != nil {
						problems <- err.Error()
						return
					}
					for _, id := range ids {
						if id == "cid-a1" || id == "cid-a2" {
							problems <- "inst-b listed inst-a's " + id
						}
					}
				}()
			}
			wg.Wait()
			close(problems)
			for p := range problems {
				t.Error(p)
			}
			d.mu.Lock()
			for _, call := range d.deletes {
				switch call {
				case "cid-a1", "stop:cid-a1", "cid-b1", "stop:cid-b1":
				default:
					t.Errorf("a delete reached %s, which neither delete targeted", call)
				}
			}
			assert.Contains(t, d.deletes, "cid-a1")
			assert.Contains(t, d.deletes, "cid-b1")
			d.mu.Unlock()

			// Restart the two-instance host on the same scope.
			h2 := prepare()
			for _, st := range h2.Status() {
				require.Equal(t, brokerhost.StateActive, st.State, "after restart %s: %s", st.Key, st.Error)
			}
			assert.Equal(t, idA.RuntimeBrokerID, statusOf(h2, "inst-a").RuntimeBrokerID, "inst-a keeps its identity")
			assert.Equal(t, idB.RuntimeBrokerID, statusOf(h2, "inst-b").RuntimeBrokerID, "inst-b keeps its identity")
			// Each instance still lists its own agent that no delete targeted,
			// and nothing of the other instance's.
			for key, want := range map[string]struct{ prefix, kept string }{
				"inst-a": {"cid-a", "cid-a2"}, "inst-b": {"cid-b", "cid-b2"},
			} {
				ids := listedOn(t, serverOf(t, h2, key))
				assert.Contains(t, ids, want.kept, "after restart %s still lists its own %s", key, want.kept)
				for _, id := range ids {
					assert.True(t, strings.HasPrefix(id, want.prefix), "after restart %s lists %s", key, id)
				}
			}
		})
	}
}

// TestFlatSharedScope_UnresolvedHistoricalObjectRefusesTheWholeGroup: an
// unlabeled (historical) agent object on a shared daemon refuses every
// instance on that daemon before any is activated; an instance on another
// daemon activates.
func TestFlatSharedScope_UnresolvedHistoricalObjectRefusesTheWholeGroup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	globalDir := t.TempDir()
	shared := &partitionDaemon{objects: []api.AgentInfo{{Name: "old-agent", ContainerID: "cid-old", Labels: map[string]string{
		"scion.agent": "true", "scion.name": "old-agent", "scion.project_id": "proj-1"}}}}
	other := &partitionDaemon{}
	h := preparePartitionHostOn(t, globalDir,
		map[string]*partitionDaemon{"docker-a": shared, "docker-b": shared, "docker-c": other},
		map[string]string{"docker-c": "other-daemon"})
	st := map[string]brokerhost.InstanceStatus{}
	for _, s := range h.Status() {
		st[s.Key] = s
	}
	for _, k := range []string{"docker-a", "docker-b"} {
		assert.Equal(t, brokerhost.StateRefused, st[k].State, k)
		assert.Equal(t, "ownership_unresolved", st[k].Reason, k)
	}
	assert.Equal(t, brokerhost.StateActive, st["docker-c"].State, "another daemon is another scope")
	shared.mu.Lock()
	assert.Empty(t, shared.deletes, "nothing on the shared daemon was touched")
	shared.mu.Unlock()
}

// TestFlatSharedScope_OneInstancesUnresolvedObjectRefusesItsSiblings: an
// object labelled for instance A with incomplete labels is unresolved for A
// only (B ignores another instance's object), yet it refuses B too: every
// instance on the scope is refused before any activates.
func TestFlatSharedScope_OneInstancesUnresolvedObjectRefusesItsSiblings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	globalDir := t.TempDir()
	idA := sharedScopeIdentity(t, globalDir, "docker-a", "shared-daemon")
	sharedScopeIdentity(t, globalDir, "docker-b", "shared-daemon")
	d := &partitionDaemon{objects: []api.AgentInfo{{Name: "half", ContainerID: "cid-half", Labels: map[string]string{
		"scion.agent": "true", api.LabelRuntimeBrokerID: idA.RuntimeBrokerID}}}}
	h := preparePartitionHostOn(t, globalDir, map[string]*partitionDaemon{"docker-a": d, "docker-b": d}, nil)
	for _, s := range h.Status() {
		assert.Equal(t, brokerhost.StateRefused, s.State, s.Key)
		assert.Equal(t, "ownership_unresolved", s.Reason, s.Key)
	}
}

// TestFlatSharedScope_ExperimentOffRefusesEveryInstance: with a real
// co-located Hub and the flat Runtime Broker experiment off, two new
// instances on one Docker daemon are both refused (experiment_disabled at
// their first registration); neither is built, and the Hub holds no row for
// either.
func TestFlatSharedScope_ExperimentOffRefusesEveryInstance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	s := newTestStore(t)
	srv := flatTestHub(t, s, false)
	globalDir := t.TempDir()
	var built []string
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir: globalDir,
		Instances: []config.V1RuntimeBrokerInstanceConfig{
			{Key: "docker-a", Name: "a", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}},
			{Key: "docker-b", Name: "b", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}},
		},
		Mode: brokerhost.ModeColocated,
		NewRuntime: func(context.Context, config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
			return &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, nil
		},
		ProbeScope: func(context.Context, config.V1RuntimeBrokerInstanceConfig, runtime.Runtime) (brokeridentity.ExecutionScope, error) {
			return brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "shared-daemon"}}, nil
		},
		Activator: &colocatedFlatActivator{hubSrv: srv, endpoint: "http://localhost:9800"},
		BuildServer: func(ic brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			built = append(built, ic.Instance.Key)
			return nil, errors.New("not built in this test")
		},
	})
	require.NoError(t, err)
	require.NoError(t, h.Prepare(ctx))
	for _, key := range []string{"docker-a", "docker-b"} {
		st := statusOf(h, key)
		assert.Equal(t, brokerhost.StateRefused, st.State, key)
		assert.Equal(t, hub.ErrCodeExperimentDisabled, st.Reason, key)
		assert.Contains(t, st.Error, hub.ErrCodeExperimentDisabled, key)
		require.NotEmpty(t, st.RuntimeBrokerID, "%s: identity loaded in pass 1", key)
		_, getErr := s.GetRuntimeBroker(ctx, st.RuntimeBrokerID)
		assert.ErrorIs(t, getErr, store.ErrNotFound, "%s: no Runtime Broker row", key)
	}
	assert.Empty(t, built, "no instance is built")
	assert.Empty(t, h.Active())
}

// TestFlatSharedScope_EachInstanceHeartbeatsAsItself: the production
// per-instance server configuration of two instances on one execution scope
// gives each its own Runtime Broker ID and its own heartbeat and control
// channel (they are never merged by scope).
func TestFlatSharedScope_EachInstanceHeartbeatsAsItself(t *testing.T) {
	scope := brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "shared-daemon"}}
	sh := flatServerShared{cfg: &config.GlobalConfig{}, mode: brokerhost.ModeRemote, workspaceLocks: runtimebroker.NewWorkspaceLocks()}
	seen := map[string]bool{}
	for _, key := range []string{"docker-a", "docker-b"} {
		id := &brokeridentity.Identity{InstanceKey: key, RuntimeBrokerID: "rb-" + key, ExecutionScope: scope}
		inst := config.V1RuntimeBrokerInstanceConfig{Key: key, Name: key, RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}}
		act := &brokerhost.Activation{RemoteCredentials: []brokercredentials.BrokerCredentials{{Name: "hub", BrokerID: "rb-" + key, SecretKey: "c2VjcmV0", HubEndpoint: "https://hub.example"}}}
		c := flatInstanceServerConfig(sh, brokerhost.InstanceContext{Instance: inst, Identity: id, Activation: act})
		assert.Equal(t, "rb-"+key, c.BrokerID, key)
		assert.True(t, c.HeartbeatEnabled, "%s heartbeats", key)
		assert.True(t, c.ControlChannelEnabled, "%s has its own control channel", key)
		require.NotNil(t, c.FlatInstance, key)
		require.Len(t, c.FlatInstance.RemoteCredentials, 1, key)
		assert.Equal(t, "rb-"+key, c.FlatInstance.RemoteCredentials[0].BrokerID, "%s heartbeats with its own credentials", key)
		assert.False(t, seen[c.BrokerID], "one Runtime Broker ID per instance")
		seen[c.BrokerID] = true
	}
}
