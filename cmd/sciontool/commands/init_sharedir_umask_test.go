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

//go:build linux

package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

// runInitChildUmask drives the real RunInit with a harness child that
// records its own umask, and returns that umask plus the umask in effect
// when setupHostUser ran. The test process's umask is restored afterwards.
func runInitChildUmask(t *testing.T, env string, granted []int) (child, atSetup string) {
	t.Helper()
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })

	setupRunInitAsRootlessScion(t, t.TempDir())
	stubRunInitSideEffects(t)
	t.Setenv(suppgroups.EnvVar, env)
	t.Cleanup(suppgroups.SetGetgroupsForTest(func() ([]int, error) { return granted, nil }))

	orig := runSetupHostUser
	runSetupHostUser = func(rpd bool) (int, int, bool) {
		m := syscall.Umask(0)
		syscall.Umask(m)
		atSetup = fmt.Sprintf("%04o", m)
		// Already the agent user (rootless shape): no credential drop, which
		// would need CAP_SETGID in the test process.
		return 0, 0, true
	}
	t.Cleanup(func() { runSetupHostUser = orig })

	out := filepath.Join(t.TempDir(), "umask")
	if rc := RunInit([]string{"sh", "-c", "umask > " + out}, InitRunOptions{DisableTermSignalForwarding: true}); rc != 0 {
		t.Fatalf("RunInit() = %d, want 0", rc)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("harness child did not run: %v", err)
	}
	return strings.TrimSpace(string(data)), atSetup
}

// Granted nfs shared-dir groups: init applies umask 002 before
// setupHostUser, and the harness child inherits it (ptone/scion#3155).
func TestRunInit_SharedDirGroups_Umask002InheritedByChild(t *testing.T) {
	child, atSetup := runInitChildUmask(t, "4242", []int{0, 4242})
	if child != "0002" {
		t.Errorf("harness child umask = %s, want 0002", child)
	}
	if atSetup != "0002" {
		t.Errorf("umask at setupHostUser = %s, want 0002 (must be applied before it)", atSetup)
	}
}

func TestRunInit_NoSharedDirGroups_UmaskUnchanged(t *testing.T) {
	for name, tc := range map[string]struct {
		env     string
		granted []int
	}{
		"unset":     {"", []int{0, 4242}},
		"invalid":   {"x,0,-1", []int{0, 4242}},
		"ungranted": {"4242", []int{0}},
	} {
		t.Run(name, func(t *testing.T) {
			if child, _ := runInitChildUmask(t, tc.env, tc.granted); child != "0022" {
				t.Errorf("harness child umask = %s, want unchanged 0022", child)
			}
		})
	}
}
