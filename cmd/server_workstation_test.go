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
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetServerFlags resets the package-level server flag variables to their
// cobra default values so tests don't leak state.
func resetServerFlags() {
	enableHub = false
	enableRuntimeBroker = false
	enableWeb = false
	enableDevAuth = false
	enableDebug = false
	serverAutoProvide = false
	serverStartForeground = false
	hostedMode = false
	hubHost = "0.0.0.0"
	hubPort = 9810
	runtimeBrokerPort = 9800
	webPort = 8080
	storageBucket = ""
	storageDir = ""
	serverConfigPath = ""
	dbURL = ""
}

func TestWorkstationModeDefaults(t *testing.T) {
	// Reset flags after test to avoid leaking into other tests
	t.Cleanup(resetServerFlags)

	// Parse with no flags — simulates bare "scion server start"
	resetServerFlags()
	require.NoError(t, serverStartCmd.ParseFlags([]string{}))

	// Simulate the workstation defaults logic from runServerStartOrDaemon
	if !hostedMode {
		if !serverStartCmd.Flags().Changed("enable-hub") {
			enableHub = true
		}
		if !serverStartCmd.Flags().Changed("enable-runtime-broker") {
			enableRuntimeBroker = true
		}
		if !serverStartCmd.Flags().Changed("enable-web") {
			enableWeb = true
		}
		if !serverStartCmd.Flags().Changed("dev-auth") {
			enableDevAuth = true
		}
		if !serverStartCmd.Flags().Changed("auto-provide") {
			serverAutoProvide = true
		}
		if !serverStartCmd.Flags().Changed("host") {
			hubHost = "127.0.0.1"
		}
	}

	assert.True(t, enableHub, "hub should be enabled in workstation mode")
	assert.True(t, enableRuntimeBroker, "runtime broker should be enabled in workstation mode")
	assert.True(t, enableWeb, "web should be enabled in workstation mode")
	assert.True(t, enableDevAuth, "dev-auth should be enabled in workstation mode")
	assert.True(t, serverAutoProvide, "auto-provide should be enabled in workstation mode")
	assert.Equal(t, "127.0.0.1", hubHost, "host should default to loopback in workstation mode")
}

func TestHostedModeNoDefaults(t *testing.T) {
	t.Cleanup(resetServerFlags)

	resetServerFlags()
	require.NoError(t, serverStartCmd.ParseFlags([]string{"--hosted"}))

	// In hosted mode, no defaults are applied
	assert.True(t, hostedMode, "hosted flag should be set")
	assert.False(t, enableHub, "hub should not be enabled by default in hosted mode")
	assert.False(t, enableRuntimeBroker, "runtime broker should not be enabled by default in hosted mode")
	assert.False(t, enableWeb, "web should not be enabled by default in hosted mode")
	assert.False(t, enableDevAuth, "dev-auth should not be enabled by default in hosted mode")
	assert.False(t, serverAutoProvide, "auto-provide should not be enabled by default in hosted mode")
	assert.Equal(t, "0.0.0.0", hubHost, "host should default to 0.0.0.0 in hosted mode")
}

func TestWorkstationModeExplicitOverrides(t *testing.T) {
	t.Cleanup(resetServerFlags)

	// Explicitly disable web and bind to all interfaces in workstation mode
	resetServerFlags()
	require.NoError(t, serverStartCmd.ParseFlags([]string{"--enable-web=false", "--host=0.0.0.0"}))

	if !hostedMode {
		if !serverStartCmd.Flags().Changed("enable-hub") {
			enableHub = true
		}
		if !serverStartCmd.Flags().Changed("enable-runtime-broker") {
			enableRuntimeBroker = true
		}
		if !serverStartCmd.Flags().Changed("enable-web") {
			enableWeb = true
		}
		if !serverStartCmd.Flags().Changed("dev-auth") {
			enableDevAuth = true
		}
		if !serverStartCmd.Flags().Changed("auto-provide") {
			serverAutoProvide = true
		}
		if !serverStartCmd.Flags().Changed("host") {
			hubHost = "127.0.0.1"
		}
	}

	assert.True(t, enableHub, "hub should be enabled (workstation default)")
	assert.True(t, enableRuntimeBroker, "runtime broker should be enabled (workstation default)")
	assert.False(t, enableWeb, "web should be disabled (explicit override)")
	assert.True(t, enableDevAuth, "dev-auth should be enabled (workstation default)")
	assert.Equal(t, "0.0.0.0", hubHost, "host should be 0.0.0.0 (explicit override)")
}

func TestHostedModeWithExplicitFlags(t *testing.T) {
	t.Cleanup(resetServerFlags)

	resetServerFlags()
	require.NoError(t, serverStartCmd.ParseFlags([]string{
		"--hosted",
		"--enable-hub",
		"--enable-web",
		"--dev-auth",
	}))

	assert.True(t, hostedMode, "hosted flag should be set")
	assert.True(t, enableHub, "hub should be enabled (explicit)")
	assert.False(t, enableRuntimeBroker, "runtime broker should not be enabled (not explicitly set)")
	assert.True(t, enableWeb, "web should be enabled (explicit)")
	assert.True(t, enableDevAuth, "dev-auth should be enabled (explicit)")
	assert.Equal(t, "0.0.0.0", hubHost, "host should default to 0.0.0.0 in hosted mode")
}

func TestBrokerDelegationUsesHostedMode(t *testing.T) {
	t.Cleanup(resetServerFlags)

	// Simulate what broker start does: parse --hosted --enable-runtime-broker
	resetServerFlags()
	require.NoError(t, serverStartCmd.ParseFlags([]string{
		"--hosted",
		"--enable-runtime-broker",
	}))

	assert.True(t, hostedMode, "hosted flag should be set")
	assert.True(t, enableRuntimeBroker, "runtime broker should be enabled")
	assert.False(t, enableHub, "hub should NOT be enabled (broker-only)")
	assert.False(t, enableWeb, "web should NOT be enabled (broker-only)")
}

