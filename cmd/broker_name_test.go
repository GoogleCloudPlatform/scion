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

package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerHub is a fake hub for 'runtime-broker register': it answers the
// health check, broker creation and join, and the broker lookup, and
// records the name each broker creation and join asked for.
type registerHub struct {
	*httptest.Server
	mu          sync.Mutex
	createNames []string
	joinNames   []string
	// hubName is the name GET /api/v1/runtime-brokers/<id> reports; empty
	// means the name of the last creation.
	hubName      string
	reregistered bool
	// matchedID, when set, is the broker ID creation returns instead of
	// the requested one (the hub matched another broker by name).
	matchedID string
}

func newRegisterHub(t *testing.T) *registerHub {
	t.Helper()
	h := &registerHub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		case r.URL.Path == "/api/v1/brokers" && r.Method == http.MethodPost:
			name, _ := body["name"].(string)
			h.createNames = append(h.createNames, name)
			id := body["brokerId"]
			if h.matchedID != "" {
				id = h.matchedID
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"brokerId":     id,
				"joinToken":    "scion_join_minted",
				"expiresAt":    "2026-10-06T16:00:00Z",
				"reregistered": h.reregistered,
			})
		case r.URL.Path == "/api/v1/brokers/join":
			name, _ := body["hostname"].(string)
			h.joinNames = append(h.joinNames, name)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"brokerId":    body["brokerId"],
				"secretKey":   base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
				"hubEndpoint": "http://" + r.Host,
			})
		case r.Method == http.MethodGet && len(r.URL.Path) > len("/api/v1/runtime-brokers/") && r.URL.Path[:len("/api/v1/runtime-brokers/")] == "/api/v1/runtime-brokers/":
			name := h.hubName
			if name == "" && len(h.createNames) > 0 {
				name = h.createNames[len(h.createNames)-1]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": r.URL.Path[len("/api/v1/runtime-brokers/"):], "name": name})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *registerHub) names() (create, join []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.createNames...), append([]string(nil), h.joinNames...)
}

// setupRegisterTest points HOME at a temp dir with a hub-enabled project
// for hub, runs a fake broker for the health check and enables --yes.
func setupRegisterTest(t *testing.T, hub *registerHub) (globalDir string) {
	t.Helper()
	isolateJoinEnv(t)
	home, globalDir := brokerTestHome(t)
	projectPath = setupSecretProject(t, home, hub.URL)
	t.Setenv("SCION_HUB_ENDPOINT", hub.URL)
	setBrokerFlagForTest(t, brokerRegisterCmd, "port", strconv.Itoa(newFakeBrokerHealthServer(t, "healthy")))
	savedYes, savedForce, savedName := autoConfirm, brokerForceRegister, brokerRegisterName
	t.Cleanup(func() { autoConfirm, brokerForceRegister, brokerRegisterName = savedYes, savedForce, savedName })
	autoConfirm = true
	return globalDir
}

func runRegisterForTest(t *testing.T) string {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = runBrokerRegister(brokerRegisterCmd, nil) })
	require.NoError(t, err, "register output: %s", out)
	return out
}

func hostnameForTest(t *testing.T) string {
	t.Helper()
	h, err := os.Hostname()
	require.NoError(t, err)
	return h
}

func TestBrokerRegister_BrokerNameFlagIsNotName(t *testing.T) {
	f := brokerRegisterCmd.Flags().Lookup("broker-name")
	require.NotNil(t, f, "register has --broker-name")
	assert.Equal(t, "", f.DefValue)
	name := brokerRegisterCmd.Flags().Lookup("name")
	require.NotNil(t, name)
	assert.Contains(t, name.Usage, "hub connection", "--name still names the hub connection")
}

// TestBrokerRegister_DefaultNameIsHostname: without --broker-name, register
// uses the hostname and saves no broker name.
func TestBrokerRegister_DefaultNameIsHostname(t *testing.T) {
	hub := newRegisterHub(t)
	setupRegisterTest(t, hub)

	out := runRegisterForTest(t)

	host := hostnameForTest(t)
	create, join := hub.names()
	assert.Equal(t, []string{host}, create)
	assert.Equal(t, []string{host}, join)
	assert.Contains(t, out, fmt.Sprintf("Broker '%s' registered successfully", host))
	assert.Empty(t, config.ConfiguredBrokerName(), "the hostname default is not saved")
}

