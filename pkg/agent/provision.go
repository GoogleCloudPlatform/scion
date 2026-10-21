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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/agentinfolock"
	"github.com/GoogleCloudPlatform/scion/pkg/agentplugin"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/resources"
	"github.com/google/uuid"
)

// ErrAgentProjectUnresolved is returned by DeleteAgentFiles when the
// project path resolves to no existing project directory.
var ErrAgentProjectUnresolved = errors.New("the agent's project directory does not exist")

// DeleteAgentFiles removes agentName's files in the project projectPath
// resolves to (its agent directory and workspace, worktree and, with
// removeBranch, branch, and its external per-agent state). It never touches
// another project's directories; the global project's agents are deleted
// only when the global project is the target.
func DeleteAgentFiles(agentName string, projectPath string, removeBranch bool) (bool, error) {
	// Every path built below joins agentName onto some directory -- the
	// project's agents dir, the external per-agent
	// state dir, or the shared worktree base -- so an unvalidated name could
	// otherwise resolve outside all of them (e.g. "../sibling"). Containment
	// under checkAgentDirContained is invariant of which root and
	// sharedWorkspace value is passed: a name it accepts is a direct,
	// single-element child of every root; a name it rejects escapes every
	// root the same way. Validating once here, before any of the joins or
	// filesystem operations below, is therefore sufficient to guard every
	// branch regardless of which directory ends up being touched.
	if _, err := checkAgentDirContained(projectPath, agentName, false); err != nil {
		return false, fmt.Errorf("delete: %w", err)
	}

	// The agent's files live under its project only. A project path that
	// resolves to no existing project directory has nothing of the agent's
	// to delete: it is reported (ErrAgentProjectUnresolved), never resolved
	// to another project.
	if pd, err := config.GetResolvedProjectDir(projectPath); err != nil {
		return false, fmt.Errorf("delete: %w: %v", ErrAgentProjectUnresolved, err)
	} else if _, statErr := os.Stat(pd); errors.Is(statErr, fs.ErrNotExist) {
		return false, fmt.Errorf("delete: %w: %s", ErrAgentProjectUnresolved, pd)
	}

	var agentsDirs []string
	branchDeleted := false
	var repoRoot string
	var externalAgentDir string
	var worktreeDir string // worktree-per-agent: agent's worktree path
	// ownWorktreePaths lists the paths where this agent's own worktree may
	// be registered. Only these entries are removed from git's worktree
	// list; other worktrees' entries are never touched (ptone/scion#5060).
	var ownWorktreePaths []string
	// sharedBaseWorktree is the agent's path under the worktree-per-agent
	// shared base. It is added to ownWorktreePaths only once the sharer
	// check below has shown that no other agent still uses it.
	var sharedBaseWorktree string
	var globalWorkspaceDir string
	// resolvedProjectDir is config.GetResolvedProjectDir's return value,
	// captured here (rather than only inside the block below) so the
	// sharer-registry calls further down can pass it through as the
	// ProvisionAgent-layout shape check's root — see
	// provision.WorktreePathIsScionCreated's doc comment on why this must be
	// the actual resolved project directory, not derived from repoRoot.
	var resolvedProjectDir string
	// layout is resolved once and shared with the guard.
	var layout agentProjectLayout
	if projectDir, err := config.GetResolvedProjectDir(projectPath); err == nil {
		resolvedProjectDir = projectDir
		agentsDirs = append(agentsDirs, filepath.Join(projectDir, "agents"))

		// The global project's own per-agent workspace
		// (~/.scion/workspace/<agentName>, see ProvisionAgent's Case 3) is
		// not under either agentsDir above, so it needs its own cleanup
		// target: unlike every other project type, where agentDir's own
		// "workspace" subdirectory is already in dirsToDelete below, a
		// global-project agent's actual workspace lives outside agentDir
		// entirely, and removing only agentDir would orphan it.
		if config.IsGlobalProjectDir(projectDir) {
			globalWorkspaceDir = filepath.Join(projectDir, "workspace", agentName)
		}

		// Determine repo root for worktree pruning and branch cleanup
		// (resolveAgentProjectLayout). For worktree-per-agent the shared
		// base lives at <projectRoot>/workspace; otherwise the repository
		// is resolved from projectDir itself, which MUST match the base
		// used at sharer registration (ProvisionAgent), or the refcount
		// lookup (FindBranchForAgent/UnregisterSharer) would miss.
		layout = resolveAgentProjectLayout(projectDir)
		repoRoot = layout.repoRoot
		if layout.sharedBaseIsRepo {
			wtPath := filepath.Join(layout.sharedBase, "worktrees", agentName)
			sharedBaseWorktree = wtPath
			if _, statErr := os.Stat(wtPath); statErr == nil {
				worktreeDir = wtPath
			}
		}

		// Check for external agent home (git project split storage)
		if layout.externalAgentsRoot != "" {
			externalAgentDir = filepath.Join(layout.externalAgentsRoot, agentName)
		}
	}

	// Every removal below passes the guard (projectAgentFilesGuard): only
	// the agent's own directories may be removed, never a project
	// workspace. Each removal acts on the path the guard returns.
	guard := projectAgentFilesGuardWithLayout(context.Background(), resolvedProjectDir, layout, agentName)
	// Empty-per-agent (design #2703): the agent's workspace is a private,
	// non-git directory that owns no worktree or branch. Even when the
	// agent ran `git init` in it, or the project sits inside an enclosing
	// repository with a same-named branch, delete must only remove the
	// agent's directories -- never a worktree, a sharer registration, a
	// prune of the enclosing repo, or a branch.
	emptyPerAgent := persistedEmptyPerAgent(agentsDirs, externalAgentDir, agentName)
	if emptyPerAgent {
		util.Debugf("delete: %s is empty-per-agent; skipping worktree and branch cleanup", agentName)
		repoRoot = ""
		worktreeDir = ""
	}

	// Phase 1: synchronous git operations (worktree removal, pruning, branch cleanup).
	// No background deletions happen here to avoid triggering macOS autofs
	// in a goroutine that could block git subprocess I/O system-wide.
	var dirsToDelete []string

	// --- Refcount path: shared-worktree teardown (#168 I3) ---
	//
	// Before the legacy worktree-removal blocks, check the sharer registry.
	// If this agent is registered as a sharer, unregister it and decide
	// whether to remove the shared worktree based on remaining sharers.
	//
	// NOTE: teardown does not hold the per-project advisory lock. The
	// provisioning path (ensureWorktree / ProvisionShared) holds the lock
	// during registration. A concurrent provision+delete race on the same
	// branch is unlikely in practice (the hub serialises agent lifecycle)
	// but not structurally excluded. Acceptable for single-node local mode
	// which has no advisory locker.
	refcountHandled := false
	// sharersRemain is set when other agents still share this agent's
	// worktree; its entry then belongs to them and is left alone.
	sharersRemain := false
	if repoRoot != "" {
		// Do NOT silently swallow registry errors and fall through to the legacy
		// path — that path could delete the shared worktree out from under live
		// joiners. On a real registry I/O error, fail loudly instead.
		branch, _, found, findErr := provision.FindBranchForAgent(repoRoot, resolvedProjectDir, agentName)
		if findErr != nil {
			return branchDeleted, fmt.Errorf("delete: FindBranchForAgent for %s: %w", agentName, findErr)
		}
		if found {
			remaining, wtPath, unregErr := provision.UnregisterSharer(repoRoot, resolvedProjectDir, branch, agentName)
			if unregErr != nil {
				return branchDeleted, fmt.Errorf("delete: UnregisterSharer for branch %s agent %s: %w", branch, agentName, unregErr)
			}
			if len(remaining) == 0 {
				// Additional check: re-check wtPath's unresolved/lexical form
				// against a scion-created shape immediately before acting on
				// it, rather than trusting that UnregisterSharer's return
				// value is still exactly what the read boundary validated.
				// This is deliberately a second LEXICAL check, not a new
				// EvalSymlinks anchor for the removal step below — the
				// resolved-containment check inside util.RemoveWorktree stays
				// anchored at repoRoot (see its own doc comment on why
				// narrowing that anchor to a per-marker subdirectory would
				// let a symlinked intermediate component pass through
				// unchecked, defeating that check's purpose). This only scopes what counts as
				// an acceptable shape going into that call, one guard closer
				// to the removal itself.
				if wtPath != "" && !provision.WorktreePathIsScionCreated(repoRoot, resolvedProjectDir, wtPath) {
					slog.Warn("delete: worktree path no longer matches a scion-created shape at removal time; skipping worktree removal",
						"agent_id", agentName, "branch", branch, "path", wtPath)
					wtPath = ""
				}
				if wtPath == "" {
					// The registry read boundary (pkg/provision.readMarker)
					// already fails closed on an out-of-tree/relative/empty
					// WorktreePath, discarding the whole marker. There is no
					// validated in-tree path to remove — do not guess one.
					slog.Warn("delete: no valid worktree path for last sharer; skipping worktree removal",
						"agent_id", agentName, "branch", branch)
				} else {
					util.Debugf("delete: last sharer for branch %s, removing worktree at %s", branch, wtPath)
					// The last sharer removes the shared worktree, which
					// the registry and the shape check above establish is
					// scion's, not the project's.
					// A worktree directory that is already gone has
					// nothing on disk to check or remove; RemoveWorktree
					// still runs (it returns at once) and git's entry is
					// removed below. Any other Lstat result goes through
					// the guard.
					var wtRemove string
					if _, lerr := os.Lstat(wtPath); !errors.Is(lerr, fs.ErrNotExist) {
						var err error
						if wtRemove, err = guard.check(wtPath, wtPath); err != nil {
							return branchDeleted, fmt.Errorf("delete: %w", err)
						}
					}
					worktreeStart := time.Now()
					if deleted, err := util.RemoveWorktree(repoRoot, wtPath, removeBranch); err == nil {
						if deleted {
							branchDeleted = true
						}
						util.Debugf("delete: shared worktree removal completed in %v (branch deleted: %v)", time.Since(worktreeStart), deleted)
					} else {
						util.Debugf("delete: shared worktree removal failed in %v: %v", time.Since(worktreeStart), err)
						if errors.Is(err, util.ErrPathNotContained) {
							// The marker's WorktreePath passed the lexical
							// read-boundary check but resolves
							// outside repoRoot once symlinks are followed
							// (e.g. a symlinked worktrees dir or leaf).
							// Do NOT fall back to a raw recursive removal —
							// that is exactly what this check exists to
							// prevent.
							slog.Warn("delete: refusing fallback removal; worktree path does not resolve under repo root",
								"agent_id", agentName, "branch", branch, "path", wtPath, "repo_root", repoRoot)
						} else {
							if wtRemove != "" {
								_ = util.RemoveAllSafe(wtRemove)
							}
						}
						// Worktree removal failed, so the branch wasn't deleted by it —
						// fall back to deleting the branch by name (like the legacy path).
						if removeBranch && !branchDeleted {
							if util.DeleteBranchIn(repoRoot, branch) {
								branchDeleted = true
								util.Debugf("delete: deleted branch %s via fallback after worktree removal failure", branch)
							}
						}
					}
				}
			} else {
				util.Debugf("delete: %d sharers remain for branch %s, detaching agent %s", len(remaining), branch, agentName)
				sharersRemain = true
			}
			if len(remaining) == 0 && wtPath != "" {
				ownWorktreePaths = append(ownWorktreePaths, wtPath)
			}
			refcountHandled = true
		}
	}

	if sharedBaseWorktree != "" && !sharersRemain {
		ownWorktreePaths = append(ownWorktreePaths, sharedBaseWorktree)
	}

	// Worktree-per-agent: remove the agent's worktree from the shared base.
	// The worktree lives at <projectDir>/workspace/worktrees/<agentName>,
	// separate from the agent config dir under agents/.
	// Skip when the refcount path already handled removal/detach.
	if worktreeDir != "" && !refcountHandled {
		worktreeRemove, err := guard.check(worktreeDir)
		if err != nil {
			return branchDeleted, fmt.Errorf("delete: %w", err)
		}
		if _, err := os.Stat(filepath.Join(worktreeDir, ".git")); err == nil {
			util.Debugf("delete: removing worktree-per-agent workspace at %s", worktreeDir)
			worktreeStart := time.Now()
			if deleted, err := util.RemoveWorktree(repoRoot, worktreeDir, removeBranch); err == nil {
				if deleted {
					branchDeleted = true
				}
				util.Debugf("delete: worktree-per-agent removal completed in %v (branch deleted: %v)", time.Since(worktreeStart), deleted)
			} else {
				util.Debugf("delete: worktree-per-agent removal failed in %v: %v", time.Since(worktreeStart), err)
				if !errors.Is(err, util.ErrPathNotContained) {
					_ = util.RemoveAllSafe(worktreeRemove)
				}
			}
		} else {
			_ = util.RemoveAllSafe(worktreeRemove)
		}
	}

	for _, dir := range agentsDirs {
		agentDir := filepath.Join(dir, agentName)
		agentWorkspace := filepath.Join(agentDir, "workspace")
		if !sharersRemain {
			ownWorktreePaths = append(ownWorktreePaths, agentWorkspace)
		}
		if _, err := os.Stat(agentDir); err != nil {
			continue
		}

		// Check if it's a worktree before trying to remove it.
		// Skip when the refcount path already handled removal/detach —
		// the shared worktree must not be removed while other sharers remain.
		if !refcountHandled && !emptyPerAgent {
			if _, err := os.Stat(filepath.Join(agentWorkspace, ".git")); err == nil {
				workspaceRemove, err := guard.check(agentWorkspace)
				if err != nil {
					return branchDeleted, fmt.Errorf("delete: %w", err)
				}
				util.Debugf("delete: removing workspace at %s", agentWorkspace)
				worktreeStart := time.Now()
				if deleted, err := util.RemoveWorktree(agentDir, agentWorkspace, removeBranch); err == nil {
					if deleted {
						branchDeleted = true
					}
					util.Debugf("delete: worktree removal completed in %v (branch deleted: %v)", time.Since(worktreeStart), deleted)
				} else {
					util.Debugf("delete: worktree removal failed in %v: %v", time.Since(worktreeStart), err)
					if !errors.Is(err, util.ErrPathNotContained) {
						_ = util.RemoveAllSafe(workspaceRemove)
					}
				}
			}
		}

		dirsToDelete = append(dirsToDelete, agentDir)
	}

	if globalWorkspaceDir != "" {
		if _, err := os.Stat(globalWorkspaceDir); err == nil {
			dirsToDelete = append(dirsToDelete, globalWorkspaceDir)
		}
	}

	// Remove stale records of this agent's own worktree. This handles cases
	// where the workspace directory was removed (e.g. by the fallback removal
	// above, or a previous incomplete cleanup) but its git worktree record
	// was not unregistered. Only this agent's paths are targeted, and an
	// entry whose directory still exists is left alone. There is no
	// repository-wide prune: it would also drop the entries of other
	// worktrees whose directories are missing or unreadable from this
	// process (ptone/scion#5060).
	if repoRoot != "" {
		for _, p := range ownWorktreePaths {
			if err := util.RemoveWorktreeEntryIn(repoRoot, p); err != nil {
				slog.Warn("delete: could not remove the worktree entry", "agent_id", agentName, "path", p, "error", err)
			}
		}

		// If the branch wasn't already deleted via RemoveWorktree (e.g. because
		// the workspace .git file didn't exist), try to delete it by name.
		// Skip when refcount handled teardown — branch lifecycle is managed
		// by the refcount path (last-sharer removes; others detach).
		if removeBranch && !branchDeleted && !refcountHandled {
			branchName := api.Slugify(agentName)
			if util.DeleteBranchIn(repoRoot, branchName) {
				branchDeleted = true
				util.Debugf("delete: deleted branch %s via fallback", branchName)
			}
		}
	}

	// Phase 2: directory removal.
	for _, agentDir := range dirsToDelete {
		dirRemove, err := guard.check(agentDir)
		if err != nil {
			return branchDeleted, fmt.Errorf("delete: %w", err)
		}
		// ProvisionAgent's worktree-mode layout nests an agent's worktree at
		// agentDir/workspace — for the creator of a shared branch (the
		// ProvisionAgent attach-to-existing-worktree case), this is the SAME
		// physical worktree another live sharer may still be using. The
		// refcount path above already decided whether that worktree may be
		// removed (skipping it here via refcountHandled when other sharers
		// remain); a blanket RemoveAllSafe(agentDir) would ignore that
		// decision and destroy it anyway, since it has no notion of a
		// worktree being nested inside the directory it's asked to remove.
		// If workspace/ is still a live worktree at this point, clean up
		// everything else in agentDir but leave it in place. An
		// empty-per-agent workspace owns no worktree and is removed with
		// agentDir even when it contains a .git.
		workspaceStillLive := false
		if _, err := os.Stat(filepath.Join(agentDir, "workspace", ".git")); err == nil && !emptyPerAgent {
			workspaceStillLive = true
		}
		if workspaceStillLive {
			entries, err := os.ReadDir(agentDir)
			if err != nil {
				return branchDeleted, fmt.Errorf("read agent directory %s: %w", agentDir, err)
			}
			for _, e := range entries {
				if e.Name() == "workspace" {
					continue
				}
				// Classified by its directory and name: a symlink
				// here is unlinked, never followed (checkLeaf).
				entryRemove, err := guard.checkLeaf(filepath.Join(agentDir, e.Name()))
				if err != nil {
					return branchDeleted, fmt.Errorf("delete: %w", err)
				}
				if err := util.RemoveAllSafe(entryRemove); err != nil {
					return branchDeleted, fmt.Errorf("failed to remove agent directory entry: %w", err)
				}
			}
			util.Debugf("delete: preserved shared workspace, removed other entries in: %s", agentDir)
			continue
		}
		util.Debugf("delete: removing directory: %s", agentDir)
		removeStart := time.Now()
		if err := util.RemoveAllSafe(dirRemove); err != nil {
			util.Debugf("delete: removal failed in %v: %v", time.Since(removeStart), err)
			return branchDeleted, fmt.Errorf("failed to remove agent directory: %w", err)
		}
		util.Debugf("delete: removal completed in %v", time.Since(removeStart))
	}

	// Phase 3: remove the external per-agent state directory (git project split
	// storage). For worktree-mode agents this contains only home/. For
	// shared-workspace agents this also contains prompt.md and scion-agent.json
	// (relocated to keep siblings from seeing them via the shared /workspace
	// mount — see .design/hub-shared-workspace-isolation.md). RemoveAll on the
	// dir handles both layouts.
	//
	// In podman rootless mode, files created as root inside the container are
	// owned by a mapped subuid on the host, making them inaccessible to the
	// normal user. If standard removal fails, try `podman unshare rm -rf`
	// which enters the user namespace where the mapped UIDs are accessible.
	if externalAgentDir != "" {
		if _, err := os.Stat(externalAgentDir); err == nil {
			extRemove, err := guard.check(externalAgentDir)
			if err != nil {
				return branchDeleted, fmt.Errorf("delete: %w", err)
			}
			util.Debugf("delete: removing external agent state dir: %s", externalAgentDir)
			if err := util.RemoveAllSafe(extRemove); err != nil {
				util.Debugf("delete: standard removal failed, trying podman unshare: %v", err)
				unshareCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if unshareErr := exec.CommandContext(unshareCtx, "podman", "unshare", "rm", "-rf", extRemove).Run(); unshareErr != nil {
					util.Debugf("delete: podman unshare removal also failed: %v", unshareErr)
				}
				cancel()
			}
		}
	}

	return branchDeleted, nil
}

// persistedEmptyPerAgent reports whether the agent's persisted
// scion-agent.json (in any of its agent dirs, or the external per-agent state
// dir) records the empty-per-agent workspace mode.
func persistedEmptyPerAgent(agentsDirs []string, externalAgentDir, agentName string) bool {
	candidates := make([]string, 0, len(agentsDirs)+1)
	for _, dir := range agentsDirs {
		candidates = append(candidates, filepath.Join(dir, agentName))
	}
	if externalAgentDir != "" {
		candidates = append(candidates, externalAgentDir)
	}
	for _, dir := range candidates {
		if !config.ScionAgentConfigExists(dir) {
			continue
		}
		if cfg, err := (&config.Template{Path: dir}).LoadConfig(); err == nil && cfg != nil && cfg.EmptyPerAgentWorkspace {
			return true
		}
	}
	return false
}

// migrateLegacyAgentState moves prompt.md, scion-agent.json and the
// shared-dir storage record from the legacy in-project location to the
// external (shared-workspace) location for agents provisioned before
// per-agent state was relocated. The legacy
// directory is removed if it ends up empty (it shouldn't contain anything
// else for shared-workspace agents — there is no per-agent worktree).
//
// Best-effort: errors are logged but do not abort provisioning. A miss here
// only means the in-project copy lingers (still readable by siblings until the
// agent re-provisions); it does not corrupt the new location.
func migrateLegacyAgentState(legacyDir, externalDir string) {
	moveFile := func(name string) {
		legacyPath := filepath.Join(legacyDir, name)
		if _, err := os.Stat(legacyPath); err != nil {
			return
		}
		externalPath := filepath.Join(externalDir, name)
		if _, err := os.Stat(externalPath); err == nil {
			// External already populated — discard the in-project residue.
			_ = os.Remove(legacyPath)
			return
		}
		if err := os.MkdirAll(externalDir, 0755); err != nil {
			util.Debugf("migrateLegacyAgentState: mkdir %s: %v", externalDir, err)
			return
		}
		if err := os.Rename(legacyPath, externalPath); err != nil {
			util.Debugf("migrateLegacyAgentState: rename %s -> %s: %v", legacyPath, externalPath, err)
		}
	}
	moveFile("prompt.md")
	moveFile("scion-agent.json")
	moveFile(sharedDirStorageRecordFile)
	// Remove the legacy dir if empty (best effort; non-empty leftovers like a
	// stale workspace/ shell are left in place to avoid surprising deletes).
	_ = os.Remove(legacyDir)
}

// StopProjectContainers finds and removes containers belonging to the given project
// that match the provided agent names. This is used during project pruning to
// clean up containers before removing the project config directory.
func StopProjectContainers(ctx context.Context, mgr Manager, projectName string, agentNames []string) []string {
	containers, err := mgr.List(ctx, map[string]string{
		"scion.agent":            "true",
		projectkeys.LabelProject: projectName,
	})
	if err != nil {
		util.Debugf("StopProjectContainers: failed to list containers for project %s: %v", projectName, err)
		return nil
	}

	nameSet := make(map[string]bool, len(agentNames))
	for _, n := range agentNames {
		nameSet[n] = true
	}

	var stopped []string
	for _, c := range containers {
		agentName := c.Labels["scion.name"]
		if agentName == "" {
			agentName = strings.TrimPrefix(c.Name, "/")
		}
		if !nameSet[agentName] || c.ContainerID == "" {
			continue
		}
		util.Debugf("StopProjectContainers: removing container %s (agent %s, project %s)", c.ContainerID, agentName, projectName)
		// Use Delete with deleteFiles=false — we only want to remove the container,
		// not the filesystem artifacts (those will be removed by RemoveProjectConfig).
		if _, err := mgr.DeleteTarget(ctx, agentName, runtime.RunRef{ID: c.ContainerID, RunID: c.RunID}, false, "", false); err != nil {
			util.Debugf("StopProjectContainers: failed to remove container for agent %s: %v", agentName, err)
		} else {
			stopped = append(stopped, agentName)
		}
	}
	return stopped
}

