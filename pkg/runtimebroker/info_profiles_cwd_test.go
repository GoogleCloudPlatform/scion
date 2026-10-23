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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// The profiles a broker advertises come from its global settings, never
// from a project found from the process working directory: a broker
// started inside a project checkout (standalone, or co-located with a hub)
// advertises the same profiles as one started anywhere else
// (ptone/scion#5363).
func TestBuildInfoProfiles_IgnoresWorkingDirectoryProject(t *testing.T) {
	for _, k := range []string{"SCION_PROJECT", "SCION_PROJECT_ID", "SCION_HUB_PROJECT_ID", "SCION_ACTIVE_PROFILE"} {
		t.Setenv(k, "")
	}
	// No process-wide DB overlay: the settings files alone decide.
	oldOverlay := config.GetGlobalSettingsOverlay()
	config.SetGlobalSettingsOverlay(nil)
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(oldOverlay) })
	writeHomeSettings(t, `schema_version: "1"
active_profile: global-prof
runtimes:
  docker:
    type: docker
profiles:
  global-prof:
    runtime: docker
`)
	root := t.TempDir()
	projectDir := filepath.Join(root, ".scion")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(`schema_version: "1"
profiles:
  project-prof:
    runtime: docker
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	srv := &Server{runtime: &runtime.MockRuntime{NameFunc: func() string { return "docker" }}}
	byName := infoProfilesByName(srv.buildInfoProfiles("docker"))
	if _, ok := byName["global-prof"]; !ok {
		t.Errorf("profiles = %v, want global-prof from the global settings", byName)
	}
	if _, ok := byName["project-prof"]; ok {
		t.Errorf("profiles = %v: project-prof comes from the working directory's project", byName)
	}
}
