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

package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// TestRunInit_PinsSessionMetricsBackstopCallSites pins where RunInit calls
// the session-metrics seams:
//   - the tombstone clear runs once, with agentHome, before the harness
//     starts (the child has not created its marker yet);
//   - the shutdown backstop runs once, with agentHome, after the child has
//     exited, with the classified exit outcome, and before the lifecycle
//     session-end hooks.
func TestRunInit_PinsSessionMetricsBackstopCallSites(t *testing.T) {
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)
	stubRunInitSideEffects(t)
	// Already the agent user (rootless shape): no credential drop, which
	// would need CAP_SETGID in the test process.
	origSetupHostUser := runSetupHostUser
	runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
	t.Cleanup(func() { runSetupHostUser = origSetupHostUser })

	var order []string
	origNewLifecycleManager := runNewLifecycleManager
	runNewLifecycleManager = func(string, int, int, bool) (*hooks.LifecycleManager, bool) {
		m := hooks.NewLifecycleManager()
		m.HooksDirs = []string{t.TempDir()} // no script hooks
		m.RegisterHandler(hooks.EventSessionEnd, func(*hooks.Event) error {
			order = append(order, "session-end hooks")
			return nil
		})
		return m, false
	}
	t.Cleanup(func() { runNewLifecycleManager = origNewLifecycleManager })

	// No harness exit-code file: the outcome comes from the child's own
	// exit code, whatever the host's fixed path holds.
	origExitCodePath := harnessExitCodePath
	harnessExitCodePath = filepath.Join(t.TempDir(), "no-harness-exit-code")
	t.Cleanup(func() { harnessExitCodePath = origExitCodePath })

	mark := filepath.Join(t.TempDir(), "child-ran")
	markExists := func() bool { _, err := os.Stat(mark); return err == nil }

	var clearHomes []string
	var clearSawMark bool
	origClear := runClearSessionTombstoneAtStartup
	runClearSessionTombstoneAtStartup = func(home string) {
		clearHomes = append(clearHomes, home)
		clearSawMark = markExists()
		order = append(order, "clear")
	}
	t.Cleanup(func() { runClearSessionTombstoneAtStartup = origClear })

	var backstopHomes []string
	var backstopSawMark bool
	var backstopOutcome exitOutcome
	origBackstop := runReportOpenSessionAtShutdown
	runReportOpenSessionAtShutdown = func(home string, outcome exitOutcome, newClient func() *hub.Client) {
		backstopHomes = append(backstopHomes, home)
		backstopSawMark = markExists()
		backstopOutcome = outcome
		if newClient == nil {
			t.Error("backstop got a nil client factory")
		}
		order = append(order, "backstop")
	}
	t.Cleanup(func() { runReportOpenSessionAtShutdown = origBackstop })

	// The child outlives RunInit's 100ms immediate-exit check, so RunInit
	// takes the normal shutdown path, then exits 3 (a crash).
	_ = RunInit([]string{"sh", "-c", `touch "` + mark + `"; sleep 0.5; exit 3`}, InitRunOptions{DisableTermSignalForwarding: true})

	if len(clearHomes) != 1 || clearHomes[0] != agentHome {
		t.Errorf("clear calls = %q, want one with %q", clearHomes, agentHome)
	}
	if clearSawMark {
		t.Error("tombstone clear ran after the harness started")
	}
	if len(backstopHomes) != 1 || backstopHomes[0] != agentHome {
		t.Errorf("backstop calls = %q, want one with %q", backstopHomes, agentHome)
	}
	if !backstopSawMark {
		t.Error("backstop ran before the child ran")
	}
	want := exitOutcome{exitCode: 3, isCrash: true, message: "Agent crashed with exit code 3"}
	if backstopOutcome != want {
		t.Errorf("backstop outcome = %+v, want %+v", backstopOutcome, want)
	}
	if got := len(order); got != 3 || order[0] != "clear" || order[1] != "backstop" || order[2] != "session-end hooks" {
		t.Errorf("call order = %q, want [clear backstop session-end hooks]", order)
	}
}