// buildProvisionContext wires the api.Context* values shared by Provision and
// Reprovision, and injects an explicit HarnessAuth override into the inline
// config so it is applied before the harness's own Provision() logic runs
// (which reads auth_selectedType to decide which env vars to inject into
// scion-agent.json). Extracted so a context value either one
// needs cannot be added to just one of them by accident.
func buildProvisionContext(ctx context.Context, opts api.StartOptions) (context.Context, *api.ScionConfig) {
	if opts.BrokerMode {
		ctx = api.ContextWithBrokerMode(ctx)
	}
	if opts.GitClone != nil {
		ctx = api.ContextWithGitClone(ctx, opts.GitClone)
	}
	if opts.FreshProvision {
		ctx = api.ContextWithFreshProvision(ctx)
	}
	if opts.MoveReincarnationID != "" {
		ctx = withMoveReincarnationID(ctx, opts.MoveReincarnationID)
	}
	if opts.SharedWorkspace {
		ctx = api.ContextWithSharedWorkspace(ctx)
	}
	ctx = api.ContextWithHubProjectID(ctx, opts.HubProjectID)
	if opts.EmptyPerAgentWorkspace {
		ctx = api.ContextWithEmptyPerAgentWorkspace(ctx)
	}
	if opts.HarnessConfigPath != "" {
		ctx = api.ContextWithHarnessConfigPath(ctx, opts.HarnessConfigPath)
	}
	if opts.TemplateName != "" {
		ctx = api.ContextWithTemplateName(ctx, opts.TemplateName)
	}
	inlineCfg := opts.InlineConfig
	if opts.HarnessAuth != "" {
		// Copy rather than mutate opts.InlineConfig in place: it is a
		// pointer the caller owns (and, for Preflight, goes on to pass
		// unchanged into Manager.Start), so writing AuthSelectedType
		// directly into it would carry this function's derived value back
		// into the caller's config.
		cfgCopy := api.ScionConfig{}
		if inlineCfg != nil {
			cfgCopy = *inlineCfg
		}
		cfgCopy.AuthSelectedType = opts.HarnessAuth
		inlineCfg = &cfgCopy
	}
	return ctx, inlineCfg
}

// finishProvision performs the steps common to Provision and Reprovision once
// the agent's on-disk config exists: persist the lightweight agent-config
// status file, (re-)stage the project pre-start hook, and persist an explicit
// HarnessAuth override onto scion-agent.json. Extracted (the same
// argument the design makes for deriveAgentConfig) so a future change to any
// of these three steps cannot silently apply to only one of the two callers.
func (m *AgentManager) finishProvision(opts api.StartOptions, agentDir, agentHome string, cfg *api.ScionConfig) error {
	_ = UpdateAgentConfig(opts.Name, opts.ProjectPath, "created", m.Runtime.Name(), opts.Profile)

	// Re-stage the project-level pre-start hook. Called unconditionally: an
	// empty script removes any previously staged file, so a hook that no
	// longer applies (e.g. deactivated between generations, or on a
	// first-time resume) cannot survive on a reused agent home.
	if err := harness.WriteProjectPreStartHook(agentHome, opts.ProjectPreStartHookScript); err != nil {
		return fmt.Errorf("stage project pre-start hook: %w", err)
	}

	// Persist harness auth override to the on-disk config (for sciontool).
	// The auth type was already applied via inlineConfig in
	// buildProvisionContext, but we re-write to ensure the final file
	// reflects the override.
	if opts.HarnessAuth != "" && cfg != nil {
		cfg.AuthSelectedType = opts.HarnessAuth
		// cfg carries the environment values ProvisionAgent merged in for
		// its caller; the file keeps the agent's own (ptone/scion#4561).
		// Likewise for the default limits (ptone/scion#623).
		persisted := cfg
		if env, ok := readPersistedAgentEnvironment(agentDir); ok {
			persisted = withEnvironment(cfg, env)
		}
		if limits, ok := readPersistedAgentLimits(agentDir); ok {
			persisted = withLimits(persisted, limits)
		}
		cfgData, marshalErr := json.MarshalIndent(persisted, "", "  ")
		if marshalErr == nil {
			_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), cfgData, 0644)
		}
	}
	return nil
}

// containerIsRunning reports whether a container named scion.name=name is
// currently running, per the runtime's own status string
// (phaseFromContainerStatus maps "Up ..."/"running" to "running"). Used by
// Reprovision's running-container precondition check (design §3.4 Amendment
// A2). A List error is not treated as "not running" by the caller — see the
// call site's comment.
//
// Agent names are unique only within a project, so entries that belong to
// another project on the same broker are skipped: otherwise a running agent
// with the same name elsewhere would make every reprovision of this one
// refuse. The scoping is a post-filter (matchAgentProject), not a runtime
// label filter, so it fails closed: an entry whose project cannot be told
// (legacy labels, project ID only on AgentInfo, or no project info at all)
// still counts as this agent's container.
func (m *AgentManager) containerIsRunning(ctx context.Context, name, projectName, projectID string) (bool, error) {
	agents, err := m.listRuntime(ctx, map[string]string{"scion.name": name})
	if err != nil {
		return false, err
	}
	for _, a := range agents {
		if !matchAgentProject(a, projectName, projectID) {
			continue
		}
		if a.Name == name || strings.TrimPrefix(a.Name, "/") == name || strings.EqualFold(a.Name, name) {
			if strings.EqualFold(a.Phase, "running") {
				return true, nil
			}
		}
	}
	return false, nil
}

// ErrReprovisionRefused wraps every refusal Reprovision returns (design §3.4
// Amendment A4.2): the workspace preconditions and the running-container
// check. Callers can match it with errors.Is to distinguish "the primitive
// refused to run" from any other failure — the runtime broker handler maps
// it to 409, and the reincarnate worker records the reason on the
// AgentReincarnation record either way.
var ErrReprovisionRefused = errors.New("reprovision refused")

// Reprovision re-renders an existing agent's on-disk configuration
// (scion-agent.json, agent-info.json, home dotfiles, and skills) from the
// current template/harness-config catalog, for a `scion reincarnate` request
// (design §3.4, Amendments A2 and A23). Unlike Provision, it always calls
// ProvisionAgent directly rather than through GetAgent's "agent dir already
// exists → keep the persisted config" branch: reincarnation's entire point is
// to replace that persisted config with a freshly resolved one.
//
// Reprovision accepts two workspace modes, gated by api.ReincarnateEligible:
//   - clone-per-agent: opts.GitClone != nil, with an existing real clone on
//     disk.
//   - explicit mount (design §3.4 Amendment A23): opts.GitClone == nil and a
//     non-empty opts.Workspace — shared-workspace and hub-managed projects.
//     The agent directory and the workspace path must already exist;
//     Reprovision never creates either.
//   - empty-per-agent (miller79/scion#167): opts.EmptyPerAgentWorkspace with
//     no GitClone, Workspace or shared workspace. The agent must have been
//     created empty-per-agent (persisted scion-agent.json), and its private
//     <agentDir>/workspace must already exist as a real directory (not a
//     symlink); it is reused in place with its content, never created or
//     cleared. Refused unless the runtime is local-disk
//     (api.IsLocalDiskRuntime).
//
// Preconditions are enforced here rather than merely documented (design §3.4
// Amendments A2.1/A2.4 and A23), before ProvisionAgent ever runs:
// ProvisionAgent's worktree-creation branch removes and recreates the
// agent's workspace directory whenever its branch lookups find no existing
// worktree (those lookups once ran against the process CWD, which on a
// broker is outside the repo; they now run against the project repository,
// ptone/scion#4439) — so calling it for a clone-per-agent agent without
// first confirming a real, existing clone risks os.RemoveAll on a live
// worktree-per-agent workspace, destroying uncommitted work. Likewise,
// ProvisionAgent unconditionally creates the agent directory (os.MkdirAll)
// before it even looks at the workspace mode, so the explicit-mount case
// needs its own existence checks to avoid silently standing up a fresh,
// empty agent directory for a name that was never actually provisioned.
// Phase 4 may replace the clone-per-agent refusal with a proper "workspace
// exists → reuse as-is" branch.
//
// Once past those checks, Reprovision never touches:
//   - the agent's workspace, clone-per-agent or explicit-mount (ProvisionAgent's
//     git-clone branch leaves an existing real clone in place; the
//     explicit-mount branch only mounts the path Reprovision already verified
//     exists — it never creates, clones, pulls, resets, or removes it);
//   - the agent's branch;
//   - unrelated files already in the agent's home directory (only files the
//     template/harness-config/platform-skill layers themselves own are
//     overlaid — see ContextWithReprovision's ForceOverwrite wiring for
//     platform skills specifically);
//   - a sibling agent's directory (both branches resolve strictly this
//     agent's own agentDir via checkAgentDirContained/CheckAgentDirContained).
func (m *AgentManager) Reprovision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	ctx = m.withWorkspaceLock(ctx)
	hasGitClone := opts.GitClone != nil
	if !api.ReincarnateEligible(hasGitClone, opts.Workspace, opts.EmptyPerAgentWorkspace) {
		return nil, fmt.Errorf("%w: agent %q is neither clone-per-agent, empty-per-agent, nor has an explicit workspace; reincarnate currently supports those workspace modes only", ErrReprovisionRefused, opts.Name)
	}
	// Empty-per-agent combined with another workspace source is
	// contradictory. ProvisionAgent refuses it too, but with an untyped
	// error (a 500 at the broker); refusing here keeps it a typed 409.
	if opts.EmptyPerAgentWorkspace && (hasGitClone || opts.Workspace != "" || opts.SharedWorkspace) {
		return nil, fmt.Errorf("%w: agent %q is empty-per-agent but the request also names a git clone, workspace path or shared workspace", ErrReprovisionRefused, opts.Name)
	}

	projectDir, pdErr := config.GetResolvedProjectDir(opts.ProjectPath)
	if pdErr != nil {
		return nil, fmt.Errorf("reprovision: resolve project dir: %w", pdErr)
	}

	// agentDir is resolved and existence-checked up front, for both
	// branches, instead of derived after ProvisionAgent returns: the
	// explicit-mount branch does not set its returned workspace to
	// agentDir/workspace (see ProvisionAgent's Case 1), so there is no
	// single after-the-fact derivation that works for both modes. It is
	// resolved exactly as ProvisionAgent resolves it below (withAgentStateDir
	// from the provisioning context: shared-workspace flag, hub project ID,
	// strict in broker mode), so both read the same broker-side agent dir.
	provCtx, _ := buildProvisionContext(ctx, opts)
	_, resolvedAgentDir, _, resolveErr := withAgentStateDir(provCtx, projectDir, opts.Name)
	if resolveErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrReprovisionRefused, resolveErr)
	}
	var agentDir string

	if hasGitClone {
		agentDir = resolvedAgentDir
		agentWorkspace := filepath.Join(agentDir, "workspace")
		if info, statErr := os.Stat(filepath.Join(agentWorkspace, ".git")); statErr != nil || !info.IsDir() {
			return nil, fmt.Errorf("%w: agent %q has no existing git clone at %s; reincarnate does not create or recreate the workspace", ErrReprovisionRefused, opts.Name, agentWorkspace)
		}
	} else if opts.EmptyPerAgentWorkspace {
		dir, err := reprovisionEmptyPerAgentPreflight(projectDir, opts.Name, m.Runtime)
		if err != nil {
			return nil, err
		}
		agentDir = dir
	} else {
		// Design §3.4 Amendment A23: explicit-mount case. Confirm this is an
		// existing agent (the agent dir resolved above, via the os.Stat
		// below) and that the workspace path — absolute, or a
		// project-relative subdir exactly as ProvisionAgent's own
		// explicit-workspace branch accepts — already exists. A miss here is
		// refused as ErrReprovisionRefused (409) rather than left to
		// ProvisionAgent's untyped error (or, worse, ProvisionAgent silently
		// creating the missing piece).
		dir := resolvedAgentDir
		if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
			return nil, fmt.Errorf("%w: agent %q has no existing agent directory at %s; reincarnate does not create it", ErrReprovisionRefused, opts.Name, dir)
		}
		agentDir = dir

		if filepath.IsAbs(opts.Workspace) {
			// Upstream review (GoogleCloudPlatform/scion#2037, comment
			// 4121261313): split the existence check from the directory
			// check, so a workspace path that exists but is a regular file
			// gets its own precise message instead of being reported as
			// "does not exist" -- the same split the relative-path branch
			// below already makes.
			info, statErr := os.Stat(opts.Workspace)
			if statErr != nil {
				return nil, fmt.Errorf("%w: agent %q workspace path does not exist: %s; reincarnate does not create it", ErrReprovisionRefused, opts.Name, opts.Workspace)
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("%w: agent %q workspace path is not a directory: %s", ErrReprovisionRefused, opts.Name, opts.Workspace)
			}
		} else {
			// Upstream review (GoogleCloudPlatform/scion#2037, comment
			// 4121261307): LoadEffectiveSettings' error was previously
			// ignored. A load failure (e.g. a malformed settings.yaml) is a
			// real environment problem, not a "this reincarnation is
			// ineligible" refusal -- resolveProjectRoot would otherwise run
			// against a nil/stale *VersionedSettings and could silently
			// resolve the wrong project root, misjudging the relative
			// --workspace containment check below. Returned unwrapped (not
			// ErrReprovisionRefused), matching the resolve-project-dir error
			// a few lines above: this surfaces as the broker's generic 500,
			// not its typed 409 refusal, because it is not a precondition
			// refusal but an inability to evaluate the request at all. This
			// runs after the hub has already stopped the agent's container
			// (§3.7), same as every other precondition here; the
			// reincarnation is recorded failed with this error as the
			// reason, and the agent stays stopped until a retry.
			settings, _, err := config.LoadEffectiveSettingsFor(ctx, projectDir)
			if err != nil {
				return nil, fmt.Errorf("reprovision: load effective settings: %w", err)
			}
			projectRoot := resolveProjectRoot(settings, projectDir)
			resolved, err := resolveWorkspaceSubdir(projectRoot, opts.Workspace)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrReprovisionRefused, err)
			}
			// O2 (review p1b-r1): resolveWorkspaceSubdir already confirms
			// the resolved path exists, but not that it is a directory —
			// unlike the absolute-path branch above. A relative workspace
			// that resolves to a regular file must be refused the same way.
			if info, statErr := os.Stat(resolved); statErr != nil || !info.IsDir() {
				return nil, fmt.Errorf("%w: agent %q workspace path is not a directory: %s", ErrReprovisionRefused, opts.Name, resolved)
			}
		}
	}

	// A broker unreachable/unresponsive List error is logged and tolerated
	// (proceeding is the pre-fail-closed behavior; the hub is expected to
	// have stopped the container already), but an affirmative "yes, still
	// running" answer is always honored.
	//
	// The check is scoped to this agent's project (ptone/scion#5510). The
	// broker sets both HubProjectID and Env SCION_PROJECT_ID from the
	// dispatch's project ID; HubProjectID is read first and the Env value is
	// the fallback. The project name scopes entries or requests without an ID.
	reprovisionProjectID := opts.HubProjectID
	if reprovisionProjectID == "" && opts.Env != nil {
		reprovisionProjectID = opts.Env["SCION_PROJECT_ID"]
	}
	if running, err := m.containerIsRunning(ctx, opts.Name, config.GetProjectName(projectDir), reprovisionProjectID); err != nil {
		util.Debugf("reprovision: failed to check running state for %s, proceeding: %v", opts.Name, err)
	} else if running {
		return nil, fmt.Errorf("%w: agent %q container is still running; stop it first", ErrReprovisionRefused, opts.Name)
	}

	// An explicit shared-dir backend change is checked before anything is
	// provisioned, and recorded only after provisioning succeeds.
	var sdChange *pendingSharedDirBackendChange
	if len(opts.SharedDirBackendChanges) > 0 || opts.AllowEmptySharedDir {
		c, err := prepareSharedDirBackendChange(ctx, projectDir, agentDir, opts)
		if err != nil {
			return nil, err
		}
		sdChange = c
	}

	ctx, inlineCfg := buildProvisionContext(ctx, opts)
	ctx = api.ContextWithReprovision(ctx)

	agentHome, _, cfg, err := ProvisionAgent(ctx, opts.Name, opts.Template, opts.Image, opts.HarnessConfig, opts.ProjectPath, opts.Profile, "created", opts.Branch, opts.Workspace, inlineCfg)
	if err != nil {
		return cfg, err
	}

	if err := m.finishProvision(opts, agentDir, agentHome, cfg); err != nil {
		return cfg, err
	}

	// prompt.md must never hold the previous generation's task once the
	// disk is re-rendered for the new one (ptone/scion#3985). The new
	// generation's first task (the hub-built preamble plus handoff) is
	// still delivered by the subsequent DispatchAgentStart call, but if
	// that start never reaches this broker's Start (a timeout, a deferred
	// or failed start), a later task-less, non-resume start falls back to
	// prompt.md and would otherwise replay generation N's task. So stage
	// the request's task here, matching AppliedConfig.Task (the hub's own
	// restart source), or empty the file when the request carries none.
	// Staging does not deliver the task: prompt.md is only read by Start as
	// a fallback when its request has no task, and a start that carries
	// the same task overwrites the file with it and delivers it once.
	// This runs before the shared dir backend change is recorded, so a
	// failed write leaves that change unrecorded ("recorded only after
	// provisioning succeeds").
	if err := writeReprovisionPrompt(agentDir, opts.Task); err != nil {
		return cfg, err
	}

	if sdChange != nil {
		if err := sdChange.record(opts, cfg); err != nil {
			return cfg, err
		}
	}

	return withProvisionedImage(ctx, opts, agentDir, cfg)
}

// writeReprovisionPrompt replaces prompt.md in agentDir with task, or
// empties it when task is empty, so no earlier generation's task survives
// a reprovision.
func writeReprovisionPrompt(agentDir, task string) error {
	promptFile := filepath.Join(agentDir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte(task), 0644); err != nil {
		return fmt.Errorf("reprovision: failed to write prompt.md: %w", err)
	}
	return nil
}

// reprovisionEmptyPerAgentPreflight runs Reprovision's empty-per-agent
// preconditions (miller79/scion#167) and returns the agent's directory. The
// agent's private <agentDir>/workspace is reused in place, so it must
// already exist as a real directory: every check here runs before
// ProvisionAgent, whose MkdirAll would otherwise create a missing workspace.
// All refusals wrap ErrReprovisionRefused (409 at the broker).
func reprovisionEmptyPerAgentPreflight(projectDir, agentName string, rt runtime.Runtime) (string, error) {
	agentDir, err := CheckAgentDirContained(projectDir, agentName, false)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrReprovisionRefused, err)
	}
	if !persistedEmptyPerAgent([]string{filepath.Dir(agentDir)}, "", agentName) {
		return "", fmt.Errorf("%w: agent %q was not created empty-per-agent", ErrReprovisionRefused, agentName)
	}
	agentWorkspace := filepath.Join(agentDir, "workspace")
	info, statErr := os.Lstat(agentWorkspace)
	if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%w: agent %q workspace missing or not a real directory at %s; reincarnate never recreates it", ErrReprovisionRefused, agentName, agentWorkspace)
	}
	// Defence in depth, on the same allow-list the hub gates on
	// (api.IsLocalDiskRuntime): on Kubernetes the empty-per-agent workspace
	// is pod-local or on an NFS export, and cloud runtimes have no
	// broker-disk directory at all.
	rtName := ""
	if rt != nil {
		rtName = rt.Name()
	}
	if !api.IsLocalDiskRuntime(rtName) {
		return "", fmt.Errorf("%w: agent %q: empty-per-agent reincarnation is supported only on local-disk runtimes (docker, podman, container), not %q", ErrReprovisionRefused, agentName, rtName)
	}
	return agentDir, nil
}

func (m *AgentManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	// A forced move's provision never touches an agent directory it did not
	// create: an existing one (even an empty leftover that GetAgent would
	// clear as stale) refuses the provision before anything runs.
	if opts.MoveReincarnationID != "" {
		agentDir, err := provisionAgentStateDir(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve the agent directory: %v", ErrMoveWorkspaceUnconfirmed, err)
		}
		if err := checkMovePathAbsent(agentDir); err != nil {
			return nil, err
		}
	}
	ctx = m.withWorkspaceLock(ctx)
	ctx, inlineCfg := buildProvisionContext(ctx, opts)
	agentDir, agentHome, _, cfg, err := GetAgent(ctx, opts.Name, opts.Template, opts.Image, opts.HarnessConfig, opts.ProjectPath, opts.Profile, "created", opts.Branch, opts.Workspace, inlineCfg)
	if err != nil {
		return cfg, err
	}

	if err := m.finishProvision(opts, agentDir, agentHome, cfg); err != nil {
		return cfg, err
	}
	// A create: the later start must not mistake this agent for one that
	// predates per-agent NFS directories (see nfsKeepSharedCheckout).
	if opts.FreshProvision {
		recordNFSAgentDir(agentDir, opts.Name)
	}

	// A provision-only create carries no run (the hub mints runs only for
	// starts), yet it may reuse, or newly provision, files under a name an
	// earlier same-named agent's late delete still targets by that agent's
	// run. Record a provision owner so such a delete leaves these files
	// alone (ptone/scion#2675). The agent's own delete names no run until
	// it is started, and its first start records its run in place of this.
	// If a runtime entry for the name still exists in this project (or the
	// runtime cannot be listed), the files stay that entry's run's.
	// The owner is recorded in the host-owned HostStateFile in agentDir
	// (recordRunOwnerInAgentDir); a failure, including a record that would
	// not land in agentDir, is logged and the provision carries on.
	if opts.FreshProvision && opts.RunID == "" && !m.hasRuntimeEntry(ctx, opts) {
		if err := recordRunOwnerInAgentDir(RunOwnerTargetFor(opts), agentDir, ProvisionOwnerPrefix+uuid.NewString()); err != nil {
			slog.Warn("Provision: failed to record the provision owner in the host state file",
				"agent", opts.Name, "error", err)
		}
	}

	// If a task was provided, write it to prompt.md for later execution
	if opts.Task != "" {
		promptFile := filepath.Join(agentDir, "prompt.md")
		if writeErr := os.WriteFile(promptFile, []byte(opts.Task), 0644); writeErr != nil {
			return cfg, fmt.Errorf("failed to write task to prompt.md: %w", writeErr)
		}
	}

	return withProvisionedImage(ctx, opts, agentDir, cfg)
}

// resolveHarnessConfigDir returns the harness-config directory for an agent,
// preferring a Hub-hydrated path recorded on the context (§7.3 step 4) over the
// on-disk FindHarnessConfigDir search. This lets a broker that lacks the
// harness-config locally use the copy fetched from the Hub's storage backend.
// The ordering itself lives in config.ResolveHarnessConfigDir, which the
// runtime broker's preflights share.
func resolveHarnessConfigDir(ctx context.Context, name, projectPath string, templatePaths ...string) (*config.HarnessConfigDir, error) {
	return config.ResolveHarnessConfigDir(api.HarnessConfigPathFromContext(ctx), name, projectPath, templatePaths...)
}

// isGitWorkspaceProject reports whether projectDir should be treated as a
// git project when deciding how to provision or validate its workspace
// source. Inside an agent container (SCION_HOST_UID set), git detection is
// suppressed: container worktrees produce path-identity mismatches, since
// --relative-paths are computed against the container mount layout, not the
// host filesystem. ProvisionAgent (resolving the workspace source on first
// provision) and Start()'s workspaceSourceRoots (validating it on every
// start, including resume) both call this, so a workspace provisioned
// through the non-git branch under SCION_HOST_UID is never re-classified as
// git later and checked against a repo root it was never provisioned
// relative to.
func isGitWorkspaceProject(projectDir string) bool {
	return util.IsGitRepoDir(projectDir) && os.Getenv("SCION_HOST_UID") == ""
}

// isProjectConfigsPath reports whether projectDir is a marker-resolved,
// externalized project directory under the global project-configs tree
// (~/.scion/project-configs/<dir>/.scion, config.ResolveProjectMarker's
// output for a hub-dispatched project whose own directory holds only a
// .scion marker file, not a full .scion directory). Unlike a plain
// externalized project's own .scion directory, filepath.Dir(projectDir)
// here names storage for the project's configuration only, never the
// project's actual files.
func isProjectConfigsPath(projectDir string) bool {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return false
	}
	projectConfigsDir := filepath.Join(globalDir, config.ProjectConfigsDir)
	return filepath.Dir(filepath.Dir(projectDir)) == projectConfigsDir
}

