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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerregistration"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

func TestFlatHostMode_SimulatedRemoteTakesRemotePath(t *testing.T) {
	origHub, origSim := enableHub, simulateRemoteBroker
	t.Cleanup(func() { enableHub, simulateRemoteBroker = origHub, origSim })
	s := newTestStore(t)
	cfg := &config.GlobalConfig{RuntimeBroker: config.RuntimeBrokerConfig{Enabled: true}}

	enableHub, simulateRemoteBroker = true, false
	assert.Equal(t, brokerhost.ModeColocated, flatHostMode(cfg, s))
	enableHub, simulateRemoteBroker = true, true
	assert.Equal(t, brokerhost.ModeRemote, flatHostMode(cfg, s), "--simulate-remote-broker takes the remote path even with a Hub in the process")
	enableHub, simulateRemoteBroker = false, false
	assert.Equal(t, brokerhost.ModeRemote, flatHostMode(cfg, s))
}

// selfReadHub answers GET /api/v1/runtime-brokers/{id} with the row it is
// told to return and records the requests.
type selfReadHub struct {
	*httptest.Server
	mu       sync.Mutex
	target   *api.RuntimeTargetDescriptor
	requests int
}

func newSelfReadHub(t *testing.T, target *api.RuntimeTargetDescriptor) *selfReadHub {
	t.Helper()
	h := &selfReadHub{target: target}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := strings.CutPrefix(r.URL.Path, "/api/v1/runtime-brokers/")
		if !ok || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		h.mu.Lock()
		h.requests++
		row := map[string]any{"id": id, "name": "example-remote"}
		if h.target != nil {
			row["runtimeTarget"] = h.target
		}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(row)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *selfReadHub) setTarget(target *api.RuntimeTargetDescriptor) {
	h.mu.Lock()
	h.target = target
	h.mu.Unlock()
}

func fakeScopeProber(daemonID string) brokerhost.ScopeProber {
	return func(context.Context, config.V1RuntimeBrokerInstanceConfig, runtime.Runtime) (brokeridentity.ExecutionScope, error) {
		return brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: daemonID}}, nil
	}
}

// newRemoteTestHost builds a remote-mode host for one instance with the
// production remote activator.
func newRemoteTestHost(t *testing.T, globalDir string, inst config.V1RuntimeBrokerInstanceConfig) (*brokerhost.Host, *[]string) {
	t.Helper()
	var built []string
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir:  globalDir,
		Instances:  []config.V1RuntimeBrokerInstanceConfig{inst},
		Mode:       brokerhost.ModeRemote,
		NewRuntime: newFlatInstanceRuntime,
		ProbeScope: fakeScopeProber("daemon-1"),
		Activator:  &remoteFlatActivator{globalDir: globalDir},
		BuildServer: func(ic brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			built = append(built, ic.Instance.Key)
			cfg := runtimebroker.DefaultServerConfig()
			cfg.BrokerID = ic.Identity.RuntimeBrokerID
			cfg.StateDir = filepath.Join(globalDir, "state", ic.Identity.RuntimeBrokerID)
			cfg.FlatInstance = &runtimebroker.FlatInstanceConfig{Identity: ic.Identity, Instance: ic.Instance,
				RemoteCredentials: ic.Activation.RemoteCredentials}
			return runtimebroker.New(cfg, ic.Manager, ic.Runtime, config.RuntimeBuildRecord{}), nil
		},
	})
	require.NoError(t, err)
	return h, &built
}

