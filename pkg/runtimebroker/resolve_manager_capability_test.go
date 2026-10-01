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

package runtimebroker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// perProfileRuntime is a MockRuntime that also implements the optional
// runtime.PerProfileInstancesRuntime capability, reporting the given value.
type perProfileRuntime struct {
	*runtime.MockRuntime
	perProfile bool
}

func (r *perProfileRuntime) PerProfileInstances() bool { return r.perProfile }

var _ runtime.PerProfileInstancesRuntime = (*perProfileRuntime)(nil)

// twoDockerProfilesProject writes settings with two profiles that both
// resolve to runtime type "docker": "local" via the runtimes-map key
// "docker", and "local-alt" via a differently-keyed entry "docker-alt".
func twoDockerProfilesProject(t *testing.T) string {
	t.Helper()
	projectDir := t.TempDir()
	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
  local-alt:
    runtime: docker-alt
runtimes:
  docker:
    type: docker
  docker-alt:
    type: docker
    host: unix:///var/run/alt-docker.sock
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
	return projectDir
}

func newResolveTestServer(t *testing.T, rt runtime.Runtime) *Server {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.ForceRuntime = ""
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	return New(cfg, mgr, rt)
}

// A default runtime without the capability keeps the type-only shortcut:
// every profile resolving to its type — the default profile, the active
// profile, and a second, differently-configured profile of the same type —
// is served by the default manager, and no auxiliary runtime is created.
func TestResolveManagerForOpts_NoCapability_SameTypeProfilesShareDefaultManager(t *testing.T) {
	projectDir := twoDockerProfilesProject(t)
	srv := newResolveTestServer(t, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})

	for _, profile := range []string{"", "local", "local-alt"} {
		// The runtime-type string return is irrelevant to this test: every
		// profile here resolves to type "docker" (that's the whole point —
		// same-type profiles sharing one manager), so it can never
		// distinguish the manager-identity property this test actually
		// checks.
		got, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "a", Profile: profile, ProjectPath: projectDir})
		if got != srv.manager {
			t.Errorf("profile %q: got a non-default manager, want the default manager", profile)
		}
	}
	srv.auxiliaryRuntimesMu.RLock()
	n := len(srv.auxiliaryRuntimes)
	srv.auxiliaryRuntimesMu.RUnlock()
	if n != 0 {
		t.Errorf("auxiliary runtimes = %d, want 0", n)
	}
}

// A default runtime that implements the capability but reports false is
// indistinguishable from one that doesn't implement it.
func TestResolveManagerForOpts_CapabilityFalse_KeepsShortcut(t *testing.T) {
	projectDir := twoDockerProfilesProject(t)
	srv := newResolveTestServer(t, &perProfileRuntime{
		MockRuntime: &runtime.MockRuntime{NameFunc: func() string { return "docker" }},
		perProfile:  false,
	})

	// The runtime-type string is irrelevant here too: "local-alt" still
	// resolves to type "docker" (see twoDockerProfilesProject), so it
	// cannot distinguish "default manager" from "own manager" either.
	got, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "a", Profile: "local-alt", ProjectPath: projectDir})
	if got != srv.manager {
		t.Error("capability reporting false: got a non-default manager, want the default manager")
	}
}

// A default runtime reporting per-profile instances skips the shortcut: a
// profile of the same type gets its own manager rather than the default's.
func TestResolveManagerForOpts_CapabilityTrue_SameTypeProfileGetsOwnManager(t *testing.T) {
	projectDir := twoDockerProfilesProject(t)
	srv := newResolveTestServer(t, &perProfileRuntime{
		MockRuntime: &runtime.MockRuntime{NameFunc: func() string { return "docker" }},
		perProfile:  true,
	})

	// Same note as the sibling tests above: the string return is "docker"
	// on both sides of this branch, so it can't distinguish the
	// manager-identity property under test here either.
	got, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "a", Profile: "local-alt", ProjectPath: projectDir})
	if got == srv.manager {
		t.Error("capability reporting true: got the default manager, want a manager of its own")
	}
}
