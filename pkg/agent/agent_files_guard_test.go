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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// guardTestTree is a project at <root>/proj with its .scion directory and
// one agent directory, agents/a, plus a project file.
type guardTestTree struct {
	root, proj, scion, agentDir, projectFile string
}

func newGuardTestTree(t *testing.T) guardTestTree {
	t.Helper()
	root := t.TempDir()
	tr := guardTestTree{
		root:  root,
		proj:  filepath.Join(root, "proj"),
		scion: filepath.Join(root, "proj", config.DotScion),
	}
	tr.agentDir = filepath.Join(tr.scion, "agents", "a")
	tr.projectFile = filepath.Join(tr.proj, "src", "main.go")
	mustMkdir(t, filepath.Join(tr.agentDir, "home"))
	mustMkdir(t, filepath.Dir(tr.projectFile))
	mustWrite(t, tr.projectFile, "package main")
	return tr
}

func (tr guardTestTree) guard() *agentFilesGuard {
	return newAgentFilesGuard("a", []string{tr.scion}, []string{tr.agentDir}, []string{tr.proj})
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// checkErr returns check's error, for assertRefused.
func checkErr(_ string, err error) error { return err }

func assertRefused(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, ErrAgentFilesDeleteRefused) {
		t.Errorf("%s: err = %v, want ErrAgentFilesDeleteRefused", what, err)
	}
}

func TestAgentFilesGuard_AllowsTheAgentsOwnDirectory(t *testing.T) {
	tr := newGuardTestTree(t)
	g := tr.guard()
	for _, p := range []string{tr.agentDir, filepath.Join(tr.agentDir, "home")} {
		if _, err := g.check(p); err != nil {
			t.Errorf("check(%s) = %v, want allowed", p, err)
		}
	}
}

func TestAgentFilesGuard_RefusesTheProjectWorkspace(t *testing.T) {
	tr := newGuardTestTree(t)
	g := tr.guard()
	for what, p := range map[string]string{
		"project root":                     tr.proj,
		"project subdirectory":             filepath.Dir(tr.projectFile),
		"directory containing the project": tr.root,
		"the .scion directory":             tr.scion,
		"another agent's directory":        mkdirReturn(t, filepath.Join(tr.scion, "agents", "b")),
	} {
		assertRefused(t, checkErr(g.check(p)), what)
	}
}

func mkdirReturn(t *testing.T, dir string) string {
	t.Helper()
	mustMkdir(t, dir)
	return dir
}

// A symlink inside the agent directory that points into the project, and an
// agent directory that is itself a symlink to the project, are refused.
func TestAgentFilesGuard_RefusesSymlinkIntoProject(t *testing.T) {
	tr := newGuardTestTree(t)
	link := filepath.Join(tr.agentDir, "workspace")
	mustSymlink(t, tr.proj, link)
	assertRefused(t, checkErr(tr.guard().check(link)), "symlink to the project root")

	linkedAgent := filepath.Join(tr.scion, "agents", "b")
	mustSymlink(t, filepath.Dir(tr.projectFile), linkedAgent)
	g := newAgentFilesGuard("b", []string{tr.scion}, []string{linkedAgent}, []string{tr.proj})
	assertRefused(t, checkErr(g.check(linkedAgent)), "agent directory symlinked into the project")
}

// An agents directory that is a symlink into the project makes the agent's
// directory resolve to a project directory of the same name: it is not the
// agent's, and is refused.
func TestAgentFilesGuard_RefusesSymlinkedAgentsDir(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	scion := filepath.Join(proj, config.DotScion)
	mustMkdir(t, filepath.Join(proj, "docs"))
	mustMkdir(t, scion)
	mustSymlink(t, proj, filepath.Join(scion, "agents"))
	agentDir := filepath.Join(scion, "agents", "docs")

	g := newAgentFilesGuard("docs", []string{scion}, []string{agentDir}, []string{proj})
	assertRefused(t, checkErr(g.check(agentDir)), "agent directory reached through a symlinked agents dir")
}

