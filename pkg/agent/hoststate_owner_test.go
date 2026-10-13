// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for the files-owner run record in the host-owned host state file
// (ptone/scion#4771): where it lives for each project layout, when no
// trusted root exists, and what a reader makes of its content.

const (
	ownerHubID   = "0a1b2c3d-1111-2222-3333-444455556666"
	ownerOtherID = "9f8e7d6c-1111-2222-3333-444455556666"
	ownerAgent   = "dev"
)

// hostStateRun returns the runId in the host state file of a local
// (no hub) agent of projectPath, without the legacy fallback.
func hostStateRun(t *testing.T, projectPath, name string) string {
	t.Helper()
	return hostStateRunFor(t, RunOwnerTarget{ProjectPath: projectPath, AgentName: name})
}

func hostStateRunFor(t *testing.T, target RunOwnerTarget) string {
	t.Helper()
	root, err := HostStateRootFor(target)
	if err != nil {
		t.Fatalf("HostStateRootFor(%+v): %v", target, err)
	}
	hs, err := ReadHostState(root, target.AgentName)
	if err != nil {
		t.Fatalf("ReadHostState: %v", err)
	}
	return hs.RunID
}

// writeHostStateRun records runID for a local agent of projectPath, whose
// agent dir must exist.
func writeHostStateRun(t *testing.T, projectPath, name, runID string) {
	t.Helper()
	target := RunOwnerTarget{ProjectPath: projectPath, AgentName: name}
	if err := RecordRunOwner(target, runID); err != nil {
		t.Fatalf("RecordRunOwner: %v", err)
	}
	if got := hostStateRunFor(t, target); got != runID {
		t.Fatalf("host state run = %q after recording %q", got, runID)
	}
}

// agentInfoRunID returns the runId key of the agent-info.json at path, ""
// when the file or the key is missing.
func agentInfoRunID(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	var info map[string]any
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("agent-info.json: %v", err)
	}
	s, _ := info["runId"].(string)
	return s
}