// resolvedTemplate is the read-only outcome of resolving an agent's template
// chain and harness-config: no provisioning, clone, file write or runtime
// call. It is the return value of resolveTemplateAndHarnessConfig.
type resolvedTemplate struct {
	// Config is the template chain's merged scion-agent config (template
	// layers plus, when supplied, the inline config), before the
	// harness-config base layer and its Harness/HarnessConfig fields are
	// applied by the caller.
	Config *api.ScionConfig
	// Chain is the resolved template chain, in merge order.
	Chain []*config.Template
	// TemplatePaths is Chain's on-disk paths, in the same order.
	TemplatePaths []string
	// HarnessConfigName is the resolved harness-config name.
	HarnessConfigName string
	// HarnessConfigDir is the loaded harness-config directory.
	HarnessConfigDir *config.HarnessConfigDir
}

// resolveTemplateAndHarnessConfig resolves templateName's template chain and
// the agent's harness-config, merging template (and, when supplied, inline)
// config along the way. It performs no provisioning, clone, file write or
// runtime call — only template/harness-config lookups and in-memory merges —
// so it is safe to call before an agent directory exists.
//
// ProvisionAgent calls this to build the on-disk config it then writes.
// Manager.Preflight (design t1-async-create-v11.md §3.1 "Admission", §7
// P1b-1) calls it too, so that an async create's ErrTemplateNotFound /
// ErrHarnessConfigNotFound surfaces synchronously during admission, exactly
// as GetAgent/ProvisionAgent would raise it — the two resolutions go through
// this one function and cannot drift apart.
//
// projectPath is the project path as the caller supplied it; the template
// chain is resolved against it. projectDir is config.GetResolvedProjectDir's
// result for it: harness-config resolution uses that single resolved project
// dir for provisioning, launch (Start's harness.Resolve) and the runtime
// broker's policy gate.
func resolveTemplateAndHarnessConfig(ctx context.Context, templateName, harnessConfig, projectPath, projectDir, profileName string, settings *config.VersionedSettings, inlineCfg *api.ScionConfig) (*resolvedTemplate, error) {
	chain, err := config.GetTemplateChainInProject(templateName, projectPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load template: %w", err)
	}

	// Each template contributes its job keys only (ptone/scion#4524):
	// templateJobConfig drops its environment keys (reported) and its env,
	// which is the separate template env layer Start applies as a final
	// fill. Template env is therefore never merged into the agent config or
	// persisted in scion-agent.json.
	finalScionCfg := &api.ScionConfig{}
	for _, tpl := range chain {
		// Load scion-agent config from this template and merge it
		tplCfg, err := tpl.LoadConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to load config from template %s: %w", templateRef(tpl), err)
		}

		// Validate: reject legacy templates that still have a 'harness' field
		if err := config.ValidateAgnosticTemplate(tplCfg); err != nil {
			return nil, fmt.Errorf("template %s: %w", templateRef(tpl), err)
		}

		finalScionCfg = config.MergeScionConfig(finalScionCfg, templateJobConfig(ctx, tpl, tplCfg))
	}

	if inlineCfg != nil {
		finalScionCfg = config.MergeScionConfig(finalScionCfg, inlineCfg)
	}

	// Resolve harness-config name (unified resolution chain)
	hcResolution, err := config.ResolveHarnessConfigName(config.HarnessConfigInputs{
		CLIFlag:     harnessConfig,
		TemplateCfg: finalScionCfg,
		Settings:    settings,
		ProfileName: profileName,
	})
	if err != nil {
		return nil, err
	}
	harnessConfigName := hcResolution.Name

	// Load harness-config from disk (check template dirs first)
	var templatePaths []string
	for _, tpl := range chain {
		templatePaths = append(templatePaths, tpl.Path)
	}
	hcDir, err := resolveHarnessConfigDir(ctx, harnessConfigName, projectDir, templatePaths...)
	if err != nil {
		return nil, fmt.Errorf("failed to find harness-config %q: %w", harnessConfigName, err)
	}
	if err := CheckHarnessConfigPolicy(ctx, harnessConfigName, harness.EffectiveConfig(harnessConfigName, hcDir, settings, profileName)); err != nil {
		return nil, err
	}

	return &resolvedTemplate{
		Config:            finalScionCfg,
		Chain:             chain,
		TemplatePaths:     templatePaths,
		HarnessConfigName: harnessConfigName,
		HarnessConfigDir:  hcDir,
	}, nil
}

// Preflight resolves opts' template and harness config through
// resolveTemplateAndHarnessConfig — the same function GetAgent/ProvisionAgent
// use — without provisioning, cloning, writing files or calling the runtime
// (design t1-async-create-v11.md §3.1 "Admission", §7 P1b-1). It surfaces
// config.ErrTemplateNotFound / config.ErrHarnessConfigNotFound synchronously,
// during admission, for an async create: Manager.Start would otherwise raise
// the same errors, but only from inside the launch goroutine.
func (m *AgentManager) Preflight(ctx context.Context, opts api.StartOptions) error {
	return PreflightResolve(ctx, opts)
}

// PreflightResolve is Preflight's resolution as a package function: it
// resolves opts' template chain and harness-config exactly as
// ProvisionAgent does (resolveTemplateAndHarnessConfig), evaluating any
// harness-config policy on ctx, and has no side effects. The runtime broker
// calls it during create admission, before any workspace step, so a policy
// refusal happens before a worktree, agent directory, staged bundle or
// container exists.
func PreflightResolve(ctx context.Context, opts api.StartOptions) error {
	ctx, inlineCfg := buildProvisionContext(ctx, opts)

	projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath)
	if err != nil {
		return err
	}

	settings, warnings, _ := config.LoadEffectiveSettingsFor(ctx, projectDir)
	config.PrintDeprecationWarnings(warnings)

	profileName := opts.Profile
	if profileName == "" && settings != nil {
		profileName = settings.ActiveProfile
	}

	templateName := opts.Template
	if templateName == "" {
		defaultTemplate := "default"
		if settings != nil && settings.DefaultTemplate != "" {
			defaultTemplate = settings.DefaultTemplate
		}
		templateName = defaultTemplate
	}

	// The real provisioning repeats this resolution and reports any ignored
	// template environment keys; stay quiet here so each is reported once.
	_, err = resolveTemplateAndHarnessConfig(withQuietTemplateEnvWarnings(ctx), templateName, opts.HarnessConfig, opts.ProjectPath, projectDir, profileName, settings, inlineCfg)
	return err
}

// plainProjectWorkspaceSource is the directory a plain non-git project's
// agents mount as their workspace (ProvisionAgent's Case 3 fallback): the
// directory that holds the project directory. DeleteAgentFiles protects it
// from removal through the same function.
func plainProjectWorkspaceSource(projectDir string) string {
	return filepath.Dir(projectDir)
}

// resolveProjectRoot determines the project root directory on this broker.
// Used for resolving relative --workspace paths against the project's logical root.
func resolveProjectRoot(settings *config.VersionedSettings, projectDir string) string {
	if settings != nil && settings.WorkspacePath != "" {
		return settings.WorkspacePath
	}
	if filepath.Base(projectDir) == config.DotScion {
		return filepath.Dir(projectDir)
	}
	return projectDir
}

// resolveWorkspaceSubdir resolves a relative workspace subdirectory path
// against a project root, with containment checks to prevent directory
// traversal and symlink escapes.
func resolveWorkspaceSubdir(projectRoot, subdir string) (string, error) {
	if filepath.IsAbs(subdir) {
		return "", fmt.Errorf("workspace subdir must be relative, got absolute path: %s", subdir)
	}

	cleaned := filepath.Clean(subdir)
	if cleaned == "." {
		return projectRoot, nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("workspace subdir %q escapes project root (contains '..')", subdir)
	}

	joined := filepath.Join(projectRoot, cleaned)

	if _, err := os.Stat(joined); os.IsNotExist(err) {
		return "", fmt.Errorf("workspace subdirectory does not exist: %s", joined)
	}

	realRoot, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		return "", fmt.Errorf("failed to resolve project root: %w", err)
	}
	realJoined, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", fmt.Errorf("failed to resolve workspace subdir: %w", err)
	}

	rel, err := filepath.Rel(realRoot, realJoined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("workspace subdir %q resolves to %s which is outside project root %s", subdir, realJoined, realRoot)
	}

	return realJoined, nil
}

// checkAgentDirContained computes the on-disk directory for agentName under
// projectDir exactly as config.GetAgentDir does, and confirms the result is
// still a direct child of the same root config.SelectAgentsRoot selected --
// the external agents dir when sharedWorkspace is true and one is
// configured, else <projectDir>/agents (GetAgentDir is defined in terms of
// SelectAgentsRoot, so the two roots can never drift apart).
//
// Both ProvisionAgent and GetAgent call this immediately after resolving
// agentName into a directory, before either one creates, removes, or
// otherwise acts on it: ProvisionAgent's git-clone and worktree branches
// clear an existing workspace under agentDir, and GetAgent's stale-
// directory branch removes agentDir outright. agentName is expected to be a
// single path element by the time it reaches either function (see
// runtimebroker's isSingleCleanPathElement, the other half of this
// defense-in-depth pair), but neither caller is guaranteed to have gone
// through that check -- Reprovision calls ProvisionAgent directly, not
// through GetAgent -- so this fails closed on its own rather than trust the
// caller.
func checkAgentDirContained(projectDir, agentName string, sharedWorkspace bool) (string, error) {
	dir, _, err := agentStateDir(projectDir, agentName, sharedWorkspace, "", false)
	return dir, err
}

// agentStateDir is the single resolution of an agent's broker-side state
// directory (scion-agent.json, prompt.md, the harness inputs and secrets
// records, image provenance) for GetAgent and ProvisionAgent. Agent state for
// shared-workspace projects is always resolved from the broker-side agent
// dir:
//   - the external agents root comes from config.AgentDirForProject, from the
//     hub-supplied project ID when there is one (never from the project-id
//     marker inside the project, which a shared workspace exposes to
//     containers);
//   - an agent whose external directory holds scion-agent.json is a
//     shared-workspace agent whatever sharedWorkspace says (in worktree mode
//     only home/ is external, never scion-agent.json), so its in-project
//     <project>/agents/<name>, which a shared workspace mount exposes to
//     containers, is never used (except when the external root is the
//     project's own agents root, as in a hub-native project; see
//     effectiveSharedWorkspace);
//   - with strict set (broker mode, or a hub-supplied project ID), a
//     shared-workspace agent whose external root cannot be determined is an
//     error (config.ErrAgentStateDirUnavailable), never the in-project root.
//     Without strict (a local CLI start of a project with no project ID and
//     no hub), there is no external root at all and the in-project root is
//     used, as for any local project.
//
// The returned shared flag is the effective one. The choice depends only on
// broker-side state and the caller's inputs, and is made before anything is
// read from the agent directory.
func agentStateDir(projectDir, agentName string, sharedWorkspace bool, hubProjectID string, strict bool) (string, bool, error) {
	shared := effectiveSharedWorkspace(projectDir, agentName, sharedWorkspace, hubProjectID)
	dir, err := config.AgentDirForProject(projectDir, agentName, shared, hubProjectID)
	if err != nil && shared && !strict && hubProjectID == "" && errors.Is(err, config.ErrAgentStateDirUnavailable) {
		dir, err = config.AgentDirForProject(projectDir, agentName, false, "")
	}
	if err != nil {
		return "", shared, err
	}
	return dir, shared, nil
}

// effectiveSharedWorkspace reports whether agentName's state lives in the
// broker-side (external) agents directory: sharedWorkspace, or an external
// agent directory (located from hubProjectID when set) holding a regular
// scion-agent.json.
//
// When the external agents root is the project's own agents root (a
// hub-native project, whose resolved project dir is the external
// project-config dir), an existing scion-agent.json there says nothing about
// the workspace mode, so only sharedWorkspace counts.
func effectiveSharedWorkspace(projectDir, agentName string, sharedWorkspace bool, hubProjectID string) bool {
	if sharedWorkspace {
		return true
	}
	ext, err := config.AgentDirForProject(projectDir, agentName, true, hubProjectID)
	if err != nil {
		return false
	}
	if sameDir(filepath.Dir(ext), filepath.Join(projectDir, "agents")) {
		return false
	}
	info, err := os.Stat(filepath.Join(ext, "scion-agent.json"))
	return err == nil && info.Mode().IsRegular()
}

// sameDir reports whether a and b name the same directory: equal cleaned
// paths, or both exist and are the same file (e.g. via a symlink).
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// withAgentStateDir resolves agentName's state directory with agentStateDir
// from ctx's shared-workspace flag and hub project ID, and returns ctx with
// the effective shared-workspace flag.
func withAgentStateDir(ctx context.Context, projectDir, agentName string) (context.Context, string, bool, error) {
	requested := api.IsSharedWorkspaceFromContext(ctx)
	hubProjectID := api.HubProjectIDFromContext(ctx)
	strict := api.IsBrokerModeFromContext(ctx) || hubProjectID != ""
	dir, shared, err := agentStateDir(projectDir, agentName, requested, hubProjectID, strict)
	if shared && !requested {
		ctx = api.ContextWithSharedWorkspace(ctx)
	}
	return ctx, dir, shared, err
}

// AgentNeedsProvision reports whether starting the agent opts names would
// provision it again rather than load its existing state: GetAgent
// provisions when the agent's state directory does not exist, or exists
// without a scion-agent.json (a stale directory it removes first). The state
// directory is resolved the way Start resolves it, from the same options
// (broker mode, shared workspace, Hub project ID). It reads the filesystem
// only and changes nothing.
func AgentNeedsProvision(ctx context.Context, opts api.StartOptions) (bool, error) {
	agentDir, err := provisionAgentStateDir(ctx, opts)
	if err != nil {
		return false, err
	}
	return !agentStateProvisioned(agentDir), nil
}

// provisionAgentStateDir resolves the broker-side state directory of the
// agent opts names exactly as Provision and Start resolve it: from the
// resolved project dir and the provisioning context (broker mode, shared
// workspace, Hub project ID). It reads the filesystem only and changes
// nothing. AgentNeedsProvision and CheckMoveWorkspaceAbsent share it, so
// neither computes the directory a second way.
func provisionAgentStateDir(ctx context.Context, opts api.StartOptions) (string, error) {
	projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath)
	if err != nil {
		return "", err
	}
	ctx, _ = buildProvisionContext(ctx, opts)
	_, agentDir, _, err := withAgentStateDir(ctx, projectDir, opts.Name)
	return agentDir, err
}

// agentStateProvisioned reports whether agentDir holds a provisioned agent:
// the directory exists and has a scion-agent.json. A directory that cannot be
// read counts as provisioned, so a caller does not treat an unreadable agent
// as one to provision again.
func agentStateProvisioned(agentDir string) bool {
	if _, err := os.Stat(agentDir); err != nil {
		return !os.IsNotExist(err)
	}
	return config.GetScionAgentConfigPath(agentDir) != ""
}

// CheckAgentDirContained is the exported form of checkAgentDirContained, for
// callers outside this package that resolve an agent directory from a
// request-supplied name and need to verify containment before their own file
// operations -- e.g. runtimebroker's deleteAgent and startAgent handlers,
// which run this alongside their own isSingleCleanPathElement check at the
// request boundary, the same defense-in-depth pairing ProvisionAgent and
// GetAgent already use within this package.
func CheckAgentDirContained(projectDir, agentName string, sharedWorkspace bool) (string, error) {
	return checkAgentDirContained(projectDir, agentName, sharedWorkspace)
}