func TestAgentFilesGuard_RefusesUnresolvablePaths(t *testing.T) {
	tr := newGuardTestTree(t)
	g := tr.guard()
	dangling := filepath.Join(tr.agentDir, "dangling")
	mustSymlink(t, filepath.Join(tr.root, "missing"), dangling)
	loop := filepath.Join(tr.agentDir, "loop")
	mustSymlink(t, loop, loop)
	for what, p := range map[string]string{
		"dangling symlink": dangling,
		"symlink loop":     loop,
		"missing path":     filepath.Join(tr.agentDir, "missing"),
		"relative path":    "agents/a",
		"empty path":       "",
	} {
		assertRefused(t, checkErr(g.check(p)), what)
	}

	// A project workspace that cannot be resolved (not merely missing)
	// refuses every path.
	bad := newAgentFilesGuard("a", []string{tr.scion}, []string{tr.agentDir}, []string{loop})
	assertRefused(t, checkErr(bad.check(tr.agentDir)), "unresolvable project workspace")
}

// Containment compares whole path elements: /x/proj does not contain
// /x/project2, and /x/a does not contain /x/ab.
func TestAgentFilesGuard_PathElementBoundary(t *testing.T) {
	root := t.TempDir()
	proj := mkdirReturn(t, filepath.Join(root, "proj"))
	project2 := mkdirReturn(t, filepath.Join(root, "project2"))
	g := newAgentFilesGuard("proj", []string{root}, []string{proj}, []string{project2})
	if _, err := g.check(proj); err != nil {
		t.Errorf("check(%s) with project workspace %s = %v, want allowed", proj, project2, err)
	}

	agents := mkdirReturn(t, filepath.Join(root, "agents"))
	a := mkdirReturn(t, filepath.Join(agents, "a"))
	ab := mkdirReturn(t, filepath.Join(agents, "ab"))
	g = newAgentFilesGuard("a", []string{agents}, []string{a}, nil)
	assertRefused(t, checkErr(g.check(ab)), "sibling whose name extends the agent's")
}

// An extra agent root is verified like the agent's own: a symlink there
// does not make where it points the agent's.
func TestAgentFilesGuard_ExtraRootIsVerified(t *testing.T) {
	tr := newGuardTestTree(t)
	g := tr.guard()
	creator := mkdirReturn(t, filepath.Join(tr.scion, "agents", "creator", "workspace"))
	if _, err := g.check(creator, creator); err != nil {
		t.Errorf("check of a verified extra root = %v, want allowed", err)
	}
	linked := filepath.Join(tr.scion, "agents", "other")
	mustSymlink(t, filepath.Dir(tr.projectFile), linked)
	assertRefused(t, checkErr(g.check(linked, linked)), "extra root symlinked into the project")
}

// DeleteAgentFiles refuses an agent directory that is a symlink into the
// project, and the project survives.
func TestDeleteAgentFiles_RefusesAgentDirSymlinkedIntoProject(t *testing.T) {
	tr := newGuardTestTree(t)
	t.Setenv("HOME", tr.root)
	link := filepath.Join(tr.scion, "agents", "b")
	mustSymlink(t, filepath.Dir(tr.projectFile), link)

	_, err := DeleteAgentFiles("b", tr.scion, false)
	assertRefused(t, err, "DeleteAgentFiles")
	if _, err := os.Stat(tr.projectFile); err != nil {
		t.Errorf("project file removed: %v", err)
	}
}

// DeleteAgentFiles refuses an agent directory reached through an agents
// directory that is a symlink into the project, where a project directory
// has the agent's name; that project directory survives.
func TestDeleteAgentFiles_RefusesSymlinkedAgentsDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	proj := filepath.Join(root, "proj")
	scion := filepath.Join(proj, config.DotScion)
	docs := filepath.Join(proj, "docs", "guide.md")
	mustMkdir(t, filepath.Dir(docs))
	mustWrite(t, docs, "# guide")
	mustMkdir(t, scion)
	mustSymlink(t, proj, filepath.Join(scion, "agents"))

	_, err := DeleteAgentFiles("docs", scion, false)
	assertRefused(t, err, "DeleteAgentFiles")
	if _, err := os.Stat(docs); err != nil {
		t.Errorf("project directory with the agent's name was removed: %v", err)
	}
}

