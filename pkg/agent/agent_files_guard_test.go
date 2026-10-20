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
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
// project; the project directory with the agent's name survives.
func TestHandleAgentDirWithoutConfig_RefusesSymlinkedAgentsDir(t *testing.T) {
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

	err := handleAgentDirWithoutConfigGuarded(projectAgentFilesGuard(context.Background(), scion, "docs", agentDir),
		"docs", agentDir, filepath.Join(agentDir, "home"), filepath.Join(agentDir, "workspace"))
	assertRefused(t, err, "stale-directory removal")
	if _, err := os.Stat(docs); err != nil {
		t.Errorf("project directory with the agent's name was removed: %v", err)
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

// testAgentDirGuard is the production guard (projectAgentFilesGuard) for an
// agent directory agentsRoot/<name> in a test tree, taking the directory
// holding agentsRoot as the project directory.
func testAgentDirGuard(agentName, agentDir string) *agentFilesGuard {
	return projectAgentFilesGuard(context.Background(), filepath.Dir(filepath.Dir(agentDir)), agentName, agentDir)
}

// testSweepExpiredKeptAgentDirs runs sweepExpiredKeptAgentDirsGuarded with
// testAgentDirGuard for each agent.
func testSweepExpiredKeptAgentDirs(agentsRoot, skipName string, homeOf func(name string) string) {
	sweepExpiredKeptAgentDirsGuarded(agentsRoot, skipName, homeOf, func(name string) *agentFilesGuard {
		return testAgentDirGuard(name, filepath.Join(agentsRoot, name))
	})
}

// testRemoveAgentDir runs removeAgentDirGuarded with testAgentDirGuard and
// fails the test on a refusal.
func testRemoveAgentDir(t *testing.T, agentDir, agentWorkspace string) {
	t.Helper()
	if err := removeAgentDirGuarded(testAgentDirGuard(filepath.Base(agentDir), agentDir), agentDir, agentWorkspace); err != nil {
		t.Fatalf("removeAgentDirGuarded(%s): %v", agentDir, err)
	}
}

// A15 (R1): the last sharer's worktree directory is already gone: the
// delete still succeeds, git's entry for it is removed, and a second
// delete is clean.
func TestDeleteAgentFiles_LastSharerWorktreeDirMissing(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	bare := initBareRepo(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: intPtr(0)}
	projectPath := filepath.Join(tmpDir, "proj")
	scionDir := filepath.Join(projectPath, config.DotScion)
	mustMkdir(t, filepath.Join(scionDir, "agents", "solo-agent"))
	base := filepath.Join(projectPath, "workspace")
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: provision.ResolvedWorkspace{HostPath: base, Backend: "local"}, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "solo-agent", AgentName: "solo-agent", GitClone: gc,
	}); err != nil {
		t.Fatalf("provision solo-agent: %v", err)
	}
	wtPath := provision.WorktreePath(base, "solo-agent")
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatal(err)
	}

	if _, err := DeleteAgentFiles("solo-agent", projectPath, true); err != nil {
		t.Fatalf("DeleteAgentFiles with the worktree directory missing: %v", err)
	}
	if strings.Contains(listWorktrees(t, base), wtPath) {
		t.Errorf("git still lists the removed worktree %s:\n%s", wtPath, listWorktrees(t, base))
	}
	if _, err := DeleteAgentFiles("solo-agent", projectPath, true); err != nil {
		t.Errorf("second DeleteAgentFiles: %v", err)
	}
}

// A15 (O1): entries beside a live shared worktree are removed as they are:
// a dangling symlink and a symlink to a file outside the agent directory
// are unlinked, their targets are untouched, and the delete succeeds; the
// shared worktree stays for the remaining sharer.
func TestDeleteAgentFiles_LinksBesideLiveWorktreeAreUnlinked(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", root)
	proj := filepath.Join(root, "proj")
	mustMkdir(t, proj)
	setupGitRepo(t, proj)
	scion := filepath.Join(proj, config.DotScion)
	agentDir := filepath.Join(scion, "agents", "a")
	ws := filepath.Join(agentDir, "workspace")
	mustMkdir(t, agentDir)
	if out, err := exec.Command("git", "-C", proj, "worktree", "add", "-q", "-b", "b1", ws).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	for _, agent := range []string{"a", "other"} {
		if err := provision.RegisterSharer(proj, scion, "b1", ws, agent); err != nil {
			t.Fatalf("RegisterSharer(%s): %v", agent, err)
		}
	}
	outside := filepath.Join(root, "outside.txt")
	mustWrite(t, outside, "keep")
	dangling := filepath.Join(agentDir, "dangling")
	mustSymlink(t, filepath.Join(root, "missing"), dangling)
	toOutside := filepath.Join(agentDir, "to-outside")
	mustSymlink(t, outside, toOutside)

	if _, err := DeleteAgentFiles("a", scion, false); err != nil {
		t.Fatalf("DeleteAgentFiles beside a live worktree: %v", err)
	}
	for _, link := range []string{dangling, toOutside} {
		if _, err := os.Lstat(link); !os.IsNotExist(err) {
			t.Errorf("link %s not removed (lstat err %v)", link, err)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Errorf("link target changed: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git")); err != nil {
		t.Errorf("shared worktree removed while another sharer remains: %v", err)
	}
}