// hostStateFilesUnder lists every host state file below dir.
func hostStateFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == HostStateFile {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// ownerLayout is a temp HOME (so ~/.scion/project-configs is private) and
// a source dir for linked projects, both with symlinks resolved.
type ownerLayout struct {
	home, global, src string
}

func newOwnerLayout(t *testing.T) ownerLayout {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := ownerLayout{home: filepath.Join(tmp, "home"), src: filepath.Join(tmp, "src")}
	l.global = filepath.Join(l.home, config.GlobalDir)
	for _, d := range []string{l.global, l.src} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", l.home)
	t.Chdir(tmp)
	return l
}

// external returns ~/.scion/project-configs/<slug>__<short id>.
func (l ownerLayout) external(slug, projectID string) string {
	return filepath.Join(l.global, config.ProjectConfigsDir, config.ProjectMarker{ProjectID: projectID, ProjectSlug: slug}.DirName())
}

func mkdirOwnerDirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeOwnerFile(t *testing.T, path, content string) {
	t.Helper()
	mkdirOwnerDirs(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// symlinkDirInPlace moves the directory dir aside and puts a symlink to
// it in its place.
func symlinkDirInPlace(t *testing.T, dir string) {
	t.Helper()
	moved := dir + "-real"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, dir); err != nil {
		t.Fatal(err)
	}
}

// realProject creates <parent>/<name>/.scion as a directory with the agent
// dir, and returns the project root.
func (l ownerLayout) realProject(t *testing.T, parent, name string) string {
	t.Helper()
	p := filepath.Join(parent, name)
	mkdirOwnerDirs(t, filepath.Join(p, config.DotScion, "agents", ownerAgent))
	return p
}

// markerProject creates <parent>/<name> whose .scion is a marker naming
// projectID and slug, and the external dir it resolves to (with the agent
// dir) for that ID and slug. It returns the project root.
func (l ownerLayout) markerProject(t *testing.T, parent, name, projectID, slug string) string {
	t.Helper()
	p := filepath.Join(parent, name)
	mkdirOwnerDirs(t, p, filepath.Join(l.external(slug, projectID), config.DotScion, "agents", ownerAgent))
	if err := config.WriteProjectMarker(filepath.Join(p, config.DotScion), &config.ProjectMarker{
		ProjectID: projectID, ProjectName: slug, ProjectSlug: slug,
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

// The root for each project layout, and the refusals. Each accepted root
// is checked against the agent dir agentStateDir (GetAgent's resolution)
// picks for the same inputs, so the record sits in the agent's own dir.
func TestHostStateRootFor_Layouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup returns the target and the expected root ("" = refused).
		setup func(t *testing.T, l ownerLayout) (RunOwnerTarget, string)
	}{
		{"linked project, worktree mode", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.realProject(t, l.src, "repo")
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}, p
		}},
		{"linked project addressed by its .scion", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.realProject(t, l.src, "repo")
			return RunOwnerTarget{ProjectPath: filepath.Join(p, config.DotScion), AgentName: ownerAgent, HubProjectID: ownerHubID}, p
		}},
		{"linked project with a marker", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.markerProject(t, l.src, "plain", ownerHubID, "plain")
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}, l.external("plain", ownerHubID)
		}},
		{"hub-native project with a marker", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.markerProject(t, filepath.Join(l.global, "projects"), "myproj", ownerHubID, "myproj")
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}, l.external("myproj", ownerHubID)
		}},
		{"hub-native project with a committed .scion", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.realProject(t, filepath.Join(l.global, "projects"), "myrepo")
			if err := config.WriteProjectID(filepath.Join(p, config.DotScion), ownerHubID); err != nil {
				t.Fatal(err)
			}
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}, p
		}},
		{"shared workspace", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.realProject(t, l.src, "shared")
			ext := l.external("shared", ownerHubID)
			mkdirOwnerDirs(t, filepath.Join(ext, config.DotScion, "agents", ownerAgent))
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID, SharedWorkspace: true}, ext
		}},
		{"shared workspace found from the external scion-agent.json", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.realProject(t, l.src, "shared")
			ext := l.external("shared", ownerHubID)
			writeOwnerFile(t, filepath.Join(ext, config.DotScion, "agents", ownerAgent, "scion-agent.json"), "{}")
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}, ext
		}},
		{"local project, no hub", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.realProject(t, l.src, "local")
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent}, p
		}},
		{"refused: local project with a marker, no hub", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.markerProject(t, l.src, "plain", ownerHubID, "plain")
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent}, ""
		}},
		{"refused: shared workspace, no hub", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.realProject(t, l.src, "shared")
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, SharedWorkspace: true}, ""
		}},
		{"refused: symlinked .scion", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			real := l.realProject(t, l.src, "real")
			p := filepath.Join(l.src, "linked")
			mkdirOwnerDirs(t, p)
			if err := os.Symlink(filepath.Join(real, config.DotScion), filepath.Join(p, config.DotScion)); err != nil {
				t.Fatal(err)
			}
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}, ""
		}},
		{"refused: marker slug differs from the project's", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			// The dir the broker-held slug names exists, so only the slug
			// check refuses it.
			p := l.markerProject(t, filepath.Join(l.global, "projects"), "myproj", ownerHubID, "other")
			mkdirOwnerDirs(t, filepath.Join(l.external("myproj", ownerHubID), config.DotScion, "agents", ownerAgent))
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}, ""
		}},
		{"refused: marker project ID differs from the hub's", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			// The dir the hub's ID names exists, so only the ID check
			// refuses it.
			p := l.markerProject(t, filepath.Join(l.global, "projects"), "myproj", ownerOtherID, "myproj")
			mkdirOwnerDirs(t, filepath.Join(l.external("myproj", ownerHubID), config.DotScion, "agents", ownerAgent))
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}, ""
		}},
		{"global project", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			mkdirOwnerDirs(t, filepath.Join(l.global, "agents", ownerAgent))
			return RunOwnerTarget{ProjectPath: "global", AgentName: ownerAgent, HubProjectID: ownerHubID}, l.home
		}},
		{"global project, symlinked home, shared workspace", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			// $HOME is a symlink: the slug is still "global", as config
			// names the global project from the unresolved global dir.
			linkHome := filepath.Join(filepath.Dir(l.home), "link-home")
			if err := os.Symlink(l.home, linkHome); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", linkHome)
			ext := filepath.Join(linkHome, config.GlobalDir, config.ProjectConfigsDir,
				config.ProjectMarker{ProjectID: ownerHubID, ProjectSlug: "global"}.DirName())
			mkdirOwnerDirs(t, filepath.Join(ext, config.DotScion, "agents", ownerAgent))
			return RunOwnerTarget{ProjectPath: "global", AgentName: ownerAgent, HubProjectID: ownerHubID, SharedWorkspace: true}, ext
		}},
		{"shared workspace, project dir name with __", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			// config names the project from the part before "__" and
			// slugifies it.
			p := l.realProject(t, l.src, "My__x")
			ext := l.external("my", ownerHubID)
			mkdirOwnerDirs(t, filepath.Join(ext, config.DotScion, "agents", ownerAgent))
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID, SharedWorkspace: true}, ext
		}},
		{"refused: marker, external dir is a symlink", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.markerProject(t, filepath.Join(l.global, "projects"), "myproj", ownerHubID, "myproj")
			symlinkDirInPlace(t, l.external("myproj", ownerHubID))
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}, ""
		}},
		{"refused: marker, external .scion is a symlink", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.markerProject(t, filepath.Join(l.global, "projects"), "myproj", ownerHubID, "myproj")
			symlinkDirInPlace(t, filepath.Join(l.external("myproj", ownerHubID), config.DotScion))
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}, ""
		}},
		{"refused: marker, hub project ID outside the dir-name grammar", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := filepath.Join(l.src, "plain")
			mkdirOwnerDirs(t, p)
			writeOwnerFile(t, filepath.Join(p, config.DotScion), "project-id: bad id!\nproject-slug: plain\n")
			// The dir that ID would give exists, so only the grammar check
			// refuses it.
			mkdirOwnerDirs(t, filepath.Join(l.global, config.ProjectConfigsDir,
				config.ProjectMarker{ProjectID: "bad id!", ProjectSlug: "plain"}.DirName(), config.DotScion, "agents", ownerAgent))
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: "bad id!"}, ""
		}},
		{"refused: shared workspace, external dir is a symlink", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.realProject(t, l.src, "shared")
			ext := l.external("shared", ownerHubID)
			mkdirOwnerDirs(t, filepath.Join(ext, config.DotScion, "agents", ownerAgent))
			symlinkDirInPlace(t, ext)
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID, SharedWorkspace: true}, ""
		}},
		{"refused: shared workspace, external dir missing", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := l.realProject(t, l.src, "shared")
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID, SharedWorkspace: true}, ""
		}},
		{"refused: shared workspace, slug outside the dir-name grammar", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			// The name slugifies to "". The dir that name would give
			// exists, so only the grammar check refuses it.
			p := l.realProject(t, l.src, "日本")
			mkdirOwnerDirs(t, filepath.Join(l.global, config.ProjectConfigsDir,
				config.ProjectMarker{ProjectID: ownerHubID, ProjectSlug: ""}.DirName(), config.DotScion, "agents", ownerAgent))
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID, SharedWorkspace: true}, ""
		}},
		{"refused: shared workspace, hub project ID outside the dir-name grammar", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			// The dir that ID would give exists, so only the grammar
			// check refuses it.
			p := l.realProject(t, l.src, "shared")
			mkdirOwnerDirs(t, filepath.Join(l.global, config.ProjectConfigsDir,
				config.ProjectMarker{ProjectID: "bad id!", ProjectSlug: "shared"}.DirName(), config.DotScion, "agents", ownerAgent))
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: "bad id!", SharedWorkspace: true}, ""
		}},
		{"shared workspace found from a symlinked external scion-agent.json", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			// Stat, as effectiveSharedWorkspace checks it.
			p := l.realProject(t, l.src, "shared")
			ext := l.external("shared", ownerHubID)
			agentDir := filepath.Join(ext, config.DotScion, "agents", ownerAgent)
			writeOwnerFile(t, filepath.Join(agentDir, "scion-agent.real.json"), "{}")
			if err := os.Symlink("scion-agent.real.json", filepath.Join(agentDir, "scion-agent.json")); err != nil {
				t.Fatal(err)
			}
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}, ext
		}},
		{"refused: no .scion", func(t *testing.T, l ownerLayout) (RunOwnerTarget, string) {
			p := filepath.Join(l.src, "empty")
			mkdirOwnerDirs(t, p)
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}, ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newOwnerLayout(t)
			target, want := tc.setup(t, l)
			got, err := HostStateRootFor(target)
			if want == "" {
				if err == nil || !errors.Is(err, errHostStateRootRefused) {
					t.Fatalf("HostStateRootFor = %q, %v; want a refusal", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("HostStateRootFor: %v", err)
			}
			if got != want {
				t.Fatalf("root = %s, want %s", got, want)
			}
			projectDir, err := config.GetResolvedProjectDir(target.ProjectPath)
			if err != nil {
				t.Fatal(err)
			}
			agentDir, _, err := agentStateDir(projectDir, ownerAgent, target.SharedWorkspace, target.HubProjectID, target.HubProjectID != "")
			if err != nil {
				t.Fatal(err)
			}
			if wantDir := filepath.Join(got, config.DotScion, "agents", ownerAgent); !sameDir(agentDir, wantDir) {
				t.Errorf("agentStateDir = %s, want the record's dir %s", agentDir, wantDir)
			}
		})
	}
}