// TestBrokerRegister_BrokerNameFlag: --broker-name is sent to the hub and
// saved, and a later register without the flag keeps it instead of going
// back to the hostname (which would match another broker on the hub).
func TestBrokerRegister_BrokerNameFlag(t *testing.T) {
	hub := newRegisterHub(t)
	setupRegisterTest(t, hub)

	setBrokerFlagForTest(t, brokerRegisterCmd, "broker-name", "rig-broker-2")
	out := runRegisterForTest(t)

	create, join := hub.names()
	assert.Equal(t, []string{"rig-broker-2"}, create)
	assert.Equal(t, []string{"rig-broker-2"}, join)
	assert.Contains(t, out, "Broker 'rig-broker-2' registered successfully")
	assert.Contains(t, out, "Broker name 'rig-broker-2' saved to global settings")
	assert.Equal(t, "rig-broker-2", config.ConfiguredBrokerName())
	assert.Equal(t, "rig-broker-2", config.LocalBrokerName("x"))

	// Re-register without the flag (--force so the hub is asked again).
	f := brokerRegisterCmd.Flags().Lookup("broker-name")
	require.NoError(t, f.Value.Set(""))
	f.Changed = false
	brokerForceRegister = true
	out = runRegisterForTest(t)
	create, _ = hub.names()
	assert.Equal(t, []string{"rig-broker-2", "rig-broker-2"}, create, "the saved name is reused")
	assert.NotContains(t, out, "saved to global settings")
}

// TestBrokerRegister_BrokerNameKeptByHub: when the hub keeps an existing
// broker's name, register reports and saves that name, not the requested one.
func TestBrokerRegister_BrokerNameKeptByHub(t *testing.T) {
	hub := newRegisterHub(t)
	hub.hubName = "old-name"
	hub.reregistered = true
	setupRegisterTest(t, hub)

	setBrokerFlagForTest(t, brokerRegisterCmd, "broker-name", "new-name")
	out := runRegisterForTest(t)

	assert.Contains(t, out, "already registered on the hub as 'old-name'")
	assert.Contains(t, out, "Found existing broker registration for 'old-name'", "the found line uses the hub's name")
	assert.Contains(t, out, "Broker 'old-name' registered successfully")
	assert.NotContains(t, out, "matched an existing broker", "an ID match keeps this host's identity")
	assert.Equal(t, "old-name", config.ConfiguredBrokerName())
	_, join := hub.names()
	assert.Equal(t, []string{"old-name"}, join)
}

// TestBrokerRegister_ReregisterReadbackWithoutFlag: without --broker-name,
// a re-registration still reports the name the hub kept, without a warning
// and without saving it.
func TestBrokerRegister_ReregisterReadbackWithoutFlag(t *testing.T) {
	hub := newRegisterHub(t)
	hub.hubName = "old-name"
	hub.reregistered = true
	setupRegisterTest(t, hub)

	out := runRegisterForTest(t)

	assert.Contains(t, out, "Found existing broker registration for 'old-name'")
	assert.Contains(t, out, "Broker 'old-name' registered successfully")
	assert.NotContains(t, out, "was not applied")
	assert.NotContains(t, out, "saved to global settings")
	assert.Empty(t, config.ConfiguredBrokerName(), "nothing is saved without --broker-name")
}

// TestBrokerRegister_NameMatchesOtherBroker: when the hub matches the name
// to a broker other than this host's broker ID, register warns that this
// host takes over that broker's identity (it still registers).
func TestBrokerRegister_NameMatchesOtherBroker(t *testing.T) {
	hub := newRegisterHub(t)
	hub.reregistered = true
	hub.matchedID = "99999999-8888-7777-6666-555555555555"
	setupRegisterTest(t, hub)

	setBrokerFlagForTest(t, brokerRegisterCmd, "broker-name", "taken-name")
	out := runRegisterForTest(t)

	assert.Contains(t, out, "Warning: the name 'taken-name' matched an existing broker on the hub (ID: 99999999-8888-7777-6666-555555555555)")
	assert.Contains(t, out, "this host now uses that broker's identity")
	assert.Contains(t, out, "registered successfully (ID: 99999999-8888-7777-6666-555555555555)")
}

