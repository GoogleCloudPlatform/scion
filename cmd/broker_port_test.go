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
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setBrokerFlagForTest sets a flag on cmd as if passed on the command line
// and restores its value and Changed state when the test ends.
func setBrokerFlagForTest(t *testing.T, cmd *cobra.Command, name, value string) {
	t.Helper()
	f := cmd.Flags().Lookup(name)
	require.NotNil(t, f, "flag --%s on %s", name, cmd.Name())
	old, oldChanged := f.Value.String(), f.Changed
	require.NoError(t, cmd.Flags().Set(name, value))
	t.Cleanup(func() {
		_ = f.Value.Set(old)
		f.Changed = oldChanged
	})
}

// newFakeBrokerServer starts a local server answering the broker /healthz
// probe (and /api/v1/hub-connections when conns is non-nil) and returns its
// port.
func newFakeBrokerServer(t *testing.T, status string, conns *BrokerHubConnectionsResponse) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(BrokerHealthResponse{Status: status, Version: "test"})
		case r.URL.Path == "/api/v1/hub-connections" && conns != nil:
			_ = json.NewEncoder(w).Encode(conns)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return port
}

func newFakeBrokerHealthServer(t *testing.T, status string) int {
	t.Helper()
	return newFakeBrokerServer(t, status, nil)
}

// writeGlobalSettings writes content as the global settings.yaml.
func writeGlobalSettings(t *testing.T, globalDir, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(content), 0644))
}

// unusedPort returns a local port with nothing listening on it.
func unusedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// brokerTestHome points HOME at a temp dir and returns the global dir.
func brokerTestHome(t *testing.T) (home, globalDir string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	globalDir = filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))
	return home, globalDir
}

func TestBrokerPortFromArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"default omitted", buildBrokerDaemonArgs(DefaultBrokerPort, false, false), DefaultBrokerPort},
		{"non-default", buildBrokerDaemonArgs(19800, true, false), 19800},
		{"separate value", []string{"server", "start", "--runtime-broker-port", "19801"}, 19801},
		{"garbage", []string{"--runtime-broker-port=abc"}, DefaultBrokerPort},
		{"empty", nil, DefaultBrokerPort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, brokerPortFromArgs(tt.args))
		})
	}
}

func TestBrokerPortFlagOnSubcommands(t *testing.T) {
	for _, c := range []*cobra.Command{brokerRegisterCmd, brokerDeregisterCmd, brokerStatusCmd, brokerStopCmd, brokerHubsCmd, brokerRestartCmd, brokerStartCmd} {
		assert.NotNil(t, c.Flags().Lookup("port"), "runtime-broker %s should accept --port", c.Name())
	}
}

func TestResolveBrokerPort(t *testing.T) {
	_, globalDir := brokerTestHome(t)

	// Nothing saved, no flag: the default port.
	assert.Equal(t, DefaultBrokerPort, resolveBrokerPort(brokerStatusCmd))

	// The port saved by start is used when --port is not given.
	require.NoError(t, daemon.SaveArgs(brokerDaemonComponent, globalDir, buildBrokerDaemonArgs(19800, false, false)))
	assert.Equal(t, 19800, resolveBrokerPort(brokerStatusCmd))
	assert.Equal(t, 19800, resolveBrokerPort(nil))

	// An explicit --port wins over the saved port.
	setBrokerFlagForTest(t, brokerStatusCmd, "port", "19900")
	assert.Equal(t, 19900, resolveBrokerPort(brokerStatusCmd))
}

func TestResolveBrokerRestartOptions(t *testing.T) {
	brokerTestHome(t)
	saved := buildBrokerDaemonArgs(19800, true, true)

	t.Run("no saved args uses flag defaults", func(t *testing.T) {
		port, autoProvide, debug := resolveBrokerRestartOptions(brokerRestartCmd, nil)
		assert.Equal(t, DefaultBrokerPort, port)
		assert.False(t, autoProvide)
		assert.False(t, debug)
	})

	t.Run("saved args are kept", func(t *testing.T) {
		port, autoProvide, debug := resolveBrokerRestartOptions(brokerRestartCmd, saved)
		assert.Equal(t, 19800, port)
		assert.True(t, autoProvide)
		assert.True(t, debug)
		assert.Equal(t, saved, buildBrokerDaemonArgs(port, autoProvide, debug))
	})

	t.Run("explicit flags win", func(t *testing.T) {
		setBrokerFlagForTest(t, brokerRestartCmd, "port", "9801")
		setBrokerFlagForTest(t, brokerRestartCmd, "debug", "false")
		port, autoProvide, debug := resolveBrokerRestartOptions(brokerRestartCmd, saved)
		assert.Equal(t, 9801, port)
		assert.True(t, autoProvide, "--auto-provide not given: kept from the saved args")
		assert.False(t, debug)
	})
}

