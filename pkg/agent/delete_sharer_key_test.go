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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Tests for ptone/scion#3405: DeleteHubAgentFiles finds the worktree and
// sharer registry entry under the sharer key (a broker's hub agent ID), and
// for the last sharer removes the worktree and git's entry for it before
// the registry entry, so a failure leaves a state a retry completes.

// sharerKeyProject is a worktree-per-agent project with its shared base at
// <project>/workspace, cloned from a real repository.
type sharerKeyProject struct {
	projectPath string
	scionDir    string
	base        string
	gc          *api.GitCloneConfig
}

func newSharerKeyProject(t *testing.T) *sharerKeyProject {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpDir)
	projectPath := filepath.Join(tmpDir, "proj")
	scionDir := filepath.Join(projectPath, config.DotScion)
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &sharerKeyProject{
		projectPath: projectPath,
		scionDir:    scionDir,
		base:        filepath.Join(projectPath, "workspace"),
		gc:          &api.GitCloneConfig{URL: initBareRepo(t), Branch: "main", Depth: intPtr(0)},
	}
}

// add provisions the agent named name, registered under key on branch,
// and returns its worktree path.
func (p *sharerKeyProject) add(t *testing.T, name, key, branch string) string {
	t.Helper()
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: provision.ResolvedWorkspace{HostPath: p.base, Backend: "local"},
		Mode:     store.SharingModeWorktreePerAgent, ProjectID: "p1",
		AgentID: key, AgentName: branch, GitClone: p.gc,
	}); err != nil {
		t.Fatalf("provision %s: %v", key, err)
	}
	if err := os.MkdirAll(filepath.Join(p.scionDir, "agents", name), 0o755); err != nil {
		t.Fatal(err)
	}
	return provision.WorktreePath(p.base, key)
}

func (p *sharerKeyProject) marker(branch string) string {
	return filepath.Join(p.base, ".git", "scion-sharers", branch+".json")
}

func (p *sharerKeyProject) entry(key string) string {
	return filepath.Join(p.base, ".git", "worktrees", key)
}

// assertKept checks the state a failed last-sharer teardown must leave:
// the registry entry still lists key and the agent directory is kept.
func (p *sharerKeyProject) assertKept(t *testing.T, name, key, branch string) {
	t.Helper()
	sharers, _, err := provision.ListSharers(p.base, "", branch)
	if err != nil {
		t.Fatalf("ListSharers: %v", err)
	}
	if !slices.Contains(sharers, key) {
		t.Errorf("registry entry for %s no longer lists %s: %v", branch, key, sharers)
	}
	if _, err := os.Stat(p.marker(branch)); err != nil {
		t.Errorf("registry file removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.scionDir, "agents", name)); err != nil {
		t.Errorf("agent directory removed: %v", err)
	}
}

// assertTornDown checks that nothing of the worktree registered under key
// on branch is left.
func (p *sharerKeyProject) assertTornDown(t *testing.T, name, key, wt, branch string) {
	t.Helper()
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree %s still present: %v", wt, err)
	}
	if _, err := os.Lstat(p.entry(key)); !os.IsNotExist(err) {
		t.Errorf("git worktree entry for %s still present: %v", key, err)
	}
	if list := listWorktrees(t, p.base); strings.Contains(list, wt) {
		t.Errorf("git still lists %s:\n%s", wt, list)
	}
	if _, err := os.Lstat(p.marker(branch)); !os.IsNotExist(err) {
		t.Errorf("registry file for %s still present: %v", branch, err)
	}
	if _, err := os.Stat(filepath.Join(p.scionDir, "agents", name)); !os.IsNotExist(err) {
		t.Errorf("agent directory still present: %v", err)
	}
}

func setSeam[T any](t *testing.T, seam *T, v T) {
	t.Helper()
	orig := *seam
	*seam = v
	t.Cleanup(func() { *seam = orig })
}

