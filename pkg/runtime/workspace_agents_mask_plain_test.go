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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// A plain workspace mount whose root holds .scion/agents (an explicit
// --workspace at a project root) gets that directory shadowed in the
// container being created, so this agent cannot see other agents' own
// directories. A workspace without it, or one that only contains a project
// deeper down, gets no mask.
func TestBuildCommonRunArgs_PlainWorkspaceMasksAgentsDir(t *testing.T) {
	run := func(ws string) string {
		t.Helper()
		args, err := buildCommonRunArgs(RunConfig{
			Harness:      &harness.Generic{},
			Name:         "b",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			Workspace:    ws,
		})
		if err != nil {
			t.Fatalf("buildCommonRunArgs: %v", err)
		}
		return strings.Join(args, " ")
	}
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".scion", "agents", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := run(project); !strings.Contains(got, "type=tmpfs,destination=/workspace/.scion/agents") {
		t.Errorf("project-root workspace: no agents-dir mask in %s", got)
	}

	// The mask applies even when .scion/agents does not exist yet, and the
	// host directory is created first so the runtime does not make a
	// root-owned mount point in the checkout.
	fresh := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fresh, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := run(fresh); !strings.Contains(got, "type=tmpfs,destination=/workspace/.scion/agents") {
		t.Errorf("project root without an agents dir: no mask in %s", got)
	}
	if fi, err := os.Stat(filepath.Join(fresh, ".scion", "agents")); err != nil || !fi.IsDir() {
		t.Errorf("the host agents dir was not created before the mount: %v", err)
	}
	plain := t.TempDir()
	if got := run(plain); strings.Contains(got, "/workspace/.scion/agents") {
		t.Errorf("workspace without an agents dir: unexpected mask in %s", got)
	}
	// Residual (documented): a workspace that is an ancestor of the project.
	if got := run(filepath.Dir(project)); strings.Contains(got, ".scion/agents") {
		t.Errorf("ancestor workspace: the mask now covers nested projects; update the item notes: %s", got)
	}
}