// TestBrokerStatus_NonDefaultPort runs status against a broker on a
// non-default port, found both from the saved start args and from --port.
func TestBrokerStatus_NonDefaultPort(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	port := newFakeBrokerHealthServer(t, "healthy")

	savedOutputFormat, savedJSON := outputFormat, brokerStatusJSON
	t.Cleanup(func() { outputFormat, brokerStatusJSON = savedOutputFormat, savedJSON })
	brokerStatusJSON = true

	runStatus := func() brokerStatusInfo {
		t.Helper()
		out := captureStdout(t, func() {
			require.NoError(t, runBrokerStatus(brokerStatusCmd, nil))
		})
		var info brokerStatusInfo
		require.NoError(t, json.Unmarshal([]byte(out), &info), "status output: %s", out)
		return info
	}

	t.Run("saved port", func(t *testing.T) {
		require.NoError(t, daemon.SaveArgs(brokerDaemonComponent, globalDir, buildBrokerDaemonArgs(port, false, false)))
		info := runStatus()
		assert.True(t, info.ServerRunning)
		assert.Equal(t, port, info.ServerPort)
		assert.Equal(t, "healthy", info.ServerStatus)
		require.NoError(t, daemon.RemoveArgs(brokerDaemonComponent, globalDir))
	})

	t.Run("--port flag", func(t *testing.T) {
		setBrokerFlagForTest(t, brokerStatusCmd, "port", strconv.Itoa(port))
		info := runStatus()
		assert.True(t, info.ServerRunning)
		assert.Equal(t, port, info.ServerPort)
	})
}

// TestBrokerStop_NonDefaultPort: with no daemon, stop probes the broker on
// the saved/flag port to tell a foreground broker from no broker.
func TestBrokerStop_NonDefaultPort(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	port := newFakeBrokerHealthServer(t, "healthy")

	require.NoError(t, daemon.SaveArgs(brokerDaemonComponent, globalDir, buildBrokerDaemonArgs(port, false, false)))
	err := runBrokerStop(brokerStopCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not as a daemon", "stop should find the foreground broker on port %d", port)

	// With --port pointing elsewhere, there is no broker to find.
	setBrokerFlagForTest(t, brokerStopCmd, "port", strconv.Itoa(unusedPort(t)))
	err = runBrokerStop(brokerStopCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broker daemon is not running")
}

// TestBrokerRegister_NonDefaultPort: register health-checks the broker on
// the non-default port instead of 9800.
func TestBrokerRegister_NonDefaultPort(t *testing.T) {
	home, globalDir := brokerTestHome(t)

	// A hub that is down, so register stops right after the broker check.
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(hub.Close)
	t.Setenv("SCION_HUB_ENDPOINT", hub.URL)

	savedProjectPath := projectPath
	t.Cleanup(func() { projectPath = savedProjectPath })
	projectPath = setupSecretProject(t, home, hub.URL)

	t.Run("no broker on the port", func(t *testing.T) {
		port := unusedPort(t)
		setBrokerFlagForTest(t, brokerRegisterCmd, "port", strconv.Itoa(port))
		err := runBrokerRegister(brokerRegisterCmd, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "broker server not running on port "+strconv.Itoa(port))
	})

	t.Run("saved port", func(t *testing.T) {
		port := newFakeBrokerHealthServer(t, "healthy")
		require.NoError(t, daemon.SaveArgs(brokerDaemonComponent, globalDir, buildBrokerDaemonArgs(port, false, false)))
		var err error
		out := captureStdout(t, func() { err = runBrokerRegister(brokerRegisterCmd, nil) })
		assert.Contains(t, out, "Broker server is running (status: healthy")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "broker server not running")
		assert.Contains(t, err.Error(), "is not responding", "register should get past the broker check to the hub check")
	})
}

// TestBrokerPort_FromSettings: server.broker.port in settings moves the
// broker off 9800 (the server reads it when no --runtime-broker-port is
// passed), so start must run on, print and save that port, and the other
// subcommands must find it.
func TestBrokerPort_FromSettings(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	port := newFakeBrokerHealthServer(t, "healthy")
	writeGlobalSettings(t, globalDir, "schema_version: \"1\"\nserver:\n  broker:\n    port: "+strconv.Itoa(port)+"\n")

	assert.Equal(t, port, settingsBrokerPort())
	assert.Equal(t, port, resolveBrokerStartPort(brokerStartCmd), "start without --port uses the settings port")
	args := pinBrokerPort(buildBrokerDaemonArgs(resolveBrokerStartPort(brokerStartCmd), false, false), port)
	assert.Equal(t, port, brokerPortFromArgs(args), "the saved args name the settings port")

	// An explicit 'start --port 9800' must win over the settings port, so
	// the launch args pin it.
	setBrokerFlagForTest(t, brokerStartCmd, "port", "9800")
	assert.Equal(t, DefaultBrokerPort, resolveBrokerStartPort(brokerStartCmd))
	pinned := pinBrokerPort(buildBrokerDaemonArgs(DefaultBrokerPort, false, false), DefaultBrokerPort)
	assert.Contains(t, pinned, "--runtime-broker-port=9800")

	// With nothing saved (for example a broker run by 'scion server start'),
	// status falls back to the settings port.
	savedOutputFormat, savedJSON := outputFormat, brokerStatusJSON
	t.Cleanup(func() { outputFormat, brokerStatusJSON = savedOutputFormat, savedJSON })
	brokerStatusJSON = true
	out := captureStdout(t, func() {
		require.NoError(t, runBrokerStatus(brokerStatusCmd, nil))
	})
	var info brokerStatusInfo
	require.NoError(t, json.Unmarshal([]byte(out), &info), "status output: %s", out)
	assert.True(t, info.ServerRunning)
	assert.Equal(t, port, info.ServerPort)

	// Restart of a daemon with no saved args also uses the settings port.
	restartPort, _, _ := resolveBrokerRestartOptions(brokerRestartCmd, nil)
	assert.Equal(t, port, restartPort)
}

func TestBrokerPort_DefaultWithoutSettings(t *testing.T) {
	brokerTestHome(t)
	assert.Equal(t, DefaultBrokerPort, settingsBrokerPort())
	args := buildBrokerDaemonArgs(DefaultBrokerPort, false, false)
	assert.Equal(t, args, pinBrokerPort(args, DefaultBrokerPort), "nothing to pin when settings use the default")
}

// TestResolveBrokerPort_CorruptArgsWarns: an unreadable args file is
// reported on stderr, not silently replaced by the default port.
func TestResolveBrokerPort_CorruptArgsWarns(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, daemon.ArgsFileName(brokerDaemonComponent)), []byte("{not json"), 0644))

	var port int
	errOut := captureStderr(t, func() { port = resolveBrokerPort(brokerStatusCmd) })
	assert.Equal(t, DefaultBrokerPort, port)
	assert.Contains(t, errOut, "broker-args.json")
	assert.Contains(t, errOut, "--port")
}

