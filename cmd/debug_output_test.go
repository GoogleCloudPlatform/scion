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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

// resetLogLevelState sets the SCION_* level variables for the test and
// makes the shared level state re-read them, restoring it (including the
// SCION_DEBUG alias handling) when the test ends.
func resetLogLevelState(t *testing.T, scionDebug, logLevel string) *bytes.Buffer {
	t.Helper()
	t.Setenv(loglevel.EnvDebug, scionDebug)
	t.Setenv(loglevel.EnvLogLevel, logLevel)
	var warn bytes.Buffer
	loglevel.SetWarningOutput(&warn)
	loglevel.Reset(true)
	t.Cleanup(func() {
		loglevel.Reset(true)
		loglevel.SetWarningOutput(os.Stderr)
	})
	return &warn
}

// The --debug path (util.EnableDebug) is sticky for the process, so these
// tests check the environment-driven level directly through loglevel.
func TestConfigureDebugOutput(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		scionDebug string
		logLevel   string
		want       bool
		wantWarn   bool
	}{
		{name: "agent mode ignores inherited SCION_DEBUG", mode: "agent", scionDebug: "1", want: false},
		{name: "agent mode with SCION_LOG_LEVEL=debug", mode: "agent", scionDebug: "1", logLevel: "debug", want: true},
		{name: "agent mode with SCION_LOG_LEVEL=info", mode: "agent", logLevel: "info", want: false},
		{name: "human mode honours SCION_DEBUG", mode: "human", scionDebug: "1", want: true, wantWarn: true},
		{name: "assistant mode honours SCION_DEBUG", mode: "assistant", scionDebug: "1", want: true, wantWarn: true},
		{name: "human mode with nothing set", mode: "human", want: false},
		{name: "human mode with SCION_LOG_LEVEL=debug", mode: "human", logLevel: "debug", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warn := resetLogLevelState(t, tt.scionDebug, tt.logLevel)
			t.Setenv("SCION_CLI_MODE", tt.mode)

			configureDebugOutput(resolveMode(), false)

			if got := loglevel.DebugEnabled(""); got != tt.want {
				t.Errorf("loglevel.DebugEnabled() = %v, want %v", got, tt.want)
			}
			if gotWarn := warn.Len() > 0; gotWarn != tt.wantWarn {
				t.Errorf("deprecation warning printed = %v, want %v (%q)", gotWarn, tt.wantWarn, warn.String())
			}
		})
	}
}

// An agent-mode invocation followed by a human-mode one in the same
// process must honour SCION_DEBUG again.
func TestConfigureDebugOutput_ModeSwitchRestoresAlias(t *testing.T) {
	resetLogLevelState(t, "1", "")

	configureDebugOutput(ModeAgent, false)
	if loglevel.DebugEnabled("") {
		t.Fatal("debug output enabled in agent mode with only SCION_DEBUG")
	}
	configureDebugOutput(ModeHuman, false)
	if !loglevel.DebugEnabled("") {
		t.Error("SCION_DEBUG not honoured after switching back to human mode")
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
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	warn := resetLogLevelState(t, "1", "")
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("SCION_HOST_UID", "")

	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"version"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("scion version: %v", err)
	}

	if loglevel.DebugEnabled("") {
		t.Error("debug output enabled in agent mode with only an inherited SCION_DEBUG")
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected deprecation warning in agent mode: %q", warn.String())
	}
}

// Environment for TestExecuteHelperProcess. TestMain clears inherited
// SCION_* variables except SCION_TEST_*, so the child gets its settings
// through these and sets the real variables itself.
const (
	executeHelperEnv = "SCION_TEST_EXECUTE_HELPER"
	executeArgsEnv   = "SCION_TEST_EXECUTE_ARGS"
	executeResultTag = "debug-enabled="
)

// TestExecuteHelperProcess is not a real test: TestExecute_AgentModeQuiet
// runs the test binary again with only this test selected, and here it
// calls Execute() like main does. Execute changes the global command tree
// (agent mode removes commands) and may exit the process, so it only runs
// in that child process.
func TestExecuteHelperProcess(t *testing.T) {
	if os.Getenv(executeHelperEnv) != "1" {
		t.Skip("helper process for TestExecute_AgentModeQuiet")
	}
	if err := os.Setenv("SCION_CLI_MODE", "agent"); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("SCION_DEBUG", "1"); err != nil {
		t.Fatal(err)
	}
	loglevel.Reset(true)
	os.Args = append([]string{"scion"}, strings.Fields(os.Getenv(executeArgsEnv))...)

	Execute()

	// Reading the level here applies the environment if nothing did yet,
	// so an unguarded SCION_DEBUG would also print its warning now.
	fmt.Fprintf(os.Stdout, "\n%s%v\n", executeResultTag, loglevel.DebugEnabled(""))
	os.Exit(0)
}

// TestExecute_AgentModeQuiet runs Execute() as main does, in agent mode
// with an inherited SCION_DEBUG, and checks that no debug output or
// SCION_DEBUG deprecation warning appears. With --help, cobra skips the
// persistent pre-run hook, so only the early configureDebugOutput call in
// Execute can keep SCION_DEBUG out; removing it fails that case.
func TestExecute_AgentModeQuiet(t *testing.T) {
	for _, args := range []string{"version", "--help"} {
		t.Run(args, func(t *testing.T) {
			// Bound the child so a command that starts blocking cannot hang
			// the whole test binary.
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExecuteHelperProcess$", "-test.count=1")
			env := []string{executeHelperEnv + "=1", executeArgsEnv + "=" + args, "HOME=" + t.TempDir()}
			for _, kv := range os.Environ() {
				name, _, _ := strings.Cut(kv, "=")
				if name == "HOME" || strings.HasPrefix(name, "SCION_") {
					continue
				}
				env = append(env, kv)
			}
			cmd.Env = env
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			// After a kill, stop waiting for output pipes a stray grandchild
			// might still hold open.
			cmd.WaitDelay = 5 * time.Second

			err := cmd.Run()
			output := fmt.Sprintf("stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("scion %s: child did not finish within the timeout\n%s", args, output)
			}
			if err != nil {
				t.Fatalf("scion %s: %v\n%s", args, err, output)
			}

			if !strings.Contains(stdout.String(), executeResultTag+"false") {
				t.Errorf("scion %s: debug output enabled in agent mode with only SCION_DEBUG\n%s", args, output)
			}
			for _, unwanted := range []string{"SCION_DEBUG is deprecated", "[DEBUG]", "[hubsync]"} {
				if strings.Contains(stderr.String(), unwanted) {
					t.Errorf("scion %s: stderr contains %q\n%s", args, unwanted, output)
				}
			}
		})
	}
}