// TestFlatRemoteActivation_BindingCheckedOnEveryStart: a remote (or
// simulated-remote) instance activates only with its instance-scoped
// credentials after the self-read binding check, on every start; saved
// credentials never skip it, and legacy credentials are never used.
func TestFlatRemoteActivation_BindingCheckedOnEveryStart(t *testing.T) {
	ctx := context.Background()
	globalDir := t.TempDir()
	inst := config.V1RuntimeBrokerInstanceConfig{Key: "remote-docker", Name: "example-remote",
		RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}}
	id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(globalDir, inst.Key), inst.Key, "docker",
		brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "daemon-1"}}, nil)
	require.NoError(t, err)
	hub := newSelfReadHub(t, &id.RuntimeTarget)
	secret := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

	// Legacy credentials for this Runtime Broker ID are present in both
	// legacy locations; with no instance credentials the instance is not
	// registered.
	legacy := &brokercredentials.BrokerCredentials{Name: "hub", BrokerID: id.RuntimeBrokerID, SecretKey: secret, HubEndpoint: hub.URL}
	require.NoError(t, brokercredentials.NewMultiStore(filepath.Join(globalDir, "hub-credentials")).Save(legacy))
	legacyJSON, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, brokercredentials.DefaultFileName), legacyJSON, brokercredentials.FileMode))
	h, built := newRemoteTestHost(t, globalDir, inst)
	require.NoError(t, h.Prepare(ctx))
	st := h.Status()[0]
	assert.Equal(t, brokerhost.StateRefused, st.State)
	assert.Contains(t, st.Error, api.ErrCodeFlatRuntimeBrokerNotRegistered)
	assert.Empty(t, *built)
	assert.Zero(t, hub.requests, "no Hub request without instance credentials")

	// Instance credentials saved by the registration: the binding is read
	// and checked at each start.
	require.NoError(t, brokercredentials.NewMultiStore(brokerregistration.InstanceCredentialsDir(globalDir, inst.Key)).Save(
		&brokercredentials.BrokerCredentials{Name: "hub", BrokerID: id.RuntimeBrokerID, SecretKey: secret, HubEndpoint: hub.URL, AuthMode: brokercredentials.AuthModeHMAC}))
	h, built = newRemoteTestHost(t, globalDir, inst)
	require.NoError(t, h.Prepare(ctx))
	require.Equal(t, brokerhost.StateActive, h.Status()[0].State)
	assert.Equal(t, []string{inst.Key}, *built)
	active := h.Active()
	require.Len(t, active, 1)
	require.Len(t, active[0].Context.Activation.RemoteCredentials, 1)
	assert.Equal(t, 1, hub.requests)

	// The next start against a Hub that no longer acknowledges the binding
	// is refused, although the credentials are saved.
	hub.setTarget(nil)
	h, built = newRemoteTestHost(t, globalDir, inst)
	require.NoError(t, h.Prepare(ctx))
	st = h.Status()[0]
	assert.Equal(t, brokerhost.StateRefused, st.State)
	assert.Contains(t, st.Error, api.ErrCodeRuntimeTargetAckMissing)
	assert.Empty(t, *built, "not activated: no Runtime Broker server")
	assert.Equal(t, 2, hub.requests, "every start reads the binding")

	// A different binding is refused the same way.
	hub.setTarget(&api.RuntimeTargetDescriptor{ID: "another-target", Type: "docker"})
	h, _ = newRemoteTestHost(t, globalDir, inst)
	require.NoError(t, h.Prepare(ctx))
	assert.Contains(t, h.Status()[0].Error, api.ErrCodeRuntimeTargetBindingConflict)
}

// registrationHub is a capable fake Hub for 'broker register --instance'.
type registrationHub struct {
	*httptest.Server
	mu     sync.Mutex
	bodies map[string]map[string]any
	auth   map[string]string
}