// TestBrokerRestart_NonDefaultPort: with no daemon, restart probes the
// saved port to tell a foreground broker from no broker.
func TestBrokerRestart_NonDefaultPort(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	port := newFakeBrokerHealthServer(t, "healthy")
	require.NoError(t, daemon.SaveArgs(brokerDaemonComponent, globalDir, buildBrokerDaemonArgs(port, false, false)))

	err := runBrokerRestart(brokerRestartCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not as a daemon", "restart should find the foreground broker on port %d", port)
}

// TestBrokerHubs_NonDefaultPort: hubs reads live connection status from the
// broker on the saved port.
func TestBrokerHubs_NonDefaultPort(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	require.NoError(t, brokercredentials.NewMultiStore("").Save(&brokercredentials.BrokerCredentials{
		Name:        "myhub",
		BrokerID:    "33333333-3333-3333-3333-333333333333",
		SecretKey:   "c2VjcmV0",
		HubEndpoint: "https://hub.example.com",
	}))
	port := newFakeBrokerServer(t, "healthy", &BrokerHubConnectionsResponse{
		Mode:        "standalone-test",
		Connections: []BrokerHubConnectionInfo{{Name: "myhub", Status: "connected-test"}},
	})
	require.NoError(t, daemon.SaveArgs(brokerDaemonComponent, globalDir, buildBrokerDaemonArgs(port, false, false)))

	savedOutputFormat, savedJSON := outputFormat, brokerHubsJSON
	t.Cleanup(func() { outputFormat, brokerHubsJSON = savedOutputFormat, savedJSON })
	outputFormat, brokerHubsJSON = "", false

	out := captureStdout(t, func() {
		require.NoError(t, runBrokerHubs(brokerHubsCmd, nil))
	})
	assert.Contains(t, out, "Mode: standalone-test")
	assert.Contains(t, out, "connected-test")
}