// The sharer key finds the worktree and registry entry a broker
// provisioned under the hub agent ID; the other sharer is detached first,
// and the last one removes everything.
func TestDeleteHubAgentFiles_SharerKey(t *testing.T) {
	p := newSharerKeyProject(t)
	wtA := p.add(t, "dev-a", "uuid-a", "shared")
	p.add(t, "dev-b", "uuid-b", "shared")

	if _, err := DeleteHubAgentFiles("dev-b", "uuid-b", p.projectPath, "", true); err != nil {
		t.Fatalf("delete dev-b: %v", err)
	}
	sharers, _, err := provision.ListSharers(p.base, "", "shared")
	if err != nil || !slices.Equal(sharers, []string{"uuid-a"}) {
		t.Fatalf("after deleting dev-b: sharers = %v (err %v), want [uuid-a]", sharers, err)
	}
	if _, err := os.Stat(filepath.Join(wtA, ".git")); err != nil {
		t.Fatalf("after deleting dev-b: shared worktree gone: %v", err)
	}

	branchDeleted, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", true)
	if err != nil {
		t.Fatalf("delete dev-a: %v", err)
	}
	if !branchDeleted {
		t.Error("the last sharer's delete did not delete the branch")
	}
	p.assertTornDown(t, "dev-a", "uuid-a", wtA, "shared")
}

// With nothing registered under the sharer key, the name is looked up: an
// agent provisioned by name (ProvisionAgent, a workstation's linked
// checkout) is still torn down when the delete carries a hub agent ID.
func TestDeleteHubAgentFiles_FallsBackToName(t *testing.T) {
	p := newSharerKeyProject(t)
	wt := p.add(t, "dev-a", "dev-a", "shared")

	if _, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", true); err != nil {
		t.Fatalf("delete: %v", err)
	}
	p.assertTornDown(t, "dev-a", "dev-a", wt, "shared")
}

// A sharer key that is not a single path element is refused before
// anything is touched.
func TestDeleteHubAgentFiles_InvalidSharerKey(t *testing.T) {
	p := newSharerKeyProject(t)
	wt := p.add(t, "dev-a", "uuid-a", "shared")
	for _, key := range []string{"..", "../uuid-a", "a/b"} {
		if _, err := DeleteHubAgentFiles("dev-a", key, p.projectPath, "", true); err == nil {
			t.Errorf("sharer key %q: expected an error", key)
		}
	}
	p.assertKept(t, "dev-a", "uuid-a", "shared")
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Errorf("worktree removed: %v", err)
	}
}

// A failed worktree removal leaves the registry entry, the worktree and the
// agent directory in place; a retry completes the teardown. This also
// covers a removal that failed after removing the directory.
func TestDeleteHubAgentFiles_WorktreeRemovalFails_RegistryKept(t *testing.T) {
	for _, tc := range []struct {
		name          string
		removeDirToo  bool
		wantDirOnFail bool
	}{
		{"nothing removed", false, true},
		{"directory removed, then failed", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newSharerKeyProject(t)
			wt := p.add(t, "dev-a", "uuid-a", "shared")

			restore := removeWorktree
			setSeam(t, &removeWorktree, func(base, path, repoRoot, branch string) (bool, error) {
				if tc.removeDirToo {
					if err := os.RemoveAll(path); err != nil {
						t.Fatal(err)
					}
				}
				return false, errors.New("forced removal failure")
			})
			if _, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", true); err == nil {
				t.Fatal("expected the delete to fail")
			}
			p.assertKept(t, "dev-a", "uuid-a", "shared")
			if _, err := os.Stat(wt); (err == nil) != tc.wantDirOnFail {
				t.Errorf("worktree directory present = %v, want %v", err == nil, tc.wantDirOnFail)
			}

			removeWorktree = restore
			if _, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", true); err != nil {
				t.Fatalf("retry: %v", err)
			}
			p.assertTornDown(t, "dev-a", "uuid-a", wt, "shared")
		})
	}
}

// A worktree directory that is still present after its removal reported
// success keeps the registry entry.
func TestDeleteHubAgentFiles_WorktreeStillPresent_RegistryKept(t *testing.T) {
	p := newSharerKeyProject(t)
	p.add(t, "dev-a", "uuid-a", "shared")
	setSeam(t, &removeWorktree, func(string, string, string, string) (bool, error) { return false, nil })

	if _, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", true); err == nil {
		t.Fatal("expected the delete to fail")
	}
	p.assertKept(t, "dev-a", "uuid-a", "shared")
}

