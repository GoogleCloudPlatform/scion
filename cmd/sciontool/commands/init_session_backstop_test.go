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
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/services"
)

// backstopPinResult is what pinBackstopCallSites observed.
type backstopPinResult struct {
	clearHomes      []string
	clearSawMark    bool
	backstopHomes   []string
	backstopSawMark bool
	backstopOutcome exitOutcome
	order           []string
}

// pinBackstopCallSites runs the real RunInit with child as the harness
// (child gets the marker path as $1 and must create it first), with every
// shutdown step of interest recorded through its seam.
func pinBackstopCallSites(t *testing.T, child string) backstopPinResult {
	t.Helper()
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)
	stubRunInitSideEffects(t)
	// Already the agent user (rootless shape): no credential drop, which
	// would need CAP_SETGID in the test process.
	origSetupHostUser := runSetupHostUser
	runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
	t.Cleanup(func() { runSetupHostUser = origSetupHostUser })

	var r backstopPinResult
	origNewLifecycleManager := runNewLifecycleManager
	runNewLifecycleManager = func(string, int, int, bool) (*hooks.LifecycleManager, bool) {
		m := hooks.NewLifecycleManager()
		m.HooksDirs = []string{t.TempDir()} // no script hooks
		m.RegisterHandler(hooks.EventSessionEnd, func(*hooks.Event) error {
			r.order = append(r.order, "session-end hooks")
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

	// One valid sidecar, so RunInit creates a services manager and its
	// shutdown step runs; starting and stopping it are stubbed.
	origReadYAML := runReadServicesYAML
	runReadServicesYAML = func(string, bool) ([]byte, error) {
		return []byte("- name: order-probe\n  command: [\"true\"]\n"), nil
	}
	t.Cleanup(func() { runReadServicesYAML = origReadYAML })
	origServicesShutdown := runServicesShutdown
	runServicesShutdown = func(context.Context, *services.Manager) error {
		r.order = append(r.order, "sidecar shutdown")
		return nil
	}
	t.Cleanup(func() { runServicesShutdown = origServicesShutdown })
	origStopping := runReportStoppingToHub
	runReportStoppingToHub = func() { r.order = append(r.order, "stopping report") }
	t.Cleanup(func() { runReportStoppingToHub = origStopping })

	mark := filepath.Join(t.TempDir(), "child-ran")
	markExists := func() bool { _, err := os.Stat(mark); return err == nil }

	origClear := runClearSessionTombstoneAtStartup
	runClearSessionTombstoneAtStartup = func(home string) {
		r.clearHomes = append(r.clearHomes, home)
		r.clearSawMark = markExists()
		r.order = append(r.order, "clear")
	}
	t.Cleanup(func() { runClearSessionTombstoneAtStartup = origClear })

	origBackstop := runReportOpenSessionAtShutdown
	runReportOpenSessionAtShutdown = func(home string, outcome exitOutcome, newClient func() *hub.Client) {
		r.backstopHomes = append(r.backstopHomes, home)
		r.backstopSawMark = markExists()
		r.backstopOutcome = outcome
		if newClient == nil {
			t.Error("backstop got a nil client factory")
		}
		r.order = append(r.order, "backstop")
	}
	t.Cleanup(func() { runReportOpenSessionAtShutdown = origBackstop })

	_ = RunInit([]string{"sh", "-c", child, "harness", mark}, InitRunOptions{DisableTermSignalForwarding: true})

	if len(r.clearHomes) != 1 || r.clearHomes[0] != agentHome {
		t.Errorf("clear calls = %q, want one with %q", r.clearHomes, agentHome)
	}
	if r.clearSawMark {
		t.Error("tombstone clear ran after the harness started")
	}
	if len(r.backstopHomes) != 1 || r.backstopHomes[0] != agentHome {
		t.Errorf("backstop calls = %q, want one with %q", r.backstopHomes, agentHome)
	}
	if !r.backstopSawMark {
		t.Error("backstop ran before the child ran")
	}
	return r
}

// TestRunInit_PinsSessionMetricsBackstopCallSites pins where RunInit calls
// the session-metrics seams, for a crash and for a limits-exceeded exit:
//   - the tombstone clear runs once, with agentHome, before the harness
//     starts (the child has not created its marker yet);
//   - the shutdown backstop runs once, with agentHome, after the child has
//     exited, with the classified exit outcome, and before the stopping
//     report, the sidecar shutdown and the lifecycle session-end hooks.
func TestRunInit_PinsSessionMetricsBackstopCallSites(t *testing.T) {
	// The children outlive RunInit's 100ms immediate-exit check, so RunInit
	// takes the normal shutdown path.
	for _, tc := range []struct {
		name  string
		child string
		want  exitOutcome
	}{
		{
			name:  "crash",
			child: `touch "$1"; sleep 0.5; exit 3`,
			want:  exitOutcome{exitCode: 3, isCrash: true, message: "Agent crashed with exit code 3"},
		},
		{
			// The harness itself exits with the limits-exceeded code.
			name:  "limits exceeded",
			child: `touch "$1"; sleep 0.5; exit ` + strconv.Itoa(handlers.ExitCodeLimitsExceeded),
			want:  exitOutcome{exitCode: handlers.ExitCodeLimitsExceeded, limitsExceeded: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := pinBackstopCallSites(t, tc.child)
			if r.backstopOutcome != tc.want {
				t.Errorf("backstop outcome = %+v, want %+v", r.backstopOutcome, tc.want)
			}
			// The backstop must come before the slow shutdown steps, which
			// can use up the runtime's stop window.
			wantOrder := []string{"clear", "backstop", "stopping report", "sidecar shutdown", "session-end hooks"}
			if !reflect.DeepEqual(r.order, wantOrder) {
				t.Errorf("call order = %q, want %q", r.order, wantOrder)
			}
		})
	}
}
