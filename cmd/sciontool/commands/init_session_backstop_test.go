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
// (child gets the marker path as $1 and must create it first, and the
// limits trigger file path as $2), with every
// shutdown step of interest recorded through its seam.
func pinBackstopCallSites(t *testing.T, child string, setup func(t *testing.T, order *[]string)) backstopPinResult {
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
	// A temp limits trigger file, never the fixed /tmp path, which a real
	// init on this host could act on. The child gets it as $2.
	trigger := filepath.Join(t.TempDir(), "limits-exceeded")
	origTrigger := limitsTriggerPath
	limitsTriggerPath = trigger
	t.Cleanup(func() { limitsTriggerPath = origTrigger })
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

	if setup != nil {
		setup(t, &r.order)
	}

	_ = RunInit([]string{"sh", "-c", child, "harness", mark, trigger}, InitRunOptions{DisableTermSignalForwarding: true})

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
// the session-metrics seams, for a crash, a harness exiting with the
// limits-exceeded code, and a limit signalled through the trigger file:
//   - the tombstone clear runs once, with agentHome, before the harness
//     starts (the child has not created its marker yet);
//   - the shutdown backstop runs once, with agentHome, after the child has
//     exited, with the classified exit outcome, and before the stopping
//     report, the sidecar shutdown and the lifecycle session-end hooks (and,
//     on the trigger-file path, after the limits_exceeded report).
func TestRunInit_PinsSessionMetricsBackstopCallSites(t *testing.T) {
	// The children outlive RunInit's 100ms immediate-exit check, so RunInit
	// takes the normal shutdown path.
	for _, tc := range []struct {
		name      string
		child     string
		setup     func(t *testing.T, order *[]string)
		want      exitOutcome
		wantOrder []string
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
		{
			// A hook process detects a limit and writes the trigger file;
			// init reports limits_exceeded, stops the harness, then runs
			// the backstop.
			name: "limits trigger file",
			// The trigger is rewritten until init stops the child, because
			// RunInit removes a stale trigger file after the child starts.
			child: `touch "$1"; while :; do sleep 0.2; touch "$2"; done`,
			setup: func(t *testing.T, order *[]string) {
				t.Setenv("SCION_MAX_TURNS", "1")
				trigger := limitsTriggerPath
				origReport := runReportHookLimitsExceeded
				runReportHookLimitsExceeded = func(_ *handlers.HubHandler, path string) {
					if path != trigger {
						t.Errorf("limits report read %q, want %q", path, trigger)
					}
					*order = append(*order, "limits report")
				}
				t.Cleanup(func() { runReportHookLimitsExceeded = origReport })
			},
			want:      exitOutcome{exitCode: handlers.ExitCodeLimitsExceeded, limitsExceeded: true},
			wantOrder: []string{"clear", "limits report", "backstop", "stopping report", "sidecar shutdown", "session-end hooks"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := pinBackstopCallSites(t, tc.child, tc.setup)
			if r.backstopOutcome != tc.want {
				t.Errorf("backstop outcome = %+v, want %+v", r.backstopOutcome, tc.want)
			}
			// The backstop must come before the slow shutdown steps, which
			// can use up the runtime's stop window.
			wantOrder := tc.wantOrder
			if wantOrder == nil {
				wantOrder = []string{"clear", "backstop", "stopping report", "sidecar shutdown", "session-end hooks"}
			}
			if !reflect.DeepEqual(r.order, wantOrder) {
				t.Errorf("call order = %q, want %q", r.order, wantOrder)
			}
		})
	}
}