// A marker whose project ID is not the hub's gives no root: nothing is read
// from or written to the external dir of either ID.
func TestHostStateRootFor_MismatchedMarkerProjectID(t *testing.T) {
	l := newOwnerLayout(t)
	p := l.markerProject(t, filepath.Join(l.global, "projects"), "myproj", ownerOtherID, "myproj")
	mkdirOwnerDirs(t, filepath.Join(l.external("myproj", ownerHubID), config.DotScion, "agents", ownerAgent))
	target := RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}

	if err := RecordRunOwner(target, "run-1"); !errors.Is(err, errHostStateRootRefused) {
		t.Fatalf("RecordRunOwner = %v, want a refusal", err)
	}
	if files := hostStateFilesUnder(t, l.home); len(files) != 0 {
		t.Errorf("host state files written: %v", files)
	}
	writeOwnerFile(t, filepath.Join(l.external("myproj", ownerOtherID), config.DotScion, "agents", ownerAgent, HostStateFile), `{"runId":"run-other"}`)
	if got, err := RecordedRunOwner(target); err == nil {
		t.Errorf("RecordedRunOwner = %q, nil; want unknown", got)
	}
}

// A project-id file in a .scion directory, edited to name another project,
// does not change the root: worktree mode uses the project root, shared
// mode the external dir of the hub's project ID.
func TestHostStateRootFor_EditedProjectIDFile(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "worktree", true: "shared"}[shared], func(t *testing.T) {
			l := newOwnerLayout(t)
			p := l.realProject(t, l.src, "repo")
			for _, id := range []string{ownerHubID, ownerOtherID} {
				mkdirOwnerDirs(t, filepath.Join(l.external("repo", id), config.DotScion, "agents", ownerAgent))
			}
			target := RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID, SharedWorkspace: shared}
			before, err := HostStateRootFor(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := config.WriteProjectID(filepath.Join(p, config.DotScion), ownerOtherID); err != nil {
				t.Fatal(err)
			}
			after, err := HostStateRootFor(target)
			if err != nil {
				t.Fatal(err)
			}
			want := p
			if shared {
				want = l.external("repo", ownerHubID)
			}
			if before != want || after != want {
				t.Fatalf("root before = %s, after = %s; want %s both times", before, after, want)
			}
			if err := RecordRunOwner(target, "run-1"); err != nil {
				t.Fatal(err)
			}
			wantFile := filepath.Join(want, config.DotScion, "agents", ownerAgent, HostStateFile)
			if files := hostStateFilesUnder(t, filepath.Dir(l.home)); len(files) != 1 || files[0] != wantFile {
				t.Errorf("host state files = %v, want only %s", files, wantFile)
			}
		})
	}
}

