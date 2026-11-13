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

//go:build unix && !no_sqlite

package cmd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerregistration"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The shared registration precheck takes the host: a co-located cloudrun
// instance passes it (the running phase of a co-located convert registers
// it), while the remote registration flow still refuses it (P1). The
// co-located convert's precheck host is ModeColocated.
func TestFlatRegistrationActivation_CloudRunByHostMode(t *testing.T) {
	hc := crHostFacts() // co-located, every cloudrun precondition met
	inst := crInst("cr-a", "us-central1", crGoodCredentials(t, &hc, t.TempDir()))

	_, err := flatRegistrationActivation(context.Background(), inst, hc)
	require.NoError(t, err, "a co-located cloudrun instance passes the precheck")

	remote := hc
	remote.mode = brokerhost.ModeRemote
	_, err = flatRegistrationActivation(context.Background(), inst, remote)
	require.Error(t, err, "the remote registration flow refuses a cloudrun instance")
	assert.Equal(t, reasonCloudRunRemoteUnsupported, brokerhost.ReasonCode(err))

	convertTestHome(t, convertTestSettings)
	assert.Equal(t, brokerhost.ModeColocated, colocatedPrecheckHost(colocatedHost{Colocated: true}, nil).mode)
}

// End-to-end Cloud Run registration precheck on a co-located convert host
// (ptone/scion#7714): saved launches pass or refuse according to what the
// server derives from the launch, and an asserted host without a saved launch
// passes with server.hub.endpoint set and refuses naming "set server.hub.endpoint"
// when it is missing.
func TestFlatRegistrationActivation_ColocatedConvertHubEndpoint(t *testing.T) {
	const nfsStorageYAML = "  project_storage:\n    backend: nfs\n    homes: nfs\n    nfs:\n      mount_root: /mnt/nfs\n      shares:\n        - id: ws1\n          server: 10.0.0.2\n          export: /x\n"

	setupHost := func(t *testing.T, serverYAML string, savedArgs []string, hostModeFlag string) (flatPrecheckHost, config.V1RuntimeBrokerInstanceConfig) {
		t.Helper()
		globalDir := isolateServerHubEndpointEnv(t)
		t.Chdir(t.TempDir())
		oldMode, oldDetect := brokerConvertHostMode, convertDetectHost
		t.Cleanup(func() { brokerConvertHostMode, convertDetectHost = oldMode, oldDetect })
		convertDetectHost = detectColocatedHost
		brokerConvertHostMode = hostModeFlag

		body := "schema_version: \"1\"\nserver:\n" + nfsStorageYAML + serverYAML
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(body), 0o600))
		if savedArgs != nil {
			require.NoError(t, daemon.SaveArgs(serverDaemonComponent, globalDir, savedArgs))
		}
		setConvertProcessEndpointFlags(t)

		host, _, _, err := convertHost(globalDir)
		require.NoError(t, err)
		settings, err := config.LoadSettings(globalDir)
		require.NoError(t, err)
		hc := colocatedPrecheckHost(host, settings)

		seedHC := crHostFacts()
		inst := crInst("cr-a", "us-central1", crGoodCredentials(t, &seedHC, t.TempDir()))
		return hc, inst
	}

	t.Run("saved launch with https --base-url passes without server.hub.endpoint", func(t *testing.T) {
		hc, inst := setupHost(t, "", []string{"server", "start", "--foreground", "--base-url=https://hub.example.com"}, "")
		_, err := flatRegistrationActivation(context.Background(), inst, hc)
		require.NoError(t, err)
	})

	t.Run("saved launch with loopback --base-url overriding https env refuses", func(t *testing.T) {
		hc, inst := setupHost(t, "  hub:\n    public_url: https://hub.example.com\n",
			[]string{"server", "start", "--foreground", "--base-url=http://localhost:8080"}, "")
		_, err := flatRegistrationActivation(context.Background(), inst, hc)
		require.Error(t, err)
		assert.Equal(t, reasonCloudRunHubEndpoint, brokerhost.ReasonCode(err))
	})

	t.Run("saved hosted launch with IAP-derived endpoint refuses", func(t *testing.T) {
		hc, inst := setupHost(t,
			"  auth:\n    proxy:\n      iap:\n        audience: /projects/12345/locations/us-central1/services/scion-hub\n",
			[]string{"server", "start", "--foreground", "--hosted", "--enable-hub", "--enable-runtime-broker"}, "")
		_, err := flatRegistrationActivation(context.Background(), inst, hc)
		require.Error(t, err)
		assert.Equal(t, reasonCloudRunHubEndpoint, brokerhost.ReasonCode(err))
		assert.Contains(t, err.Error(), "IAP audience")
	})

	t.Run("asserted mode with server.hub.endpoint set passes", func(t *testing.T) {
		hc, inst := setupHost(t, "  hub:\n    public_url: https://hub.example.com\n", nil, brokerconvert.HostModeColocated)
		_, err := flatRegistrationActivation(context.Background(), inst, hc)
		require.NoError(t, err)
	})

	t.Run("asserted mode without server.hub.endpoint refuses naming set server.hub.endpoint", func(t *testing.T) {
		hc, inst := setupHost(t, "", nil, brokerconvert.HostModeColocated)
		_, err := flatRegistrationActivation(context.Background(), inst, hc)
		require.Error(t, err)
		assert.Equal(t, reasonCloudRunHubEndpoint, brokerhost.ReasonCode(err))
		assert.Contains(t, err.Error(), "set server.hub.endpoint")
	})
}