// Removing git's entry for the worktree: a failure keeps the registry
// entry, and an entry or directory that is already gone counts as removed.
func TestDeleteHubAgentFiles_WorktreeEntry(t *testing.T) {
	t.Run("entry removal fails", func(t *testing.T) {
		p := newSharerKeyProject(t)
		wt := p.add(t, "dev-a", "uuid-a", "shared")
		restore := removeWorktreeEntry
		setSeam(t, &removeWorktreeEntry, func(string, string) error { return errors.New("forced entry failure") })

		if _, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", true); err == nil {
			t.Fatal("expected the delete to fail")
		}
		p.assertKept(t, "dev-a", "uuid-a", "shared")

		removeWorktreeEntry = restore
		if _, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", true); err != nil {
			t.Fatalf("retry: %v", err)
		}
		p.assertTornDown(t, "dev-a", "uuid-a", wt, "shared")
	})
	for _, tc := range []struct {
		name                 string
		removeEntry, removeD bool
	}{
		{"entry already gone", true, false},
		{"directory already gone", false, true},
		{"entry and directory already gone", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newSharerKeyProject(t)
			wt := p.add(t, "dev-a", "uuid-a", "shared")
			if tc.removeEntry {
				if err := os.RemoveAll(p.entry("uuid-a")); err != nil {
					t.Fatal(err)
				}
			}
			if tc.removeD {
				if err := os.RemoveAll(wt); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", true); err != nil {
				t.Fatalf("delete: %v", err)
			}
			p.assertTornDown(t, "dev-a", "uuid-a", wt, "shared")
			// The removal found the worktree or its entry already gone,
			// so it could not confirm the worktree's branch: the
			// registry branch is kept (no by-name fallback on success).
			if out, err := exec.Command("git", "-C", p.base, "branch", "--list", "shared").Output(); err != nil || strings.TrimSpace(string(out)) == "" {
				t.Errorf("branch shared deleted, want kept: %q (err %v)", out, err)
			}
		})
	}
}

// The registry entry is still rewritten atomically while other sharers
// remain, and the default key is the agent name (local mode).
func TestDeleteHubAgentFiles_DefaultKeyIsName(t *testing.T) {
	p := newSharerKeyProject(t)
	wt := p.add(t, "dev-a", "dev-a", "shared")
	p.add(t, "dev-b", "dev-b", "shared")

	if _, err := DeleteHubAgentFiles("dev-b", "", p.projectPath, "", true); err != nil {
		t.Fatalf("delete dev-b: %v", err)
	}
	if sharers, _, err := provision.ListSharers(p.base, "", "shared"); err != nil || !slices.Equal(sharers, []string{"dev-a"}) {
		t.Fatalf("sharers = %v (err %v), want [dev-a]", sharers, err)
	}
	if _, err := DeleteAgentFiles("dev-a", p.projectPath, true); err != nil {
		t.Fatalf("delete dev-a: %v", err)
	}
	p.assertTornDown(t, "dev-a", "dev-a", wt, "shared")
}

// A locked worktree entry (a user's lock, or the lock an interrupted
// "git worktree add" leaves) cannot be removed: the delete fails with the
// registry entry and the agent directory kept, on every attempt, until the
// entry is unlocked; the retry after that completes the teardown.
func TestDeleteHubAgentFiles_LockedWorktreeEntry_FailsClosed(t *testing.T) {
	p := newSharerKeyProject(t)
	wt := p.add(t, "dev-a", "uuid-a", "shared")
	if out, err := exec.Command("git", "-C", p.base, "worktree", "lock", wt).CombinedOutput(); err != nil {
		t.Fatalf("git worktree lock: %v\n%s", err, out)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", true); err == nil {
			t.Fatalf("attempt %d: expected the delete of a locked worktree to fail", attempt)
		}
		p.assertKept(t, "dev-a", "uuid-a", "shared")
		if _, err := os.Stat(p.entry("uuid-a")); err != nil {
			t.Errorf("attempt %d: git worktree entry removed: %v", attempt, err)
		}
	}

	// The worktree directory is already gone, so remove the lock the way
	// "git worktree unlock" does.
	if err := os.Remove(filepath.Join(p.entry("uuid-a"), "locked")); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if _, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", true); err != nil {
		t.Fatalf("retry after unlock: %v", err)
	}
	p.assertTornDown(t, "dev-a", "uuid-a", wt, "shared")
}