// The content of a host state file never selects a root: a valid file in
// a candidate dir does not make a refused layout acceptable, nor move an
// accepted one.
func TestHostStateRootFor_IgnoresHostStateContent(t *testing.T) {
	l := newOwnerLayout(t)
	refused := l.markerProject(t, filepath.Join(l.global, "projects"), "myproj", ownerHubID, "other")
	for _, slug := range []string{"myproj", "other"} {
		writeOwnerFile(t, filepath.Join(l.external(slug, ownerHubID), config.DotScion, "agents", ownerAgent, HostStateFile), `{"runId":"run-1"}`)
	}
	if got, err := HostStateRootFor(RunOwnerTarget{ProjectPath: refused, AgentName: ownerAgent, HubProjectID: ownerHubID}); err == nil {
		t.Errorf("a host state file made a refused layout acceptable: root %s", got)
	}

	p := l.realProject(t, l.src, "repo")
	writeOwnerFile(t, filepath.Join(l.external("repo", ownerHubID), config.DotScion, "agents", ownerAgent, HostStateFile), `{"runId":"run-1"}`)
	if got, err := HostStateRootFor(RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}); err != nil || got != p {
		t.Errorf("root = %s, %v; want the project root %s", got, err, p)
	}
}

// No write goes through a refused root, whatever the layout: nothing is
// created under the home or the source tree, and a symlinked host state
// file's target is left as it is.
func TestRecordRunOwner_RefusedRootWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, l ownerLayout) RunOwnerTarget
	}{
		{"symlinked .scion", func(t *testing.T, l ownerLayout) RunOwnerTarget {
			real := l.realProject(t, l.src, "real")
			p := filepath.Join(l.src, "linked")
			mkdirOwnerDirs(t, p)
			if err := os.Symlink(filepath.Join(real, config.DotScion), filepath.Join(p, config.DotScion)); err != nil {
				t.Fatal(err)
			}
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}
		}},
		{"marker slug differs", func(t *testing.T, l ownerLayout) RunOwnerTarget {
			p := l.markerProject(t, filepath.Join(l.global, "projects"), "myproj", ownerHubID, "other")
			mkdirOwnerDirs(t, filepath.Join(l.external("myproj", ownerHubID), config.DotScion, "agents", ownerAgent))
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}
		}},
		{"marker project ID differs", func(t *testing.T, l ownerLayout) RunOwnerTarget {
			p := l.markerProject(t, filepath.Join(l.global, "projects"), "myproj", ownerOtherID, "myproj")
			mkdirOwnerDirs(t, filepath.Join(l.external("myproj", ownerHubID), config.DotScion, "agents", ownerAgent))
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}
		}},
		{"marker, external dir is a symlink", func(t *testing.T, l ownerLayout) RunOwnerTarget {
			p := l.markerProject(t, filepath.Join(l.global, "projects"), "myproj", ownerHubID, "myproj")
			symlinkDirInPlace(t, l.external("myproj", ownerHubID))
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}
		}},
		{"shared workspace, external dir is a symlink", func(t *testing.T, l ownerLayout) RunOwnerTarget {
			p := l.realProject(t, l.src, "shared")
			ext := l.external("shared", ownerHubID)
			mkdirOwnerDirs(t, filepath.Join(ext, config.DotScion, "agents", ownerAgent))
			symlinkDirInPlace(t, ext)
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID, SharedWorkspace: true}
		}},
		{"marker without hub identity", func(t *testing.T, l ownerLayout) RunOwnerTarget {
			p := l.markerProject(t, l.src, "plain", ownerHubID, "plain")
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent}
		}},
		{"shared workspace without hub identity", func(t *testing.T, l ownerLayout) RunOwnerTarget {
			p := l.realProject(t, l.src, "shared")
			return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, SharedWorkspace: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newOwnerLayout(t)
			target := tc.setup(t, l)
			if err := RecordRunOwner(target, "run-1"); !errors.Is(err, errHostStateRootRefused) {
				t.Fatalf("RecordRunOwner = %v, want a refusal", err)
			}
			if files := hostStateFilesUnder(t, filepath.Dir(l.home)); len(files) != 0 {
				t.Errorf("host state files written through a refused root: %v", files)
			}
			if got, err := RecordedRunOwner(target); err == nil {
				t.Errorf("RecordedRunOwner = %q, nil; want unknown", got)
			}
		})
	}

	t.Run("planted symlinked host state file", func(t *testing.T) {
		l := newOwnerLayout(t)
		p := l.realProject(t, filepath.Join(l.global, "projects"), "myrepo")
		other := filepath.Join(l.src, "other.json")
		writeOwnerFile(t, other, `{"runId":"run-other"}`)
		if err := os.Symlink(other, filepath.Join(p, config.DotScion, "agents", ownerAgent, HostStateFile)); err != nil {
			t.Fatal(err)
		}
		target := RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}
		if err := RecordRunOwner(target, "run-1"); err == nil {
			t.Fatal("RecordRunOwner wrote through a symlinked host state file")
		}
		if data, _ := os.ReadFile(other); string(data) != `{"runId":"run-other"}` {
			t.Errorf("symlink target changed: %s", data)
		}
		if got, err := RecordedRunOwner(target); err == nil {
			t.Errorf("RecordedRunOwner = %q, nil; want unknown", got)
		}
	})
}

