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
	webSessionSecret = ""
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
	// Hosted standalone broker (broker enabled, hub disabled) without --host
	// defaults to loopback.
	cfg := config.DefaultGlobalConfig()
	cfg.RuntimeBroker.Enabled = true

	applyServerListenHosts(&cfg, true, false, "0.0.0.0", false)

	assert.Equal(t, "127.0.0.1", cfg.RuntimeBroker.Host,
		"standalone broker in hosted mode should default to loopback")
}

func TestBrokerDelegationExplicitHostKeepsValue(t *testing.T) {
	// Hosted standalone broker with --host keeps the configured broker host.
	cfg := config.DefaultGlobalConfig()
	cfg.RuntimeBroker.Enabled = true

	applyServerListenHosts(&cfg, true, true, "10.0.0.5", false)

	assert.Equal(t, "0.0.0.0", cfg.RuntimeBroker.Host,
		"explicit --host should keep the configured broker host")
	assert.Equal(t, "10.0.0.5", cfg.Hub.Host, "--host sets the hub listener")
}

func TestApplyServerListenHosts(t *testing.T) {
	tests := []struct {
		name       string
		hosted     bool
		brokerHost string // "" keeps the default from DefaultGlobalConfig
		hostSet    bool
		host       string
		hubEnabled bool
		wantHub    string
		wantBroker string
	}{
		{
			name:       "workstation without --host",
			hubEnabled: true,
			wantHub:    "127.0.0.1",
			wantBroker: "127.0.0.1",
		},
		{
			name:       "workstation with --host moves only the hub",
			hostSet:    true,
			host:       "0.0.0.0",
			hubEnabled: true,
			wantHub:    "0.0.0.0",
			wantBroker: "127.0.0.1",
		},
		{
			name:       "workstation with --host keeps explicit broker host",
			brokerHost: "192.168.1.10",
			hostSet:    true,
			host:       "0.0.0.0",
			hubEnabled: true,
			wantHub:    "0.0.0.0",
			wantBroker: "192.168.1.10",
		},
		{
			name:       "workstation broker host 0.0.0.0 counts as unset",
			brokerHost: "0.0.0.0",
			hostSet:    true,
			host:       "0.0.0.0",
			hubEnabled: true,
			wantHub:    "0.0.0.0",
			wantBroker: "127.0.0.1",
		},
		{
			name:       "workstation without --host keeps explicit broker host",
			brokerHost: "192.168.1.10",
			hubEnabled: true,
			wantHub:    "127.0.0.1",
			wantBroker: "192.168.1.10",
		},
		{
			name:       "hosted combo without --host is unchanged",
			hosted:     true,
			hubEnabled: true,
			wantHub:    "0.0.0.0",
			wantBroker: "0.0.0.0",
		},
		{
			name:       "hosted combo with --host moves only the hub",
			hosted:     true,
			hostSet:    true,
			host:       "10.0.0.5",
			hubEnabled: true,
			wantHub:    "10.0.0.5",
			wantBroker: "0.0.0.0",
		},
		{
			name:       "hosted standalone broker without --host uses loopback",
			hosted:     true,
			brokerHost: "10.0.0.7",
			wantHub:    "0.0.0.0",
			wantBroker: "127.0.0.1",
		},
		{
			name:       "hosted keeps configured broker host",
			hosted:     true,
			brokerHost: "10.0.0.7",
			hubEnabled: true,
			wantHub:    "0.0.0.0",
			wantBroker: "10.0.0.7",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultGlobalConfig()
			cfg.RuntimeBroker.Enabled = true
			if tt.brokerHost != "" {
				cfg.RuntimeBroker.Host = tt.brokerHost
			}

			applyServerListenHosts(&cfg, tt.hosted, tt.hostSet, tt.host, tt.hubEnabled)

			assert.Equal(t, tt.wantHub, cfg.Hub.Host, "hub listen host")
			assert.Equal(t, tt.wantBroker, cfg.RuntimeBroker.Host, "broker listen host")
		})
	}
}

func TestPrintWorkstationQuickstart(t *testing.T) {
	// Create a temp dir with a dev-token file
	dir := t.TempDir()
	token := "scion_dev_abc123"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dev-token"), []byte(token+"\n"), 0600))

	stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)

	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printWorkstationQuickstart(false, dir, nil, "127.0.0.1", 8080, true, true, true, true)

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	output := buf.String()

	assert.Contains(t, output, "http://127.0.0.1:8080", "should show web UI URL")
	assert.Contains(t, output, wantDevTokenExport(filepath.Join(dir, "dev-token")), "should show the dev token file export")
	assert.NotContains(t, output, "scion_dev_", "should not print the dev token value")
}

// TestPrintWorkstationQuickstart_ConfiguredTokenFile: with
// server.auth.dev_token_file set, the quickstart shows that file (the one
// the server uses) and reads the onboarding token from it, not from the
// default <globalDir>/dev-token.
func TestPrintWorkstationQuickstart_ConfiguredTokenFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dev-token"), []byte("scion_dev_stale\n"), 0600))
	tokenFile := filepath.Join(t.TempDir(), "custom-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(fixtureDevToken+"\n"), 0600))
	cfg := &config.GlobalConfig{Auth: config.DevAuthConfig{Enabled: true, TokenFile: tokenFile}}

	rec := stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)
	output := captureStdout(t, func() {
		printWorkstationQuickstart(false, dir, cfg, "127.0.0.1", 8080, true, true, true, true)
	})

	assert.Contains(t, output, wantDevTokenExport(tokenFile), "should show the configured token file export")
	assert.NotContains(t, output, filepath.Join(dir, "dev-token"), "should not show the default token file")
	assert.NotContains(t, output, "scion_dev_", "should not print the dev token value")
	assert.Equal(t, fixtureDevToken, rec.statusToken, "status call uses the configured token file")
}