// A hub agent with both a worktree under the sharer key and a state
// directory resolved from its Hub project ID (distinct from the project-id
// marker): one delete removes both, keeps the project workspace and the
// repository, and a second delete is a no-op.
func TestDeleteHubAgentFiles_WorktreeAndHubStateDir(t *testing.T) {
	const (
		markerID = "11111111-aaaa-4aaa-8aaa-111111111111"
		hubID    = "33333333-cccc-4ccc-8ccc-333333333333"
	)
	p := newSharerKeyProject(t)
	wt := p.add(t, "dev-a", "uuid-a", "shared")
	if err := config.WriteProjectID(p.scionDir, markerID); err != nil {
		t.Fatalf("WriteProjectID: %v", err)
	}
	stateDir, err := config.AgentDirForProject(p.scionDir, "dev-a", true, hubID)
	if err != nil {
		t.Fatalf("AgentDirForProject: %v", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Precondition: the Hub-ID state dir is a directory of its own, not the
	// marker-derived one or the in-project agent dir, so the delete must
	// remove it on top of what DeleteAgentFiles removes.
	markerDir, err := config.AgentDirForProject(p.scionDir, "dev-a", true, markerID)
	if err != nil {
		t.Fatalf("AgentDirForProject(marker): %v", err)
	}
	if stateDir == markerDir || stateDir == filepath.Join(p.scionDir, "agents", "dev-a") {
		t.Fatalf("setup: Hub-ID state dir %s is not distinct (marker dir %s)", stateDir, markerDir)
	}

	for call := 1; call <= 2; call++ {
		if _, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, hubID, true); err != nil {
			t.Fatalf("call %d: DeleteHubAgentFiles: %v", call, err)
		}
		p.assertTornDown(t, "dev-a", "uuid-a", wt, "shared")
		if out, err := exec.Command("git", "-C", p.base, "branch", "--list", "shared").Output(); err != nil || strings.TrimSpace(string(out)) != "" {
			t.Errorf("call %d: branch shared not deleted: %q (err %v)", call, out, err)
		}
		if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
			t.Errorf("call %d: Hub-ID state dir %s still present: %v", call, stateDir, err)
		}
		if _, err := os.Stat(filepath.Join(p.base, ".git")); err != nil {
			t.Errorf("call %d: repository removed: %v", call, err)
		}
		if _, err := os.Stat(filepath.Join(p.base, "README.md")); err != nil {
			t.Errorf("call %d: project workspace removed: %v", call, err)
		}
	}
}

// The last sharer's worktree removal gets the registry branch only when the
// delete asks to remove the branch: with removeBranch false the branch
// argument is empty and the branch is kept; with true it names the
// worktree's branch, which is deleted.
func TestDeleteHubAgentFiles_RemoveBranchArgument(t *testing.T) {
	for _, tc := range []struct {
		name         string
		removeBranch bool
		wantArg      string
		wantKept     bool
	}{
		{"keep branch", false, "", true},
		{"remove branch", true, "shared", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newSharerKeyProject(t)
			wt := p.add(t, "dev-a", "uuid-a", "shared")
			var gotArg []string
			orig := removeWorktree
			setSeam(t, &removeWorktree, func(base, path, repoRoot, branch string) (bool, error) {
				gotArg = append(gotArg, branch)
				return orig(base, path, repoRoot, branch)
			})
			if _, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", tc.removeBranch); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if len(gotArg) != 1 || gotArg[0] != tc.wantArg {
				t.Errorf("removeWorktree branch arguments = %q, want [%q]", gotArg, tc.wantArg)
			}
			p.assertTornDown(t, "dev-a", "uuid-a", wt, "shared")
			out, err := exec.Command("git", "-C", p.base, "branch", "--list", "shared").Output()
			if err != nil {
				t.Fatalf("git branch --list: %v", err)
			}
			if kept := strings.TrimSpace(string(out)) != ""; kept != tc.wantKept {
				t.Errorf("branch shared kept = %v, want %v", kept, tc.wantKept)
			}
		})
	}
}