func TestWarnBrokerIdentityTakeover(t *testing.T) {
	var buf bytes.Buffer
	warnBrokerIdentityTakeover(&buf, "n", "id-1", "id-1")
	assert.Empty(t, buf.String(), "same broker ID: no warning")
	warnBrokerIdentityTakeover(&buf, "n", "", "id-1")
	assert.Empty(t, buf.String())
	warnBrokerIdentityTakeover(&buf, "n", "id-2", "id-1")
	assert.Contains(t, buf.String(), "matched an existing broker on the hub (ID: id-2), not this host's broker ID (id-1)")
}

// TestPersistBrokerName_ServerStartReadsIt: the name register saves is the
// name 'server start' gives the broker (resolveBrokerName), so the hub
// record and the running broker agree.
func TestPersistBrokerName_ServerStartReadsIt(t *testing.T) {
	cases := map[string]string{
		"no settings file": "",
		"versioned file":   "schema_version: \"1\"\nserver:\n  broker:\n    port: 9800\n",
		"legacy file":      "hub:\n  endpoint: http://hub.example\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, globalDir := brokerTestHome(t)
			if content != "" {
				writeGlobalSettings(t, globalDir, content)
			}
			persistBrokerName(io.Discard, globalDir, "rig-c")

			cfg, err := config.LoadGlobalConfig("")
			require.NoError(t, err)
			settings, _ := loadServerSettings(globalDir)
			var vsBroker *config.V1BrokerConfig
			if vs, _, err := config.LoadEffectiveSettings(""); err == nil && vs != nil && vs.Server != nil {
				vsBroker = vs.Server.Broker
			}
			assert.Equal(t, "rig-c", resolveBrokerName(cfg, settings, vsBroker))
		})
	}
}