func newRegistrationHub(t *testing.T) *registrationHub {
	t.Helper()
	h := &registrationHub{bodies: map[string]map[string]any{}, auth: map[string]string{}}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.bodies[r.URL.Path] = body
		h.auth[r.URL.Path] = r.Header.Get("Authorization")
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/brokers":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"brokerId": body["brokerId"], "joinToken": "scion_join_t",
				"expiresAt": "2026-10-09T05:00:00Z", "runtimeTarget": body["runtimeTarget"]})
		case "/api/v1/brokers/join":
			_ = json.NewEncoder(w).Encode(map[string]any{"brokerId": body["brokerId"],
				"secretKey":   base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
				"hubEndpoint": "http://" + r.Host, "runtimeTarget": body["runtimeTarget"]})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func setupRegisterInstanceTest(t *testing.T, hubURL string) string {
	t.Helper()
	isolateJoinEnv(t)
	_, globalDir := brokerTestHome(t)
	t.Setenv("SCION_HUB_ENDPOINT", hubURL)
	t.Setenv("SCION_DEV_TOKEN", joinTestDevToken)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(
		"schema_version: \"1\"\nserver:\n  broker:\n    enabled: true\n    instances:\n      - key: remote-docker\n        name: example-remote\n        runtime_target:\n          type: docker\n          display_name: Remote Docker\n"), 0o644))
	origProber, origInstance := flatScopeProber, brokerRegisterInstance
	t.Cleanup(func() { flatScopeProber, brokerRegisterInstance = origProber, origInstance })
	flatScopeProber = fakeScopeProber("daemon-1")
	brokerRegisterCmd.SetContext(context.Background())
	return globalDir
}

func TestBrokerRegisterInstance_RegistersAndSavesInstanceCredentials(t *testing.T) {
	hub := newRegistrationHub(t)
	globalDir := setupRegisterInstanceTest(t, hub.URL)
	brokerRegisterInstance = "remote-docker"

	require.NoError(t, runBrokerRegister(brokerRegisterCmd, nil))

	id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(globalDir, "remote-docker"), "remote-docker", "docker",
		brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "daemon-1"}}, nil)
	require.NoError(t, err)
	list, err := brokerregistration.LoadInstanceCredentials(globalDir, id)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, id.RuntimeBrokerID, list[0].BrokerID)
	_, err = os.Stat(filepath.Join(globalDir, "hub-credentials"))
	assert.True(t, errors.Is(err, os.ErrNotExist), "legacy credentials are not written")

	create := hub.bodies["/api/v1/brokers"]
	assert.Equal(t, id.RuntimeBrokerID, create["brokerId"])
	assert.Equal(t, "example-remote", create["name"])
	assert.Equal(t, map[string]any{"id": id.RuntimeTarget.ID, "type": "docker", "displayName": "Remote Docker"}, create["runtimeTarget"])
	join := hub.bodies["/api/v1/brokers/join"]
	assert.Contains(t, join["capabilities"], "asyncLaunch", "static capabilities are sent at registration")
	assert.NotContains(t, join, "profiles")
	assert.Equal(t, "Bearer "+joinTestDevToken, hub.auth["/api/v1/brokers"], "the registration uses the user credential")
}