func ProvisionAgent(ctx context.Context, agentName string, templateName string, agentImage string, harnessConfig string, projectPath string, profileName string, optionalStatus string, branch string, workspace string, inlineConfig ...*api.ScionConfig) (_ string, _ string, _ *api.ScionConfig, retErr error) {
	provisionStart := time.Now()
	// 1. Prepare agent directories
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return "", "", nil, err
	}

	settings, warnings, _ := config.LoadEffectiveSettingsFor(ctx, projectDir)
	config.PrintDeprecationWarnings(warnings)
	if profileName == "" && settings != nil {
		profileName = settings.ActiveProfile
	}

	projectName := config.GetProjectName(projectDir)
	isGitWorkspace := util.IsGitRepoDir(projectDir) // preserve for skill configuration before container override
	isGit := isGitWorkspaceProject(projectDir)

	// Verify .gitignore if in a repo
	if isGit {
		// Find the projectDir relative to repo root if possible
		root, err := util.RepoRootDir(projectDir)
		if err == nil {
			rel, err := filepath.Rel(root, projectDir)
			if err == nil && !strings.HasPrefix(rel, "..") {
				agentsPath := filepath.Join(rel, "agents")
				if !util.IsIgnored(root, agentsPath+"/") {
					return "", "", nil, fmt.Errorf("security error: '%s/' must be in .gitignore when using a project-local project", agentsPath)
				}
				// Note: .scion/agents/ is the security-critical path (checked above).
				// .scion/ itself is intentionally NOT fully gitignored so that
				// templates/ and other config can be committed.
			}
		}
	}
	ctx, agentDir, sharedWorkspace, err := withAgentStateDir(ctx, projectDir, agentName)
	if err != nil {
		return "", "", nil, err
	}
	emptyPerAgent := api.IsEmptyPerAgentWorkspaceFromContext(ctx)
	if emptyPerAgent {
		// Empty-per-agent (design #2703) always means the private
		// agents/<slug>/workspace directory below; a request that also names
		// another workspace source is contradictory, so refuse it rather than
		// guess which one wins.
		switch {
		case workspace != "":
			return "", "", nil, fmt.Errorf("empty-per-agent workspace does not take a workspace path (got %q)", workspace)
		case sharedWorkspace:
			return "", "", nil, fmt.Errorf("empty-per-agent workspace cannot be combined with a shared workspace")
		case api.GitCloneFromContext(ctx) != nil:
			return "", "", nil, fmt.Errorf("empty-per-agent workspace cannot be combined with a git clone")
		}
	}

	agentHome := config.GetAgentHomePath(projectDir, agentName)
	// filesGuard is the agent files guard for every removal below, built on
	// first use (projectAgentFilesGuard).
	var filesGuardMemo *agentFilesGuard
	filesGuard := func() *agentFilesGuard {
		if filesGuardMemo == nil {
			filesGuardMemo = projectAgentFilesGuard(ctx, projectDir, agentName, agentDir, agentHome)
		}
		return filesGuardMemo
	}
	// In worktree mode the workspace lives under agentDir so git's relative
	// worktree pointers resolve correctly. In shared-workspace mode there is
	// no per-agent workspace dir — the project-wide checkout is mounted directly.
	var agentWorkspace string
	if !sharedWorkspace {
		agentWorkspace = filepath.Join(agentDir, "workspace")
	}

	// Migrate any pre-existing in-project state for shared-workspace agents to
	// the external location so siblings stop seeing it via /workspace. This
	// covers agents provisioned before the shared-workspace isolation change.
	if sharedWorkspace {
		legacyDir := filepath.Join(projectDir, "agents", agentName)
		if legacyDir != agentDir {
			migrateLegacyAgentState(legacyDir, agentDir)
		}
	}

	// A reincarnation reprovision of an empty-per-agent agent reuses the
	// private workspace in place and never creates it (miller79/scion#167).
	// Manager.Reprovision already checked it, but it is checked again here,
	// before anything is written, so a workspace removed in between is
	// refused rather than recreated empty by the MkdirAll below.
	reprovision := api.IsReprovisionFromContext(ctx)
	if emptyPerAgent && reprovision {
		if info, statErr := os.Lstat(agentWorkspace); statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", "", nil, fmt.Errorf("%w: agent %q workspace at %s is gone or no longer a real directory at provision time; reincarnate never recreates it", ErrReprovisionRefused, agentName, agentWorkspace)
		}
	}

	// 2a-inline. Capture the inline config (if provided) for the merge below.
	var inlineCfg *api.ScionConfig
	if len(inlineConfig) > 0 && inlineConfig[0] != nil {
		inlineCfg = inlineConfig[0]
	}

	// Capture the inline config's own image/pull-policy — if any — before
	// harness-config resolution. Deliberately NOT finalScionCfg.Image: that
	// already includes the template's contribution, and a template is
	// re-read live on every Start (see run.go), so persisting it here would
	// let a create-time template snapshot outrank the CURRENT template on a
	// later restart. Only the inline config has no live source to re-derive
	// from at Start time — a local restart's request has no --config unless
	// the caller repeats it — so only its contribution needs to survive via
	// agent-info.json (AgentInfo.ExplicitImage / .ExplicitImagePullPolicy),
	// as the explicit tier's fallback when the current Start request has no
	// inline image/pull-policy of its own (ptone/scion#2156).
	explicitImage := ""
	explicitPullPolicy := ""
	if inlineCfg != nil {
		explicitImage = inlineCfg.Image
		if inlineCfg.Kubernetes != nil {
			explicitPullPolicy = inlineCfg.Kubernetes.ImagePullPolicy
		}
	}

	// 2, 2b, 2c. Load the template chain, merge configs and resolve the
	// harness-config, through the same resolution Preflight uses. This runs
	// before the agent directory and workspace are created, so a resolution
	// failure (unknown template or harness-config) leaves nothing behind:
	// no agent dir, no worktree and no branch (ptone/scion#3135).
	rt, err := resolveTemplateAndHarnessConfig(ctx, templateName, harnessConfig, projectPath, projectDir, profileName, settings, inlineCfg)
	if err != nil {
		return "", "", nil, err
	}
	// Claim the agent dir and home by creating them: os.Mkdir succeeds only
	// for the caller that actually creates the directory, so of two
	// concurrent first provisions only one treats it as its own, and a
	// directory that already existed is never treated as created here.
	if err := os.MkdirAll(filepath.Dir(agentDir), 0755); err != nil {
		return "", "", nil, fmt.Errorf("failed to create agent directory: %w", err)
	}
	newAgentDir, err := claimDir(agentDir)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to create agent directory: %w", err)
	}
	// A forced move never provisions into an agent directory it did not
	// create.
	moveID := moveReincarnationIDFrom(ctx)
	if moveID != "" && !newAgentDir {
		return "", "", nil, fmt.Errorf("%w: the agent directory already exists", ErrMoveWorkspacePresent)
	}
	newAgentHome := false
	newAgentHomeParent := false
	if newAgentDir {
		// A failed first provision removes what this call created, so the
		// next start provisions the agent afresh. Without this, the copied
		// home would be left with no scion-agent.json, which GetAgent keeps
		// and refuses rather than removes (ptone/scion#4438). A directory
		// that existed before this call is never removed here.
		defer func() {
			if retErr == nil {
				return
			}
			slog.Warn("provision: failed before completing; removing the agent directory it created",
				"agent", agentName, "agent_dir", agentDir, "error", retErr)
			// A refusal is logged by the guard; nothing is removed then.
			_ = removeAgentDirGuarded(filesGuard(), agentDir, agentWorkspace)
			if newAgentHome && !pathWithin(agentHome, agentDir) {
				_ = removeAgentDirGuarded(filesGuard(), agentHome, "")
			}
			// The home's parent this call claimed, whether or not the home
			// itself was claimed (the home claim can fail after it).
			if homeParent := filepath.Dir(agentHome); newAgentHomeParent && !pathWithin(homeParent, agentDir) {
				if moveID != "" {
					// The move marker this call wrote there.
					_ = os.Remove(filepath.Join(homeParent, MoveMarkerFile))
				}
				// os.Remove only removes an empty directory.
				_ = os.Remove(homeParent)
			}
		}()
	}
	// A forced move's provision marks the agent directory it created as
	// the move's before anything else is written there (a failed write
	// fails the provision, and the deferred cleanup above removes the
	// directory); a rollback delete removes only directories marked with
	// the move's reincarnation ID (CheckMoveMarkers).
	if moveID != "" {
		if err := writeMoveMarker(agentDir, moveID); err != nil {
			return "", "", nil, fmt.Errorf("failed to write the move marker: %w", err)
		}
	}
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return "", "", nil, fmt.Errorf("failed to create agent directory: %w", err)
	}
	if newAgentDir {
		// A new agent chooses its home storage at its first start; see
		// resolveHomeStorage.
		if err := markHomeStoragePending(agentDir); err != nil {
			return "", "", nil, fmt.Errorf("failed to record the agent's home storage: %w", err)
		}
	}
	homeParent := filepath.Dir(agentHome)
	if err := os.MkdirAll(filepath.Dir(homeParent), 0755); err != nil {
		return "", "", nil, fmt.Errorf("failed to create agent home: %w", err)
	}
	if newAgentHomeParent, err = claimDir(homeParent); err != nil {
		return "", "", nil, fmt.Errorf("failed to create agent home: %w", err)
	}
	// In a split-storage layout the home's parent (the external agents
	// root's <name>) is another agent directory a forced move creates, and
	// one its rollback delete removes: mark it as the move's too, right
	// after claiming it.
	if moveID != "" && newAgentHomeParent {
		if err := writeMoveMarker(homeParent, moveID); err != nil {
			_ = os.Remove(filepath.Join(homeParent, MoveMarkerFile))
			_ = os.Remove(homeParent)
			return "", "", nil, fmt.Errorf("failed to write the move marker: %w", err)
		}
	}
	if newAgentHome, err = claimAgentHome(agentHome); err != nil {
		return "", "", nil, fmt.Errorf("failed to create agent home: %w", err)
	}
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		return "", "", nil, fmt.Errorf("failed to create agent home: %w", err)
	}

	// Create empty prompt.md in agent root
	promptFile := filepath.Join(agentDir, "prompt.md")
	if _, err := os.Stat(promptFile); os.IsNotExist(err) {
		if err := os.WriteFile(promptFile, []byte(""), 0644); err != nil {
			return "", "", nil, fmt.Errorf("failed to create prompt.md: %w", err)
		}
	}

	var workspaceSource string
	shouldCreateWorktree := false
	explicitWorkspace := false

	// Check for git clone mode from context
	gitClone := api.GitCloneFromContext(ctx)

	// Reject relative workspace for git-clone projects early, before the
	// workspace resolution logic where gitClone takes priority.
	if gitClone != nil && workspace != "" && !filepath.IsAbs(workspace) {
		return "", "", nil, fmt.Errorf("relative --workspace is not supported for git-clone projects; use an absolute path or remove --workspace")
	}

	// Workspace Resolution Logic
	if emptyPerAgent {
		// Empty-per-agent (design #2703): a private, initially empty, non-git
		// directory at <projectDir>/agents/<slug>/workspace. Checked first so
		// that neither settings.WorkspacePath, the global-project directory,
		// a surrounding git repository nor the filepath.Dir(projectDir)
		// fallback of Case 3 can hand the agent a shared directory. Existing
		// content is kept: a restart must not wipe the agent's work. A
		// reprovision never creates it; its existence was checked above.
		if !reprovision {
			if err := os.MkdirAll(agentWorkspace, 0755); err != nil {
				return "", "", nil, fmt.Errorf("failed to create workspace directory: %w", err)
			}
		}
	} else if gitClone != nil {
		// Git clone mode: ensure the workspace directory exists and is ready
		// for sciontool to clone into at container startup.
		//
		// If the directory already exists with a real git clone (.git as a
		// directory), preserve it — this is a stopped agent being restarted
		// and sciontool will skip the clone correctly.
		//
		// If the directory has a .git FILE (worktree pointer from a previous
		// local-mode run) or other non-clone content, clear it so sciontool
		// sees an empty workspace and performs a fresh clone.
		if info, err := os.Stat(agentWorkspace); err == nil && info.IsDir() {
			gitDir := filepath.Join(agentWorkspace, ".git")
			gitDirInfo, gitErr := os.Stat(gitDir)
			isRealClone := gitErr == nil && gitDirInfo.IsDir()
			if !isRealClone {
				util.Debugf("provision: clearing stale workspace before git clone: %s", agentWorkspace)
				target, err := filesGuard().check(agentWorkspace)
				if err != nil {
					return "", "", nil, fmt.Errorf("failed to clear stale workspace dir: %w", err)
				}
				_ = util.MakeWritableRecursive(target)
				if err := os.RemoveAll(target); err != nil {
					return "", "", nil, fmt.Errorf("failed to clear stale workspace dir: %w", err)
				}
			}
		}
		if err := os.MkdirAll(agentWorkspace, 0755); err != nil {
			return "", "", nil, fmt.Errorf("failed to create workspace dir: %w", err)
		}
	} else if workspace != "" {
		// Case 1: Explicit Workspace provided
		// This overrides everything else. We mount this path directly.
		if filepath.IsAbs(workspace) {
			// Current behavior: mount this exact host path
			absWorkspace, err := filepath.Abs(workspace)
			if err != nil {
				return "", "", nil, fmt.Errorf("failed to resolve absolute path for workspace %s: %w", workspace, err)
			}
			if _, err := os.Stat(absWorkspace); os.IsNotExist(err) {
				return "", "", nil, fmt.Errorf("workspace path does not exist: %s", absWorkspace)
			}
			workspaceSource = absWorkspace
			agentWorkspace = ""
			explicitWorkspace = true
		} else {
			// NEW: resolve relative subdir against project root
			// (gitClone + relative workspace is rejected above)
			projectRoot := resolveProjectRoot(settings, projectDir)
			resolved, err := resolveWorkspaceSubdir(projectRoot, workspace)
			if err != nil {
				return "", "", nil, err
			}
			workspaceSource = resolved
			agentWorkspace = ""
			explicitWorkspace = true
		}

	} else if isGit {
		// Case 2: Git Repository (and no explicit workspace)
		// A flat Runtime Broker instance serializes the repository's
		// worktree and sharer changes with every instance of its host (the
		// host's workspace lock), through the rest of provisioning.
		if repoRoot, rootErr := util.RepoRootDir(projectDir); rootErr == nil && repoRoot != "" {
			unlockRepo, lockErr := lockWorkspace(ctx, repoRoot)
			if lockErr != nil {
				return "", "", nil, fmt.Errorf("workspace lock: %w", lockErr)
			}
			defer unlockRepo()
		}
		targetBranch := branch
		if targetBranch == "" {
			// Use slugified agent name for valid git branch names
			targetBranch = api.Slugify(agentName)
		}

		// Check if we should use an existing worktree. The branch and
		// worktree lookups run against the project's repository, not the
		// process's current directory: a runtime broker runs outside the
		// repository, and a CWD-relative lookup there misses the agent's
		// existing branch (ptone/scion#4439). That matters most when a
		// worktree-per-agent agent is resumed after its project switched to
		// a shared workspace: agentWorkspace is empty in shared mode, so the
		// agent's existing worktree is the only workspace it can reuse.
		//
		// Only attach to a worktree that is still in place: its directory
		// and its .git entry exist. A registration that fails this is not
		// pruned here: it may be another agent's worktree that this process
		// cannot see. The create path below removes the entry at this
		// agent's own workspace path, and git refuses to check the branch
		// out while any other worktree still holds it (ptone/scion#5071).
		usedExistingWorktree := false
		repoRoot, repoRootErr := util.RepoRootDir(projectDir)
		if repoRootErr == nil && util.BranchExistsIn(repoRoot, targetBranch) {
			if existingPath, err := util.FindWorktreeByBranchIn(repoRoot, targetBranch); err == nil && existingPath != "" && worktreeInPlace(existingPath) {
				workspaceSource = existingPath
				agentWorkspace = "" // Using external worktree
				usedExistingWorktree = true
				fmt.Printf("Warning: Relying on existing worktree for branch '%s' at '%s'\n", targetBranch, existingPath)
				// Register as sharer for refcounted teardown (I3). Fail loudly:
				// an untracked agent breaks the refcount (premature/leaked removal).
				if regErr := provision.RegisterSharer(repoRoot, projectDir, targetBranch, existingPath, agentName); regErr != nil {
					return "", "", nil, fmt.Errorf("register sharer (attach): %w", regErr)
				}
			}
		}

		if !usedExistingWorktree {
			shouldCreateWorktree = true
			// agentWorkspace remains set to agents/<name>/workspace
		}

	} else {
		// Case 3: Non-Git Repository (and no explicit workspace)
		agentWorkspace = "" // Using external mount, except in the project-configs branch below.
		if config.IsGlobalProjectDir(projectDir) {
			// The global project has no repository or externalized workspace
			// path of its own, so it owns a dedicated workspace directory
			// under its own project directory rather than mounting whatever
			// directory the CLI happened to be invoked from. Bootstrap it on
			// first use, the same way the sibling git-clone branch above
			// creates agentWorkspace. Namespaced by agentName, the same way
			// every other project type gives each agent its own workspace:
			// a bare ~/.scion/workspace shared by every global-project agent
			// would let two such agents silently read and write the same
			// files. agentName is already confirmed to be a single, safe
			// path element by checkAgentDirContained above.
			globalWorkspace := filepath.Join(projectDir, "workspace", agentName)
			if err := os.MkdirAll(globalWorkspace, 0755); err != nil {
				return "", "", nil, fmt.Errorf("failed to create global project workspace directory: %w", err)
			}
			workspaceSource = globalWorkspace
		} else if settings != nil && settings.WorkspacePath != "" {
			// Externalized project: use workspace-path from settings
			workspaceSource = settings.WorkspacePath
		} else if isProjectConfigsPath(projectDir) {
			// Hub-dispatched, non-git project whose projectDir was marker-
			// resolved (config.ResolveProjectMarker) to its externalized
			// ~/.scion/project-configs/<dir>/.scion: that directory holds
			// only the project's own configuration, never the project's
			// actual files, so mounting filepath.Dir(projectDir) directly
			// (the plain-externalized-project fallback below) would give
			// the agent an empty config directory to work in instead of a
			// real workspace. Use the same per-agent workspace shape the
			// git-clone and worktree branches above already create.
			agentWorkspace = filepath.Join(agentDir, "workspace")
			if err := os.MkdirAll(agentWorkspace, 0755); err != nil {
				return "", "", nil, fmt.Errorf("failed to create workspace directory: %w", err)
			}
		} else {
			workspaceSource = plainProjectWorkspaceSource(projectDir)
		}
	}

	// Worktree Creation (if needed)
	if shouldCreateWorktree && agentWorkspace == "" {
		// Shared-workspace mode has no per-agent workspace directory to
		// create a worktree in. Refuse clearly instead of running git
		// worktree creation with an empty path.
		return "", "", nil, fmt.Errorf("agent %q has no existing worktree for its branch in the project repository and the project uses a shared workspace, so there is no per-agent worktree path to create one in; pass a workspace path or re-create the agent", agentName)
	}
	if shouldCreateWorktree {
		worktreeStart := time.Now()
		// Remove existing workspace dir if it exists to allow worktree add
		if _, err := os.Lstat(agentWorkspace); err == nil {
			target, err := filesGuard().check(agentWorkspace)
			if err != nil {
				return "", "", nil, fmt.Errorf("failed to clear the workspace for the worktree: %w", err)
			}
			_ = util.MakeWritableRecursive(target)
			_ = os.RemoveAll(target)
		}
		// Remove a stale entry for this agent's own workspace path, so git
		// lets the worktree be added there again. Only that entry is
		// touched; when no repository holds the workspace there is nothing
		// of this agent's to remove (ptone/scion#5071).
		removeWorkspaceWorktreeEntry(agentWorkspace)

		worktreeBranch := branch
		if worktreeBranch == "" {
			// Use slugified agent name for valid git branch names
			worktreeBranch = api.Slugify(agentName)
		}

		if err := util.CreateWorktree(agentWorkspace, worktreeBranch); err != nil {
			return "", "", nil, fmt.Errorf("failed to create git worktree: %w", err)
		}
		util.Debugf("provision: worktree created in %s", time.Since(worktreeStart))
		// Register as sharer for refcounted teardown (I3). Fail loudly:
		// an untracked agent breaks the refcount (premature/leaked removal).
		root, rootErr := util.RepoRootDir(projectDir)
		if rootErr != nil {
			return "", "", nil, fmt.Errorf("resolve repo root for sharer registration: %w", rootErr)
		}
		if regErr := provision.RegisterSharer(root, projectDir, worktreeBranch, agentWorkspace, agentName); regErr != nil {
			return "", "", nil, fmt.Errorf("register sharer (create): %w", regErr)
		}

		// Write a .scion project marker into the worktree so in-container CLI
		// can discover the project context. Worktrees don't contain .scion
		// (it's gitignored), so without this marker the CLI would report
		// "not in a scion project" inside the container.
		if projectID, err := config.ReadProjectID(projectDir); err == nil && projectID != "" {
			projectSlug := api.Slugify(projectName)
			if writeErr := config.WriteWorkspaceMarker(agentWorkspace, projectID, projectName, projectSlug); writeErr != nil {
				util.Debugf("provision: failed to write workspace marker: %v", writeErr)
			}
		}
	}

	chain := rt.Chain
	finalScionCfg := rt.Config
	harnessConfigName := rt.HarnessConfigName
	templatePaths := rt.TemplatePaths
	hcDir := rt.HarnessConfigDir
	util.Debugf("ProvisionAgent: harness-config loaded from disk: path=%s harness=%q image=%q",
		hcDir.Path, hcDir.Config.Harness, hcDir.Config.Image)
	finalScionCfg.Harness = hcDir.Config.Harness
	finalScionCfg.HarnessConfig = harnessConfigName

	// Merge harness-config scalars into finalScionCfg (harness-config is base, template overrides)
	hcCfg := &api.ScionConfig{}

	// Image and Kubernetes.ImagePullPolicy precedence (ptone/scion#2156:
	// settings win over the harness-config file's own default; an explicit template or
	// inline-config override, merged in below via finalScionCfg, still
	// outranks settings). Start from the on-disk harness-config file's
	// values, then let a Hub settings harness_configs.<h> entry (including
	// any profiles.<p>.harness_overrides.<h> override) replace them if set.
	// See docs-site/src/content/docs/reference/settings-precedence.md.
	hcImage := hcDir.Config.Image
	hcPullPolicy := hcDir.Config.ImagePullPolicy
	if settings != nil {
		if settingsHC, err := settings.ResolveHarnessConfig(profileName, harnessConfigName); err == nil {
			if settingsHC.Image != "" {
				hcImage = settingsHC.Image
				util.Debugf("ProvisionAgent: image overridden by settings harness-config %q: %s", harnessConfigName, hcImage)
			}
			if settingsHC.ImagePullPolicy != "" {
				hcPullPolicy = settingsHC.ImagePullPolicy
			}
		}
	}
	if hcImage != "" {
		hcCfg.Image = hcImage
	}
	if hcPullPolicy != "" {
		hcCfg.Kubernetes = &api.KubernetesConfig{ImagePullPolicy: hcPullPolicy}
	}
	if hcDir.Config.Model != "" {
		hcCfg.Model = hcDir.Config.Model
	}
	if len(hcDir.Config.Args) > 0 {
		hcCfg.CommandArgs = hcDir.Config.Args
	}
	if hcDir.Config.TaskFlag != "" {
		hcCfg.TaskFlag = hcDir.Config.TaskFlag
	}
	if hcDir.Config.Env != nil {
		hcCfg.Env = hcDir.Config.Env
	}
	if hcDir.Config.Volumes != nil {
		hcCfg.Volumes = hcDir.Config.Volumes
	}
	if hcDir.Config.AuthSelectedType != "" {
		hcCfg.AuthSelectedType = hcDir.Config.AuthSelectedType
	}
	// Harness-config is base layer; template config overrides it
	finalScionCfg = config.MergeScionConfig(hcCfg, finalScionCfg)
	// Ensure harness and harness_config fields are not overridden by the merge
	finalScionCfg.Harness = hcDir.Config.Harness
	finalScionCfg.HarnessConfig = harnessConfigName

	// Warn about deprecated harness-specific fields in template config
	config.PrintDeprecationWarnings(config.WarnDeprecatedTemplateFields(finalScionCfg))

	// Resolve model size aliases (small/medium/large → concrete model name)
	if finalScionCfg.Model != "" && hcDir.Config.ModelAliases != nil {
		resolved := config.ResolveModelAlias(finalScionCfg.Model, hcDir.Config.ModelAliases)
		if resolved != finalScionCfg.Model {
			util.Debugf("ProvisionAgent: resolved model alias %q → %q", finalScionCfg.Model, resolved)
			finalScionCfg.Model = resolved
		}
	}

	// 2d. Compose agent home directory
	homeCopyStart := time.Now()

	// Step 1: Copy harness-config base home → agentHome
	hcHome := filepath.Join(hcDir.Path, "home")
	if info, err := os.Stat(hcHome); err == nil && info.IsDir() {
		if err := util.CopyDir(hcHome, agentHome); err != nil {
			return "", "", nil, fmt.Errorf("failed to copy harness-config home: %w", err)
		}
	}

	// Step 2: Copy template home → agentHome (overlay; template files win on conflict)
	templateHomeCopied := false
	for _, tpl := range chain {
		templateHome := filepath.Join(tpl.Path, "home")
		if info, err := os.Stat(templateHome); err == nil && info.IsDir() {
			if err := util.CopyDir(templateHome, agentHome); err != nil {
				return "", "", nil, fmt.Errorf("failed to copy template home %s: %w", templateRef(tpl), err)
			}
			templateHomeCopied = true
		}
	}

	// Safety net: an agent must never come up without its base home files.
	// When the template chain is empty (e.g. the "default" template was not
	// found locally during resolution), copy the EMBEDDED default template
	// home directly from the binary. This is a floor, not a workaround for
	// a single broker — the embedded home contains .tmux.conf (pane-exited
	// hook for sandbox exit detection), .zshrc, .gitconfig, and .gemini.
	if !templateHomeCopied {
		slog.Warn("template chain produced no home files — applying embedded default template home as floor",
			"agent", agentName, "template", templateName)
		embeddedHome := "embeds/templates/default/home"
		if err := fs.WalkDir(config.EmbedsFS, embeddedHome, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relPath, err := filepath.Rel(embeddedHome, path)
			if err != nil {
				return err
			}
			if relPath == "." {
				return nil
			}
			targetPath := filepath.Join(agentHome, relPath)
			if d.IsDir() {
				return os.MkdirAll(targetPath, 0755)
			}
			// Only write if the file does not already exist (harness-config
			// step 1 may have written files we should not overwrite).
			if _, statErr := os.Stat(targetPath); statErr == nil {
				return nil
			}
			data, readErr := config.EmbedsFS.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			return os.WriteFile(targetPath, data, 0644)
		}); err != nil {
			slog.Warn("failed to apply embedded default template home",
				"agent", agentName, "error", err)
		}
	}

	// Step 3: Copy skills directories into harness-specific location
	resolved, err := harness.Resolve(ctx, harness.ResolveOptions{
		Name:          harnessConfigName,
		ProjectPath:   projectDir,
		TemplatePaths: templatePaths,
		ProfileName:   profileName,
		Settings:      settings,
		ConfigDirPath: api.HarnessConfigPathFromContext(ctx),
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to resolve harness for %q: %w", harnessConfigName, err)
	}
	// resolveTemplateAndHarnessConfig evaluated the same effective entry;
	// this check also ties the policy to this harness construction, as the
	// call-site guard requires for every one.
	if err := CheckHarnessConfigPolicy(ctx, harnessConfigName, resolved.Config); err != nil {
		return "", "", nil, err
	}
	// Clear the staged provisioning state (wrapper and whole bundle,
	// inputs/ included) before staging, for every harness
	// (resetStagedProvisioning): content copied from harness-config or
	// template home/ trees cannot land in the bundle, and the control plane
	// stages its inputs below.
	if err := resetStagedProvisioning(agentHome); err != nil {
		return "", "", nil, err
	}
	h := resolved.Harness
	util.Debugf("ProvisionAgent: harness implementation=%s for harness=%q", resolved.Implementation, finalScionCfg.Harness)
	skillsDir := h.SkillsDir()
	// templateSkillNames records which skill directory names the current
	// template chain provides, independent of what may already be on disk
	// from a previous provisioning of this same agent home. Platform-skill
	// injection (Step 3a2) uses this — rather than a plain "does the
	// directory already exist" check — to decide precedence, so that
	// force-overwrite (reincarnation reprovisioning) can refresh a stale
	// platform skill without clobbering a template's intentional override.
	templateSkillNames := make(map[string]bool)
	if skillsDir != "" {
		skillsDest := filepath.Join(agentHome, skillsDir)

		// Copy skills from each template in the chain (overlay behavior)
		for _, tpl := range chain {
			tplSkills := filepath.Join(tpl.Path, "skills")
			if entries, err := os.ReadDir(tplSkills); err == nil {
				for _, e := range entries {
					if e.IsDir() {
						templateSkillNames[e.Name()] = true
					}
				}
			}
			if info, err := os.Stat(tplSkills); err == nil && info.IsDir() {
				if err := os.MkdirAll(skillsDest, 0755); err != nil {
					return "", "", nil, fmt.Errorf("failed to create skills dir: %w", err)
				}
				if err := util.CopyDir(tplSkills, skillsDest); err != nil {
					return "", "", nil, fmt.Errorf("failed to copy template skills %s: %w", templateRef(tpl), err)
				}
			}
		}
	}
	util.Debugf("provision: home/skills copy completed in %s", time.Since(homeCopyStart))

	// Step 3a2: Inject platform skills from embedded resources
	hubEnabled := platformSkillsHubEnabled(ctx, settings)
	injCtx := workspaceSkillsInjectionContext{
		IsGit:              isGitWorkspace,
		HubEnabled:         hubEnabled,
		ForceOverwrite:     api.IsReprovisionFromContext(ctx),
		TemplateSkillNames: templateSkillNames,
	}
	if skillsDir != "" {
		if err := injectPlatformSkills(resources.PlatformSkillsFS(), agentHome, skillsDir, injCtx); err != nil {
			return "", "", nil, fmt.Errorf("failed to inject platform skills: %w", err)
		}
	}

	// Step 3b: Resolve and install referenced skills from skill bank
	var resolvedSkillsRecord *SkillResolutionRecord
	if len(finalScionCfg.Skills) > 0 {
		resolver := SkillResolverFromContext(ctx)
		if resolver == nil {
			// S1: Fail closed for required skills
			requiredURIs := collectRequiredSkillURIs(finalScionCfg.Skills)
			if len(requiredURIs) > 0 {
				return "", "", nil, fmt.Errorf(
					"skill resolution failed: %d required skill(s) declared but no skill resolver available\n"+
						"  skills: %s\n"+
						"  hint: connect to a Hub or mark skills as optional",
					len(requiredURIs), strings.Join(requiredURIs, ", "))
			}
			util.Debugf("provision: %d optional skill(s) declared but no resolver available, skipping", len(finalScionCfg.Skills))
		} else {
			projectID := ResolveProjectIDFromContext(ctx)
			if projectID == "" {
				projectID, _ = config.ReadProjectID(projectDir)
			}
			resolveOpts := ResolveOpts{
				ProjectID: projectID,
				UserID:    ResolveUserIDFromContext(ctx),
			}

			skillFetchStart := time.Now()
			result, err := resolver.Resolve(ctx, finalScionCfg.Skills, resolveOpts)
			slog.Info("provision: skill fetch complete", "agent", agentName,
				"elapsed_ms", time.Since(skillFetchStart).Milliseconds(),
				"requested", len(finalScionCfg.Skills), "ok", err == nil)
			if err != nil {
				return "", "", nil, fmt.Errorf("skill resolution failed: %w", err)
			}

			// S1 completeness: build requested URI set
			requestedURIs := make(map[string]*api.SkillReference, len(finalScionCfg.Skills))
			for i := range finalScionCfg.Skills {
				requestedURIs[finalScionCfg.Skills[i].URI] = &finalScionCfg.Skills[i]
			}

			resolvedURIs := make(map[string]bool)
			errorURIs := make(map[string]bool)

			for _, rs := range result.Resolved {
				if _, ok := requestedURIs[rs.URI]; !ok {
					return "", "", nil, fmt.Errorf(
						"resolver returned unrequested skill %q — possible resolver bug or injection", rs.URI)
				}
				if resolvedURIs[rs.URI] {
					return "", "", nil, fmt.Errorf(
						"resolver returned duplicate resolved skill %q", rs.URI)
				}
				resolvedURIs[rs.URI] = true
			}

			for _, re := range result.Errors {
				errorURIs[re.URI] = true
				ref := requestedURIs[re.URI]
				if ref == nil || !ref.Optional {
					return "", "", nil, &SkillResolutionError{URI: re.URI, Code: re.Code, Message: re.Message, RetryAfter: re.RetryAfter, Status: re.Status}
				}
				slog.WarnContext(ctx, "optional skill could not be resolved; skipped",
					"skill_uri", LoggedSkillRef(re.URI), "code", re.Code,
					"reason", ResolverReasonForLog(re.URI, re.Message))
			}

			// S1: verify every requested URI has an outcome
			for uri, ref := range requestedURIs {
				if !resolvedURIs[uri] && !errorURIs[uri] {
					if ref.Optional {
						slog.WarnContext(ctx, "optional skill missing from resolver response; skipped",
							"skill_uri", LoggedSkillRef(uri), "reason", "missing from resolver response")
					} else {
						return "", "", nil, fmt.Errorf(
							"required skill %q missing from resolver response — S1 fail-closed", LoggedSkillRef(uri))
					}
				}
			}

			// Capture local skills before installing registry skills (M2: avoid duplication)
			var localSkills []SkillResolutionEntry
			if skillsDir != "" {
				localSkills = enumerateLocalSkills(agentHome, skillsDir)
			}

			if len(result.Resolved) > 0 {
				if skillsDir == "" {
					return "", "", nil, fmt.Errorf("harness does not support skills (no skills directory configured)")
				}
				skillsDest := filepath.Join(agentHome, skillsDir)
				// The requested ref is authoritative for Optional: a resolver
				// that does not copy it must not make an optional skill's
				// install failure fail the create (#5020).
				for i := range result.Resolved {
					result.Resolved[i].Optional = requestedURIs[result.Resolved[i].URI].Optional
				}
				// Skipped optional skills are logged as warnings by
				// installResolvedSkills.
				record, _, err := installResolvedSkills(ctx, result.Resolved, skillsDest, agentHome)
				if err != nil {
					// A required skill's install failure is already a
					// *SkillResolutionError naming the skill; return it as
					// is so the broker reports it as a skill resolution
					// failure rather than a generic runtime error.
					var skillErr *SkillResolutionError
					if errors.As(err, &skillErr) {
						return "", "", nil, skillErr
					}
					return "", "", nil, fmt.Errorf("skill installation failed: %w", err)
				}
				record.Resolver = resolverName(resolver)
				record.Skills = append(localSkills, record.Skills...)
				resolvedSkillsRecord = record
			}
		}
	}

	// Write resolution record (S4)
	if resolvedSkillsRecord != nil {
		recordPath := filepath.Join(agentHome, ".scion", "resolved-skills.json")
		if err := writeResolutionRecord(recordPath, resolvedSkillsRecord); err != nil {
			util.Debugf("provision: failed to write resolution record: %v", err)
		}

		// Stage resolved-skills.json for container-script harnesses
		if _, isContainerScript := h.(*harness.ContainerScriptHarness); isContainerScript {
			recordData, _ := json.MarshalIndent(resolvedSkillsRecord, "", "  ")
			inputPath := filepath.Join(agentHome, ".scion", "harness", "inputs", "resolved-skills.json")
			if err := os.MkdirAll(filepath.Dir(inputPath), 0755); err == nil {
				_ = os.WriteFile(inputPath, recordData, 0644)
			}
		}
	}

	// Step 3c: Stage agent plugins (hub.agent_plugins). Bundles are staged
	// into inputs/plugins/ even when this harness cannot install them, so a
	// later harness-config switch at Start works without a re-provision.
	templatePlugins := hasAgentPlugins(chain)
	if !templatePlugins && inlineCfg != nil && len(inlineCfg.AgentPlugins) > 0 {
		// Only the inline config declares agent_plugins: nothing to stage,
		// but say why it is ignored.
		if !agentPluginsEnabled(ctx, settings) {
			addStartWarning(ctx, "agent_plugins: ignored: experiment hub.agent_plugins is off")
		} else {
			warnInlineAgentPlugins(ctx, inlineCfg, nil)
		}
	}
	if templatePlugins {
		switch {
		case !agentPluginsEnabled(ctx, settings):
			addStartWarning(ctx, "agent_plugins: ignored: experiment hub.agent_plugins is off")
		case !isContainerScriptHarness(h):
			addStartWarning(ctx, fmt.Sprintf("agent_plugins: ignored: harness %q is not a container-script harness", harnessConfigName))
		default:
			sources := agentPluginTemplateSources(chain)
			warnInlineAgentPlugins(ctx, inlineCfg, sources)
			res, err := agentplugin.Stage(ctx, agentplugin.StageRequest{
				Chain:              sources,
				AgentHome:          agentHome,
				ReservedSkillNames: reservedSkillNames(sortedKeys(templateSkillNames), platformSkillNames(), resolvedSkillNames(resolvedSkillsRecord)),
				Limits:             agentplugin.DefaultLimits,
			})
			if err != nil {
				return "", "", nil, fmt.Errorf("agent plugins: %w", err)
			}
			for _, d := range res.Diagnostics {
				addStartWarning(ctx, d.String())
			}
			if res.Index != nil && agentPluginsCapabilityMissing(h) {
				addStartWarning(ctx, unsupportedAgentPluginsMessage(harnessConfigName, h))
			}
		}
	}

	// Step 4: Inject agent instructions

	// Load mandatory preamble — prepended to all agent instructions.
	mandatoryPreamble, err := loadMandatoryPreamble(resources.MandatoryBoilerplateFS())
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to load mandatory boilerplate: %w", err)
	}

	// Determine whether inline config provided content directly (already resolved).
	// If so, we skip template-based file resolution for that field.
	inlineProvidedAgentInstructions := inlineCfg != nil && inlineCfg.AgentInstructions != ""
	inlineProvidedSystemPrompt := inlineCfg != nil && inlineCfg.SystemPrompt != ""

	if len(chain) > 0 {
		lastTpl := chain[len(chain)-1]

		// Convention-based auto-detection: if agent_instructions is not set in
		// the template config but an agents.md file exists in the template
		// directory, use it automatically. This prevents a common oversight
		// where a template author creates the file but forgets to reference it
		// in scion-agent.yaml.
		if finalScionCfg.AgentInstructions == "" {
			conventionPath := filepath.Join(lastTpl.Path, "agents.md")
			if _, err := os.Stat(conventionPath); err == nil {
				util.Debugf("ProvisionAgent: agent_instructions not set in config, auto-detected agents.md in template %s", lastTpl.Path)
				finalScionCfg.AgentInstructions = "agents.md"
			}
		}

		if finalScionCfg.AgentInstructions != "" {
			var content []byte
			if inlineProvidedAgentInstructions {
				// Inline config already has resolved content — use it directly
				content = []byte(finalScionCfg.AgentInstructions)
				util.Debugf("ProvisionAgent: using inline agent_instructions (%d bytes)", len(content))
			} else {
				util.Debugf("ProvisionAgent: resolving agent_instructions=%q across template chain (%d templates)", finalScionCfg.AgentInstructions, len(chain))
				var err error
				content, err = config.ResolveContentInChain(chain, finalScionCfg.AgentInstructions)
				if err != nil {
					return "", "", nil, fmt.Errorf("failed to resolve agent_instructions: %w", err)
				}
			}
			composed := composeInstructions(mandatoryPreamble, content)
			if composed != nil {
				util.Debugf("ProvisionAgent: injecting agent instructions (%d bytes) into %s", len(composed), agentHome)
				if err := h.InjectAgentInstructions(agentHome, composed); err != nil {
					return "", "", nil, fmt.Errorf("failed to inject agent instructions: %w", err)
				}
			} else {
				util.Debugf("ProvisionAgent: both mandatory preamble and template content are nil, skipping injection")
			}
		} else if len(mandatoryPreamble) > 0 {
			util.Debugf("ProvisionAgent: no agent_instructions configured; injecting mandatory preamble only (%d bytes)", len(mandatoryPreamble))
			if err := h.InjectAgentInstructions(agentHome, mandatoryPreamble); err != nil {
				return "", "", nil, fmt.Errorf("failed to inject agent instructions: %w", err)
			}
		} else {
			util.Debugf("ProvisionAgent: no agent_instructions configured and no agents.md found in template")
		}

		// Step 5: Inject system prompt
		// Convention-based auto-detection for system prompt as well.
		if finalScionCfg.SystemPrompt == "" {
			conventionPath := filepath.Join(lastTpl.Path, "system-prompt.md")
			if _, err := os.Stat(conventionPath); err == nil {
				util.Debugf("ProvisionAgent: system_prompt not set in config, auto-detected system-prompt.md in template %s", lastTpl.Path)
				finalScionCfg.SystemPrompt = "system-prompt.md"
			}
		}

		if finalScionCfg.SystemPrompt != "" {
			var content []byte
			if inlineProvidedSystemPrompt {
				// Inline config already has resolved content — use it directly
				content = []byte(finalScionCfg.SystemPrompt)
				util.Debugf("ProvisionAgent: using inline system_prompt (%d bytes)", len(content))
			} else {
				util.Debugf("ProvisionAgent: resolving system_prompt=%q across template chain (%d templates)", finalScionCfg.SystemPrompt, len(chain))
				var err error
				content, err = config.ResolveContentInChain(chain, finalScionCfg.SystemPrompt)
				if err != nil {
					return "", "", nil, fmt.Errorf("failed to resolve system_prompt: %w", err)
				}
			}
			if content != nil {
				util.Debugf("ProvisionAgent: injecting system prompt (%d bytes) into %s", len(content), agentHome)
				if err := h.InjectSystemPrompt(agentHome, content); err != nil {
					return "", "", nil, fmt.Errorf("failed to inject system prompt: %w", err)
				}
			}
		}
	} else if inlineCfg != nil {
		// No template chain, but inline config may have content fields
		if finalScionCfg.AgentInstructions != "" {
			content := []byte(finalScionCfg.AgentInstructions)
			composed := composeInstructions(mandatoryPreamble, content)
			util.Debugf("ProvisionAgent: injecting inline agent_instructions (%d bytes, no template)", len(composed))
			if err := h.InjectAgentInstructions(agentHome, composed); err != nil {
				return "", "", nil, fmt.Errorf("failed to inject agent instructions: %w", err)
			}
		} else if len(mandatoryPreamble) > 0 {
			util.Debugf("ProvisionAgent: injecting mandatory preamble only (%d bytes, no template, no inline instructions)", len(mandatoryPreamble))
			if err := h.InjectAgentInstructions(agentHome, mandatoryPreamble); err != nil {
				return "", "", nil, fmt.Errorf("failed to inject agent instructions: %w", err)
			}
		}
		if finalScionCfg.SystemPrompt != "" {
			content := []byte(finalScionCfg.SystemPrompt)
			util.Debugf("ProvisionAgent: injecting inline system_prompt (%d bytes, no template)", len(content))
			if err := h.InjectSystemPrompt(agentHome, content); err != nil {
				return "", "", nil, fmt.Errorf("failed to inject system prompt: %w", err)
			}
		}
	}

	// 2e. Merge settings env, volumes and auth if available. The volumes,
	// like the harness-config image, pull policy and volumes above, are
	// environment values: kept in the returned config, not persisted
	// (ptone/scion#4561).
	if settings != nil {
		hConfig, err := settings.ResolveHarnessConfig(profileName, harnessConfigName)
		if err == nil {
			settingsCfg := &api.ScionConfig{}
			if hConfig.Env != nil {
				settingsCfg.Env = hConfig.Env
			}
			if hConfig.Volumes != nil {
				settingsCfg.Volumes = hConfig.Volumes
			}
			if hConfig.AuthSelectedType != "" {
				settingsCfg.AuthSelectedType = hConfig.AuthSelectedType
			}
			// settings.Telemetry is deliberately NOT merged here
			// (ptone/scion#4241). finalScionCfg is persisted to
			// scion-agent.json below, where a baked settings value would
			// rank as agent config at Start, above the hub default
			// telemetry tier, and later settings changes would never reach
			// the agent. Start applies settings telemetry in its own tier
			// on every start.
			//
			// Template has highest priority IN THIS MERGE, so it overrides
			// settings here. We construct a config with ONLY the settings env,
			// then merge finalScionCfg over it.
			//
			// This is no longer the whole story for the CONTAINER env. In
			// broker mode, harness-config env is separately injected into
			// opts.Env by resolveAuthEnvOverlay (run.go), and opts.Env is
			// passed as extraEnv to buildAgentEnv (run.go), where it overrides
			// finalScionCfg.Env. So for a key declared by both the template and
			// the harness config, the harness-config value is what reaches the
			// container, even though the template wins the merge below.
			// Pinned by TestBrokerMode_HarnessConfigEnvOutranksTemplateEnv.
			finalScionCfg = config.MergeScionConfig(settingsCfg, finalScionCfg)
		}
	}

	// Limits: the template and inline config (both merged into
	// finalScionCfg at step 2b) are the agent's own and are persisted; the
	// hub operational agent_defaults and then this broker's settings.yaml
	// defaults fill only what they leave unset (applyDefaultLimits). Those
	// defaults are applied to the returned config here and again by Start
	// on every start, never persisted (ptone/scion#623), so a later change
	// to either reaches the agent at its next start.
	//
	// The hub sends nothing in file mode, so the hub tier never fires there
	// and a co-located broker applies the same settings.yaml values itself
	// at the bottom tier. The rank of hub agent_defaults is therefore
	// mode-dependent — bottom of the broker chain in file mode, just above
	// broker settings in Postgres mode. That is deliberate; see design
	// §3.2.4 and alternative A7.
	//
	// Note the asymmetry for Resources: hub agent_defaults.resources ranks
	// above default_resources ONLY. Broker profile resources and harness
	// overrides win per-field over it, while broker default_max_turns loses
	// to hub default_max_turns. That is the conservative direction (§3.2.4
	// explicitly does not want hub defaults silently overriding broker
	// profile resources). Resources are resolved by
	// resolveEnvironmentResources below and again by Start, never persisted.
	ownLimits := limitsOf(finalScionCfg)
	applyDefaultLimits(ctx, finalScionCfg, settings)

	// Resources: settings default_resources < hub agent_defaults.resources
	// < profile resources < the agent's own < profile harness_overrides, then
	// the built-in CPU default as the LOWEST-priority tier
	// (applyEnvironmentResources). Docker and Podman emit cgroup flags only
	// for non-empty fields, so an agent with no CPU limit runs completely
	// unconstrained and can saturate every core on the host; only the CPU
	// limit is filled in, and only when nothing else supplied one. Gated by
	// runtime.enforce_resource_defaults (default true). The built-in limit
	// also never conflicts with a larger CPU request (ptone/scion#3407).
	//
	// These are environment values: they are applied to the config returned
	// here, but not persisted to scion-agent.json; Start applies them from
	// current settings on every start (ptone/scion#4561).
	applyEnvironmentResources(ctx, finalScionCfg, settings, profileName, harnessConfigName)

	// Mount the resolved workspace if an external source was determined
	var workspaceVolume *api.VolumeMount
	if workspaceSource != "" {
		workspaceVolume = &api.VolumeMount{
			Source:   workspaceSource,
			Target:   "/workspace",
			ReadOnly: false,
		}
		finalScionCfg.Volumes = append(finalScionCfg.Volumes, *workspaceVolume)
	}
	if explicitWorkspace {
		finalScionCfg.ExplicitWorkspace = true
	}
	if emptyPerAgent {
		finalScionCfg.EmptyPerAgentWorkspace = true
	}

	// Update agent-specific scion-agent.json
	if finalScionCfg == nil {
		finalScionCfg = &api.ScionConfig{}
	}

	// Create the Info object which will go into agent-info.json.
	// Use the resolved template name from the chain (human-friendly) rather
	// than the raw templateName which may be a cache path or remote URI. A
	// content-hash cache directory is never recorded as the name; the slug
	// carried in ctx is used instead when known.
	displayTemplateName, templateHash := infoTemplateFields(ctx, templateName, chain)
	projectID, _ := config.ReadProjectID(projectDir)
	info := &api.AgentInfo{
		Project:               projectName,
		ProjectID:             projectID,
		ProjectPath:           projectDir,
		Name:                  agentName,
		Template:              displayTemplateName,
		TemplateHash:          templateHash,
		HarnessConfig:         harnessConfigName,
		HarnessConfigRevision: config.ComputeHarnessConfigRevision(hcDir.Path),
		HarnessConfigSource:   string(hcDir.Source),
		Profile:               profileName,
	}
	if optionalStatus != "" {
		info.Phase = optionalStatus
	} else {
		info.Phase = "created"
	}
	// info.Image is kept for display and listing only (agent list, the
	// broker's agent response); Start never reads it. The request image
	// Start replays is recorded in broker-side image provenance below.
	if agentImage != "" {
		info.Image = agentImage
	}
	if explicitImage != "" {
		info.ExplicitImage = explicitImage
	}
	if explicitPullPolicy != "" {
		info.ExplicitImagePullPolicy = explicitPullPolicy
	}
	// Persist the broker-provisioned worktree's repo root now, if the ctx
	// signal validates against workspaceSource: run.go's Start does not
	// always run after this call returns — the hub's provision-only flow
	// (Manager.Provision, DispatchAgentProvision) can provision an agent
	// without starting it in the same dispatch, and a later start/restart
	// carries no ctx signal of its own. (Reprovision is clone-per-agent only
	// today, so it never carries this signal; if it gains worktree support,
	// this call already covers it, since Reprovision also reaches
	// ProvisionAgent directly.) Start still carries its own call to the same
	// gate, for the one case this function never runs at all: GetAgent
	// skipping ProvisionAgent because the agent directory already exists.
	// See persistProvisionedWorktreeRepoRootIfValid.
	persistProvisionedWorktreeRepoRootIfValid(agentDir, api.ProvisionedWorktreeRepoRootFromContext(ctx), workspaceSource)

	// scion-agent.json carries only the requester's own environment values
	// (the inline image, kubernetes, resources and volumes) and the workspace
	// mount resolved above. The harness-config file, settings, profile, hub
	// default and built-in values merged into finalScionCfg are applied by
	// Start from current settings on every start, so a later settings or
	// profile change reaches the agent on its next start (ptone/scion#4561).
	// The returned config keeps the merged values (see the reload below).
	// Likewise it carries only the agent's own limits (ptone/scion#623).
	persistedCfg := withLimits(withEnvironment(finalScionCfg, provisionedAgentEnvironment(inlineCfg, workspaceVolume)), ownLimits)
	agentCfgData, err := json.MarshalIndent(persistedCfg, "", "  ")
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to marshal agent config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), agentCfgData, 0644); err != nil {
		return "", "", nil, fmt.Errorf("failed to write agent config: %w", err)
	}
	// Record each image input at its own tier (ptone/scion#1799), so Start
	// never has to reuse the merged finalScionCfg.Image — which folds in
	// the inline, settings and profile values of this moment. No template
	// image is recorded: a template's image is an ignored environment key
	// (ptone/scion#4524). Image provenance is recorded in broker-side agent
	// state (the agent dir), never in the agent home, which is inside a
	// container mount.
	if err := writeImageProvenance(agentDir, imageProvenance{
		RequestImage:          agentImage,
		InlineImage:           explicitImage,
		InlineImagePullPolicy: explicitPullPolicy,
		Profile:               profileName,
		Template:              displayTemplateName,
		TemplateHash:          templateHash,
	}); err != nil {
		return "", "", nil, fmt.Errorf("failed to write image provenance: %w", err)
	}
	// Record the run that owns these files (ptone/scion#2675) in the
	// host-owned HostStateFile in agentDir, never in agent-info.json
	// (ptone/scion#4771): a delete naming a different run then leaves them
	// alone. A provision with no run (provision-only, reprovision) leaves
	// the record as it is, since the files still belong to that run's
	// runtime entry (and copies a legacy agent-info.json runId into it,
	// below). A failure (including a record that would not land in
	// agentDir) is logged and the provision carries on, as for the
	// agent-info.json write below.
	ownerTarget := RunOwnerTarget{
		ProjectPath:     projectPath,
		AgentName:       agentName,
		HubProjectID:    api.HubProjectIDFromContext(ctx),
		SharedWorkspace: sharedWorkspace,
	}
	// Transitional (ptone/scion#4771): the agent-info.json rewrite below
	// would drop a runId an older broker left there, so a provision
	// without a run (reincarnate, provision-only) first copies it into the
	// host state file (migrateLegacyRunOwner, which never replaces a run
	// already recorded), keeping the owner base carried over. When the
	// copy does not succeed, the rewrite keeps the value as it was read.
	if runID := api.RunIDFromContext(ctx); runID != "" {
		if err := recordRunOwnerInAgentDir(ownerTarget, agentDir, runID); err != nil {
			slog.Warn("Provision: failed to record the run in the host state file; a delete for this run may leave the agent's files behind",
				"agent", agentName, "run_id", runID, "error", err)
		}
	} else {
		recorded, legacy, err := migrateLegacyRunOwner(ownerTarget, func(legacy string) (bool, error) {
			return recordLegacyRunOwnerInAgentDir(ownerTarget, agentDir, legacy)
		})
		if err != nil {
			slog.Warn("Provision: failed to copy the legacy run ID into the host state file; keeping it in agent-info.json",
				"agent", agentName, "error", err)
		}
		if legacy != "" && !recorded {
			// Not copied: keep it as it was read, as updateSavedAgentInfo
			// does, for the legacy fallback read.
			info.RunID = legacy
		}
	}

	// Now attach Info to the config object for return and for writing agent-info.json
	finalScionCfg.Info = info

	// Write agent-info.json to home for container access
	if finalScionCfg.Info != nil {
		infoData, err := json.MarshalIndent(finalScionCfg.Info, "", "  ")
		if err == nil {
			// Temp file and rename, so a reader never sees a truncated
			// file (#4681). No container runs during provisioning; the
			// lock keeps the write ordered with any other host writer.
			infoPath := filepath.Join(agentHome, "agent-info.json")
			if err := agentinfolock.Update(infoPath, agentinfolock.Options{}, func() error {
				return writeAgentInfoFile(infoPath, infoData, 0o644)
			}); err != nil {
				slog.Warn("Provision: failed to write agent-info.json",
					"agent", agentName, "error", err)
			}
		}
	}

	// Write scion-services.yaml for sciontool to consume at container startup
	if len(finalScionCfg.Services) > 0 {
		scionDir := filepath.Join(agentHome, ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			return "", "", nil, fmt.Errorf("failed to create agent .scion directory: %w", err)
		}
		servicesData, err := yaml.Marshal(finalScionCfg.Services)
		if err != nil {
			return "", "", nil, fmt.Errorf("failed to marshal services config: %w", err)
		}
		if err := os.WriteFile(filepath.Join(scionDir, "scion-services.yaml"), servicesData, 0644); err != nil {
			return "", "", nil, fmt.Errorf("failed to write scion-services.yaml: %w", err)
		}
	}

	// 2f. Configure git credential helper for shared-workspace projects.
	// The credential helper is written to $HOME/.gitconfig so it doesn't
	// pollute the shared workspace. This pre-configures a basic credential
	// helper using GITHUB_TOKEN env var. When GitHub App is enabled,
	// sciontool init's configureSharedWorkspaceGit() will upgrade this to
	// use `sciontool credential-helper` for on-demand token refresh.
	if api.IsSharedWorkspaceFromContext(ctx) {
		gitconfigPath := filepath.Join(agentHome, ".gitconfig")
		credentialSection := "\n[credential]\n\thelper = !f() { echo \"username=oauth2\"; echo \"password=${GITHUB_TOKEN}\"; }; f\n"
		// Append to existing .gitconfig (which may have [safe] directory config)
		f, err := os.OpenFile(gitconfigPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return "", "", nil, fmt.Errorf("failed to open .gitconfig for credential helper: %w", err)
		}
		if _, err := f.WriteString(credentialSection); err != nil {
			_ = f.Close()
			return "", "", nil, fmt.Errorf("failed to write credential helper to .gitconfig: %w", err)
		}
		_ = f.Close()
		util.Debugf("provision: configured git credential helper for shared workspace in %s", gitconfigPath)
	}

	// Record the per-agent inputs staged above (the control plane is their
	// only writer since resetStagedProvisioning cleared the bundle), so Start
	// can restage exactly this content on every launch.
	// A harness that is not container-script stages no inputs; its record is
	// created empty, so a later switch to a container-script harness never
	// treats this agent as one provisioned before the record existed.
	if _, isContainerScript := h.(*harness.ContainerScriptHarness); isContainerScript {
		if err := snapshotControlPlaneInputs(agentDir, agentHome); err != nil {
			return "", "", nil, fmt.Errorf("record harness inputs: %w", err)
		}
	} else if err := resetControlPlaneInputsRecord(agentDir); err != nil {
		return "", "", nil, fmt.Errorf("record harness inputs: %w", err)
	}
	// The secrets record is created empty when absent and otherwise kept: a
	// re-render does not stage secrets, which are recorded on start.
	if err := ensureSecretsRecord(agentDir); err != nil {
		return "", "", nil, fmt.Errorf("record harness secrets: %w", err)
	}

	// 3. Harness provisioning
	if err := h.Provision(ctx, agentName, agentDir, agentHome, agentWorkspace); err != nil {
		return "", "", nil, fmt.Errorf("harness provisioning failed: %w", err)
	}

	// Stage capture-auth assets (capture_auth.py + capture-auth-config.json)
	// into the harness bundle so they are available at a known path in the
	// container. Container-script harnesses stage these during their own
	// Provision(); this path handles non-container-script fallbacks.
	if _, isContainerScript := h.(*harness.ContainerScriptHarness); !isContainerScript && hcDir != nil {
		if err := harness.StageCaptureAuthAssets(agentHome, hcDir.Path, hcDir.Config.Auth); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: capture-auth asset staging failed: %v\n", err)
		}
	}

	// Reload config to get harness updates (e.g. Env vars injected by harness).
	// The reloaded file holds only the agent's own environment values; the
	// returned config carries the effective ones merged above (for the
	// create response and other callers), which Start re-derives from
	// current settings on every start (ptone/scion#4561).
	// The same holds for the default limits (ptone/scion#623).
	effectiveEnv := environmentOf(finalScionCfg)
	effectiveLimits := limitsOf(finalScionCfg)
	reloadTpl := &config.Template{Path: agentDir}
	if updatedCfg, err := reloadTpl.LoadConfig(); err == nil {
		updatedCfg.Info = finalScionCfg.Info // Re-attach info
		finalScionCfg = withLimits(withEnvironment(updatedCfg, effectiveEnv), effectiveLimits)
	} else {
		fmt.Fprintf(os.Stderr, "Warning: failed to reload agent config after harness provisioning: %v\n", err)
	}

	util.Debugf("provision: total ProvisionAgent completed in %s", time.Since(provisionStart))
	slog.Info("provision: agent provisioning complete", "agent", agentName,
		"elapsed_ms", time.Since(provisionStart).Milliseconds())
	return agentHome, agentWorkspace, finalScionCfg, nil
}

