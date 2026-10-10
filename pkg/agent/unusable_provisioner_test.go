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

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

const (
	unusableBuiltinHC      = "harness: generic\nimage: scion-base:test\nuser: scion\nprovisioner:\n  type: builtin\n  interface_version: 1\n"
	unusableEmptyCommandHC = "harness: generic\nimage: scion-base:test\nuser: scion\nprovisioner:\n  type: container-script\n  interface_version: 1\n"
)

func assertUnusableProvisionerErr(t *testing.T, err error, name, wantFix string) {
	t.Helper()
	var ue *harness.UnusableProvisionerError
	if !errors.As(err, &ue) || !errors.Is(err, harness.ErrUnusableProvisioner) {
		t.Fatalf("expected an UnusableProvisionerError, got %v", err)
	}
	if ue.Name != name {
		t.Errorf("error names %q, want %q", ue.Name, name)
	}
	if !strings.Contains(err.Error(), wantFix) {
		t.Errorf("error does not name the fix %q: %v", wantFix, err)
	}
}

// An unusable provisioner (legacy "builtin", or a container-script
// provisioner without a command) fails Start before any container runs,
// with or without a harness-config policy attached, for a new agent and for
// a relaunch of an agent whose harness-config became unusable after it was
// provisioned (ptone/scion#611).
func TestStart_UnusableProvisionerFailsLaunch(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"builtin", unusableBuiltinHC},
		{"empty-command", unusableEmptyCommandHC},
	} {
		for _, withPolicy := range []bool{false, true} {
			label := tc.name + "/no-policy"
			if withPolicy {
				label = tc.name + "/policy"
			}
			t.Run(label+"/new-agent", func(t *testing.T) {
				e := newPolicyTestEnv(t)
				e.projectHC(t, "hc-bad", tc.body)
				ctx := context.Background()
				if withPolicy {
					ctx = (&recordingPolicy{}).ctx()
				}
				runs := 0
				_, err := policyTestManager(&runs).Start(ctx, api.StartOptions{
					Name: "fresh", ProjectPath: e.scion, HarnessConfig: "hc-bad", NoAuth: true,
				})
				assertUnusableProvisionerErr(t, err, "hc-bad", projectFix(e))
				if runs != 0 {
					t.Errorf("container ran %d times; want none", runs)
				}
			})
			t.Run(label+"/relaunch", func(t *testing.T) {
				e := newPolicyTestEnv(t)
				e.projectHC(t, "hc-bad", policyTestScripted)
				runs := 0
				mgr := policyTestManager(&runs)
				opts := api.StartOptions{Name: "relaunch", ProjectPath: e.scion, HarnessConfig: "hc-bad", NoAuth: true}
				if _, err := mgr.Start(context.Background(), opts); err != nil {
					t.Fatalf("first Start: %v", err)
				}
				// The harness-config becomes unusable after provisioning.
				writePolicyHC(t, filepath.Join(e.scion, "harness-configs", "hc-bad"), tc.body)
				ctx := context.Background()
				if withPolicy {
					ctx = (&recordingPolicy{}).ctx()
				}
				runsBefore := runs
				_, err := mgr.Start(ctx, opts)
				assertUnusableProvisionerErr(t, err, "hc-bad", projectFix(e))
				if runs != runsBefore {
					t.Error("the container must not run for an unusable provisioner")
				}
			})
		}
	}
}

// projectFix is the advice for the project-scoped hc-bad: edit its
// config.yaml (`upgrade` operates only on the global directory).
func projectFix(e *policyTestEnv) string {
	return "To fix it, edit " + filepath.Join(e.scion, "harness-configs", "hc-bad", "config.yaml")
}

// A global harness-config of a bundled harness type is repaired with
// `upgrade`, and the error says so.
func TestStart_UnusableProvisionerGlobalNamesUpgrade(t *testing.T) {
	e := newPolicyTestEnv(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	writePolicyHC(t, filepath.Join(home, ".scion", "harness-configs", "hc-global"),
		"harness: claude\nimage: scion-claude:test\nuser: scion\nprovisioner:\n  type: builtin\n")
	runs := 0
	_, err = policyTestManager(&runs).Start(context.Background(), api.StartOptions{
		Name: "global", ProjectPath: e.scion, HarnessConfig: "hc-global", NoAuth: true,
	})
	assertUnusableProvisionerErr(t, err, "hc-global", "scion harness-config upgrade hc-global --activate-script")
	if runs != 0 {
		t.Errorf("container ran %d times; want none", runs)
	}
}