func TestBrokerRegisterInstance_UnknownKeyRefused(t *testing.T) {
	hub := newRegistrationHub(t)
	setupRegisterInstanceTest(t, hub.URL)
	brokerRegisterInstance = "not-configured"

	err := runBrokerRegister(brokerRegisterCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no Runtime Broker instance with key "not-configured"`)
	assert.Empty(t, hub.bodies, "nothing is sent to the Hub")
}

// colocatedTestHost prepares a co-located host over hubSrv for instances,
// each on its own fake Docker daemon.
func colocatedTestHost(t *testing.T, hubSrv *hub.Server, globalDir string, insts ...config.V1RuntimeBrokerInstanceConfig) *brokerhost.Host {
	t.Helper()
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir:  globalDir,
		Instances:  insts,
		Mode:       brokerhost.ModeColocated,
		NewRuntime: newFlatInstanceRuntime,
		ProbeScope: func(_ context.Context, inst config.V1RuntimeBrokerInstanceConfig, _ runtime.Runtime) (brokeridentity.ExecutionScope, error) {
			return brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "daemon-" + inst.Key}}, nil
		},
		Activator: &colocatedFlatActivator{hubSrv: hubSrv, endpoint: "http://localhost:9800/"},
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

// TestFlatHost_RegisteredEndpointIsPrefixedAndStable: every co-located flat
// instance, a singleton included, registers its instance-qualified
// endpoint, and it stays the same when siblings are added and removed.
func TestFlatHost_RegisteredEndpointIsPrefixedAndStable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	srv := flatTestHub(t, s, true)
	globalDir := t.TempDir()
	a := config.V1RuntimeBrokerInstanceConfig{Key: "docker-a", Name: "a", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}}
	b := config.V1RuntimeBrokerInstanceConfig{Key: "docker-b", Name: "b", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}}

	endpointOf := func(h *brokerhost.Host, key string) string {
		t.Helper()
		for _, st := range h.Status() {
			if st.Key == key {
				require.Equal(t, brokerhost.StateActive, st.State, st.Error)
				row, err := s.GetRuntimeBroker(ctx, st.RuntimeBrokerID)
				require.NoError(t, err)
				return row.Endpoint
			}
		}
		t.Fatalf("no instance %s", key)
		return ""
	}

	h := colocatedTestHost(t, srv, globalDir, a)
	idA := h.Status()[0].RuntimeBrokerID
	want := "http://localhost:9800/instances/" + idA
	assert.Equal(t, want, endpointOf(h, "docker-a"), "a singleton registers its prefixed endpoint")

	h = colocatedTestHost(t, srv, globalDir, a, b)
	assert.Equal(t, want, endpointOf(h, "docker-a"), "unchanged when a sibling is added")
	assert.Equal(t, "http://localhost:9800/instances/"+statusByKeyCmd(h)["docker-b"].RuntimeBrokerID, endpointOf(h, "docker-b"))

	h = colocatedTestHost(t, srv, globalDir, a)
	assert.Equal(t, want, endpointOf(h, "docker-a"), "unchanged when the sibling is removed")
}

func statusByKeyCmd(h *brokerhost.Host) map[string]brokerhost.InstanceStatus {
	out := map[string]brokerhost.InstanceStatus{}
	for _, st := range h.Status() {
		out[st.Key] = st
	}
	return out
}

// TestFlatHost_HubTransportReachesPrefixedInstance proves the whole HTTP
// chain: the Hub's authenticated broker client appends its routes to each
// instance's registered (prefixed) endpoint and signs the full path with
// that instance's secret; the host routes to exactly that instance, which
// verifies the signature once and answers as it does over the control
// channel (same status and body for an existing-agent error).
func TestFlatHost_HubTransportReachesPrefixedInstance(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	srv := flatAuthTestHub(t, s)
	globalDir := t.TempDir()
	insts := []config.V1RuntimeBrokerInstanceConfig{
		{Key: "docker-a", Name: "a", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}},
		{Key: "docker-b", Name: "b", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}},
	}
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir: globalDir,
		Instances: insts,
		Mode:      brokerhost.ModeColocated,
		NewRuntime: func(context.Context, config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) { // a stand-in daemon, so the answer is deterministic
			return &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, nil
		},
		ProbeScope: func(_ context.Context, inst config.V1RuntimeBrokerInstanceConfig, _ runtime.Runtime) (brokeridentity.ExecutionScope, error) {
			return brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "daemon-" + inst.Key}}, nil
		},
		Activator: &colocatedFlatActivator{hubSrv: srv, endpoint: "http://localhost:9800"},
		BuildServer: func(ic brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			cfg := runtimebroker.DefaultServerConfig()
			cfg.BrokerID = ic.Identity.RuntimeBrokerID
			cfg.StateDir = filepath.Join(globalDir, "state", ic.Identity.RuntimeBrokerID)
			cfg.HubEnabled = true
			cfg.HubEndpoint = "http://127.0.0.1:1"
			cfg.InMemoryCredentials = ic.Activation.InMemoryCredentials
			cfg.FlatInstance = &runtimebroker.FlatInstanceConfig{Identity: ic.Identity, Instance: ic.Instance, HubInProcess: true}
			return runtimebroker.New(cfg, ic.Manager, ic.Runtime, config.RuntimeBuildRecord{}), nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, h.Prepare(ctx))
	require.Len(t, h.Active(), 2)
	listener := httptest.NewServer(h.Handler())
	t.Cleanup(listener.Close)
	client := hub.NewAuthenticatedBrokerClient(s, false)

	for _, a := range h.Active() {
		id := a.Context.Identity.RuntimeBrokerID
		row, err := s.GetRuntimeBroker(ctx, id)
		require.NoError(t, err)
		require.True(t, strings.HasSuffix(row.Endpoint, "/instances/"+id), row.Endpoint)
		endpoint := listener.URL + "/instances/" + id

		_, httpErr := client.GetAgentLogs(ctx, id, endpoint, "no-such-agent", "", 0)
		require.Error(t, httpErr)

		// The same request as the control channel delivers it: unprefixed,
		// signed by the Hub for that path, to the instance's own handler.
		secret, err := s.GetBrokerSecret(ctx, id)
		require.NoError(t, err)
		ccReq := httptest.NewRequest(http.MethodGet, "/api/v1/agents/no-such-agent/logs", nil)
		require.NoError(t, (&apiclient.HMACAuth{BrokerID: id, SecretKey: secret.SecretKey}).ApplyAuth(ccReq))
		rec := httptest.NewRecorder()
		a.Server.Handler().ServeHTTP(rec, ccReq)
		assert.Contains(t, httpErr.Error(), strconvItoa(rec.Code), "same status over HTTP and the control channel")
		assert.Contains(t, httpErr.Error(), strings.TrimSpace(rec.Body.String()), "same body over HTTP and the control channel")
		assert.NotContains(t, httpErr.Error(), "401", "signature over the prefixed path verified")
	}

	// One instance's identity on the other's route: refused, not executed.
	act := h.Active()
	idA, idB := act[0].Context.Identity.RuntimeBrokerID, act[1].Context.Identity.RuntimeBrokerID
	_, err = client.GetAgentLogs(ctx, idA, listener.URL+"/instances/"+idB, "no-such-agent", "", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func strconvItoa(n int) string { return fmt.Sprintf("%d", n) }

// flatAuthTestHub is a Hub with broker HMAC authentication (the default
// config) and hub.flat_runtime_brokers on.
func flatAuthTestHub(t *testing.T, s store.Store) *hub.Server {
	t.Helper()
	ctx := context.Background()
	_, err := s.UpsertHubSetting(ctx, "experiments",
		json.RawMessage(fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.FlatRuntimeBrokers)), "test", -1, "managed")
	require.NoError(t, err)
	hubCfg := hub.DefaultServerConfig()
	hubCfg.DisableCloudLogQuery = true
	require.True(t, hubCfg.BrokerAuthConfig.Enabled, "the default Hub config signs broker requests")
	srv, err := hub.New(hubCfg, s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	ops := hub.NewOperationalSettings(s, koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	return srv
}

// ccTestRig is two co-located flat instances connected to a Hub over their
// control channels.
type ccTestRig struct {
	s      store.Store
	mgr    *hub.ControlChannelManager
	active []brokerhost.ActiveInstance
	build  func(ic brokerhost.InstanceContext) *runtimebroker.Server
	ctx    context.Context
	hubURL string
	ids    [2]string
	// stateDirs maps each instance's Runtime Broker ID to its state root.
	stateDirs map[string]string
}

func newCCTestRig(t *testing.T) *ccTestRig {
	t.Helper()
	isolateAgentFiles(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := newTestStore(t)
	srv := flatAuthTestHub(t, s)
	hubTS := httptest.NewServer(srv.Handler())
	t.Cleanup(hubTS.Close)
	globalDir := t.TempDir()
	stateDirs := map[string]string{}
	build := func(ic brokerhost.InstanceContext) *runtimebroker.Server {
		cfg := runtimebroker.DefaultServerConfig()
		cfg.BrokerID = ic.Identity.RuntimeBrokerID
		cfg.StateDir = filepath.Join(globalDir, "state", ic.Identity.RuntimeBrokerID)
		stateDirs[ic.Identity.RuntimeBrokerID] = cfg.StateDir
		cfg.HubEnabled = true
		cfg.HubEndpoint = hubTS.URL
		cfg.ControlChannelEnabled = true
		cfg.InMemoryCredentials = ic.Activation.InMemoryCredentials
		cfg.FlatInstance = &runtimebroker.FlatInstanceConfig{Identity: ic.Identity, Instance: ic.Instance, HubInProcess: true}
		return runtimebroker.New(cfg, ic.Manager, ic.Runtime, config.RuntimeBuildRecord{})
	}
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
		ProbeScope: func(_ context.Context, inst config.V1RuntimeBrokerInstanceConfig, _ runtime.Runtime) (brokeridentity.ExecutionScope, error) {
			return brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "daemon-" + inst.Key}}, nil
		},
		Activator:   &colocatedFlatActivator{hubSrv: srv, endpoint: "http://localhost:9800", hubEndpoint: hubTS.URL},
		BuildServer: func(ic brokerhost.InstanceContext) (*runtimebroker.Server, error) { return build(ic), nil },
	})
	require.NoError(t, err)
	require.NoError(t, h.Prepare(ctx))
	active := h.Active()
	require.Len(t, active, 2)
	for _, a := range active {
		require.NoError(t, a.Server.StartServices(ctx))
	}
	r := &ccTestRig{s: s, mgr: srv.GetControlChannelManager(), active: active, build: build, ctx: ctx, hubURL: hubTS.URL, stateDirs: stateDirs,
		ids: [2]string{active[0].Context.Identity.RuntimeBrokerID, active[1].Context.Identity.RuntimeBrokerID}}
	require.Eventually(t, func() bool { return r.mgr.IsConnected(r.ids[0]) && r.mgr.IsConnected(r.ids[1]) }, 10*time.Second, 50*time.Millisecond)
	return r
}

// infoVia sends a signed GET /api/v1/info for instance id through the Hub
// connection c (whatever session it is), with a short timeout, and returns
// the status. Only a live broker answers.
func (r *ccTestRig) infoVia(t *testing.T, c *hub.BrokerConnection, id string) (int, error) {
	t.Helper()
	secret, err := r.s.GetBrokerSecret(r.ctx, id)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "http://runtime-broker/api/v1/info", nil)
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: id, SecretKey: secret.SecretKey}).ApplyAuth(req))
	headers := map[string]string{}
	for k := range req.Header {
		headers[k] = req.Header.Get(k)
	}
	tctx, tcancel := context.WithTimeout(r.ctx, 500*time.Millisecond)
	defer tcancel()
	resp, err := c.TunnelRequest(tctx, wsprotocol.NewRequestEnvelope("req-via-"+id+"-"+time.Now().Format(time.RFC3339Nano), http.MethodGet, "/api/v1/info", "", headers, nil))
	if err != nil {
		return 0, err
	}
	return resp.StatusCode, nil
}

// info tunnels a Hub-signed GET /api/v1/info to id over its control channel.
func (r *ccTestRig) info(t *testing.T, id string) (int, string, error) {
	t.Helper()
	secret, err := r.s.GetBrokerSecret(r.ctx, id)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "http://runtime-broker/api/v1/info", nil)
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: id, SecretKey: secret.SecretKey}).ApplyAuth(req))
	headers := map[string]string{}
	for k := range req.Header {
		headers[k] = req.Header.Get(k)
	}
	tctx, tcancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer tcancel()
	resp, err := r.mgr.TunnelRequest(tctx, id, wsprotocol.NewRequestEnvelope("req-"+id+"-"+time.Now().Format(time.RFC3339Nano), http.MethodGet, "/api/v1/info", "", headers, nil))
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, string(resp.Body), nil
}

// TestFlatHost_ControlChannelTwoInstancesDistinct: two co-located flat
// instances in one process each hold their own control channel to the Hub;
// a request tunnelled to one is served by that instance only, and closing
// one instance's channel leaves the other connected and serving.
func TestFlatHost_ControlChannelTwoInstancesDistinct(t *testing.T) {
	r := newCCTestRig(t)
	idA, idB := r.ids[0], r.ids[1]
	for _, pair := range [][2]string{{idA, idB}, {idB, idA}} {
		code, body, err := r.info(t, pair[0])
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, code, body)
		assert.Contains(t, body, pair[0], "served by the addressed instance")
		assert.NotContains(t, body, pair[1])
	}

	// Closing B's services removes only B's channel.
	require.NoError(t, r.active[1].Server.Shutdown(context.Background()))
	require.Eventually(t, func() bool { return !r.mgr.IsConnected(idB) }, 10*time.Second, 50*time.Millisecond)
	assert.True(t, r.mgr.IsConnected(idA))
	code, body, err := r.info(t, idA)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, idA)
	_, _, err = r.info(t, idB)
	assert.Error(t, err, "a request for B is never served by A")
}

// TestFlatHost_ControlChannelStaleSessionFenced: instance A's control
// channel is replaced by a new session for the same identity; when the old
// A session then disconnects, the new A connection stays registered and
// serving, and B stays usable throughout.
func TestFlatHost_ControlChannelStaleSessionFenced(t *testing.T) {
	r := newCCTestRig(t)
	idA, idB := r.ids[0], r.ids[1]
	oldA := r.active[0].Server
	oldSession := r.mgr.GetConnection(idA)
	require.NotNil(t, oldSession)

	// A replacement session for A (same identity and credentials).
	newA := r.build(r.active[0].Context)
	require.NoError(t, newA.StartServices(r.ctx))
	require.Eventually(t, func() bool {
		c := r.mgr.GetConnection(idA)
		return c != nil && c != oldSession
	}, 10*time.Second, 50*time.Millisecond, "the new A session replaces the old one")

	// The old A session disconnects: it must not remove the new connection.
	require.NoError(t, oldA.Shutdown(context.Background()))
	// While both A servers ran they shared one identity, so their sessions
	// may have displaced each other: the session the Hub holds for A when
	// the old server stops may be the OLD server's last one, whose socket is
	// closed but whose teardown the Hub has not processed yet. Waiting for
	// the first old session to fail, or for A to look connected, does not
	// tell them apart. Wait instead until the session the Hub holds for A
	// answers a tunneled request: only a live server can answer, and the
	// old one is stopped, so that session is the new A server's.
	require.Eventually(t, func() bool {
		c := r.mgr.GetConnection(idA)
		if c == nil {
			return false
		}
		code, err := r.infoVia(t, c, idA)
		return err == nil && code == http.StatusOK
	}, 15*time.Second, 50*time.Millisecond, "the Hub holds the new A server's session")
	// From then on no teardown of an old A session drops it: the Hub only
	// removes the session it holds, never a newer one.
	require.Never(t, func() bool { return !r.mgr.IsConnected(idA) }, 300*time.Millisecond, 10*time.Millisecond,
		"the stale session's disconnect leaves the new A connection")
	code, body, err := r.info(t, idA)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, idA)

	// B is usable throughout.
	assert.True(t, r.mgr.IsConnected(idB))
	code, body, err = r.info(t, idB)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, idB)
	t.Cleanup(func() { _ = newA.Shutdown(context.Background()) })
}

// TestFlatHostHealth_NoRefusalText: the broker health shown on the Hub's
// public /healthz carries reason codes, never refusal text.
func TestFlatHostHealth_NoRefusalText(t *testing.T) {
	clearK8sEnv(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "private", "missing.kubeconfig")
	h, _ := prepareK8sHost(t, t.TempDir(), k8sInstance("k8s-a", missing, "agents"), k8sInstance("k8s-b", missing, "other"))
	data, err := json.Marshal(flatHostHealth(context.Background(), h))
	require.NoError(t, err)
	assert.NotContains(t, string(data), missing, "no kubeconfig path in public health")
	assert.NotContains(t, string(data), `"error"`)
	assert.Contains(t, string(data), `"reason":"runtime_build_failed"`)
	assert.Contains(t, statusOf(h, "k8s-a").Error, "missing.kubeconfig", "in-process callers keep the full text")
}
