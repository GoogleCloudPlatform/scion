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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// testProjectMarker is a valid .scion marker for the project-path tests.
var testProjectMarker = &config.ProjectMarker{
	ProjectID:   "0b9d7f2e-6c1a-4d3e-9f80-1a2b3c4d5e6f",
	ProjectName: "marker-project",
	ProjectSlug: "marker-project",
}

// mkdirs creates each directory (and its parents).
func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// requireDirEmpty fails unless dir exists and holds no entries.
func requireDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("%s: expected no entries, found %v", dir, names)
	}
}

// requireNotInitialized fails unless err is the refusal for configured, and
// names only that path.
func requireNotInitialized(t *testing.T, err error, configured string, mustNotName ...string) {
	t.Helper()
	if !errors.Is(err, ErrProjectPathNotInitialized) {
		t.Fatalf("expected ErrProjectPathNotInitialized, got %v", err)
	}
	if !config.IsAgentStateConflict(err) {
		t.Errorf("expected the refusal to be an agent-state conflict (409), got %v", err)
	}
	var pe *ProjectPathNotInitializedError
	if !errors.As(err, &pe) || pe.Path != configured {
		t.Errorf("expected the refusal to name the configured path %q, got %v", configured, err)
	}
	for _, other := range mustNotName {
		if strings.Contains(err.Error(), other) {
			t.Errorf("the refusal must name only the configured path, but it names %q: %v", other, err)
		}
	}
}

func checkPath(t *testing.T, projectPath string) error {
	t.Helper()
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		t.Fatalf("resolve %s: %v", projectPath, err)
	}
	return checkExplicitProjectPath(projectPath, projectDir, "", false)
}