// skillFrontmatter holds parsed YAML frontmatter from SKILL.md files.
type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	InjectWhen  string `yaml:"inject_when"`
}

// parseSkillFrontmatter extracts YAML frontmatter from a SKILL.md file.
// Returns zero-value skillFrontmatter if no frontmatter is found.
func parseSkillFrontmatter(data []byte) skillFrontmatter {
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return skillFrontmatter{}
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return skillFrontmatter{}
	}
	var fm skillFrontmatter
	if err := yaml.Unmarshal([]byte(content[4:4+end]), &fm); err != nil {
		util.Debugf("provision: failed to parse SKILL.md frontmatter: %v", err)
	}
	return fm
}

// workspaceSkillsInjectionContext holds the context needed to evaluate
// conditional injection of platform skills.
type workspaceSkillsInjectionContext struct {
	IsGit      bool
	HubEnabled bool
	// ForceOverwrite re-injects a platform skill even when a directory of the
	// same name already exists in the agent's home, provided it wasn't just
	// placed there by a template in this same provisioning call (which still
	// takes precedence — see TemplateSkillNames). Set for reincarnation
	// reprovisioning (design §3.4), where a newer broker binary may carry
	// updated platform-skill content that a plain existence check would
	// otherwise leave stale.
	ForceOverwrite bool
	// TemplateSkillNames holds the skill directory names provided by the
	// current template chain (independent of what's already on disk), so
	// ForceOverwrite can still respect "template skills take precedence"
	// rather than clobbering a template's intentional override.
	TemplateSkillNames map[string]bool
}