// RecordRunOwner refuses an invalid run ID and writes nothing; it is a
// no-op for an agent whose dir does not exist yet.
func TestRecordRunOwner_InvalidRunIDAndMissingAgentDir(t *testing.T) {
	l := newOwnerLayout(t)
	p := l.realProject(t, l.src, "repo")
	target := RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent}
	for _, bad := range []string{"", "a,b", "run/1", "x!y"} {
		if err := RecordRunOwner(target, bad); !errors.Is(err, errInvalidRecordedRunID) {
			t.Errorf("RecordRunOwner(%q) = %v, want errInvalidRecordedRunID", bad, err)
		}
	}
	if files := hostStateFilesUnder(t, p); len(files) != 0 {
		t.Errorf("host state files written for an invalid run ID: %v", files)
	}

	missing := RunOwnerTarget{ProjectPath: p, AgentName: "not-provisioned"}
	if err := RecordRunOwner(missing, "run-1"); err != nil {
		t.Fatalf("RecordRunOwner for a missing agent dir = %v, want a no-op", err)
	}
	if _, err := os.Stat(filepath.Join(p, config.DotScion, "agents", "not-provisioned")); !os.IsNotExist(err) {
		t.Errorf("RecordRunOwner created the agent dir (stat err %v)", err)
	}
}