// closedAddr returns a loopback address with nothing listening on it.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// stallAddr returns a loopback address that accepts connections and never
// replies.
func stallAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return l.Addr().String()
}

func TestDarkEndpointProbe(t *testing.T) {
	ctx := context.Background()

	t.Run("any HTTP status answers, no credential sent", func(t *testing.T) {
		var sawAuth atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" || len(r.Cookies()) > 0 {
				sawAuth.Store(true)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		answered, err := darkEndpointProbe(ctx, []string{srv.URL})
		require.NoError(t, err)
		assert.True(t, answered)
		assert.False(t, sawAuth.Load(), "the probe must not send a credential")
	})

	t.Run("a TLS reply with an untrusted certificate answers", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer srv.Close()
		answered, err := darkEndpointProbe(ctx, []string{srv.URL})
		require.NoError(t, err)
		assert.True(t, answered)
	})

	t.Run("a redirect answers and is not followed", func(t *testing.T) {
		var targetHits atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
		defer target.Close()
		srv := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
		defer srv.Close()
		answered, err := darkEndpointProbe(ctx, []string{srv.URL})
		require.NoError(t, err)
		assert.True(t, answered)
		assert.Zero(t, targetHits.Load())
	})

	t.Run("a connection that stalls answers", func(t *testing.T) {
		answered, err := darkEndpointProbe(ctx, []string{"http://" + stallAddr(t)})
		require.NoError(t, err)
		assert.True(t, answered)
	})

	t.Run("a refused connection is dark", func(t *testing.T) {
		answered, err := darkEndpointProbe(ctx, []string{"http://" + closedAddr(t)})
		require.NoError(t, err)
		assert.False(t, answered)
	})

	t.Run("one answering endpoint of several answers", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer srv.Close()
		answered, err := darkEndpointProbe(ctx, []string{"http://" + closedAddr(t), srv.URL})
		require.NoError(t, err)
		assert.True(t, answered)
	})

	for name, eps := range map[string][]string{
		"none":        nil,
		"blank":       {"  "},
		"bad scheme":  {"ftp://hub.example"},
		"no host":     {"http://"},
		"userinfo":    {"http://u:p@127.0.0.1:1"},
		"unparseable": {"http://[::1"},
	} {
		t.Run("cannot probe: "+name, func(t *testing.T) {
			_, err := darkEndpointProbe(ctx, eps)
			assert.ErrorIs(t, err, errEndpointUnprobeable)
		})
	}
}

func TestClassifyProbeError(t *testing.T) {
	dns := &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "hub.invalid", IsNotFound: true}}
	_, err := classifyProbeError(dns, false)
	assert.ErrorIs(t, err, errEndpointUnprobeable)

	answered, err := classifyProbeError(errors.New("tls: handshake failure"), true)
	require.NoError(t, err)
	assert.True(t, answered, "a failure after connect is an answer")

	answered, err = classifyProbeError(&net.OpError{Op: "dial", Err: errors.New("connection refused")}, false)
	require.NoError(t, err)
	assert.False(t, answered)
}

func listenersAt(t *testing.T, addr string) serverListeners {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	off, offPortStr, _ := net.SplitHostPort(closedAddr(t))
	offPort, _ := strconv.Atoi(offPortStr)
	return serverListeners{HubHost: host, BrokerHost: off, WebPort: port, HubPort: offPort, BrokerPort: offPort}
}