// platformSkillsHubEnabled reports whether hub_enabled platform skills are
// injected: always for an agent a broker provisions, and for a local agent
// when a hub endpoint is configured. hub.enabled is not consulted.
func platformSkillsHubEnabled(ctx context.Context, settings *config.VersionedSettings) bool {
	return api.IsBrokerModeFromContext(ctx) || (settings != nil && settings.IsHubConfigured())
}

// shouldInjectSkill checks whether a skill should be injected based on its
// inject_when frontmatter condition and the current provisioning context.
func shouldInjectSkill(fm skillFrontmatter, injCtx workspaceSkillsInjectionContext) bool {
	switch fm.InjectWhen {
	case "":
		return true
	case "git_workspace":
		return injCtx.IsGit
	case "hub_enabled":
		return injCtx.HubEnabled
	default:
		util.Debugf("provision: unknown inject_when=%q for skill %q, skipping", fm.InjectWhen, fm.Name)
		return false
	}
}

// loadMandatoryPreamble reads all .md files from the mandatory boilerplate FS
// in lexical filename order and concatenates them separated by double newlines.
// Returns nil if the FS contains no non-empty .md files.
func loadMandatoryPreamble(boilerplateFS fs.FS) ([]byte, error) {
	if boilerplateFS == nil {
		return nil, nil
	}
	entries, err := fs.ReadDir(boilerplateFS, ".")
	if err != nil {
		return nil, fmt.Errorf("read mandatory boilerplate: %w", err)
	}
	var parts [][]byte
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := fs.ReadFile(boilerplateFS, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read mandatory boilerplate %s: %w", e.Name(), err)
		}
		if len(bytes.TrimSpace(data)) > 0 {
			parts = append(parts, bytes.TrimRight(data, "\r\n"))
		}
	}
	if len(parts) == 0 {
		return nil, nil
	}
	return bytes.Join(parts, []byte("\n\n")), nil
}

// composeInstructions prepends the mandatory preamble to template content.
// If preamble is nil/empty, returns templateContent unchanged.
// If templateContent is nil/empty, returns preamble alone.
func composeInstructions(preamble, templateContent []byte) []byte {
	if len(preamble) == 0 {
		return templateContent
	}
	if len(bytes.TrimSpace(templateContent)) == 0 {
		return preamble
	}
	result := make([]byte, 0, len(preamble)+2+len(templateContent))
	result = append(result, preamble...)
	result = append(result, '\n', '\n')
	result = append(result, templateContent...)
	return result
}

// injectPlatformSkills copies platform skills from the embedded filesystem
// into the agent's skills directory. Skills with inject_when conditions are
// evaluated against the injection context (e.g. git_workspace skills are
// only injected when isGit is true). Template skills take precedence: if the
// current template chain provides a skill with the same directory name
// (injCtx.TemplateSkillNames), the platform skill is skipped.
//
// Otherwise, a platform skill whose directory already exists on disk is
// normally left alone (idempotent restart/resume). injCtx.ForceOverwrite
// (reincarnation reprovisioning) refreshes it instead, so a newer broker
// binary's platform-skill content actually reaches an agent that already had
// an older copy — a plain existence check cannot distinguish "up to date"
// from "stale leftover from a previous generation".
func injectPlatformSkills(
	skillsFS fs.FS,
	agentHome string,
	skillsDir string,
	injCtx workspaceSkillsInjectionContext,
) error {
	entries, err := fs.ReadDir(skillsFS, ".")
	if err != nil {
		return fmt.Errorf("failed to read platform skills FS: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillName := entry.Name()

		skillMDData, err := fs.ReadFile(skillsFS, skillName+"/SKILL.md")
		if err != nil {
			util.Debugf("provision: platform skill %q has no SKILL.md, skipping", skillName)
			continue
		}

		fm := parseSkillFrontmatter(skillMDData)
		if !shouldInjectSkill(fm, injCtx) {
			util.Debugf("provision: skipping platform skill %q (inject_when=%q not satisfied)", skillName, fm.InjectWhen)
			continue
		}

		skillDest := filepath.Join(agentHome, skillsDir, skillName)

		// Template skills take precedence, regardless of ForceOverwrite: a
		// template's intentional override must never be clobbered by the
		// platform default.
		if injCtx.TemplateSkillNames[skillName] {
			util.Debugf("provision: platform skill %q skipped (template skill takes precedence)", skillName)
			continue
		}

		if !injCtx.ForceOverwrite {
			if _, err := os.Stat(skillDest); err == nil {
				util.Debugf("provision: platform skill %q skipped (already present)", skillName)
				continue
			}
		} else {
			// A plain overlay copy only adds/updates files; it
			// never removes one the new platform-skill version dropped, so a
			// stale file from an earlier generation would linger forever
			// under ForceOverwrite. Safe to RemoveAll: skillDest is
			// platform-owned at this point (the TemplateSkillNames check
			// above already exempted anything template-owned), so there is
			// nothing here that isn't about to be fully replaced anyway.
			if err := os.RemoveAll(skillDest); err != nil {
				return fmt.Errorf("failed to clear stale platform skill %q before overwrite: %w", skillName, err)
			}
		}

		// Walk the embedded skill directory and copy all files
		skillRoot := skillName
		if err := fs.WalkDir(skillsFS, skillRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(skillRoot, path)
			if err != nil {
				return err
			}
			target := filepath.Join(skillDest, rel)

			if d.IsDir() {
				return os.MkdirAll(target, 0755)
			}
			data, err := fs.ReadFile(skillsFS, path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0644)
		}); err != nil {
			return fmt.Errorf("failed to copy platform skill %s: %w", skillName, err)
		}
		util.Debugf("provision: injected platform skill %q into %s", skillName, skillDest)
	}

	return nil
}

// SavedProfileState says where SavedProfileOnDisk found an agent's profile.
type SavedProfileState int

const (
	// SavedProfileMissing: the agent has no agent-info.json (it was never
	// provisioned on this host, or projectPath does not resolve).
	SavedProfileMissing SavedProfileState = iota
	// SavedProfileFound: the profile is the Profile in agent-info.json.
	SavedProfileFound
	// SavedProfileUnreadable: agent-info.json exists but could not be read
	// or parsed; the profile is the provisioned one from the image
	// provenance record when there is one, else "".
	SavedProfileUnreadable
)

// SavedProfileOnDisk returns the saved profile of the agent agentName in
// projectPath on this host, as the start path would see it, for the hub's
// built-in profile reference count (ptone/scion#5113). It reads
// agent-info.json exactly as GetSavedProfile does (GetResolvedProjectDir,
// then GetAgentHomePath). When that file exists but cannot be used, the
// start path's runtime selection would use the provisioned profile from the
// image provenance record (ProvisionedProfile), so that is returned instead.
// sharedWorkspace and hubProjectID are passed to ProvisionedProfile.
//
// It is read-only and has no other side effects (no writes, no caches, no
// logging), so the rebuild preflight can call it directly.
func SavedProfileOnDisk(projectPath, agentName string, sharedWorkspace bool, hubProjectID string) (string, SavedProfileState) {
	info, state := readSavedAgentInfo(agentName, projectPath)
	switch state {
	case SavedProfileFound:
		return info.Profile, SavedProfileFound
	case SavedProfileUnreadable:
		if profile, ok, err := ProvisionedProfile(projectPath, agentName, sharedWorkspace, hubProjectID, false); err == nil && ok {
			return profile, SavedProfileUnreadable
		}
		return "", SavedProfileUnreadable
	default:
		return "", SavedProfileMissing
	}
}

func getSavedAgentInfo(agentName string, projectPath string) *api.AgentInfo {
	info, _ := readSavedAgentInfo(agentName, projectPath)
	return info
}

// readSavedAgentInfo reads agent agentName's agent-info.json in projectPath:
// the one reader behind GetSaved* and SavedProfileOnDisk, so they always
// look at the same file. info is non-nil only when state is
// SavedProfileFound.
func readSavedAgentInfo(agentName string, projectPath string) (*api.AgentInfo, SavedProfileState) {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return nil, SavedProfileMissing
	}
	agentInfoPath := filepath.Join(config.GetAgentHomePath(projectDir, agentName), "agent-info.json")
	if _, err := os.Stat(agentInfoPath); err != nil {
		return nil, SavedProfileMissing
	}
	data, err := os.ReadFile(agentInfoPath)
	if err != nil {
		return nil, SavedProfileUnreadable
	}
	var info api.AgentInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, SavedProfileUnreadable
	}
	return &info, SavedProfileFound
}

// hasRuntimeEntry reports whether the runtime holds an entry for opts.Name
// in its project, or cannot tell (a List error).
func (m *AgentManager) hasRuntimeEntry(ctx context.Context, opts api.StartOptions) bool {
	if m.Runtime == nil {
		return false
	}
	projectName := ""
	if projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath); err == nil {
		projectName = config.GetProjectName(projectDir)
	}
	projectID := ""
	if opts.Env != nil {
		projectID = opts.Env["SCION_PROJECT_ID"]
	}
	entries, err := m.listRuntime(ctx, map[string]string{"scion.name": api.Slugify(opts.Name)})
	if err != nil {
		return true
	}
	for _, e := range entries {
		if matchAgentProject(e, projectName, projectID) {
			return true
		}
	}
	return false
}

// ProvisionOwnerPrefix prefixes the owner a provision-only create records
// (RecordRunOwner) in place of a run (see Provision). It never
// equals a run ID the hub sends.
const ProvisionOwnerPrefix = "provision-"

func GetSavedProfile(agentName string, projectPath string) string {
	if info := getSavedAgentInfo(agentName, projectPath); info != nil {
		return info.Profile
	}
	return ""
}

// GetSavedRuntime returns the runtime name agent-info.json records the
// agent last ran on, or "" if none is recorded.
func GetSavedRuntime(agentName string, projectPath string) string {
	if info := getSavedAgentInfo(agentName, projectPath); info != nil {
		return info.Runtime
	}
	return ""
}

func GetSavedHarnessConfig(agentName string, projectPath string) string {
	if info := getSavedAgentInfo(agentName, projectPath); info != nil {
		return info.HarnessConfig
	}
	return ""
}

func GetSavedPhase(agentName string, projectPath string) string {
	if info := getSavedAgentInfo(agentName, projectPath); info != nil {
		return info.Phase
	}
	return ""
}

func updateSavedAgentInfo(agentName string, projectPath string, update func(*api.AgentInfo)) error {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return err
	}
	agentHome := config.GetAgentHomePath(projectDir, agentName)
	agentInfoPath := filepath.Join(agentHome, "agent-info.json")

	// If agent-info.json doesn't exist, we can't update it.
	// This might happen if provisioning failed or hasn't finished.
	// Checked before the lock too, so no lock file is left in a home
	// that has no agent-info.json.
	if _, err := os.Stat(agentInfoPath); os.IsNotExist(err) {
		return nil
	}

	// Transitional (ptone/scion#4771): a runId an older broker left here
	// is copied into the host state file before this update drops it, and
	// it is dropped only once the host state file records a run. This
	// caller holds no hub project ID or shared-workspace flag, so the copy
	// is made only where the record's dir is the agent's own state dir
	// (config.ResolveAgentDir, holding a regular scion-agent.json, which a
	// shared-workspace agent's in-project dir never does). Otherwise, or
	// when the copy fails, the value is left in place for the legacy
	// fallback read; it is never written here.
	ownerTarget := RunOwnerTarget{ProjectPath: projectPath, AgentName: agentName}
	ownerRecorded, legacyRun, migrateErr := migrateLegacyRunOwner(ownerTarget, func(runID string) (bool, error) {
		agentDir := config.ResolveAgentDir(projectDir, agentName)
		if fi, err := os.Stat(filepath.Join(agentDir, "scion-agent.json")); err != nil || !fi.Mode().IsRegular() {
			return false, fmt.Errorf("%w: %s holds no scion-agent.json", errRunOwnerDirMismatch, agentDir)
		}
		return recordLegacyRunOwnerInAgentDir(ownerTarget, agentDir, runID)
	})
	if migrateErr != nil {
		// Expected on every update of such an agent until it next starts
		// (for example a hub-managed marker project, which this caller
		// cannot resolve without the hub project ID), so not a warning.
		slog.Debug("Did not copy the legacy agent-info.json run ID into the host state file; keeping it in agent-info.json",
			"agent", agentName, "error", migrateErr)
	}
	keepLegacyRun := legacyRun != "" && !ownerRecorded

	// The read, update and write run under the agent-info lock, so a
	// concurrent writer in the container (sciontool) or another host
	// process cannot lose this update or have its own lost (#4681).
	return agentinfolock.Update(agentInfoPath, agentinfolock.Options{}, func() error {
		mode := os.FileMode(0o644)
		if fi, err := os.Stat(agentInfoPath); os.IsNotExist(err) {
			return nil
		} else if err == nil {
			mode = fi.Mode().Perm()
		}

		data, err := os.ReadFile(agentInfoPath)
		if err != nil {
			return err
		}

		var info api.AgentInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return err
		}

		update(&info)
		// The files-owner run lives in HostStateFile (ptone/scion#4771);
		// a runId left by an older broker is dropped once HostStateFile
		// records a run (see above), and otherwise left as it was read.
		if !keepLegacyRun || info.RunID != legacyRun {
			info.RunID = ""
		}

		newData, err := marshalAgentInfoKeepUnknown(data, &info)
		if err != nil {
			return err
		}

		return writeAgentInfoFile(agentInfoPath, newData, mode)
	})
}

// ErrAgentInfoNotDurable marks a writeAgentInfoFile failure after the
// rename: the new content is in place, but syncing the parent directory
// failed, so the change may not survive a crash. Repeating the write is
// safe.
var ErrAgentInfoNotDurable = errors.New("agent file replaced but the change may not be durable")

// writeAgentInfoFile replaces path with data through a temp file in the
// same directory, syncing the contents before the rename and the parent
// directory after it. An error wrapping ErrAgentInfoNotDurable means the
// content was replaced but may not survive a crash; any other error means
// path was left untouched.
func writeAgentInfoFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	// Flush the contents before the rename, so a crash cannot leave a
	// truncated file under the final name (#4821).
	if err := agentInfoFileSync(tmp); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	// Sync the parent directory so the rename itself survives a crash.
	dir := filepath.Dir(path)
	if err := syncAgentInfoDir(dir); err != nil {
		return fmt.Errorf("%w: sync directory %s: %w", ErrAgentInfoNotDurable, dir, err)
	}
	return nil
}

// Test seams for writeAgentInfoFile's disk syncs.
var (
	// agentInfoFileSync flushes the temp file's contents to disk.
	agentInfoFileSync = func(f *os.File) error { return f.Sync() }
	// agentInfoDirSync flushes the parent directory, opened by
	// syncAgentInfoDir, after the rename.
	agentInfoDirSync = func(d *os.File) error { return d.Sync() }
	// agentInfoDirOpen opens the parent directory for that sync.
	agentInfoDirOpen = openAgentInfoDir
)

// syncAgentInfoDir fsyncs dir, as pkg/config's syncDir does. Windows cannot
// fsync a directory, so it is a no-op there. A directory that cannot be
// opened, or whose filesystem reports directory fsync as unsupported
// (EINVAL, ENOTSUP, errors.ErrUnsupported), is skipped: the new content is
// already in place.
func syncAgentInfoDir(dir string) error {
	if goruntime.GOOS == "windows" {
		return nil
	}
	d, err := agentInfoDirOpen(dir)
	if err != nil {
		return nil
	}
	defer func() { _ = d.Close() }()
	if err := agentInfoDirSync(d); err != nil && !isDirSyncUnsupported(err) {
		return err
	}
	return nil
}

// isDirSyncUnsupported reports whether err is a filesystem saying it does
// not support fsync on a directory.
func isDirSyncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, errors.ErrUnsupported)
}

// provisionedWorktreeStateFile is the broker-owned file that persists the
// broker-provisioned worktree's repo root (see
// api.ContextWithProvisionedWorktreeRepoRoot). It lives directly in agentDir,
// a sibling of prompt.md and scion-agent.json — never in agentHome or the
// agent's workspace, which pkg/runtime/common.go bind-mounts read-write into
// the agent container. agentDir itself is not mounted for hub-native/broker
// agents. It can fall inside a read-write mount in some local, non-broker
// configurations (e.g. an explicit --workspace pointed at the project root);
// provision.ValidateWorktreeForBase is the backstop that applies regardless
// of where this file lives, so its correctness never depends on the storage
// location alone.
const provisionedWorktreeStateFile = "provisioned-worktree.json"

// provisionedWorktreeState is the on-disk shape of provisionedWorktreeStateFile.
type provisionedWorktreeState struct {
	RepoRoot string `json:"repoRoot"`
}

// writeProvisionedWorktreeRepoRoot persists repoRoot to agentDir's
// broker-owned state file (atomic write via writeAgentInfoFile). A write
// failure only means a later resume falls back to detectRepoRoot — a
// functional regression, not a security issue — so callers may log and
// continue rather than fail the whole dispatch.
func writeProvisionedWorktreeRepoRoot(agentDir, repoRoot string) error {
	data, err := json.Marshal(provisionedWorktreeState{RepoRoot: repoRoot})
	if err != nil {
		return err
	}
	return writeAgentInfoFile(filepath.Join(agentDir, provisionedWorktreeStateFile), data, 0o644)
}

// readProvisionedWorktreeRepoRoot reads the value written by
// writeProvisionedWorktreeRepoRoot, or "" if the file is absent or doesn't
// parse. Never returns an error: a missing or unreadable state file is
// exactly equivalent to "no broker-provisioned worktree recorded", which is
// the correct, common state for every agent that isn't one.
func readProvisionedWorktreeRepoRoot(agentDir string) string {
	data, err := os.ReadFile(filepath.Join(agentDir, provisionedWorktreeStateFile))
	if err != nil {
		return ""
	}
	var state provisionedWorktreeState
	if err := json.Unmarshal(data, &state); err != nil {
		return ""
	}
	return state.RepoRoot
}

