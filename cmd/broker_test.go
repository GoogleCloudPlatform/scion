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
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBrokerRestartCmdRegistered(t *testing.T) {
	// Verify the restart command is registered as a subcommand of broker
	found := false
	for _, cmd := range brokerCmd.Commands() {
		if cmd.Use == "restart" {
			found = true
			break
		}
	}
	assert.True(t, found, "restart should be a subcommand of broker")
}

func TestBrokerRestartCmdFlags(t *testing.T) {
	// Verify restart command has the expected flags
	portFlag := brokerRestartCmd.Flags().Lookup("port")
	assert.NotNil(t, portFlag, "--port flag should be registered")
	// 0 means "not set": restart then keeps the running daemon's port
	// (see resolveBrokerRestartOptions), else uses DefaultBrokerPort.
	assert.Equal(t, "0", portFlag.DefValue, "unset --port should keep the daemon's port")

	autoProvideFlag := brokerRestartCmd.Flags().Lookup("auto-provide")
	assert.NotNil(t, autoProvideFlag, "--auto-provide flag should be registered")
	assert.Equal(t, "false", autoProvideFlag.DefValue, "default auto-provide should be false")

	debugFlag := brokerRestartCmd.Flags().Lookup("debug")
	assert.NotNil(t, debugFlag, "--debug flag should be registered")
	assert.Equal(t, "false", debugFlag.DefValue, "default debug should be false")
}

func TestBrokerRestartCmdMetadata(t *testing.T) {
	assert.Equal(t, "restart", brokerRestartCmd.Use)
	assert.Equal(t, "Restart the Runtime Broker daemon", brokerRestartCmd.Short)
	assert.NotEmpty(t, brokerRestartCmd.Long)
	assert.NotNil(t, brokerRestartCmd.RunE)
}

func TestBrokerCmdLongDescriptionIncludesRestart(t *testing.T) {
	assert.Contains(t, brokerCmd.Long, "restart")
}

// TestBuildBrokerForegroundArgsIncludesForeground is the regression guard for
// a bug where `runtime-broker start --foreground` used to build the
// underlying `server start` args without `--foreground`, so
// serverStartCmd.RunE (runServerStartOrDaemon) daemonized instead of running
// inline. Under a systemd Type=simple unit that meant the parent exited
// immediately and the orphaned daemon child got reaped by cgroup cleanup.
// --foreground must always be present, and first, regardless of the other
// flags.
func TestBuildBrokerForegroundArgsIncludesForeground(t *testing.T) {
	got := buildBrokerForegroundArgs(DefaultBrokerPort, false, false)
	assert.Equal(t, []string{"--foreground", "--hosted", "--enable-runtime-broker"}, got)
	assert.Equal(t, "--foreground", got[0], "--foreground must be forwarded to the underlying server start invocation")
}

// TestBuildBrokerForegroundArgsForwardsAllFlags pins the full constructed
// argv for every combination of the other runtime-broker start flags, so a
// future change cannot silently drop --auto-provide, --debug, or the
// non-default port the way --foreground was dropped.
func TestBuildBrokerForegroundArgsForwardsAllFlags(t *testing.T) {
	assert.Equal(t,
		[]string{"--foreground", "--hosted", "--enable-runtime-broker", "--runtime-broker-port=9801", "--auto-provide", "--debug"},
		buildBrokerForegroundArgs(9801, true, true),
	)
	assert.Equal(t,
		[]string{"--foreground", "--hosted", "--enable-runtime-broker", "--auto-provide"},
		buildBrokerForegroundArgs(DefaultBrokerPort, true, false),
	)
	assert.Equal(t,
		[]string{"--foreground", "--hosted", "--enable-runtime-broker", "--debug"},
		buildBrokerForegroundArgs(DefaultBrokerPort, false, true),
	)
}

// TestBuildBrokerDaemonArgsForwardsAllFlags pins the argv used to launch (or
// relaunch, on restart) the broker as a background daemon: the re-exec'd
// child always runs with --foreground itself (daemon.Start is what
// backgrounds it), and the other broker start/restart flags must survive.
func TestBuildBrokerDaemonArgsForwardsAllFlags(t *testing.T) {
	assert.Equal(t,
		[]string{"server", "start", "--foreground", "--hosted", "--enable-runtime-broker"},
		buildBrokerDaemonArgs(DefaultBrokerPort, false, false),
	)
	assert.Equal(t,
		[]string{"server", "start", "--foreground", "--hosted", "--enable-runtime-broker", "--runtime-broker-port=9801", "--auto-provide", "--debug"},
		buildBrokerDaemonArgs(9801, true, true),
	)
}

// TestBrokerRegistrationCapabilities_IncludesEmptyPerAgentWorkspace pins
// that the broker CLI reports emptyPerAgentWorkspace at join (design #2703
// P2); the hub's capabilitiesFromStrings parses it.
func TestBrokerRegistrationCapabilities_IncludesEmptyPerAgentWorkspace(t *testing.T) {
	if !slices.Contains(brokerRegistrationCapabilities(), "emptyPerAgentWorkspace") {
		t.Errorf("brokerRegistrationCapabilities() = %v, want it to include emptyPerAgentWorkspace", brokerRegistrationCapabilities())
	}
}

// TestBrokerRegistrationCapabilities_IncludesAgentMove pins that the broker
// CLI reports agentMove at join (ptone/scion#2727); the hub refuses a move
// unless both brokers report it.
func TestBrokerRegistrationCapabilities_IncludesAgentMove(t *testing.T) {
	if !slices.Contains(brokerRegistrationCapabilities(), "agentMove") {
		t.Errorf("brokerRegistrationCapabilities() = %v, want it to include agentMove", brokerRegistrationCapabilities())
	}
}