// TestPrintWorkstationQuickstart_TokenFileNotYetCreated: the server creates
// a missing token file at startup, so the quickstart shows the path even
// when the file does not exist when it is printed.
func TestPrintWorkstationQuickstart_TokenFileNotYetCreated(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "not-yet", "dev-token")
	cfg := &config.GlobalConfig{Auth: config.DevAuthConfig{Enabled: true, TokenFile: tokenFile}}

	stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)
	output := captureStdout(t, func() {
		printWorkstationQuickstart(false, dir, cfg, "127.0.0.1", 8080, true, true, true, true)
	})

	assert.Contains(t, output, "Developer token (for CLI authentication):")
	assert.Contains(t, output, wantDevTokenExport(tokenFile), "should show the configured token file before it exists")
}

// TestPrintWorkstationQuickstart_SettingsToken: a token set in the server
// settings has no file, so the quickstart prints the plain note naming
// where it is set, not a command and not the value.
func TestPrintWorkstationQuickstart_SettingsToken(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dev-token"), []byte("scion_dev_stale\n"), 0600))
	cfg := &config.GlobalConfig{Auth: config.DevAuthConfig{Enabled: true, Token: fixtureDevToken}}

	rec := stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)
	output := captureStdout(t, func() {
		printWorkstationQuickstart(false, dir, cfg, "127.0.0.1", 8080, true, true, true, true)
	})

	assert.Contains(t, output, "  "+devTokenStartupHint(cfg, dir)+"\n", "should show the settings note")
	assert.Contains(t, output, "server.auth.dev_token", "the note names the setting")
	assert.NotContains(t, output, "SCION_DEV_TOKEN=$(cat", "should not show a file command")
	assert.NotContains(t, output, "scion_dev_", "should not print the dev token value")
	assert.Equal(t, fixtureDevToken, rec.statusToken, "status call uses the settings token")
}

// TestPrintWorkstationQuickstart_RelativeTokenFile: the daemon child runs in
// the global directory, so a relative server.auth.dev_token_file is shown
// and read from there, not from the directory 'server start' was run in.
func TestPrintWorkstationQuickstart_RelativeTokenFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(t.TempDir())
	prevGlobal := globalMode
	t.Cleanup(func() { globalMode = prevGlobal })
	globalMode = false
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tokens"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tokens", "dev-token"), []byte(fixtureDevToken+"\n"), 0600))
	cfg := &config.GlobalConfig{Auth: config.DevAuthConfig{Enabled: true, TokenFile: filepath.Join("tokens", "dev-token")}}

	rec := stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)
	output := captureStdout(t, func() {
		printWorkstationQuickstart(false, dir, cfg, "127.0.0.1", 8080, true, true, true, true)
	})

	assert.Contains(t, output, wantDevTokenExport(filepath.Join(dir, "tokens", "dev-token")), "should show the file under the global directory")
	assert.NotContains(t, output, "scion_dev_", "should not print the dev token value")
	assert.Equal(t, fixtureDevToken, rec.statusToken, "status call reads the file under the global directory")
}

// TestPrintWorkstationQuickstart_RelativeTokenFileGlobal: with --global the
// daemon child changes to the home directory, so a relative token file is
// shown and read from there.
func TestPrintWorkstationQuickstart_RelativeTokenFileGlobal(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(t.TempDir())
	prevGlobal := globalMode
	t.Cleanup(func() { globalMode = prevGlobal })
	globalMode = true
	cfg := &config.GlobalConfig{Auth: config.DevAuthConfig{Enabled: true, TokenFile: filepath.Join("tokens", "dev-token")}}

	// stubQuickstartProbes points HOME at a fresh temp dir.
	rec := stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)
	home := os.Getenv("HOME")
	require.NoError(t, os.MkdirAll(filepath.Join(home, "tokens"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "tokens", "dev-token"), []byte(fixtureDevToken+"\n"), 0600))
	output := captureStdout(t, func() {
		printWorkstationQuickstart(false, dir, cfg, "127.0.0.1", 8080, true, true, true, true)
	})

	assert.Contains(t, output, wantDevTokenExport(filepath.Join(home, "tokens", "dev-token")), "should show the file under the home directory")
	assert.NotContains(t, output, "scion_dev_", "should not print the dev token value")
	assert.Equal(t, fixtureDevToken, rec.statusToken, "status call reads the file under the home directory")
}

func TestPrintWorkstationQuickstart_NoWeb(t *testing.T) {
	dir := t.TempDir()
	stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printWorkstationQuickstart(false, dir, nil, "127.0.0.1", 8080, false, false, true, true)

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	output := buf.String()

	assert.NotContains(t, output, "Web UI", "should not show web UI when disabled")
	assert.NotContains(t, output, "SCION_DEV_TOKEN", "should not show token when dev-auth disabled")
}

func TestPrintWorkstationQuickstart_WildcardHost(t *testing.T) {
	dir := t.TempDir()
	stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printWorkstationQuickstart(false, dir, nil, "0.0.0.0", 9090, true, false, true, true)

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
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

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
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

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
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

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
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

	_ = w.Close()
	_ = wErr.Close()
	os.Stdout = old
	os.Stderr = oldErr

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	output := buf.String()

	var errBuf bytes.Buffer
	_, _ = io.Copy(&errBuf, rErr)
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