// A removal that succeeds but keeps the branch (the worktree was not on it)
// is final: the registry branch is not deleted by name.
func TestDeleteHubAgentFiles_OffBranchRemovalKeepsBranch(t *testing.T) {
	p := newSharerKeyProject(t)
	wt := p.add(t, "dev-a", "uuid-a", "shared")
	setSeam(t, &removeWorktree, func(base, path, repoRoot, branch string) (bool, error) {
		// As util.RemoveWorktree does for a worktree off its branch:
		// remove the directory, delete no branch.
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		return false, nil
	})
	var byName []string
	setSeam(t, &deleteBranchByName, func(repoRoot, branch string) bool {
		byName = append(byName, branch)
		return false
	})
	if _, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", true); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(byName) != 0 {
		t.Errorf("branch deleted by name after a successful removal: %q", byName)
	}
	p.assertTornDown(t, "dev-a", "uuid-a", wt, "shared")
	out, err := exec.Command("git", "-C", p.base, "branch", "--list", "shared").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Errorf("branch shared deleted, want kept: %q (err %v)", out, err)
	}
}

// A refused removal (the path resolves outside the repository) deletes no
// branch itself, so the registry branch is deleted by name when the delete
// asks to remove the branch, and kept otherwise.
func TestDeleteHubAgentFiles_RefusedRemovalDeletesBranchByName(t *testing.T) {
	for _, tc := range []struct {
		name         string
		removeBranch bool
	}{
		{"remove branch", true},
		{"keep branch", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newSharerKeyProject(t)
			p.add(t, "dev-a", "uuid-a", "shared")
			setSeam(t, &removeWorktree, func(base, path, repoRoot, branch string) (bool, error) {
				return false, fmt.Errorf("forced: %w", util.ErrPathNotContained)
			})
			var byName []string
			setSeam(t, &deleteBranchByName, func(repoRoot, branch string) bool {
				byName = append(byName, repoRoot+" "+branch)
				return true
			})
			branchDeleted, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", tc.removeBranch)
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
			if !tc.removeBranch {
				if len(byName) != 0 || branchDeleted {
					t.Errorf("removeBranch false: by-name branch deletes = %q, branchDeleted %v; want none and false", byName, branchDeleted)
				}
				return
			}
			if want := p.base + " shared"; len(byName) != 1 || byName[0] != want {
				t.Errorf("by-name branch deletes = %q, want [%q]", byName, want)
			}
			if !branchDeleted {
				t.Error("branch deleted by name, but DeleteHubAgentFiles reported it kept")
			}
		})
	}
}

// A removal that deleted the branch itself leaves nothing for the by-name
// fallback.
func TestDeleteHubAgentFiles_RemovalDeletedBranchNoFallback(t *testing.T) {
	p := newSharerKeyProject(t)
	wt := p.add(t, "dev-a", "uuid-a", "shared")
	orig := removeWorktree
	var gotArg []string
	setSeam(t, &removeWorktree, func(base, path, repoRoot, branch string) (bool, error) {
		gotArg = append(gotArg, branch)
		return orig(base, path, repoRoot, branch)
	})
	var byName []string
	setSeam(t, &deleteBranchByName, func(repoRoot, branch string) bool {
		byName = append(byName, branch)
		return false
	})
	branchDeleted, err := DeleteHubAgentFiles("dev-a", "uuid-a", p.projectPath, "", true)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(gotArg) != 1 || gotArg[0] != "shared" {
		t.Errorf("removeWorktree branch arguments = %q, want [\"shared\"]", gotArg)
	}
	if len(byName) != 0 {
		t.Errorf("by-name branch deletes after the removal deleted the branch: %q", byName)
	}
	if !branchDeleted {
		t.Error("the removal deleted the branch, but DeleteHubAgentFiles reported it kept")
	}
	p.assertTornDown(t, "dev-a", "uuid-a", wt, "shared")
	if out, err := exec.Command("git", "-C", p.base, "branch", "--list", "shared").Output(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("branch shared kept, want deleted: %q (err %v)", out, err)
	}
}