// Deleting one agent of a project leaves another agent's directory and the
// project's files as they were, for agent directories in the project and
// in the external agents root of a shared workspace.
func TestDeleteAgentFiles_SiblingAndProjectSurvive(t *testing.T) {
	t.Run("in-project agent directories", func(t *testing.T) {
		tr := newGuardTestTree(t)
		t.Setenv("HOME", tr.root)
		sibling := filepath.Join(tr.scion, "agents", "b", "home", "notes.md")
		mustMkdir(t, filepath.Dir(sibling))
		mustWrite(t, sibling, "keep")

		if _, err := DeleteAgentFiles("a", tr.scion, false); err != nil {
			t.Fatalf("DeleteAgentFiles: %v", err)
		}
		if _, err := os.Stat(tr.agentDir); !os.IsNotExist(err) {
			t.Errorf("agent directory not removed (stat err %v)", err)
		}
		for _, p := range []string{sibling, tr.projectFile} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s removed: %v", p, err)
			}
		}
	})
	t.Run("shared workspace with external agent directories", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("HOME", root)
		t.Setenv("SCION_HOST_UID", "")
		proj := filepath.Join(root, "proj")
		mustMkdir(t, proj)
		setupGitRepo(t, proj)
		scion := filepath.Join(proj, config.DotScion)
		mustMkdir(t, scion)
		if err := config.WriteProjectID(scion, "550e8400-e29b-41d4-a716-446655440000"); err != nil {
			t.Fatal(err)
		}
		shared := filepath.Join(proj, "README.md")
		mustWrite(t, shared, "shared checkout")
		ext, err := config.GetGitProjectExternalAgentsDir(scion)
		if err != nil || ext == "" {
			t.Fatalf("GetGitProjectExternalAgentsDir: %q, %v", ext, err)
		}
		for _, name := range []string{"a", "b"} {
			mustMkdir(t, filepath.Join(ext, name, "home"))
			mustWrite(t, filepath.Join(ext, name, "scion-agent.json"), "{}")
		}

		if _, err := DeleteAgentFiles("a", scion, false); err != nil {
			t.Fatalf("DeleteAgentFiles: %v", err)
		}
		if _, err := os.Stat(filepath.Join(ext, "a")); !os.IsNotExist(err) {
			t.Errorf("external agent directory not removed (stat err %v)", err)
		}
		for _, p := range []string{filepath.Join(ext, "b", "scion-agent.json"), shared} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s removed: %v", p, err)
			}
		}
	})
}

