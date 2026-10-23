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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// layoutBind is one bind mount in a docker argument list.
type layoutBind struct {
	source, target string
	readOnly       bool
}

// layoutBinds collects the bind mounts in args, from both the -v
// src:dst[:opts] form and the --mount type=bind,... form.
func layoutBinds(t *testing.T, args []string) []layoutBind {
	t.Helper()
	var binds []layoutBind
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "-v", "--volume":
			parts := strings.Split(args[i+1], ":")
			if len(parts) < 2 {
				continue
			}
			b := layoutBind{source: parts[0], target: parts[1]}
			if len(parts) > 2 {
				for _, opt := range strings.Split(parts[2], ",") {
					if opt == "ro" {
						b.readOnly = true
					}
				}
			}
			binds = append(binds, b)
		case "--mount":
			var b layoutBind
			isBind := false
			for _, kv := range strings.Split(args[i+1], ",") {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "type":
					isBind = v == "bind"
				case "source", "src":
					b.source = v
				case "target", "destination", "dst":
					b.target = v
				case "readonly", "ro":
					b.readOnly = v == "" || v == "true" || v == "1"
				}
			}
			if isBind {
				binds = append(binds, b)
			}
		}
	}
	return binds
}

func layoutGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
	}
}

func layoutRealPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", p, err)
	}
	return r
}

// For a project checked out in a linked worktree, an agent worktree's
// layout is RepoRoot = the main repository and Workspace = the agent's
// worktree (pkg/agent detectRepoRoot). The project checkout's own .git
// pointer file, which pkg/agent reads to derive the project's common git
// dir, must never sit under a read-write bind in the agent container,
// whether the checkout is next to the main repository (the outside-root
// fallback, which mounts only <main>/.git and the workspace) or inside it
// (the worktree layout).
func TestBuildCommonRunArgs_LinkedWorktreeProjectPointerNotWritable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		checkout func(base, main string) string
	}{
		{name: "checkout next to the main repository", checkout: func(base, main string) string { return filepath.Join(base, "wt") }},
		{name: "checkout inside the main repository", checkout: func(base, main string) string { return filepath.Join(main, "wt") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("HOME", filepath.Join(base, "home"))
			main := filepath.Join(base, "main")
			if err := os.MkdirAll(main, 0o755); err != nil {
				t.Fatal(err)
			}
			layoutGit(t, main, "init", "-q")
			layoutGit(t, main, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
			checkout := tc.checkout(base, main)
			layoutGit(t, main, "worktree", "add", "-q", "-b", "wt", checkout)
			ws := filepath.Join(checkout, ".scion", "agents", "dev", "workspace")
			if err := os.MkdirAll(filepath.Dir(ws), 0o755); err != nil {
				t.Fatal(err)
			}
			layoutGit(t, checkout, "worktree", "add", "-q", "-b", "dev", ws)

			pointer := layoutRealPath(t, filepath.Join(checkout, ".git"))
			if info, err := os.Lstat(pointer); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("fixture: %s should be a .git pointer file", pointer)
			}

			args, err := buildCommonRunArgs(RunConfig{
				Harness:      &harness.Generic{},
				Name:         "dev",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				RuntimeName:  "docker",
				RepoRoot:     main,
				Workspace:    ws,
				// Both layouts (the in-repo worktree and the registered
				// outside-root worktree) now bind a per-agent private
				// .git/config on docker; seed its source so a missing config
				// source does not refuse the start and mask the layout under test.
				PrivateGitConfigSource: seedTestPrivateGitConfig(t),
			})
			if err != nil {
				t.Fatalf("buildCommonRunArgs: %v", err)
			}
			binds := layoutBinds(t, args)
			sawWorkspace := false
			for _, b := range binds {
				src := b.source
				if r, err := filepath.EvalSymlinks(src); err == nil {
					src = r
				}
				if src == layoutRealPath(t, ws) {
					sawWorkspace = true
				}
				if b.readOnly {
					continue
				}
				if rel, err := filepath.Rel(src, pointer); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					t.Errorf("the project checkout's .git pointer %s is under the read-write bind %s -> %s", pointer, b.source, b.target)
				}
			}
			if !sawWorkspace {
				t.Fatalf("fixture: the agent workspace %s is not bind-mounted; binds: %+v", ws, binds)
			}
		})
	}
}