func TestBrokerDelegationDefaultsToLoopback(t *testing.T) {
	// Verify the standalone broker loopback logic:
	// When hostedMode=true, host not changed, broker enabled, hub disabled,
	// the broker host should be forced to loopback.
	t.Cleanup(resetServerFlags)
	resetServerFlags()

	hostedMode = true
	enableRuntimeBroker = true
	enableHub = false

	// Simulate the reconciliation logic: in hosted mode with broker-only,
	// if host wasn't explicitly set, default broker to loopback.
	cfg := config.DefaultGlobalConfig()
	cfg.RuntimeBroker.Enabled = enableRuntimeBroker
	hostChanged := false // simulates !cmd.Flags().Changed("host")

	if hostedMode && !hostChanged && cfg.RuntimeBroker.Enabled && !enableHub {
		cfg.RuntimeBroker.Host = "127.0.0.1"
	}

	assert.Equal(t, "127.0.0.1", cfg.RuntimeBroker.Host,
		"standalone broker in hosted mode should default to loopback")
}

func TestBrokerDelegationExplicitHostKeepsValue(t *testing.T) {
	// When --host is explicitly provided, the broker should use that value.
	t.Cleanup(resetServerFlags)
	resetServerFlags()

	hostedMode = true
	enableRuntimeBroker = true
	enableHub = false

	cfg := config.DefaultGlobalConfig()
	cfg.RuntimeBroker.Enabled = enableRuntimeBroker
	hostChanged := true // simulates cmd.Flags().Changed("host")

	if hostedMode && !hostChanged && cfg.RuntimeBroker.Enabled && !enableHub {
		cfg.RuntimeBroker.Host = "127.0.0.1"
	}

	assert.Equal(t, "0.0.0.0", cfg.RuntimeBroker.Host,
		"explicit --host should keep the configured value")
}

func TestPrintWorkstationQuickstart(t *testing.T) {
	// Create a temp dir with a dev-token file
	dir := t.TempDir()
	token := "scion_dev_abc123"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dev-token"), []byte(token+"\n"), 0600))

	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printWorkstationQuickstart(dir, "127.0.0.1", 8080, true, true)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	output := buf.String()

	assert.Contains(t, output, "http://127.0.0.1:8080", "should show web UI URL")
	assert.Contains(t, output, "export SCION_DEV_TOKEN="+token, "should show dev token export")
}

func TestPrintWorkstationQuickstart_NoWeb(t *testing.T) {
	dir := t.TempDir()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printWorkstationQuickstart(dir, "127.0.0.1", 8080, false, false)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	output := buf.String()

	assert.NotContains(t, output, "Web UI", "should not show web UI when disabled")
	assert.NotContains(t, output, "SCION_DEV_TOKEN", "should not show token when dev-auth disabled")
}

func TestPrintWorkstationQuickstart_WildcardHost(t *testing.T) {
	dir := t.TempDir()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printWorkstationQuickstart(dir, "0.0.0.0", 9090, true, false)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	output := buf.String()

	assert.Contains(t, output, "http://127.0.0.1:9090", "should replace 0.0.0.0 with 127.0.0.1")
}

func TestGenerateSystemdUnit(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd tests only run on linux")
	}

	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := generateSystemdUnit("/usr/local/bin/scion", false)
	require.NoError(t, err)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	output := buf.String()

	assert.Contains(t, output, "[Unit]")
	assert.Contains(t, output, "Scion Workstation Server")
	assert.Contains(t, output, "ExecStart=/usr/local/bin/scion server start --foreground")
	assert.NotContains(t, output, "--hosted")
	assert.Contains(t, output, "[Install]")
}

func TestGenerateSystemdUnit_Hosted(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd tests only run on linux")
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := generateSystemdUnit("/usr/local/bin/scion", true)
	require.NoError(t, err)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	output := buf.String()

	assert.Contains(t, output, "Scion Server (Hosted)")
	assert.Contains(t, output, "--hosted")
}

func TestGenerateLaunchdPlist(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("launchd tests only run on darwin")
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := generateLaunchdPlist("/usr/local/bin/scion", false)
	require.NoError(t, err)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	output := buf.String()

	assert.Contains(t, output, "io.scion.server")
	assert.Contains(t, output, "<string>/usr/local/bin/scion</string>")
	assert.Contains(t, output, "<string>--foreground</string>")
	assert.NotContains(t, output, "--hosted")
}

func TestRunServerInstall(t *testing.T) {
	// Test that install runs on the current platform without error
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Also capture stderr (install hints go there)
	oldErr := os.Stderr
	rErr, wErr, _ := os.Pipe()
	os.Stderr = wErr

	err := runServerInstall(nil, nil)

	w.Close()
	wErr.Close()
	os.Stdout = old
	os.Stderr = oldErr

	var buf bytes.Buffer
	io.Copy(&buf, r)
	output := buf.String()

	var errBuf bytes.Buffer
	io.Copy(&errBuf, rErr)
	stderrOutput := errBuf.String()

	switch goruntime.GOOS {
	case "linux":
		require.NoError(t, err)
		assert.Contains(t, output, "[Unit]")
		assert.Contains(t, stderrOutput, "systemd")
	case "darwin":
		require.NoError(t, err)
		assert.Contains(t, output, "plist")
		assert.Contains(t, stderrOutput, "launchd")
	default:
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported platform")
	}

}