// persistProvisionedWorktreeRepoRootIfValid validates ctxRepoRoot against
// workspace (via validatedWorktreeRepoRoot) and, only if it validates and
// differs from what is already on disk, persists it to agentDir's
// broker-owned state file. A no-op when ctxRepoRoot is empty or fails to
// validate.
//
// workspace must be passed in its original, as-given form — the same
// spelling the caller received it in, not a form resolved ahead of this
// call; validatedWorktreeRepoRoot applies its own resolution internally.
// run.go's Start must pass the value effectiveWorkspace held before calling
// runtime.ValidateWorkspaceSource, not the resolved value that call
// produces — the two calls run.go makes to validatedWorktreeRepoRoot (its
// own RunConfig.RepoRoot resolution and this persistence gate) only agree
// with each other when both are given the same pre-resolution spelling.
//
// This is the single persistence gate shared by every call site that can be
// the first to see a fresh ctx signal for a given dispatch:
//   - ProvisionAgent, for a fresh create (via GetAgent, for
//     Manager.Provision/Manager.Start's normal first-provision path) — the
//     hub's provision-only flow provisions without ever calling Start in the
//     same dispatch, so this is the only chance to record the value there.
//     Reprovision is clone-per-agent only today, so it never carries this
//     signal; if it gains worktree support, the persist in ProvisionAgent
//     already covers it, since Reprovision also calls ProvisionAgent
//     directly.
//   - run.go's Start, for the case ProvisionAgent never runs at all: GetAgent
//     skips it when the agent directory already exists on disk (e.g. a
//     leftover from a deleted hub agent recreated under the same name).
//
// A write failure only means a later resume falls back to detectRepoRoot;
// see writeProvisionedWorktreeRepoRoot.
func persistProvisionedWorktreeRepoRootIfValid(agentDir, ctxRepoRoot, workspace string) {
	if ctxRepoRoot == "" || validatedWorktreeRepoRoot(ctxRepoRoot, workspace) != ctxRepoRoot {
		return
	}
	if readProvisionedWorktreeRepoRoot(agentDir) == ctxRepoRoot {
		return
	}
	if err := writeProvisionedWorktreeRepoRoot(agentDir, ctxRepoRoot); err != nil {
		util.Debugf("persistProvisionedWorktreeRepoRootIfValid: failed to persist for %s: %v", agentDir, err)
	}
}

func UpdateAgentConfig(agentName string, projectPath string, status string, runtime string, profile string) error {
	return updateSavedAgentInfo(agentName, projectPath, func(info *api.AgentInfo) {
		if status != "" {
			info.Phase = status
		}
		if runtime != "" {
			info.Runtime = runtime
		}
		if profile != "" {
			info.Profile = profile
		}
	})
}

// MarkAgentSoftDeleted sets Phase "deleted" and, when deletedAt is not
// zero, DeletedAt in one locked update of agent-info.json, so a concurrent
// writer never sees or keeps only half of the soft-delete mark.
func MarkAgentSoftDeleted(agentName string, projectPath string, deletedAt time.Time) error {
	return updateSavedAgentInfo(agentName, projectPath, func(info *api.AgentInfo) {
		info.Phase = softDeletedPhase
		if !deletedAt.IsZero() {
			info.DeletedAt = deletedAt
		}
	})
}

// UpdateAgentDeletedAt writes the deletedAt timestamp to agent-info.json.
func UpdateAgentDeletedAt(agentName string, projectPath string, deletedAt time.Time) error {
	return updateSavedAgentInfo(agentName, projectPath, func(info *api.AgentInfo) {
		info.DeletedAt = deletedAt
	})
}

// handleAgentDirWithoutConfigGuarded deals with an agent directory that exists but
// holds no scion-agent config file (ptone/scion#4438). Such a dir is either
// left by a provisioning attempt that failed before writing
// scion-agent.json, or belongs to an agent created before scion-agent.json
// existed. It is a no-op when agentDir does not exist or has a config file.
//
//   - A populated home (non-empty home/, either agentHome or agentDir/home)
//     with a stored agent record (agent-info.json in agentHome) is kept: a
//     minimal scion-agent.json is written from the record, and nothing is
//     removed.
//   - A populated home with no usable record, or a home that cannot be
//     read, is never removed: the call returns an error wrapping
//     config.ErrAgentStateConflict. The error text names no host path.
//   - A dir with no home state whose in-dir workspace may hold work is
//     never removed: the call returns an error wrapping
//     config.ErrAgentStateConflict (ptone/scion#4460). See
//     worktreeMayHoldWork; only worktrees that belong to the project
//     repository are inspected, and anything else that may hold data, or
//     cannot be checked, is kept.
//   - A dir kept for its workspace records when and why in a marker file;
//     once the soft-delete retention window has passed, it is removed
//     without inspecting the workspace (ptone/scion#4742, see
//     kept_agent_dir.go).
//   - Any other dir with no home state is stale: it is removed, with git's
//     entry for its own worktree, so the caller re-provisions it.
//
// Every removal passes the agent files guard g (GetAgent passes the
// project's guard, projectAgentFilesGuard).
func handleAgentDirWithoutConfigGuarded(g *agentFilesGuard, agentName, agentDir, agentHome, agentWorkspace string) error {
	if _, err := os.Stat(agentDir); err != nil {
		return nil
	}
	if config.GetScionAgentConfigPath(agentDir) != "" {
		return nil
	}

	homes := []string{agentHome}
	if inDir := filepath.Join(agentDir, "home"); filepath.Clean(inDir) != filepath.Clean(agentHome) {
		homes = append(homes, inDir)
	}
	populatedHome := ""
	for _, h := range homes {
		has, err := dirHasEntries(h)
		if err != nil {
			// Fail closed: a home that cannot be read may hold data.
			slog.Warn("GetAgent: agent home could not be read; keeping the agent directory",
				"agent", agentName, "agent_dir", agentDir, "home", h, "error", err)
			return fmt.Errorf("%w: agent %q state directory kept: could not read the agent's home; check its permissions, or delete and re-create the agent to reset it", config.ErrAgentStateConflict, agentName)
		}
		if has {
			populatedHome = h
			break
		}
	}

	if populatedHome != "" {
		info := readStoredAgentRecord(agentHome)
		if info == nil {
			slog.Warn("GetAgent: agent directory has home data but no config file and no agent-info.json record; keeping it",
				"agent", agentName, "agent_dir", agentDir, "home", populatedHome)
			return fmt.Errorf("%w: agent %q state directory kept: it has home data but no agent record; delete and re-create the agent to reset it", config.ErrAgentStateConflict, agentName)
		}
		cfg := minimalAgentConfigFromRecord(info)
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal agent config: %w", err)
		}
		if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), data, 0644); err != nil {
			return fmt.Errorf("failed to write agent config: %w", err)
		}
		// The dir is live again: a marker from an earlier keep no longer
		// applies (ptone/scion#4742).
		clearKeptAgentDirMarker(agentName, agentDir)
		slog.Info("GetAgent: agent directory had no config file; wrote a minimal scion-agent.json from agent-info.json and kept the home",
			"agent", agentName, "agent_dir", agentDir, "home", populatedHome,
			"template", info.Template, "harness_config", info.HarnessConfig)
		return nil
	}

	// A directory kept earlier (see recordKeptAgentDir) whose soft-delete
	// retention window has passed is removed without inspecting its
	// workspace, and the caller re-provisions it (ptone/scion#4742).
	if hasKeptAgentDirMarker(agentDir) {
		if retention := keptAgentDirRetention(); retention > 0 && purgeExpiredKeptAgentDirGuarded(g, agentName, agentDir, agentHome, retention) {
			return nil
		}
	}

	// No home state, but the in-dir workspace may still hold work
	// (ptone/scion#4460). Keep the dir when it does, or when that cannot be
	// checked. A shared-workspace agent has no workspace path, but its state
	// dir can be the one an earlier worktree-per-agent life used, so the
	// in-dir workspace/ is checked too (ptone/scion#4526). The entry
	// removal below gets the original path, so an empty one removes
	// nothing, except for a clean leftover worktree that git inspected as
	// one of the project repository's: its entry is removed through its
	// in-dir path, whose parent is agentDir, so the repository resolves
	// from the agent dir and never from the process's current directory
	// (ptone/scion#4511). Otherwise the project repository would keep the
	// removed worktree registered, and its branch would stay "in use" until
	// its entry was removed by hand.
	inDirWorkspace := agentWorkspace
	if inDirWorkspace == "" {
		inDirWorkspace = filepath.Join(agentDir, "workspace")
	}
	entryPath := agentWorkspace
	if pathWithin(inDirWorkspace, agentDir) {
		hasWork, inspected, err := worktreeMayHoldWork(agentDir, inDirWorkspace)
		if err != nil {
			slog.Warn("GetAgent: agent workspace could not be checked for uncommitted work; keeping the agent directory",
				"agent", agentName, "agent_dir", agentDir, "workspace", inDirWorkspace, "error", err)
			recordKeptAgentDir(agentName, agentDir, keptReasonWorkspaceCheckFailed)
			return fmt.Errorf("%w: agent %q state directory kept: could not check the agent's workspace for uncommitted work; check its ownership and permissions, or delete and re-create the agent to reset it", config.ErrAgentStateConflict, agentName)
		}
		if hasWork {
			slog.Warn("GetAgent: agent directory has no config file but its workspace may hold work; keeping it",
				"agent", agentName, "agent_dir", agentDir, "workspace", inDirWorkspace)
			recordKeptAgentDir(agentName, agentDir, keptReasonWorkspaceMayHoldWork)
			return fmt.Errorf("%w: agent %q state directory kept: its workspace may hold work (files, uncommitted changes, a standalone clone, or commits on no branch); save it, or delete and re-create the agent to reset it", config.ErrAgentStateConflict, agentName)
		}
		if entryPath == "" && inspected {
			entryPath = inDirWorkspace
		}
	}

	slog.Info("GetAgent: stale agent directory (no config file, no home state); removing it",
		"agent", agentName, "agent_dir", agentDir)
	if err := removeAgentDirGuarded(g, agentDir, entryPath); err != nil {
		return fmt.Errorf("%w: agent %q state directory kept: %w", config.ErrAgentStateConflict, agentName, err)
	}
	return nil
}

// worktreeMayHoldWork reports whether the workspace ws inside agentDir
// holds work that removing it would lose, and whether git inspected it. A
// missing ws, or an empty directory with no .git entry, holds none. Only a
// worktree that belongs to the project repository (the repository holding
// agentDir) is inspected with git; anything else that may hold data reports
// true without running git:
//
//   - a ws that is not a plain directory, or a non-empty one with no .git;
//   - a .git directory (a standalone clone: removing it would delete its
//     local branches and stash) or any .git entry that is not a regular
//     file;
//   - a .git pointer file whose worktree metadata does not point back to
//     the project repository (see projectWorktreeOf).
//
// A worktree whose admin dir has any entry an idle worktree does not have
// (see idleWorktreeAdminEntries: for example a lock, or an in-progress
// rebase, merge, cherry-pick, revert, sequencer or bisect, whose state can
// hold work such as a rebase autostash), or a per-worktree ref under
// refs/, or any worktree while the project repository has per-worktree
// config on (extensions.worktreeConfig), also reports true without running
// git. Worktrees with submodules are always
// kept: a gitlink entry in the index reports true before git status runs,
// because status would read each submodule's own repository metadata, and
// only worktree metadata that points back to the project repository is
// trusted.
//
// For a worktree of the project repository, work is an uncommitted or
// untracked change, any index entry with the assume-unchanged or
// skip-worktree bit (git status does not report edits to those files), or
// a HEAD that is not on a branch (detached, or a symbolic ref outside
// refs/heads/) whose commit no branch or remote ref reaches. A worktree on
// a named branch is fine for the detached-HEAD check: removing the
// directory does not delete the branch, which lives in the project
// repository, so its commits stay reachable. Any failure, including a git
// command that exceeds worktreeGitTimeout, is returned, and callers must
// treat it as "may hold work".
//
// Deliberate exceptions to "keep when unsure", for a clean worktree of the
// project repository that is removed:
//
//   - Files ignored by any .gitignore inside the workspace (tracked or
//     untracked, including one that ignores itself) or by the repository's
//     info/exclude (for example .env, local config, build outputs) are not
//     reported by git status and are removed with the worktree. Keeping on
//     them would make almost every used worktree a permanent conflict
//     (node_modules, dist and similar). A global excludes file is not
//     applied, so it cannot hide an untracked file. The number of ignored
//     status entries (a fully ignored directory counts once) and a few
//     paths are logged at Warn before the removal.
//   - The idle per-worktree admin state goes with the entry removal: the HEAD
//     reflog, ORIG_HEAD, FETCH_HEAD and AUTO_MERGE. Commits reachable only
//     from them are lost.
func worktreeMayHoldWork(agentDir, ws string) (hasWork, inspected bool, err error) {
	info, err := os.Lstat(ws)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, false, nil
		}
		return false, false, err
	}
	if !info.IsDir() {
		return true, false, nil
	}
	gitInfo, err := os.Lstat(filepath.Join(ws, ".git"))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return false, false, err
		}
		has, err := dirHasEntries(ws)
		if err != nil {
			return false, false, err
		}
		return has, false, nil
	}
	if !gitInfo.Mode().IsRegular() {
		return true, false, nil
	}
	wt, ok := projectWorktreeOf(agentDir, ws)
	if !ok {
		return true, false, nil
	}
	// An entry an idle worktree does not have (an in-progress operation, a
	// lock, a per-worktree ref) may hold work git status does not report.
	idle, err := worktreeAdminDirIdle(wt.gitDir)
	if err != nil {
		return false, false, err
	}
	if !idle {
		return true, false, nil
	}
	defer func() {
		if err == nil && inspected && !hasWork {
			warnIgnoredFiles(wt)
		}
	}()
	// With per-worktree config on, git would read a config.worktree in the
	// gitdir, which projectWorktreeOf only checked before git starts.
	on, err := worktreeConfigEnabled(wt.commonDir)
	if err != nil {
		return false, false, err
	}
	if on {
		return true, false, nil
	}

	// Only trust worktree metadata that points back to the project
	// repository: git status would read each submodule's own repository,
	// so a worktree with a gitlink entry is kept before status runs.
	// Listing the index does not descend into submodules.
	staged, err := runWorktreeGit(wt, "ls-files", "-s", "-z")
	if err != nil {
		return false, true, fmt.Errorf("git ls-files: %w", err)
	}
	if hasGitlinkEntries(staged) {
		return true, true, nil
	}

	status, err := runWorktreeGit(wt, "status", "--porcelain", "--untracked-files=normal", "--ignore-submodules=dirty")
	if err != nil {
		return false, true, fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return true, true, nil
	}
	// git status does not report edits to files with the assume-unchanged
	// or skip-worktree index bit, so a clean status cannot be trusted when
	// any entry has one.
	entries, err := runWorktreeGit(wt, "ls-files", "-v", "-z")
	if err != nil {
		return false, true, fmt.Errorf("git ls-files: %w", err)
	}
	if hasHiddenIndexEntries(entries) {
		return true, true, nil
	}

	// symbolic-ref -q exits 1 with no output for a detached HEAD. A
	// symbolic ref outside refs/heads/ (for example refs/worktree/x) is not
	// a branch, so it gets the same reachability check as a detached HEAD.
	head, err := runWorktreeGit(wt, "symbolic-ref", "-q", "HEAD")
	if err == nil {
		if strings.HasPrefix(strings.TrimSpace(head), "refs/heads/") {
			return false, true, nil
		}
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return false, true, fmt.Errorf("git symbolic-ref: %w", err)
		}
	}
	refs, err := runWorktreeGit(wt, "for-each-ref", "--count=1", "--contains", "HEAD", "--format=%(refname)", "refs/heads/", "refs/remotes/")
	if err != nil {
		return false, true, fmt.Errorf("git for-each-ref: %w", err)
	}
	return strings.TrimSpace(refs) == "", true, nil
}

// idleWorktreeAdminEntries are the entries the admin dir of an idle linked
// worktree (<common dir>/worktrees/<id>) has. Observed with git 2.56 after
// worktree add, commit, switch, fetch, merge, reset, rebase and stash
// push/pop; see also git-worktree(1) DETAILS and REFS, and
// gitrepository-layout(5) worktrees/<id>/. Anything else, for example
// locked, config.worktree, rebase-merge, rebase-apply, MERGE_HEAD,
// CHERRY_PICK_HEAD, REVERT_HEAD, sequencer or BISECT_LOG, is not idle.
var idleWorktreeAdminEntries = map[string]bool{
	"HEAD":           true,
	"ORIG_HEAD":      true,
	"FETCH_HEAD":     true,
	"AUTO_MERGE":     true,
	"COMMIT_EDITMSG": true,
	"commondir":      true,
	"gitdir":         true,
	"index":          true,
	"logs":           true,
	"refs":           true,
}

// worktreeAdminDirIdle reports whether the worktree admin dir gitDir has
// only idleWorktreeAdminEntries, and whether its refs/ directory, which
// holds the per-worktree refs (refs/bisect, refs/worktree,
// refs/rewritten), has no entry other than directories. It reads the
// filesystem only. A read failure is returned.
func worktreeAdminDirIdle(gitDir string) (bool, error) {
	entries, err := os.ReadDir(gitDir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !idleWorktreeAdminEntries[e.Name()] {
			return false, nil
		}
	}
	idle := true
	err = filepath.WalkDir(filepath.Join(gitDir, "refs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			idle = false
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return idle, nil
}

// warnIgnoredFiles logs, at Warn, how many ignored git status entries the
// clean worktree wt has and a few of their paths, because they are removed
// with it. The count is of entries, not files: a fully ignored directory is
// one entry however many files it holds. It changes nothing; a failure is
// logged too.
func warnIgnoredFiles(wt projectWorktree) {
	out, err := runWorktreeGit(wt, "status", "--porcelain", "-z", "--untracked-files=normal", "--ignored=matching")
	if err != nil {
		slog.Warn("GetAgent: could not list the ignored files of a clean stale agent workspace before removing it",
			"workspace", wt.workTree, "error", err)
		return
	}
	var ignored []string
	for _, entry := range strings.Split(out, "\x00") {
		if p, ok := strings.CutPrefix(entry, "!! "); ok {
			ignored = append(ignored, p)
		}
	}
	if len(ignored) == 0 {
		return
	}
	sample := ignored
	if len(sample) > 5 {
		sample = sample[:5]
	}
	slog.Warn("GetAgent: clean stale agent workspace has ignored files; they are removed with it",
		"workspace", wt.workTree, "ignored_entries", len(ignored), "ignored_sample", sample)
}

// hasHiddenIndexEntries reports whether NUL-separated git ls-files -v
// output has an entry whose tag is lowercase (assume-unchanged) or 'S'
// (skip-worktree).
func hasHiddenIndexEntries(lsFiles string) bool {
	for _, entry := range strings.Split(lsFiles, "\x00") {
		if entry == "" {
			continue
		}
		if tag := entry[0]; tag == 'S' || (tag >= 'a' && tag <= 'z') {
			return true
		}
	}
	return false
}

// hasGitlinkEntries reports whether NUL-separated git ls-files -s output
// has an entry with mode 160000 (a submodule).
func hasGitlinkEntries(lsFiles string) bool {
	for _, entry := range strings.Split(lsFiles, "\x00") {
		if strings.HasPrefix(entry, "160000 ") {
			return true
		}
	}
	return false
}

// worktreeConfigEnabled reports whether the project repository's config
// file in commonDir turns on extensions.worktreeConfig. git reads only that
// file (no includes, no system or global config), with the same scrubbed
// environment as worktreeGit. A value git cannot read as a boolean, or any
// other failure, is returned as an error.
func worktreeConfigEnabled(commonDir string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), worktreeGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "config", "--file", filepath.Join(commonDir, "config"), "--type=bool", "--get", "extensions.worktreeConfig")
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = commonDir
	cmd.Env = worktreeGitEnv()
	out, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("git config: %w", ctxErr)
	}
	if err != nil {
		// --get exits 1 when the key is not set.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git config: %w", err)
	}
	switch strings.TrimSpace(string(out)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("git config: unexpected value %q", strings.TrimSpace(string(out)))
	}
}

// projectWorktree names a worktree of the project repository by its
// resolved paths: the work tree, its per-worktree gitdir under
// <commonDir>/worktrees/, and the project repository's common git dir.
type projectWorktree struct {
	workTree  string
	gitDir    string
	commonDir string
}

// projectWorktreeOf reports whether ws is a worktree of the project
// repository, the repository holding agentDir, and returns its resolved
// paths. It reads the filesystem only and runs no git. It only trusts
// worktree metadata that points back to the project repository; all paths
// are compared after resolving symlinks, and all of these must hold:
//
//   - the .git pointer file in ws names a gitdir that is a direct child of
//     the project repository's <common dir>/worktrees/;
//   - that gitdir's commondir file exists and names exactly the project
//     repository's common dir;
//   - that gitdir has no config.worktree file;
//   - that gitdir's gitdir file names ws/.git.
//
// Anything else, including a project repository that cannot be found or a
// path that cannot be resolved, reports false.
func projectWorktreeOf(agentDir, ws string) (projectWorktree, bool) {
	common, ok := projectRepoCommonDir(filepath.Dir(filepath.Clean(agentDir)))
	if !ok {
		return projectWorktree{}, false
	}
	commonDir, err := filepath.EvalSymlinks(common)
	if err != nil {
		return projectWorktree{}, false
	}
	workTree, err := filepath.EvalSymlinks(ws)
	if err != nil {
		return projectWorktree{}, false
	}
	dotGit := filepath.Join(workTree, ".git")
	pointer, ok := readGitdirPointer(dotGit)
	if !ok {
		return projectWorktree{}, false
	}
	gitDir, err := filepath.EvalSymlinks(pointer)
	if err != nil {
		return projectWorktree{}, false
	}
	name := filepath.Base(gitDir)
	if filepath.Dir(gitDir) != filepath.Join(commonDir, "worktrees") || name == "." || name == ".." || name == string(filepath.Separator) {
		return projectWorktree{}, false
	}
	if target, ok := readPathFile(filepath.Join(gitDir, "commondir"), gitDir); !ok || target != commonDir {
		return projectWorktree{}, false
	}
	if _, err := os.Lstat(filepath.Join(gitDir, "config.worktree")); !errors.Is(err, fs.ErrNotExist) {
		return projectWorktree{}, false
	}
	if target, ok := readPathFile(filepath.Join(gitDir, "gitdir"), gitDir); !ok || target != dotGit {
		return projectWorktree{}, false
	}
	return projectWorktree{workTree: workTree, gitDir: gitDir, commonDir: commonDir}, true
}

// readPathFile reads a git admin file that holds a single path (commondir,
// gitdir), resolves it against base when relative, and returns it with
// symlinks resolved.
func readPathFile(path, base string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	p := strings.TrimSpace(strings.TrimRight(line, "\r"))
	if p == "" {
		return "", false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	return resolved, true
}

// projectRepoCommonDir walks up from dir to the nearest .git entry and
// returns that repository's common git dir: the .git directory itself, or,
// for a .git pointer file (a project checked out in a linked worktree), the
// directory its gitdir's commondir file names. It reads the filesystem
// only.
func projectRepoCommonDir(dir string) (string, bool) {
	for {
		dotGit := filepath.Join(dir, ".git")
		if info, err := os.Lstat(dotGit); err == nil {
			if info.IsDir() {
				return dotGit, true
			}
			if !info.Mode().IsRegular() {
				return "", false
			}
			gitdir, ok := readGitdirPointer(dotGit)
			if !ok {
				return "", false
			}
			data, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return gitdir, true
				}
				return "", false
			}
			common := strings.TrimSpace(string(data))
			if common == "" {
				return "", false
			}
			if !filepath.IsAbs(common) {
				common = filepath.Join(gitdir, common)
			}
			return filepath.Clean(common), true
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// readGitdirPointer parses a .git pointer file ("gitdir: <path>") and
// returns the gitdir it names, resolved against the file's directory when
// relative.
func readGitdirPointer(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	gitdir, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "gitdir: ")
	gitdir = strings.TrimSpace(gitdir)
	if !ok || gitdir == "" {
		return "", false
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(path), gitdir)
	}
	return filepath.Clean(gitdir), true
}