func TestCheckExplicitProjectPath(t *testing.T) {
	p := newHermeticProject(t)
	base := filepath.Join(p.home, "paths")

	t.Run("bare directory is refused", func(t *testing.T) {
		bare := filepath.Join(base, "bare")
		mkdirs(t, bare)
		requireNotInitialized(t, checkPath(t, bare), bare)
	})
	t.Run("directory with .scion is accepted", func(t *testing.T) {
		if err := checkPath(t, p.root); err != nil {
			t.Fatalf("expected %s to be accepted: %v", p.root, err)
		}
	})
	t.Run("path naming the .scion directory is accepted", func(t *testing.T) {
		if err := checkPath(t, p.scion); err != nil {
			t.Fatalf("expected %s to be accepted: %v", p.scion, err)
		}
	})
	t.Run("configured path that is a symlink to an initialized directory is accepted", func(t *testing.T) {
		link := filepath.Join(base, "link-to-project")
		mkdirs(t, base)
		if err := os.Symlink(p.root, link); err != nil {
			t.Fatal(err)
		}
		if err := checkPath(t, link); err != nil {
			t.Fatalf("expected %s to be accepted: %v", link, err)
		}
	})
	t.Run(".scion symlink leading out of the path is refused", func(t *testing.T) {
		other := filepath.Join(base, "elsewhere")
		redirect := filepath.Join(base, "redirect")
		mkdirs(t, filepath.Join(other, config.DotScion), redirect)
		if err := os.Symlink(filepath.Join(other, config.DotScion), filepath.Join(redirect, config.DotScion)); err != nil {
			t.Fatal(err)
		}
		requireNotInitialized(t, checkPath(t, redirect), redirect, other)
	})
	t.Run("invalid marker is refused", func(t *testing.T) {
		bad := filepath.Join(base, "bad-marker")
		mkdirs(t, bad)
		if err := os.WriteFile(filepath.Join(bad, config.DotScion), []byte("project-id: \"../escape\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		requireNotInitialized(t, checkPath(t, bad), bad)
	})
	t.Run("valid marker is accepted", func(t *testing.T) {
		marked := filepath.Join(base, "marked")
		mkdirs(t, marked)
		if err := config.WriteProjectMarker(filepath.Join(marked, config.DotScion), testProjectMarker); err != nil {
			t.Fatal(err)
		}
		if err := checkPath(t, marked); err != nil {
			t.Fatalf("expected a valid marker to be accepted: %v", err)
		}
	})
	t.Run("not checked without an explicit path, with a workspace or an own workspace", func(t *testing.T) {
		bare := filepath.Join(base, "bare-exempt")
		mkdirs(t, bare)
		projectDir, err := config.GetResolvedProjectDir(bare)
		if err != nil {
			t.Fatal(err)
		}
		for name, err := range map[string]error{
			"no path":       checkExplicitProjectPath("", projectDir, "", false),
			"workspace":     checkExplicitProjectPath(bare, projectDir, "/some/workspace", false),
			"own workspace": checkExplicitProjectPath(bare, projectDir, "", true),
		} {
			if err != nil {
				t.Errorf("%s: expected no check, got %v", name, err)
			}
		}
	})
}

// workspaceSourceOf returns the /workspace volume source of cfg, or ws when
// the agent uses its own workspace directory.
func workspaceSourceOf(t *testing.T, cfg *api.ScionConfig, ws string) string {
	t.Helper()
	if cfg != nil {
		for _, v := range cfg.Volumes {
			if v.Target == "/workspace" {
				return v.Source
			}
		}
	}
	if ws == "" {
		t.Fatal("no /workspace volume and no agent workspace")
	}
	return ws
}

func evalPath(t *testing.T, p string) string {
	t.Helper()
	e, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("eval %s: %v", p, err)
	}
	return e
}

// TestProvisionAgentExplicitProjectPath provisions plain projects through
// ProvisionAgent with an explicit project path, as a Runtime Broker does with
// a provider's LocalPath.
func TestProvisionAgentExplicitProjectPath(t *testing.T) {
	p := newHermeticProject(t)
	base := filepath.Join(p.home, "local-paths")

	t.Run("initialized path is the workspace source", func(t *testing.T) {
		_, ws, cfg, err := ProvisionAgent(context.Background(), "init-agent", "default", "", "", p.root, "", "", "", "")
		if err != nil {
			t.Fatalf("ProvisionAgent: %v", err)
		}
		if got, want := evalPath(t, workspaceSourceOf(t, cfg, ws)), evalPath(t, p.root); got != want {
			t.Fatalf("workspace source = %q, want the configured path %q", got, want)
		}
	})
	t.Run("bare path is refused and nothing is written under it", func(t *testing.T) {
		parent := filepath.Join(base, "bare-parent")
		bare := filepath.Join(parent, "bare")
		mkdirs(t, bare)
		_, _, _, err := ProvisionAgent(context.Background(), "bare-agent", "default", "", "", bare, "", "", "", "")
		requireNotInitialized(t, err, bare)
		requireDirEmpty(t, bare)
		entries, _ := os.ReadDir(parent)
		if len(entries) != 1 {
			t.Fatalf("expected the parent %s to hold only %s, found %d entries", parent, bare, len(entries))
		}
	})
	t.Run(".scion symlink leading out of the path is refused before any write", func(t *testing.T) {
		other := filepath.Join(base, "elsewhere")
		redirect := filepath.Join(base, "redirect")
		mkdirs(t, filepath.Join(other, config.DotScion), redirect)
		if err := os.Symlink(filepath.Join(other, config.DotScion), filepath.Join(redirect, config.DotScion)); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := ProvisionAgent(context.Background(), "redirect-agent", "default", "", "", redirect, "", "", "", "")
		requireNotInitialized(t, err, redirect, other)
		requireDirEmpty(t, filepath.Join(other, config.DotScion))
	})
	t.Run("invalid marker is refused before any write", func(t *testing.T) {
		bad := filepath.Join(base, "bad-marker")
		mkdirs(t, bad)
		if err := os.WriteFile(filepath.Join(bad, config.DotScion), []byte("project-id: \"../escape\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := ProvisionAgent(context.Background(), "bad-marker-agent", "default", "", "", bad, "", "", "", "")
		requireNotInitialized(t, err, bad)
		entries, _ := os.ReadDir(bad)
		if len(entries) != 1 {
			t.Fatalf("expected %s to hold only its marker, found %d entries", bad, len(entries))
		}
	})
	t.Run("valid marker gives the agent its own project-configs workspace", func(t *testing.T) {
		marked := filepath.Join(base, "marked")
		mkdirs(t, marked)
		if err := config.WriteProjectMarker(filepath.Join(marked, config.DotScion), testProjectMarker); err != nil {
			t.Fatal(err)
		}
		external, err := testProjectMarker.ExternalProjectPath()
		if err != nil {
			t.Fatal(err)
		}
		if err := config.InitProject(external, getTestHarnesses()); err != nil {
			t.Fatalf("InitProject(%s): %v", external, err)
		}
		_, ws, cfg, err := ProvisionAgent(context.Background(), "marker-agent", "default", "", "", marked, "", "", "", "")
		if err != nil {
			t.Fatalf("ProvisionAgent: %v", err)
		}
		got := evalPath(t, workspaceSourceOf(t, cfg, ws))
		configs := evalPath(t, filepath.Dir(external))
		if !strings.HasPrefix(got, configs+string(filepath.Separator)) {
			t.Fatalf("workspace source = %q, want the agent's own workspace under %q", got, configs)
		}
		for _, notAllowed := range []string{evalPath(t, marked), evalPath(t, filepath.Dir(marked))} {
			if got == notAllowed {
				t.Fatalf("workspace source = %q, which is the configured path or its parent", got)
			}
		}
	})
}

// TestStartRefusesUninitializedProjectPath: a start (create, start or
// restart all enter here) of a plain project whose explicit path holds no
// .scion entry is refused before any side effect: no runtime call and
// nothing written under the path.
func TestStartRefusesUninitializedProjectPath(t *testing.T) {
	p := newHermeticProject(t)
	bare := filepath.Join(p.home, "start-bare")
	mkdirs(t, bare)
	ran := false
	mgr := NewManager(&runtime.MockRuntime{
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			ran = true
			return "mock-id", nil
		},
	})
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:           "start-agent",
		ProjectPath:    bare,
		NoAuth:         true,
		FreshProvision: true,
	})
	requireNotInitialized(t, err, bare)
	if ran {
		t.Fatal("the runtime was called for a refused start")
	}
	requireDirEmpty(t, bare)
}