// check returns the path to remove: the real location of the parent
// directory joined with the last element, which is kept as it is, so a
// symlink there is unlinked, not followed.
func TestAgentFilesGuard_CheckReturnsRealParentPath(t *testing.T) {
	tr := newGuardTestTree(t)
	linkRoot := filepath.Join(t.TempDir(), "via-link")
	mustSymlink(t, tr.root, linkRoot)
	lexScion := filepath.Join(linkRoot, "proj", config.DotScion)
	lexAgent := filepath.Join(lexScion, "agents", "a")
	g := newAgentFilesGuard("a", []string{lexScion}, []string{lexAgent}, []string{filepath.Join(linkRoot, "proj")})
	realAgent, err := resolveRealPath(tr.agentDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.check(lexAgent)
	if err != nil || got != realAgent {
		t.Errorf("check(%s) = %q, %v; want %q", lexAgent, got, err, realAgent)
	}

	leaf := filepath.Join(tr.agentDir, "home-link")
	mustSymlink(t, filepath.Join(tr.agentDir, "home"), leaf)
	got, err = tr.guard().check(leaf)
	if want := filepath.Join(realAgent, "home-link"); err != nil || got != want {
		t.Errorf("check(%s) = %q, %v; want %q (the link itself)", leaf, got, err, want)
	}
}

// A13 (R1): the stale-directory removal (GetAgent) refuses an agent
// directory reached through an agents directory that is a symlink into the
// project, with the project's guard and with the guard that knows only the
// agent directory; the project directory with the agent's name survives.
func TestHandleAgentDirWithoutConfig_RefusesSymlinkedAgentsDir(t *testing.T) {
	for _, which := range []string{"project guard", "agent directory guard"} {
		t.Run(which, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			proj := filepath.Join(root, "proj")
			scion := filepath.Join(proj, config.DotScion)
			docs := filepath.Join(proj, "docs", "guide.md")
			mustMkdir(t, filepath.Dir(docs))
			mustWrite(t, docs, "# guide")
			mustMkdir(t, scion)
			mustSymlink(t, proj, filepath.Join(scion, "agents"))
			agentDir := filepath.Join(scion, "agents", "docs")
			home := filepath.Join(agentDir, "home")
			ws := filepath.Join(agentDir, "workspace")

			var err error
			if which == "project guard" {
				err = handleAgentDirWithoutConfigGuarded(projectAgentFilesGuard(context.Background(), scion, "docs", agentDir), "docs", agentDir, home, ws)
			} else {
				err = handleAgentDirWithoutConfig("docs", agentDir, home, ws)
			}
			assertRefused(t, err, "stale-directory removal")
			if _, err := os.Stat(docs); err != nil {
				t.Errorf("project directory with the agent's name was removed: %v", err)
			}
		})
	}
}

// A12 (O3): the repository holding a project in a subdirectory anchors the
// last sharer's worktree, which lies in the repository outside the project
// directory.
func TestProjectAgentFilesGuard_RepoRootAnchorsWorktreeOutsideProject(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	repo := filepath.Join(root, "repo")
	mustMkdir(t, repo)
	setupGitRepo(t, repo)
	scion := filepath.Join(repo, "nested", "project", config.DotScion)
	mustMkdir(t, scion)
	wt := mkdirReturn(t, filepath.Join(repo, ".scion-worktrees", "branch-a"))

	g := projectAgentFilesGuard(context.Background(), scion, "a")
	if _, err := g.check(wt, wt); err != nil {
		t.Errorf("check of the last sharer's worktree in the repository = %v, want allowed", err)
	}
	src := mkdirReturn(t, filepath.Join(repo, "src"))
	assertRefused(t, checkErr(g.check(src)), "repository directory outside the agent's directories")
}

// A12 (O4): when the project's settings cannot be loaded, the delete path
// refuses every removal rather than fall back to defaults.
func TestDeleteAgentFiles_RefusesWhenSettingsCannotBeLoaded(t *testing.T) {
	tr := newGuardTestTree(t)
	t.Setenv("HOME", tr.root)
	mustWrite(t, filepath.Join(tr.scion, "settings.yaml"), "runtimes: [unclosed\n")

	_, err := DeleteAgentFiles("a", tr.scion, false)
	assertRefused(t, err, "DeleteAgentFiles with unreadable settings")
	if _, err := os.Stat(tr.agentDir); err != nil {
		t.Errorf("agent directory removed although the guard could not be prepared: %v", err)
	}
}

// The partial-worktree cleanup's check: a worktree that is really inside
// the checkout's worktrees directory may be removed; one that is a symlink
// to the checkout is refused, and so is the checkout itself.
func TestCheckAgentWorktreeRemoval(t *testing.T) {
	base := t.TempDir()
	wt := mkdirReturn(t, filepath.Join(base, "worktrees", "a"))
	if _, err := CheckAgentWorktreeRemoval("a", base, wt); err != nil {
		t.Errorf("worktree inside the checkout: %v, want allowed", err)
	}
	linked := filepath.Join(base, "worktrees", "b")
	mustSymlink(t, base, linked)
	assertRefused(t, checkErr(CheckAgentWorktreeRemoval("b", base, linked)), "worktree symlinked to the checkout")
	assertRefused(t, checkErr(CheckAgentWorktreeRemoval("a", base, base)), "the checkout itself")
}
