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

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func symlinkOrFatal(t *testing.T, target, link string) string {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// TestSameAsPathOrAncestor checks the file-identity helper finds a directory
// through another name (a symlinked ancestor), which a string comparison does
// not.
func TestSameAsPathOrAncestor(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(target, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := symlinkOrFatal(t, root, filepath.Join(t.TempDir(), "alias"))
	inside := filepath.Join(alias, "real", "sub")
	if pathAtOrBelow(inside, target) {
		t.Fatal("fixture: the alias spelling already matches as a string")
	}
	if !sameAsPathOrAncestor(target, inside) {
		t.Error("sameAsPathOrAncestor did not find the directory through another name")
	}
	if sameAsPathOrAncestor(target, filepath.Join(root, "other")) {
		t.Error("sameAsPathOrAncestor matched an unrelated directory")
	}
}

func agentsTreeFixture(t *testing.T) (root, pc string) {
	t.Helper()
	root = t.TempDir()
	pc = filepath.Join(root, "home", ".scion", "project-configs")
	for _, d := range []string{filepath.Join(pc, "p1", ".scion", "agents", "a1", "workspace"), filepath.Join(pc, "p1", "other")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	orig := projectConfigsDirFunc
	projectConfigsDirFunc = func() (string, error) { return pc, nil }
	t.Cleanup(func() { projectConfigsDirFunc = orig })
	return root, pc
}

func TestCheckVolumeSourceOutsideAgentsTrees(t *testing.T) {
	root, pc := agentsTreeFixture(t)
	agents := filepath.Join(pc, "p1", ".scion", "agents")
	alias := symlinkOrFatal(t, filepath.Join(pc, "p1"), filepath.Join(root, "alias"))
	refused := map[string]bool{
		filepath.Join(agents, "a1"): true,
		agents:                      true,
		filepath.Join(alias, ".scion", "agents", "a1"): true,
		filepath.Join(agents, "a1") + "/../a1":         true,
		pc:                                             false,
		filepath.Join(pc, "p1"):                        false,
		filepath.Join(pc, "p1", ".scion"):              false,
	}
	for src, ro := range refused {
		if err := checkVolumeSourceOutsideAgentsTrees(src, ro); !errors.Is(err, errVolumeNamesAgentsTree) {
			t.Errorf("checkVolumeSourceOutsideAgentsTrees(%q, readOnly=%v) = %v, want errVolumeNamesAgentsTree", src, ro, err)
		}
	}
	allowed := map[string]bool{
		filepath.Join(pc, "p1", "other"): false,
		filepath.Join(pc, "p1"):          true,
		filepath.Join(root, "elsewhere"): false,
	}
	for src, ro := range allowed {
		if err := checkVolumeSourceOutsideAgentsTrees(src, ro); err != nil {
			t.Errorf("checkVolumeSourceOutsideAgentsTrees(%q, readOnly=%v) = %v, want nil", src, ro, err)
		}
	}
}

// A volume for a target that is already mounted (the workspace) is not
// checked again: it would not be added.
func TestBuildCommonRunArgs_WorkspaceVolumeNotRechecked(t *testing.T) {
	_, pc := agentsTreeFixture(t)
	ws := filepath.Join(pc, "p1", ".scion", "agents", "a1", "workspace")
	if _, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "a1",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Workspace:    ws,
		Volumes:      []api.VolumeMount{{Source: ws, Target: "/workspace"}},
	}); err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}
	_, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "a1",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Workspace:    ws,
		Volumes:      []api.VolumeMount{{Source: filepath.Dir(ws), Target: "/mnt/agent"}},
	})
	if !errors.Is(err, errVolumeNamesAgentsTree) {
		t.Fatalf("buildCommonRunArgs with an agents-tree volume: err = %v, want errVolumeNamesAgentsTree", err)
	}
}