// TestCheckedPlainProjectWorkspaceSource pins the provisioning backstop: a
// plain workspace is derived only from a .scion directory, never from a bare
// directory, whatever path led there.
func TestCheckedPlainProjectWorkspaceSource(t *testing.T) {
	root := t.TempDir()
	if got, err := checkedPlainProjectWorkspaceSource(filepath.Join(root, config.DotScion)); err != nil || got != root {
		t.Fatalf("a .scion directory: got %q, %v; want %q", got, err, root)
	}
	if _, err := checkedPlainProjectWorkspaceSource(root); !errors.Is(err, ErrProjectPathNotInitialized) {
		t.Fatalf("a bare directory: want ErrProjectPathNotInitialized, got %v", err)
	}
}

// TestCheckExplicitProjectPath_Exemptions pins the paths the check leaves
// alone because their agents never take the parent of projectDir as the
// workspace: the global project (by keyword or directory) and a git work
// tree.
func TestCheckExplicitProjectPath_Exemptions(t *testing.T) {
	p := newHermeticProject(t)
	if err := config.InitGlobal(getTestHarnesses()); err != nil {
		t.Fatalf("InitGlobal: %v", err)
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"global", "home", globalDir} {
		projectDir, err := config.GetResolvedProjectDir(path)
		if err != nil {
			t.Fatalf("resolve %s: %v", path, err)
		}
		if err := checkExplicitProjectPath(path, projectDir, "", false); err != nil {
			t.Errorf("%s: expected the global project to be exempt, got %v", path, err)
		}
	}

	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	work := filepath.Join(p.home, "git-work-tree")
	mkdirs(t, work)
	if out, err := exec.Command(gitBin, "-C", work, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	projectDir, err := config.GetResolvedProjectDir(work)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkExplicitProjectPath(work, projectDir, "", false); err != nil {
		t.Errorf("a git work tree without .scion is out of scope and exempt, got %v", err)
	}
}

// TestProjectPathNotInitializedErrorText: the message names the configured
// path and its .scion directory (once, also for a path naming .scion), and
// says the .scion must be a directory, not a symlink elsewhere.
func TestProjectPathNotInitializedErrorText(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/srv/proj", "create /srv/proj/.scion as a directory"},
		{"/srv/proj/.scion", "create /srv/proj/.scion as a directory"},
	} {
		msg := (&ProjectPathNotInitializedError{Path: tc.path}).Error()
		if !strings.Contains(msg, tc.want) || !strings.Contains(msg, "not a symlink to another location") || strings.Contains(msg, ".scion/.scion") {
			t.Errorf("Path %q: message %q, want it to contain %q and the symlink note", tc.path, msg, tc.want)
		}
	}
}
