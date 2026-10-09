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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

func TestConfigureDebugOutput(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		explicit   bool
		scionDebug string
		logLevel   string
		want       bool
	}{
		{name: "agent mode ignores inherited SCION_DEBUG", mode: "agent", scionDebug: "1", want: false},
		{name: "agent mode with --debug", mode: "agent", explicit: true, scionDebug: "1", want: true},
		{name: "agent mode with SCION_LOG_LEVEL=debug", mode: "agent", scionDebug: "1", logLevel: "debug", want: true},
		{name: "human mode honours SCION_DEBUG", mode: "human", scionDebug: "1", want: true},
		{name: "assistant mode honours SCION_DEBUG", mode: "assistant", scionDebug: "1", want: true},
		{name: "human mode with nothing set", mode: "human", want: false},
		{name: "human mode with --debug", mode: "human", explicit: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(func() { util.SetExplicitDebug(false); util.SetAgentDebugPolicy(false) })
			t.Setenv("SCION_CLI_MODE", tt.mode)
			t.Setenv("SCION_DEBUG", tt.scionDebug)
			t.Setenv("SCION_LOG_LEVEL", tt.logLevel)

			configureDebugOutput(resolveMode(), tt.explicit)

			if got := util.DebugEnabled(); got != tt.want {
				t.Errorf("util.DebugEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigureDebugOutput_ClearsEarlierExplicitDebug(t *testing.T) {
	t.Cleanup(func() { util.SetExplicitDebug(false); util.SetAgentDebugPolicy(false) })
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("SCION_DEBUG", "1")
	t.Setenv("SCION_LOG_LEVEL", "")

	configureDebugOutput(ModeAgent, true)
	configureDebugOutput(ModeAgent, false)

	if util.DebugEnabled() {
		t.Error("debug output still enabled after an invocation without --debug")
	}
}

// TestRootPreRun_AgentModeIgnoresInheritedSCIONDebug runs the real root
// command so that removing the configureDebugOutput call from the
// persistent pre-run hook fails a test.
func TestRootPreRun_AgentModeIgnoresInheritedSCIONDebug(t *testing.T) {
	restoreAllSilenceUsage(t)
	origNonInteractive, origAutoConfirm, origDebug := nonInteractive, autoConfirm, debugMode
	t.Cleanup(func() {
		nonInteractive, autoConfirm, debugMode = origNonInteractive, origAutoConfirm, origDebug
		util.SetExplicitDebug(false)
		util.SetAgentDebugPolicy(false)
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("SCION_DEBUG", "1")
	t.Setenv("SCION_LOG_LEVEL", "")
	t.Setenv("SCION_HOST_UID", "")

	// Simulate state left over from an earlier explicit request.
	util.EnableDebug()

	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"version"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("scion version: %v", err)
	}

	if util.DebugEnabled() {
		t.Error("debug output enabled in agent mode with only an inherited SCION_DEBUG")
	}
}