func TestBrokerRegister_EmptyBrokerName(t *testing.T) {
	brokerTestHome(t)
	savedName := brokerRegisterName
	t.Cleanup(func() { brokerRegisterName = savedName })
	setBrokerFlagForTest(t, brokerRegisterCmd, "broker-name", "  ")
	err := runBrokerRegister(brokerRegisterCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--broker-name must not be empty")
}

func TestResolveRegisterBrokerName(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	savedName := brokerRegisterName
	t.Cleanup(func() { brokerRegisterName = savedName })

	name, set, err := resolveRegisterBrokerName(brokerRegisterCmd)
	require.NoError(t, err)
	assert.False(t, set)
	assert.Equal(t, hostnameForTest(t), name, "default is the hostname")

	writeGlobalSettings(t, globalDir, "schema_version: \"1\"\nserver:\n  broker:\n    broker_nickname: saved-name\n")
	name, set, err = resolveRegisterBrokerName(brokerRegisterCmd)
	require.NoError(t, err)
	assert.False(t, set)
	assert.Equal(t, "saved-name", name, "a saved name wins over the hostname")

	setBrokerFlagForTest(t, brokerRegisterCmd, "broker-name", " flag-name ")
	name, set, err = resolveRegisterBrokerName(brokerRegisterCmd)
	require.NoError(t, err)
	assert.True(t, set)
	assert.Equal(t, "flag-name", name, "the flag wins")
}

func TestPersistBrokerName(t *testing.T) {
	_, globalDir := brokerTestHome(t)

	var buf bytes.Buffer
	persistBrokerName(&buf, globalDir, "rig-a")
	assert.Contains(t, buf.String(), "saved to global settings")
	assert.Equal(t, "rig-a", config.ConfiguredBrokerName())

	buf.Reset()
	persistBrokerName(&buf, globalDir, "rig-a")
	assert.Empty(t, buf.String(), "saving the same name again is silent")
}

func TestKeepHubBrokerName(t *testing.T) {
	var buf bytes.Buffer
	assert.Equal(t, "hub", keepHubBrokerName(&buf, "local", "hub", false))
	assert.Empty(t, buf.String(), "no warning when the name was not asked for")
	assert.Equal(t, "same", keepHubBrokerName(&buf, "same", "same", true))
	assert.Empty(t, buf.String())
	assert.Equal(t, "hub", keepHubBrokerName(&buf, "asked", "hub", true))
	assert.Contains(t, buf.String(), "--broker-name 'asked' was not applied")
}

// TestBrokerStatus_ConfiguredBrokerName: with no hub to ask, status shows
// the saved broker name; without one, none (unchanged).
func TestBrokerStatus_ConfiguredBrokerName(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	setBrokerFlagForTest(t, brokerStatusCmd, "port", strconv.Itoa(unusedPort(t)))
	savedOutputFormat, savedJSON := outputFormat, brokerStatusJSON
	t.Cleanup(func() { outputFormat, brokerStatusJSON = savedOutputFormat, savedJSON })
	brokerStatusJSON = true

	runStatus := func() brokerStatusInfo {
		t.Helper()
		out := captureStdout(t, func() { require.NoError(t, runBrokerStatus(brokerStatusCmd, nil)) })
		var info brokerStatusInfo
		require.NoError(t, json.Unmarshal([]byte(out), &info), "status output: %s", out)
		return info
	}

	assert.Empty(t, runStatus().BrokerName)
	writeGlobalSettings(t, globalDir, "schema_version: \"1\"\nserver:\n  broker:\n    broker_nickname: rig-b\n")
	info := runStatus()
	assert.Equal(t, "rig-b", info.BrokerName)
	assert.Equal(t, hostnameForTest(t), info.Hostname, "hostname still reports the OS hostname")
}

// ---------------------------------------------------------------------------
// Combined server broker port
// ---------------------------------------------------------------------------

func TestServerBrokerPort(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	assert.Equal(t, DefaultBrokerPort, serverBrokerPort(globalDir), "default")

	writeGlobalSettings(t, globalDir, "schema_version: \"1\"\nserver:\n  broker:\n    port: 9811\n")
	assert.Equal(t, 9811, serverBrokerPort(globalDir), "settings port")

	require.NoError(t, daemon.SaveArgs(serverDaemonComponent, globalDir, []string{"server", "start", "--foreground"}))
	assert.Equal(t, 9811, serverBrokerPort(globalDir), "saved args without a broker port use the settings port")

	require.NoError(t, daemon.SaveArgs(serverDaemonComponent, globalDir, []string{"server", "start", "--foreground", "--runtime-broker-port=9822"}))
	assert.Equal(t, 9822, serverBrokerPort(globalDir), "the server's --runtime-broker-port wins")
}

// TestIsServerDaemonManagingBroker_NonDefaultPort: start/stop/restart find a
// server-managed broker on the server's broker port, not only on 9800.
func TestIsServerDaemonManagingBroker_NonDefaultPort(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	require.NoError(t, daemon.WritePIDComponent(serverDaemonComponent, globalDir, os.Getpid()))

	port := newFakeBrokerHealthServer(t, "healthy")
	require.NoError(t, daemon.SaveArgs(serverDaemonComponent, globalDir, []string{"server", "start", "--foreground", "--runtime-broker-port=" + strconv.Itoa(port)}))
	managed, pid := isServerDaemonManagingBroker(globalDir)
	assert.True(t, managed)
	assert.Equal(t, os.Getpid(), pid)

	require.NoError(t, daemon.SaveArgs(serverDaemonComponent, globalDir, []string{"server", "start", "--foreground", "--runtime-broker-port=" + strconv.Itoa(unusedPort(t))}))
	managed, _ = isServerDaemonManagingBroker(globalDir)
	assert.False(t, managed, "no broker answers on the server's broker port")
}

// TestServerStatus_BrokerOnSettingsPort: 'server status' probes the broker
// on server.broker.port instead of 9800.
func TestServerStatus_BrokerOnSettingsPort(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	port := newFakeBrokerHealthServer(t, "healthy")
	writeGlobalSettings(t, globalDir, "schema_version: \"1\"\nserver:\n  broker:\n    port: "+strconv.Itoa(port)+"\n")

	savedJSON := serverStatusJSON
	t.Cleanup(func() { serverStatusJSON = savedJSON })
	serverStatusJSON = true
	out := captureStdout(t, func() { require.NoError(t, runServerStatus(serverStatusCmd, nil)) })
	var info serverStatusInfo
	require.NoError(t, json.Unmarshal([]byte(out), &info), "status output: %s", out)
	assert.True(t, info.BrokerRunning, "broker on port %d is found", port)
}