// A recorded value is data: a planted file in a committed .scion is read
// as the owner it names (which only keeps files for any other run), and a
// value that is not a valid run ID, or a file that cannot be read, is
// unknown.
func TestRecordedRunOwner_PlantedHostStateFile(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
		unknown             bool
	}{
		{"foreign run ID", `{"runId":"run-foreign"}`, "run-foreign", false},
		{"no run ID", `{"other":"x"}`, "", false},
		{"invalid run ID", `{"runId":"a,b"}`, "", true},
		{"corrupt", `{"runId":`, "", true},
		{"not an object", `["run-1"]`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newOwnerLayout(t)
			p := l.realProject(t, filepath.Join(l.global, "projects"), "myrepo")
			writeOwnerFile(t, filepath.Join(p, config.DotScion, "agents", ownerAgent, HostStateFile), tc.content)
			got, err := RecordedRunOwner(RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID})
			if tc.unknown {
				if err == nil {
					t.Fatalf("RecordedRunOwner = %q, nil; want unknown", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("RecordedRunOwner = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// The transitional legacy fallback: with no run in the host state file,
// the runId of the agent-info.json in the home is returned (a caller keeps
// the files when it names another run); once the host state file records a
// run, the legacy value is ignored. A symlinked, oversized or invalid
// legacy value is none.
func TestRecordedRunOwner_LegacyAgentInfoRunID(t *testing.T) {
	setup := func(t *testing.T) (RunOwnerTarget, string) {
		l := newOwnerLayout(t)
		p := l.realProject(t, l.src, "repo")
		info := filepath.Join(config.GetAgentHomePath(filepath.Join(p, config.DotScion), ownerAgent), "agent-info.json")
		return RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent}, info
	}

	t.Run("used when the host state file has no run", func(t *testing.T) {
		target, info := setup(t)
		writeOwnerFile(t, info, `{"name":"dev","runId":"run-old"}`)
		if got, err := RecordedRunOwner(target); err != nil || got != "run-old" {
			t.Fatalf("RecordedRunOwner = %q, %v; want the legacy run-old", got, err)
		}
	})
	t.Run("ignored once the host state file records a run", func(t *testing.T) {
		target, info := setup(t)
		writeOwnerFile(t, info, `{"name":"dev","runId":"run-old"}`)
		if err := RecordRunOwner(target, "run-new"); err != nil {
			t.Fatal(err)
		}
		if got, err := RecordedRunOwner(target); err != nil || got != "run-new" {
			t.Fatalf("RecordedRunOwner = %q, %v; want run-new", got, err)
		}
		if got := agentInfoRunID(t, info); got != "run-old" {
			t.Errorf("RecordRunOwner changed agent-info.json runId to %q", got)
		}
	})
	t.Run("symlinked agent-info.json is none", func(t *testing.T) {
		target, info := setup(t)
		other := filepath.Join(filepath.Dir(info), "elsewhere.json")
		writeOwnerFile(t, other, `{"runId":"run-old"}`)
		if err := os.Symlink(other, info); err != nil {
			t.Fatal(err)
		}
		if got, err := RecordedRunOwner(target); err != nil || got != "" {
			t.Fatalf("RecordedRunOwner = %q, %v; want none", got, err)
		}
	})
	t.Run("invalid legacy run ID is none", func(t *testing.T) {
		target, info := setup(t)
		writeOwnerFile(t, info, `{"runId":"a,b"}`)
		if got, err := RecordedRunOwner(target); err != nil || got != "" {
			t.Fatalf("RecordedRunOwner = %q, %v; want none", got, err)
		}
	})
	t.Run("oversized agent-info.json is none", func(t *testing.T) {
		target, info := setup(t)
		big := make([]byte, maxLegacyAgentInfoSize+1)
		for i := range big {
			big[i] = ' '
		}
		writeOwnerFile(t, info, `{"runId":"run-old"}`+string(big))
		if got, err := RecordedRunOwner(target); err != nil || got != "" {
			t.Fatalf("RecordedRunOwner = %q, %v; want none", got, err)
		}
	})
}

// updateSavedAgentInfo copies a runId an older broker left in
// agent-info.json into the host state file before it drops it; where it
// cannot (the record's dir is not the agent's own state dir), the value
// stays in agent-info.json for the fallback read. An invalid value is
// dropped.
func TestUpdateSavedAgentInfo_LegacyRunID(t *testing.T) {
	for _, tc := range []struct {
		name, legacy       string
		scionAgentJSON     bool
		wantHost, wantInfo string
	}{
		{"copied, then dropped", "run-old", true, "run-old", ""},
		{"no agent state dir: kept", "run-old", false, "", "run-old"},
		{"invalid: dropped", "a,b", true, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectDir := filepath.Join(t.TempDir(), config.DotScion)
			agentDir := filepath.Join(projectDir, "agents", ownerAgent)
			mkdirOwnerDirs(t, agentDir)
			if tc.scionAgentJSON {
				writeOwnerFile(t, filepath.Join(agentDir, "scion-agent.json"), "{}")
			}
			info := filepath.Join(config.GetAgentHomePath(projectDir, ownerAgent), "agent-info.json")
			writeOwnerFile(t, info, `{"name":"dev","phase":"stopped","runId":"`+tc.legacy+`"}`)
			if err := UpdateAgentConfig(ownerAgent, projectDir, "running", "", ""); err != nil {
				t.Fatal(err)
			}
			if got := hostStateRun(t, projectDir, ownerAgent); got != tc.wantHost {
				t.Errorf("host state run = %q, want %q", got, tc.wantHost)
			}
			if got := agentInfoRunID(t, info); got != tc.wantInfo {
				t.Errorf("agent-info.json runId = %q, want %q", got, tc.wantInfo)
			}
		})
	}
}