// TestLocalServerAnswers backs the stopped-phase server-down check: any
// answer on any listener, or a live daemon PID, is up.
func TestLocalServerAnswers(t *testing.T) {
	ctx := context.Background()
	globalDir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	assert.True(t, localServerAnswers(ctx, globalDir, listenersAt(t, srv.Listener.Addr().String())), "an answering web port is up")

	down := listenersAt(t, closedAddr(t))
	assert.False(t, localServerAnswers(ctx, globalDir, down), "nothing listening and no PID file is down")

	// A live PID (this test process) is up even with every port closed.
	require.NoError(t, daemon.WritePIDComponent(serverDaemonComponent, globalDir, os.Getpid()))
	assert.True(t, localServerAnswers(ctx, globalDir, down))

	// An invalid PID file fails closed, and the check never removes it.
	pidPath := filepath.Join(globalDir, daemon.PIDFileName(serverDaemonComponent))
	require.NoError(t, os.WriteFile(pidPath, []byte("not-a-pid"), 0o600))
	assert.True(t, localServerAnswers(ctx, globalDir, down))
	_, err := os.Stat(pidPath)
	require.NoError(t, err, "the check must not remove the PID file")
}

func TestArgsSimulateRemoteBroker(t *testing.T) {
	assert.False(t, argsSimulateRemoteBroker(nil))
	assert.False(t, argsSimulateRemoteBroker([]string{"--foreground", "--enable-hub"}))
	assert.True(t, argsSimulateRemoteBroker([]string{"--enable-hub", "--simulate-remote-broker", "--unknown=x"}))
	assert.False(t, argsSimulateRemoteBroker([]string{"--simulate-remote-broker=false"}))
}

func TestColocatedStoreTopology(t *testing.T) {
	no := func() bool { return false }
	yes := func() bool { return true }
	mk := func(driver, url, storage, auth string) *config.GlobalConfig {
		cfg := config.DefaultGlobalConfig()
		cfg.Database.Driver, cfg.Database.URL = driver, url
		cfg.Storage.Provider, cfg.Auth.Mode = storage, auth
		return &cfg
	}
	cases := []struct {
		name  string
		cfg   *config.GlobalConfig
		crun  func() bool
		wantT brokerconvert.StoreTopology
	}{
		{"nil config", nil, no, brokerconvert.TopologyUndetermined},
		{"sqlite", mk("sqlite", "/x/hub.db", "local", ""), no, brokerconvert.TopologySingleSQLite},
		{"default driver", mk("", "/x/hub.db", "local", ""), no, brokerconvert.TopologySingleSQLite},
		{"sqlite without a path", mk("sqlite", "", "local", ""), no, brokerconvert.TopologyUndetermined},
		{"postgres", mk("postgres", "postgres://h/db", "local", ""), no, brokerconvert.TopologySingleExternal},
		{"cloud run service", mk("sqlite", "/x/hub.db", "local", ""), yes, brokerconvert.TopologyReplica},
		{"gcs with proxy auth", mk("postgres", "postgres://h/db", "gcs", "proxy"), no, brokerconvert.TopologyReplica},
		{"unknown driver", mk("mysql", "m://h", "local", ""), no, brokerconvert.TopologyUndetermined},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.wantT, colocatedStoreTopology(tc.cfg, tc.crun), tc.name)
	}
}

// TestLegacyStoreSQLite: the read-only count names the legacy
// broker's non-terminal agents (an agent with no phase included), and a
// missing store refuses without being created.
func TestLegacyStoreSQLite(t *testing.T) {
	h := newPreflightHub(t)
	pid := h.project(t, &store.Project{Slug: "p"})
	other := uuid.NewString()
	require.NoError(t, h.store.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID: other, Name: "other", Slug: "other", Status: store.BrokerStatusOnline,
	}))
	running := h.agent(t, &store.Agent{Slug: "running", ProjectID: pid, RuntimeBrokerID: h.brokerID, Phase: "running"}, "")
	noPhase := h.agent(t, &store.Agent{Slug: "nophase", ProjectID: pid, RuntimeBrokerID: h.brokerID}, "")
	h.agent(t, &store.Agent{Slug: "stopped", ProjectID: pid, RuntimeBrokerID: h.brokerID, Phase: "stopped"}, "")
	h.agent(t, &store.Agent{Slug: "errored", ProjectID: pid, RuntimeBrokerID: h.brokerID, Phase: "error"}, "")
	h.agent(t, &store.Agent{Slug: "elsewhere", ProjectID: pid, RuntimeBrokerID: other, Phase: "running"}, "")

	facts, err := legacyStoreSQLite(context.Background(), h.dbPath, h.brokerID)
	require.NoError(t, err)
	assert.True(t, facts.RowPresent, "the store holds this host's legacy row")
	ids := make([]string, 0, len(facts.Agents))
	for _, a := range facts.Agents {
		ids = append(ids, a.ID)
		assert.Equal(t, pid, a.ProjectID)
	}
	sort.Strings(ids)
	want := []string{running, noPhase}
	sort.Strings(want)
	assert.Equal(t, want, ids)

	missing := filepath.Join(t.TempDir(), "absent.db")
	_, err = legacyStoreSQLite(context.Background(), missing, h.brokerID)
	assert.ErrorIs(t, err, errStoreUnreadable)
	_, statErr := os.Stat(missing)
	assert.True(t, errors.Is(statErr, os.ErrNotExist), "a missing store must not be created")

	// Store identity: a store that does not hold this host's legacy row (a
	// stale hub.db from another Hub) reports RowPresent false, and no count.
	facts, err = legacyStoreSQLite(context.Background(), h.dbPath, uuid.NewString())
	require.NoError(t, err)
	assert.False(t, facts.RowPresent)
	assert.Empty(t, facts.Agents)
}