// runWorktreeGit runs git in a worktree of the project repository. Tests
// replace it to observe whether git ran.
var runWorktreeGit = worktreeGit

// worktreeGitTimeout bounds each git command worktreeGit runs. A command
// that exceeds it fails, so the workspace counts as "may hold work".
var worktreeGitTimeout = 30 * time.Second

// worktreeGit runs git in the worktree wt with a fixed locale, with the
// file-system monitor and the untracked cache off, with only the ignore
// rules inside the repository and workspace (no global excludes file), within
// worktreeGitTimeout, and without GIT_* variables from the process
// environment, so the answer does not depend on how the process was
// started. GIT_DIR, GIT_COMMON_DIR and GIT_WORK_TREE are pinned to the paths
// projectWorktreeOf checked, so git does not follow the worktree's own
// metadata files again when it runs, mirroring the pin in
// provision.SafeGitCommand.
func worktreeGit(wt projectWorktree, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), worktreeGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{
		"-c", "core.fsmonitor=false",
		"-c", "core.untrackedCache=false",
		"-c", "core.excludesFile=" + os.DevNull,
		"-C", wt.workTree,
	}, args...)...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(worktreeGitEnv(),
		"GIT_DIR="+wt.gitDir,
		"GIT_COMMON_DIR="+wt.commonDir,
		"GIT_WORK_TREE="+wt.workTree,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), ctxErr)
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	// git status warns on stderr and still exits 0 when it skips a
	// directory it cannot read, so its output would be incomplete.
	if len(args) > 0 && args[0] == "status" {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git status wrote to stderr: %s", msg)
		}
	}
	return string(out), nil
}

// worktreeGitEnv returns the process environment without GIT_* variables
// (other than GIT_CONFIG_NOSYSTEM and GIT_CONFIG_GLOBAL) and with LC_ALL=C.
func worktreeGitEnv() []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "GIT_CONFIG_NOSYSTEM" || k == "GIT_CONFIG_GLOBAL":
			// These only choose which system and global config files git
			// reads, as for any git the process runs; they add no
			// repository settings. Keeping them lets tests skip the host's
			// system config.
		case strings.HasPrefix(k, "GIT_") || k == "LC_ALL":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "LC_ALL=C")
}

// removeAgentDirGuarded removes agentDir, once the guard g allows it
// (removeCheckedAgentDir does the removal, at the path the guard returns),
// and returns the guard's refusal otherwise.
func removeAgentDirGuarded(g *agentFilesGuard, agentDir, agentWorkspace string) error {
	target, err := g.check(agentDir)
	if err != nil {
		return err
	}
	removeCheckedAgentDir(target, agentWorkspace)
	return nil
}

// removeCheckedAgentDir removes agentDir, a path the agent files guard
// already allowed, and then git's entry for the worktree at agentWorkspace,
// so git forgets a worktree that pointed into the removed directory and a
// later provision can recreate it. agentWorkspace is the path git
// registered the worktree under, which can differ from a path inside
// agentDir (agentDir may have been moved before removal). An empty
// agentWorkspace (a shared-workspace agent, or a directory with no worktree
// of its own) removes no entry.
func removeCheckedAgentDir(agentDir, agentWorkspace string) {
	// chmod to ensure we can remove root-owned files left by containers.
	// Links are skipped, so their targets are left unchanged; RemoveAll
	// removes the link itself.
	_ = filepath.WalkDir(agentDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		_ = os.Chmod(path, 0755)
		return nil
	})
	_ = os.RemoveAll(agentDir)
	removeWorkspaceWorktreeEntry(agentWorkspace)
}

// removeWorkspaceWorktreeEntry removes git's entry for the worktree at
// agentWorkspace from the repository holding it, once its directory is
// gone. Only that one entry is touched: a repository-wide prune would also
// drop the entries of other agents' worktrees whose directories this
// process cannot see (ptone/scion#5071). An empty agentWorkspace (a
// shared-workspace agent) has no worktree of its own, so nothing is done:
// resolving the repository from an empty path would pick the process's
// current directory, which is unrelated to the agent (ptone/scion#4511).
// Failures are logged; there is no fallback.
func removeWorkspaceWorktreeEntry(agentWorkspace string) {
	if agentWorkspace == "" {
		return
	}
	root, err := util.RepoRootDir(filepath.Dir(agentWorkspace))
	if err != nil {
		return
	}
	if err := util.RemoveWorktreeEntryIn(root, agentWorkspace); err != nil {
		slog.Warn("could not remove the worktree entry", "path", agentWorkspace, "error", err)
	}
}

// claimAgentHome is claimDir for ProvisionAgent's agent home; a variable
// so a test can make the home claim fail.
var claimAgentHome = claimDir

// claimDir creates dir with os.Mkdir and reports whether this call created
// it. A dir that already exists is not an error and reports false.
func claimDir(dir string) (bool, error) {
	err := os.Mkdir(dir, 0755)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	return false, err
}

// pathWithin reports whether path is dir or lies under it.
func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// dirHasEntries reports whether path is a directory with at least one entry.
// A missing path has no entries. Any other error is returned, and callers
// must treat it as "may hold data".
func dirHasEntries(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return len(entries) > 0, nil
}

// readStoredAgentRecord returns the agent-info.json in agentHome (the home
// GetAgent reads it from) if it parses and names a template or
// harness-config, or nil otherwise.
func readStoredAgentRecord(agentHome string) *api.AgentInfo {
	data, err := os.ReadFile(filepath.Join(agentHome, "agent-info.json"))
	if err != nil {
		return nil
	}
	var info api.AgentInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil
	}
	if info.Template == "" && info.HarnessConfig == "" {
		return nil
	}
	return &info
}

// minimalAgentConfigFromRecord builds the scion-agent.json written for an
// agent adopted by handleAgentDirWithoutConfigGuarded. It records only the
// harness-config; GetAgent layers the template named in agent-info.json
// beneath it as for any existing agent.
func minimalAgentConfigFromRecord(info *api.AgentInfo) *api.ScionConfig {
	return &api.ScionConfig{HarnessConfig: info.HarnessConfig}
}

func GetAgent(ctx context.Context, agentName string, templateName string, agentImage string, harnessConfig string, projectPath string, profileName string, optionalStatus string, branch string, workspace string, inlineConfig ...*api.ScionConfig) (string, string, string, *api.ScionConfig, error) {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return "", "", "", nil, err
	}

	util.Debugf("GetAgent: agentName=%s templateName=%q harnessConfig=%q projectPath=%q projectDir=%s",
		agentName, templateName, harnessConfig, projectPath, projectDir)

	ctx, agentDir, sharedWorkspace, err := withAgentStateDir(ctx, projectDir, agentName)
	if err != nil {
		return "", "", "", nil, err
	}
	// A broker start or restart of a shared-workspace agent requires its
	// broker-side agent dir; only a create provisions a new one. A missing
	// dir fails closed rather than provisioning anywhere else.
	if sharedWorkspace && api.IsBrokerModeFromContext(ctx) && !api.IsFreshProvisionFromContext(ctx) {
		if _, statErr := os.Stat(agentDir); os.IsNotExist(statErr) {
			return "", "", "", nil, fmt.Errorf("%w: agent %q has no broker-side state directory; re-create the agent", config.ErrAgentStateDirUnavailable, agentName)
		}
	}

	agentHome := config.GetAgentHomePath(projectDir, agentName)
	var agentWorkspace string
	if !sharedWorkspace {
		agentWorkspace = filepath.Join(agentDir, "workspace")
	}

	// Remove other agents' kept directories in the same agents root whose
	// soft-delete retention window has passed (ptone/scion#4742).
	sweepExpiredKeptAgentDirsGuarded(filepath.Dir(agentDir), agentName, func(name string) string {
		return config.GetAgentHomePath(projectDir, name)
	}, func(name string) *agentFilesGuard {
		return projectAgentFilesGuard(ctx, projectDir, name, filepath.Join(filepath.Dir(agentDir), name))
	})

	// Handle an agent directory that exists but has no config file. See
	// handleAgentDirWithoutConfigGuarded: a populated home is kept (adopted from
	// agent-info.json, or refused), and only a dir with no home state is
	// removed so it can be re-provisioned.
	if err := handleAgentDirWithoutConfigGuarded(projectAgentFilesGuard(ctx, projectDir, agentName, agentDir), agentName, agentDir, agentHome, agentWorkspace); err != nil {
		return "", "", "", nil, err
	}

	// An explicit --workspace agent has no managed per-agent worktree: its mount
	// is the operator's own directory, recovered on resume from the persisted
	// /workspace volume (see effectiveWorkspace in run.go). Treat it like a
	// shared-workspace agent by clearing agentWorkspace, so the managed-worktree
	// recovery below is skipped. Otherwise, when projectDir is itself a git repo,
	// that recovery would CreateWorktree a throwaway managed worktree and the
	// agent would silently edit that phantom branch instead of the operator's
	// explicit tree — breaking "edit the real tree in place" on resume.
	//
	// An empty-per-agent agent's persisted mode (design #2703) is ORed into
	// ctx, so a resume whose request lost the mode still recreates its
	// private directory below instead of a worktree or nothing.
	if config.ScionAgentConfigExists(agentDir) {
		if persisted, cfgErr := (&config.Template{Path: agentDir}).LoadConfig(); cfgErr != nil {
			util.Debugf("GetAgent: could not load persisted config to check workspace mode: %v", cfgErr)
		} else if persisted != nil {
			if persisted.EmptyPerAgentWorkspace {
				ctx = api.ContextWithEmptyPerAgentWorkspace(ctx)
			}
			if agentWorkspace != "" && persisted.ExplicitWorkspace {
				util.Debugf("GetAgent: explicit-workspace agent %q — skipping managed-worktree recovery", agentName)
				agentWorkspace = ""
			}
		}
	}

	// If the managed workspace directory doesn't exist, try to recreate it.
	// Only do this for existing, fully-provisioned agents (config file present).
	// For new agents or stale directories, ProvisionAgent handles worktree creation.
	// Skipped for shared-workspace agents (agentWorkspace == "") because they
	// share the project-wide checkout and have no per-agent worktree.
	if agentWorkspace != "" && config.GetScionAgentConfigPath(agentDir) != "" {
		if _, err := os.Stat(agentWorkspace); os.IsNotExist(err) {
			if api.IsEmptyPerAgentWorkspaceFromContext(ctx) {
				// Empty-per-agent (design #2703): recreate the private
				// directory empty. Never a worktree, even when projectDir
				// sits in a git repository, and never cleared so the agent
				// falls back to another mount.
				if err := os.MkdirAll(agentWorkspace, 0755); err != nil {
					return "", "", "", nil, fmt.Errorf("failed to recreate workspace directory: %w", err)
				}
			} else if util.IsGitRepoDir(projectDir) {
				// Recreate the worktree for git-backed workspaces.
				targetBranch := branch
				if targetBranch == "" {
					targetBranch = api.Slugify(agentName)
				}
				removeWorkspaceWorktreeEntry(agentWorkspace)
				if err := util.CreateWorktree(agentWorkspace, targetBranch); err != nil {
					slog.Warn("GetAgent: could not recreate the missing worktree; continuing without a workspace",
						"agent", agentName, "path", agentWorkspace, "branch", targetBranch, "error", err)
					agentWorkspace = ""
				} else {
					util.Debugf("GetAgent: recreated missing worktree at %s (branch %s)", agentWorkspace, targetBranch)
				}
			} else {
				agentWorkspace = ""
			}
		}
	}

	// Load settings for default template
	vs, vsWarnings, err := config.LoadEffectiveSettingsFor(ctx, projectDir)
	if err != nil {
		util.Debugf("failed to load effective settings: %v", err)
	}
	config.PrintDeprecationWarnings(vsWarnings)
	defaultTemplate := "default"
	if vs != nil && vs.DefaultTemplate != "" {
		defaultTemplate = vs.DefaultTemplate
	}

	if _, err := os.Stat(agentDir); os.IsNotExist(err) {
		if templateName == "" {
			templateName = defaultTemplate
		}
		util.Debugf("GetAgent: agent dir does not exist, provisioning with template=%q", templateName)
		var ic *api.ScionConfig
		if len(inlineConfig) > 0 {
			ic = inlineConfig[0]
		}
		home, ws, cfg, err := ProvisionAgent(ctx, agentName, templateName, agentImage, harnessConfig, projectPath, profileName, optionalStatus, branch, workspace, ic)
		if err != nil {
			util.Debugf("GetAgent: ProvisionAgent failed: %v", err)
		} else {
			util.Debugf("GetAgent: ProvisionAgent succeeded, harness=%q harnessConfig=%q image=%q",
				cfg.Harness, cfg.HarnessConfig, cfg.Image)
		}
		return agentDir, home, ws, cfg, err
	}

	util.Debugf("GetAgent: agent dir exists, loading existing config from %s", agentDir)

	// When git clone is configured on a fresh provision (hub-dispatched
	// create), clear the workspace so sciontool performs a fresh clone. The
	// agent directory may be left over from a previous agent with the same
	// name that was deleted via the hub but whose local files were not
	// cleaned up. Without this, sciontool sees the old clone as "already
	// populated" and skips cloning.
	//
	// Gated on FreshProvision, not just GitClone being set: start also
	// carries GitClone, so a workspace that didn't survive a stop can be
	// recreated, but it must never clear a workspace that did survive —
	// that would discard un-pushed work.
	if gitClone := api.GitCloneFromContext(ctx); gitClone != nil && api.IsFreshProvisionFromContext(ctx) {
		if info, err := os.Stat(agentWorkspace); err == nil && info.IsDir() {
			if !isWorkspaceEmptyDir(agentWorkspace) {
				util.Debugf("GetAgent: clearing existing workspace for git-clone re-provision: %s", agentWorkspace)
				target, err := projectAgentFilesGuard(ctx, projectDir, agentName, agentDir).check(agentWorkspace)
				if err != nil {
					return "", "", "", nil, fmt.Errorf("failed to clear the workspace for a fresh clone: %w", err)
				}
				_ = util.MakeWritableRecursive(target)
				if err := os.RemoveAll(target); err != nil {
					util.Debugf("GetAgent: failed to clear workspace: %v", err)
				}
				if err := os.MkdirAll(agentWorkspace, 0755); err != nil {
					util.Debugf("GetAgent: failed to recreate workspace: %v", err)
				}
			}
		}
	}

	// Try to load agent-info.json first to get the template
	agentInfoPath := filepath.Join(agentHome, "agent-info.json")
	var agentInfo *api.AgentInfo
	effectiveTemplate := defaultTemplate

	// A template recorded as loaded from a content-addressed cache (a
	// TemplateHash, or a content hash stored as the name by older versions)
	// is not available by name on this broker, so it is not looked up by
	// name: the agent loads from its persisted config alone, as it did when
	// the stored name was the cache directory's hash.
	hydratedTemplate := false
	if infoData, err := os.ReadFile(agentInfoPath); err == nil {
		if err := json.Unmarshal(infoData, &agentInfo); err == nil {
			hydratedTemplate = normalizeHydratedTemplateInfo(agentInfo, api.TemplateNameFromContext(ctx))
			if agentInfo.Template != "" {
				effectiveTemplate = agentInfo.Template
			}
		}
	}

	// Load the agent's scion-agent.json from agent root
	// This might not contain Info anymore, but might contain other overrides
	tpl := &config.Template{Path: agentDir}
	agentCfg, err := tpl.LoadConfig()
	if err != nil {
		return agentDir, agentHome, agentWorkspace, nil, fmt.Errorf("failed to load agent config: %w", err)
	}

	var chain []*config.Template
	if hydratedTemplate {
		err = config.NewTemplateNotFoundError(effectiveTemplate, fmt.Sprintf("template %q was loaded from a content-addressed cache (%s)", effectiveTemplate, agentInfo.TemplateHash))
	} else {
		chain, err = config.GetTemplateChainInProject(effectiveTemplate, projectPath)
	}
	if err != nil {
		util.Debugf("GetAgent: template chain for %q not found: %v, returning agentCfg only (harness=%q image=%q)",
			effectiveTemplate, err, agentCfg.Harness, agentCfg.Image)
		resolveModelAliasForExistingAgent(ctx, agentCfg, projectDir)
		// Populate Info from agent-info.json here too, matching the
		// successful-lookup path below. scion-agent.json never carries Info
		// (json:"-"), so without this, run.go's own independent
		// template/profile resolution loses finalScionCfg.Info.Template and
		// .Profile whenever the on-disk template can't be found for the
		// same reason this lookup just failed — silently disabling both the
		// unresolvable-template image fallback and the saved-profile
		// fallback exactly when they matter most (ptone/scion#2156).
		if agentInfo != nil {
			agentCfg.Info = agentInfo
		}
		return agentDir, agentHome, agentWorkspace, agentCfg, nil
	}

	// Each template contributes its job keys only (ptone/scion#4524):
	// templateJobConfig drops its environment keys and its env, so a restart
	// never reintroduces them under the stored config. Start applies the
	// live template env as a final fill.
	mergedCfg := &api.ScionConfig{}
	for _, tpl := range chain {
		tplCfg, err := tpl.LoadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to load config from template %s, skipping: %v\n", templateRef(tpl), err)
			continue
		}
		mergedCfg = config.MergeScionConfig(mergedCfg, templateJobConfig(ctx, tpl, tplCfg))
	}

	finalCfg := config.MergeScionConfig(mergedCfg, agentCfg)

	// Resolve model size aliases for existing agents.
	// Mirrors the alias resolution in ProvisionAgent (line 778-785).
	// This covers the case where scion-agent.json was written with a raw alias
	// (e.g. by applyInlineConfigUpdate before the hub-side fix) or where the
	// agent was created before the hub resolved aliases at storage time.
	resolveModelAliasForExistingAgent(ctx, finalCfg, projectDir)

	// Ensure Info is populated from agent-info.json if available
	if agentInfo != nil {
		finalCfg.Info = agentInfo
	}

	util.Debugf("GetAgent: existing agent config loaded, harness=%q harnessConfig=%q image=%q defaultHarnessConfig=%q",
		finalCfg.Harness, finalCfg.HarnessConfig, finalCfg.Image, finalCfg.DefaultHarnessConfig)

	return agentDir, agentHome, agentWorkspace, finalCfg, nil
}

// resolveModelAliasForExistingAgent resolves cfg.Model in place when it is
// still a size alias (e.g. "large"). It first tries the harness-config's
// model_aliases map resolved from disk (resolveHarnessConfigDir); when that
// can't be resolved — which is the common case for hub-dispatched agents
// that have no local template chain — it falls back to the harness's
// built-in alias table (harnesses/<name>/config.yaml, via
// harness.DefaultModelAliases). This is a no-op if cfg is nil, cfg.Model is
// empty, or cfg.Model is not a known alias.
//
// projectDir is the resolved project dir (config.GetResolvedProjectDir), the
// same one harness-config resolution uses elsewhere.
func resolveModelAliasForExistingAgent(ctx context.Context, cfg *api.ScionConfig, projectDir string) {
	if cfg == nil || cfg.Model == "" {
		return
	}

	aliases := map[string]string{}
	hcName := cfg.HarnessConfig
	if hcName == "" {
		hcName = cfg.DefaultHarnessConfig
	}
	if hcName != "" {
		if hcDir, err := resolveHarnessConfigDir(ctx, hcName, projectDir); err == nil && hcDir != nil {
			// hcDir.Config is a config.HarnessConfigEntry value (not a
			// pointer), so it can never itself be nil here; only its
			// ModelAliases map can be nil/empty, which len() handles safely.
			if len(hcDir.Config.ModelAliases) > 0 {
				aliases = hcDir.Config.ModelAliases
			}
		}
	}
	if len(aliases) == 0 && cfg.Harness != "" {
		aliases = harness.DefaultModelAliases(cfg.Harness)
	}
	if len(aliases) == 0 {
		return
	}

	resolved := config.ResolveModelAlias(cfg.Model, aliases)
	if resolved != cfg.Model {
		util.Debugf("resolveModelAliasForExistingAgent: resolved model alias %q → %q", cfg.Model, resolved)
		cfg.Model = resolved
	}
}

// isWorkspaceEmptyDir returns true if the directory is empty or contains only
// provisioning artifacts (e.g. .scion/, .scion-volumes/).
func isWorkspaceEmptyDir(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return true
	}
	for _, e := range entries {
		switch e.Name() {
		case ".scion", ".scion-volumes":
			continue
		default:
			return false
		}
	}
	return true
}

// infoTemplateFields returns the template name and content hash to record in
// agent-info.json for a template resolved to chain from templateName. When
// the template was loaded from a content-addressed cache directory, the
// directory's content hash is returned as hash and is never used as the
// name; the name is the slug carried by api.ContextWithTemplateName, or
// empty when no slug is known.
func infoTemplateFields(ctx context.Context, templateName string, chain []*config.Template) (name, hash string) {
	name = templateName
	dir := ""
	if len(chain) > 0 {
		last := chain[len(chain)-1]
		name = last.Name
		dir = last.Path
	} else if filepath.IsAbs(templateName) {
		dir = templateName
		name = filepath.Base(templateName)
	}
	if dir != "" && transfer.IsContentHash(filepath.Base(dir)) {
		hash = filepath.Base(dir)
	}
	if transfer.IsContentHash(name) {
		if hash == "" {
			hash = name
		}
		name = ""
	}
	if hash != "" {
		name = ""
		if slug := api.TemplateNameFromContext(ctx); slug != "" && !transfer.IsContentHash(slug) {
			name = slug
		}
	}
	return name, hash
}

// normalizeHydratedTemplateInfo reports whether info records a template
// loaded from a content-addressed cache, and in that case makes sure
// info.Template is a display name, never a content hash. Older versions
// stored the cache directory's hash as the template name; such a value is
// moved to TemplateHash. The name becomes slug when one is known, otherwise
// it is left empty. info is changed in memory only.
func normalizeHydratedTemplateInfo(info *api.AgentInfo, slug string) bool {
	if info == nil {
		return false
	}
	if transfer.IsContentHash(info.Template) {
		if info.TemplateHash == "" {
			info.TemplateHash = info.Template
		}
		info.Template = ""
	}
	if info.TemplateHash == "" {
		return false
	}
	if slug != "" && !transfer.IsContentHash(slug) {
		info.Template = slug
	}
	return true
}

// templateRef names tpl in messages: its name, or its path when it has no
// name (a template in a content-hash cache directory).
func templateRef(tpl *config.Template) string {
	if tpl.Name != "" {
		return tpl.Name
	}
	return tpl.Path
}

// applyBuiltinResourceDefaults applies config.ApplyBuiltinDefaultResources to
// cfg: the built-in CPU limit, raised for a larger CPU request, and a
// kubernetes.resources CPU limit for a larger Kubernetes-only CPU request.
//
// cfg.Kubernetes is replaced with a copy when its resources change, never
// mutated in place: the Kubernetes block may be shared with the template or
// harness config it was resolved from.
func applyBuiltinResourceDefaults(cfg *api.ScionConfig) {
	var k8sRes *api.K8sResources
	if cfg.Kubernetes != nil {
		k8sRes = cfg.Kubernetes.Resources
	}
	resources, newK8sRes := config.ApplyBuiltinDefaultResources(cfg.Resources, k8sRes)
	cfg.Resources = resources
	if newK8sRes != k8sRes {
		kc := *cfg.Kubernetes
		kc.Resources = newK8sRes
		cfg.Kubernetes = &kc
	}
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// worktreeInPlace reports whether path is a directory holding a .git entry,
// the minimum for a registered worktree to be usable. A registration whose
// directory or .git file was removed fails this.
func worktreeInPlace(path string) bool {
	if !dirExists(path) {
		return false
	}
	_, err := os.Lstat(filepath.Join(path, ".git"))
	return err == nil
}