func writeInstanceCreds(t *testing.T, globalDir, key string, names ...string) string {
	t.Helper()
	dir := brokerregistration.InstanceCredentialsDir(globalDir, key)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	for _, n := range names {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte(`{"secretKey":"planted-fake-secret"}`), 0o600))
	}
	return dir
}

// TestColocatedCleanup_RemovesEveryInstanceCredentialsFile: after cleanup
// no instance credentials file or directory remains, and the identity file
// beside it is kept. Mutant "leave an instance credentials file after
// cleanup" turns it RED.
func TestColocatedCleanup_RemovesEveryInstanceCredentialsFile(t *testing.T) {
	globalDir := t.TempDir()
	keys := []string{"alpha", "beta"}
	for _, k := range keys {
		writeInstanceCreds(t, globalDir, k, "hub.json", ".hub.json.tmp-1")
		require.NoError(t, os.WriteFile(filepath.Join(brokeridentity.InstanceDir(globalDir, k), "identity.json"), []byte("{}"), 0o600))
	}
	require.True(t, instanceCredentialsRemain(globalDir, keys), "control: the files exist before cleanup")

	for _, k := range keys {
		require.NoError(t, removeInstanceCredentials(globalDir, k))
	}
	assert.False(t, instanceCredentialsRemain(globalDir, keys))
	for _, k := range keys {
		_, err := os.Stat(filepath.Join(brokeridentity.InstanceDir(globalDir, k), "identity.json"))
		assert.NoError(t, err, "the identity must be kept")
	}
	// Idempotent: a second run, and an absent instance, are no-ops.
	require.NoError(t, removeInstanceCredentials(globalDir, "alpha"))
	require.NoError(t, removeInstanceCredentials(globalDir, "never-created"))
}

func TestRemoveInstanceCredentials_RefusesLinks(t *testing.T) {
	t.Run("a link entry refuses and nothing is removed", func(t *testing.T) {
		globalDir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.json")
		require.NoError(t, os.WriteFile(outside, []byte("x"), 0o600))
		dir := writeInstanceCreds(t, globalDir, "k", "hub.json")
		require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link.json")))

		assert.ErrorIs(t, removeInstanceCredentials(globalDir, "k"), errCredentialsCleanup)
		_, err := os.Stat(filepath.Join(dir, "hub.json"))
		assert.NoError(t, err, "no entry is removed when one refuses")
		_, err = os.Stat(outside)
		assert.NoError(t, err, "the link target is untouched")
	})

	t.Run("a linked credentials directory refuses", func(t *testing.T) {
		globalDir := t.TempDir()
		realDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(realDir, "hub.json"), []byte("x"), 0o600))
		inst := brokeridentity.InstanceDir(globalDir, "k")
		require.NoError(t, os.MkdirAll(inst, 0o700))
		require.NoError(t, os.Symlink(realDir, filepath.Join(inst, brokerregistration.HubCredentialsDirName)))

		assert.ErrorIs(t, removeInstanceCredentials(globalDir, "k"), errCredentialsCleanup)
		_, err := os.Stat(filepath.Join(realDir, "hub.json"))
		assert.NoError(t, err)
	})

	t.Run("a linked instance directory refuses", func(t *testing.T) {
		globalDir := t.TempDir()
		realDir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(globalDir, brokeridentity.RuntimeBrokersDirName), 0o700))
		require.NoError(t, os.Symlink(realDir, brokeridentity.InstanceDir(globalDir, "k")))
		assert.ErrorIs(t, removeInstanceCredentials(globalDir, "k"), errCredentialsCleanup)
	})

	t.Run("a subdirectory refuses", func(t *testing.T) {
		globalDir := t.TempDir()
		dir := writeInstanceCreds(t, globalDir, "k", "hub.json")
		require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
		assert.ErrorIs(t, removeInstanceCredentials(globalDir, "k"), errCredentialsCleanup)
	})
}
