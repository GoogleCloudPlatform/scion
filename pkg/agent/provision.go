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
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

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

func DeleteAgentFiles(agentName string, projectPath string, removeBranch bool) (bool, error) {
	// Every path built below joins agentName onto some directory -- the
	// project's agents dir, the global agents dir, the external per-agent
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

	var agentsDirs []string
	branchDeleted := false
	var repoRoot string
	var externalAgentDir string
	var worktreeDir string // worktree-per-agent: agent's worktree path
	var globalWorkspaceDir string
	// resolvedProjectDir is config.GetResolvedProjectDir's return value,
	// captured here (rather than only inside the block below) so the
	// sharer-registry calls further down can pass it through as the
	// ProvisionAgent-layout shape check's root — see
	// provision.WorktreePathIsScionCreated's doc comment on why this must be
	// the actual resolved project directory, not derived from repoRoot.
	var resolvedProjectDir string
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

		// Determine repo root for worktree pruning and branch cleanup.
		// For worktree-per-agent the shared base lives at
		// <projectRoot>/workspace where projectRoot is the actual project
		// directory. GetResolvedProjectDir may have appended .scion
		// (e.g. hub-managed projects), so strip that suffix to match the
		// path the workspace backend used during provisioning.
		projectRoot := projectDir
		if filepath.Base(projectDir) == config.DotScion {
			projectRoot = filepath.Dir(projectDir)
		}
		sharedBase := filepath.Join(projectRoot, "workspace")
		// Accept .git as either a directory (normal clone) or a file (gitdir
		// pointer, e.g. if the base is itself a linked worktree/submodule) —
		// existence is enough to identify a valid repo root. (upstream #351 review)
		if _, statErr := os.Stat(filepath.Join(sharedBase, ".git")); statErr == nil {
			repoRoot = sharedBase
			wtPath := filepath.Join(sharedBase, "worktrees", agentName)
			if _, statErr := os.Stat(wtPath); statErr == nil {
				worktreeDir = wtPath
			}
		}

		// Fallback: resolve repo root from projectDir itself. Passing projectDir
		// (not its parent) is robust for both local projects (where projectDir is
		// the repo root) and hub-managed projects (where it is the .scion subdir).
		// MUST match the base used at sharer registration (ProvisionAgent), or the
		// refcount lookup (FindBranchForAgent/UnregisterSharer) would miss.
		if repoRoot == "" {
			if root, err := util.RepoRootDir(projectDir); err == nil {
				repoRoot = root
			}
		}

		// Check for external agent home (git project split storage)
		if extDir, err := config.GetGitProjectExternalAgentsDir(projectDir); err == nil && extDir != "" {
			externalAgentDir = filepath.Join(extDir, agentName)
		}
	}
	// Also check global just in case
	if globalDir, err := config.GetGlobalAgentsDir(); err == nil {
		agentsDirs = append(agentsDirs, globalDir)
	}

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
							_ = util.RemoveAllSafe(wtPath)
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
			}
			refcountHandled = true
		}
	}

	// Worktree-per-agent: remove the agent's worktree from the shared base.
	// The worktree lives at <projectDir>/workspace/worktrees/<agentName>,
	// separate from the agent config dir under agents/.
	// Skip when the refcount path already handled removal/detach.
	if worktreeDir != "" && !refcountHandled {
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
					_ = util.RemoveAllSafe(worktreeDir)
				}
			}
		} else {
			_ = util.RemoveAllSafe(worktreeDir)
		}
	}

	for _, dir := range agentsDirs {
		agentDir := filepath.Join(dir, agentName)
		if _, err := os.Stat(agentDir); err != nil {
			continue
		}

		agentWorkspace := filepath.Join(agentDir, "workspace")
		// Check if it's a worktree before trying to remove it.
		// Skip when the refcount path already handled removal/detach —
		// the shared worktree must not be removed while other sharers remain.
		if !refcountHandled && !emptyPerAgent {
			if _, err := os.Stat(filepath.Join(agentWorkspace, ".git")); err == nil {
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
						_ = util.RemoveAllSafe(agentWorkspace)
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

	// Prune stale worktree records from the repo. This handles cases where the
	// workspace directory was removed (e.g. by os.RemoveAll above, or a previous
	// incomplete cleanup) but the git worktree record was not properly unregistered.
	if repoRoot != "" {
		util.Debugf("delete: pruning stale worktrees in %s", repoRoot)
		pruneStart := time.Now()
		_ = util.PruneWorktreesIn(repoRoot)
		util.Debugf("delete: prune completed in %v", time.Since(pruneStart))

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
				if err := util.RemoveAllSafe(filepath.Join(agentDir, e.Name())); err != nil {
					return branchDeleted, fmt.Errorf("failed to remove agent directory entry: %w", err)
				}
			}
			util.Debugf("delete: preserved shared workspace, removed other entries in: %s", agentDir)
			continue
		}
		util.Debugf("delete: removing directory: %s", agentDir)
		removeStart := time.Now()
		if err := util.RemoveAllSafe(agentDir); err != nil {
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
			util.Debugf("delete: removing external agent state dir: %s", externalAgentDir)
			if err := util.RemoveAllSafe(externalAgentDir); err != nil {
				util.Debugf("delete: standard removal failed, trying podman unshare: %v", err)
				unshareCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if unshareErr := exec.CommandContext(unshareCtx, "podman", "unshare", "rm", "-rf", externalAgentDir).Run(); unshareErr != nil {
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
	if opts.SharedWorkspace {
		ctx = api.ContextWithSharedWorkspace(ctx)
	}
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
		cfgData, marshalErr := json.MarshalIndent(cfg, "", "  ")
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
func (m *AgentManager) containerIsRunning(ctx context.Context, name string) (bool, error) {
	agents, err := m.Runtime.List(ctx, map[string]string{"scion.name": name})
	if err != nil {
		return false, err
	}
	for _, a := range agents {
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
// ProvisionAgent's worktree-creation branch decides whether to create a
// worktree using CWD-dependent git checks (util.BranchExists), which are
// always wrong on a broker (its CWD is outside the repo) — so calling it for
// a clone-per-agent agent without first confirming a real, existing clone
// risks os.RemoveAll on a live worktree-per-agent workspace, destroying
// uncommitted work. Likewise, ProvisionAgent unconditionally creates the
// agent directory (os.MkdirAll) before it even looks at the workspace mode,
// so the explicit-mount case needs its own existence checks to avoid
// silently standing up a fresh, empty agent directory for a name that was
// never actually provisioned. Phase 4 may replace the clone-per-agent
// refusal with a proper "workspace exists → reuse as-is" branch that does
// not depend on CWD.
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
	// single after-the-fact derivation that works for both modes.
	var agentDir string

	if hasGitClone {
		agentDir = config.GetAgentDir(projectDir, opts.Name, opts.SharedWorkspace)
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
		// existing agent (CheckAgentDirContained both resolves the
		// containment-checked path and, via the os.Stat below, its
		// existence) and that the workspace path — absolute, or a
		// project-relative subdir exactly as ProvisionAgent's own
		// explicit-workspace branch accepts — already exists. A miss here is
		// refused as ErrReprovisionRefused (409) rather than left to
		// ProvisionAgent's untyped error (or, worse, ProvisionAgent silently
		// creating the missing piece).
		dir, err := CheckAgentDirContained(projectDir, opts.Name, opts.SharedWorkspace)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrReprovisionRefused, err)
		}
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
			settings, _, err := config.LoadEffectiveSettings(projectDir)
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
	if running, err := m.containerIsRunning(ctx, opts.Name); err != nil {
		util.Debugf("reprovision: failed to check running state for %s, proceeding: %v", opts.Name, err)
	} else if running {
		return nil, fmt.Errorf("%w: agent %q container is still running; stop it first", ErrReprovisionRefused, opts.Name)
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

	// Deliberately no prompt.md write here: the new generation's first task
	// (the hub-built preamble plus handoff) is delivered by the subsequent
	// DispatchAgentStart call, not pre-staged as a file.
	return cfg, nil
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
	ctx, inlineCfg := buildProvisionContext(ctx, opts)
	agentDir, agentHome, _, cfg, err := GetAgent(ctx, opts.Name, opts.Template, opts.Image, opts.HarnessConfig, opts.ProjectPath, opts.Profile, "created", opts.Branch, opts.Workspace, inlineCfg)
	if err != nil {
		return cfg, err
	}

	if err := m.finishProvision(opts, agentDir, agentHome, cfg); err != nil {
		return cfg, err
	}

	// A provision-only create carries no run (the hub mints runs only for
	// starts), yet it may reuse, or newly provision, files under a name an
	// earlier same-named agent's late delete still targets by that agent's
	// run. Record a provision owner so such a delete leaves these files
	// alone (ptone/scion#2675). The agent's own delete names no run until
	// it is started, and its first start records its run in place of this.
	// If a runtime entry for the name still exists in this project (or the
	// runtime cannot be listed), the files stay that entry's run's.
	if opts.FreshProvision && opts.RunID == "" && !m.hasRuntimeEntry(ctx, opts) {
		if err := SetSavedRunID(opts.Name, opts.ProjectPath, ProvisionOwnerPrefix+uuid.NewString()); err != nil {
			slog.Warn("Provision: failed to record the provision owner in agent-info.json",
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

	return cfg, nil
}

// resolveHarnessConfigDir returns the harness-config directory for an agent,
// preferring a Hub-hydrated path recorded on the context (§7.3 step 4) over the
// on-disk FindHarnessConfigDir search. This lets a broker that lacks the
// harness-config locally use the copy fetched from the Hub's storage backend.
func resolveHarnessConfigDir(ctx context.Context, name, projectPath string, templatePaths ...string) (*config.HarnessConfigDir, error) {
	if hcPath := api.HarnessConfigPathFromContext(ctx); hcPath != "" {
		return config.LoadHarnessConfigDir(hcPath)
	}
	return config.FindHarnessConfigDir(name, projectPath, templatePaths...)
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
func resolveTemplateAndHarnessConfig(ctx context.Context, templateName, harnessConfig, projectPath, profileName string, settings *config.VersionedSettings, inlineCfg *api.ScionConfig) (*resolvedTemplate, error) {
	chain, err := config.GetTemplateChainInProject(templateName, projectPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load template: %w", err)
	}

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

		finalScionCfg = config.MergeScionConfig(finalScionCfg, tplCfg)
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
	hcDir, err := resolveHarnessConfigDir(ctx, harnessConfigName, projectPath, templatePaths...)
	if err != nil {
		return nil, fmt.Errorf("failed to find harness-config %q: %w", harnessConfigName, err)
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
	ctx, inlineCfg := buildProvisionContext(ctx, opts)

	projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath)
	if err != nil {
		return err
	}

	settings, warnings, _ := config.LoadEffectiveSettings(projectDir)
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

	_, err = resolveTemplateAndHarnessConfig(ctx, templateName, opts.HarnessConfig, opts.ProjectPath, profileName, settings, inlineCfg)
	return err
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
	agentDir := config.GetAgentDir(projectDir, agentName, sharedWorkspace)
	agentsRoot := filepath.Clean(config.SelectAgentsRoot(projectDir, sharedWorkspace))
	cleanAgentDir := filepath.Clean(agentDir)
	// Both conditions matter: Dir(...) != root catches a name that is
	// outside the root entirely (e.g. "../sibling"); Base(...) != agentName
	// catches a name that cleans down to a direct child of the root but
	// isn't the single path element it claims to be (e.g. "x/../y" cleans
	// to <root>/y, a direct child, even though agentName itself is not
	// "y"). The broker's isSingleCleanPathElement enforces the same
	// single-element rule at the request boundary; this enforces it again
	// here, independently, for every caller.
	if filepath.Dir(cleanAgentDir) != agentsRoot || filepath.Base(cleanAgentDir) != agentName {
		return "", fmt.Errorf("agent %q is not a single path element under %s", agentName, agentsRoot)
	}
	return agentDir, nil
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

func ProvisionAgent(ctx context.Context, agentName string, templateName string, agentImage string, harnessConfig string, projectPath string, profileName string, optionalStatus string, branch string, workspace string, inlineConfig ...*api.ScionConfig) (string, string, *api.ScionConfig, error) {
	provisionStart := time.Now()
	// 1. Prepare agent directories
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return "", "", nil, err
	}

	settings, warnings, _ := config.LoadEffectiveSettings(projectDir)
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
	sharedWorkspace := api.IsSharedWorkspaceFromContext(ctx)
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
	agentDir, err := checkAgentDirContained(projectDir, agentName, sharedWorkspace)
	if err != nil {
		return "", "", nil, err
	}
	agentHome := config.GetAgentHomePath(projectDir, agentName)
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

	_, agentDirStatErr := os.Lstat(agentDir)
	newAgentDir := errors.Is(agentDirStatErr, fs.ErrNotExist)
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
				_ = util.MakeWritableRecursive(agentWorkspace)
				if err := os.RemoveAll(agentWorkspace); err != nil {
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
		targetBranch := branch
		if targetBranch == "" {
			// Use slugified agent name for valid git branch names
			targetBranch = api.Slugify(agentName)
		}

		// Check if we should use an existing worktree
		usedExistingWorktree := false
		if util.BranchExists(targetBranch) {
			if existingPath, err := util.FindWorktreeByBranch(targetBranch); err == nil && existingPath != "" {
				workspaceSource = existingPath
				agentWorkspace = "" // Using external worktree
				usedExistingWorktree = true
				fmt.Printf("Warning: Relying on existing worktree for branch '%s' at '%s'\n", targetBranch, existingPath)
				// Register as sharer for refcounted teardown (I3). Fail loudly:
				// an untracked agent breaks the refcount (premature/leaked removal).
				root, rootErr := util.RepoRootDir(projectDir)
				if rootErr != nil {
					return "", "", nil, fmt.Errorf("resolve repo root for sharer registration: %w", rootErr)
				}
				if regErr := provision.RegisterSharer(root, projectDir, targetBranch, existingPath, agentName); regErr != nil {
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
			workspaceSource = filepath.Dir(projectDir)
		}
	}

	// Worktree Creation (if needed)
	if shouldCreateWorktree {
		worktreeStart := time.Now()
		// Remove existing workspace dir if it exists to allow worktree add
		_ = util.MakeWritableRecursive(agentWorkspace)
		_ = os.RemoveAll(agentWorkspace)
		// Prune worktrees to clean up any stale entries.
		// Use repo-root-aware prune so it works when the process CWD is
		// outside the repository (e.g. runtime broker).
		if root, err := util.RepoRootDir(filepath.Dir(agentWorkspace)); err == nil {
			_ = util.PruneWorktreesIn(root)
		} else {
			_ = util.PruneWorktrees()
		}

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
	// harness-config, through the same resolution Preflight uses.
	rt, err := resolveTemplateAndHarnessConfig(ctx, templateName, harnessConfig, projectPath, profileName, settings, inlineCfg)
	if err != nil {
		return "", "", nil, err
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
		ProjectPath:   projectPath,
		TemplatePaths: templatePaths,
		ProfileName:   profileName,
		Settings:      settings,
		ConfigDirPath: api.HarnessConfigPathFromContext(ctx),
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to resolve harness for %q: %w", harnessConfigName, err)
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
	hubEnabled := (settings != nil && settings.IsHubEnabled()) || api.IsBrokerModeFromContext(ctx)
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
					return "", "", nil, &SkillResolutionError{URI: re.URI, Code: re.Code, Message: re.Message, RetryAfter: re.RetryAfter}
				}
				util.Debugf("provision: optional skill %q skipped: %s", re.URI, re.Message)
			}

			// S1: verify every requested URI has an outcome
			for uri, ref := range requestedURIs {
				if !resolvedURIs[uri] && !errorURIs[uri] {
					if ref.Optional {
						util.Debugf("provision: optional skill %q missing from resolver response, skipping", uri)
					} else {
						return "", "", nil, fmt.Errorf(
							"required skill %q missing from resolver response — S1 fail-closed", uri)
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
				record, err := installResolvedSkills(ctx, result.Resolved, skillsDest, agentHome)
				if err != nil {
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
		recordData, _ := json.MarshalIndent(resolvedSkillsRecord, "", "  ")
		inputPath := filepath.Join(agentHome, ".scion", "harness", "inputs", "resolved-skills.json")
		if info, err := os.Stat(filepath.Dir(inputPath)); err == nil && info.IsDir() {
			_ = os.WriteFile(inputPath, recordData, 0644)
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

	// 2e. Merge settings env, auth, and resources if available
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
			if settings.Telemetry != nil {
				settingsCfg.Telemetry = config.ConvertV1TelemetryToAPI(settings.Telemetry)
			}
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

		// Merge profile-level resources (lower priority than template/agent-level resources).
		effectiveProfile := profileName
		if effectiveProfile == "" {
			effectiveProfile = settings.ActiveProfile
		}
		if p, ok := settings.Profiles[effectiveProfile]; ok && p.Resources != nil {
			if finalScionCfg.Resources == nil {
				cpy := *p.Resources
				finalScionCfg.Resources = &cpy
			}
			merged := config.MergeResourceSpec(p.Resources, finalScionCfg.Resources)
			finalScionCfg.Resources = merged
		}

		// Merge harness-override resources on top of everything.
		if p, ok := settings.Profiles[effectiveProfile]; ok && p.HarnessOverrides != nil {
			if ho, ok := p.HarnessOverrides[harnessConfigName]; ok && ho.Resources != nil {
				finalScionCfg.Resources = config.MergeResourceSpec(finalScionCfg.Resources, ho.Resources)
			}
		}
	}

	// Hub operational agent_defaults: below the template and below inline
	// config (both of which have already populated finalScionCfg by the merge
	// at step 2b), above this broker's own settings.yaml defaults (applied
	// immediately below). Same only-if-zero shape as that block, so the two
	// tiers compose without either needing to know about the other.
	//
	// The placement IS the design. Move this after the settings block and the
	// hub tier loses to settings.yaml; stamp these values hub-side into
	// InlineConfig instead and they arrive as top-of-chain and beat a
	// template's explicit max_turns — the inversion this channel exists to
	// avoid (design §3.2.1, rejected alternative A5).
	//
	// The hub sends nothing in file mode, so this never fires there and
	// file-mode behaviour is unchanged: a co-located broker reads the same
	// settings.yaml and applies these values itself at the BOTTOM tier below.
	// The rank of hub agent_defaults is therefore mode-dependent — bottom of
	// the broker chain in file mode, just above broker settings in Postgres
	// mode. That is deliberate; see design §3.2.4 and alternative A7.
	//
	// Note the asymmetry for Resources: "above this broker's own settings.yaml
	// defaults" means default_resources ONLY. Broker profile resources and
	// harness overrides live in that same file but are merged in step 2e ABOVE,
	// so they win per-field over the hub default_resources applied here — while
	// broker default_max_turns loses to hub default_max_turns. The hub tier
	// therefore sits in a different place for Resources than for the three
	// scalars. That falls out of the insertion point the design specifies and is
	// the conservative direction (§3.2.4 explicitly does not want hub defaults
	// silently overriding broker profile resources).
	if hd := api.HubAgentDefaultsFromContext(ctx); hd != nil && finalScionCfg != nil {
		if finalScionCfg.MaxTurns == 0 && hd.MaxTurns > 0 {
			finalScionCfg.MaxTurns = hd.MaxTurns
		}
		if finalScionCfg.MaxModelCalls == 0 && hd.MaxModelCalls > 0 {
			finalScionCfg.MaxModelCalls = hd.MaxModelCalls
		}
		if finalScionCfg.MaxDuration == "" && hd.MaxDuration != "" {
			finalScionCfg.MaxDuration = hd.MaxDuration
		}
		if hd.Resources != nil {
			// Hub defaults in BASE position: per-field merge with the
			// agent/template value winning any field it sets. MergeResourceSpec
			// returns base itself when override is nil, so copy first rather
			// than aliasing the context's spec into the persisted config.
			base := *hd.Resources
			finalScionCfg.Resources = config.MergeResourceSpec(&base, finalScionCfg.Resources)
		}
	}

	// Apply default limits from settings (hub global defaults) if not already set
	// by template or inline config. Priority: agent > template > settings defaults.
	if settings != nil && finalScionCfg != nil {
		if finalScionCfg.MaxTurns == 0 && settings.DefaultMaxTurns > 0 {
			finalScionCfg.MaxTurns = settings.DefaultMaxTurns
		}
		if finalScionCfg.MaxModelCalls == 0 && settings.DefaultMaxModelCalls > 0 {
			finalScionCfg.MaxModelCalls = settings.DefaultMaxModelCalls
		}
		if finalScionCfg.MaxDuration == "" && settings.DefaultMaxDuration != "" {
			finalScionCfg.MaxDuration = settings.DefaultMaxDuration
		}
		if settings.DefaultResources != nil {
			if finalScionCfg.Resources == nil {
				cpy := *settings.DefaultResources
				finalScionCfg.Resources = &cpy
			} else {
				// Merge: settings defaults are lower priority, so use them as base
				finalScionCfg.Resources = config.MergeResourceSpec(settings.DefaultResources, finalScionCfg.Resources)
			}
		}
	}

	// Apply built-in resource defaults as the LOWEST-priority tier, below the
	// settings defaults above. Docker and Podman emit cgroup flags only for
	// non-empty fields, so an agent that reaches this point with no CPU limit
	// runs completely unconstrained and can saturate every core on the host.
	//
	// Only the CPU limit is filled in, and only when nothing else supplied one;
	// MergeResourceSpec keeps every field that a higher tier already set, so an
	// explicit memory-only configuration still gains a CPU limit here.
	// Gated by runtime.enforce_resource_defaults (default true) so an operator
	// can restore the previous unlimited behaviour without a rollback.
	//
	// ApplyBuiltinDefaultResources also keeps the built-in limit from
	// conflicting with a larger CPU request (ptone/scion#3407): a larger
	// resources.requests.cpu raises the built-in limit, and a larger
	// kubernetes.resources.requests.cpu sets kubernetes.resources.limits.cpu,
	// which only the Kubernetes runtime reads.
	if finalScionCfg != nil && config.ShouldEnforceResourceDefaults(settings) {
		applyBuiltinResourceDefaults(finalScionCfg)
	}

	// Mount the resolved workspace if an external source was determined
	if workspaceSource != "" {
		finalScionCfg.Volumes = append(finalScionCfg.Volumes, api.VolumeMount{
			Source:   workspaceSource,
			Target:   "/workspace",
			ReadOnly: false,
		})
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
		Profile:               profileName,
	}
	if optionalStatus != "" {
		info.Phase = optionalStatus
	} else {
		info.Phase = "created"
	}
	if agentImage != "" {
		info.Image = agentImage
	}
	if explicitImage != "" {
		info.ExplicitImage = explicitImage
	}
	if explicitPullPolicy != "" {
		info.ExplicitImagePullPolicy = explicitPullPolicy
	}
	// Record the run that owns these files (ptone/scion#2675): a delete
	// naming a different run then leaves them alone. A provision with no run
	// (provision-only, reprovision) keeps whatever run already owned them,
	// since the files still belong to that run's runtime entry.
	info.RunID = api.RunIDFromContext(ctx)
	if info.RunID == "" {
		info.RunID = readAgentInfoRunID(filepath.Join(agentHome, "agent-info.json"))
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

	agentCfgData, err := json.MarshalIndent(finalScionCfg, "", "  ")
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to marshal agent config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), agentCfgData, 0644); err != nil {
		return "", "", nil, fmt.Errorf("failed to write agent config: %w", err)
	}

	// Now attach Info to the config object for return and for writing agent-info.json
	finalScionCfg.Info = info

	// Write agent-info.json to home for container access
	if finalScionCfg.Info != nil {
		infoData, err := json.MarshalIndent(finalScionCfg.Info, "", "  ")
		if err == nil {
			_ = os.WriteFile(filepath.Join(agentHome, "agent-info.json"), infoData, 0644)
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

	// Reload config to get harness updates (e.g. Env vars injected by harness)
	reloadTpl := &config.Template{Path: agentDir}
	if updatedCfg, err := reloadTpl.LoadConfig(); err == nil {
		updatedCfg.Info = finalScionCfg.Info // Re-attach info
		finalScionCfg = updatedCfg
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

func getSavedAgentInfo(agentName string, projectPath string) *api.AgentInfo {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return nil
	}
	agentInfoPath := filepath.Join(config.GetAgentHomePath(projectDir, agentName), "agent-info.json")
	if _, err := os.Stat(agentInfoPath); err != nil {
		return nil
	}
	data, err := os.ReadFile(agentInfoPath)
	if err != nil {
		return nil
	}
	var info api.AgentInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil
	}
	return &info
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
	entries, err := m.Runtime.List(ctx, map[string]string{"scion.name": api.Slugify(opts.Name)})
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
// in agent-info.json runId in place of a run (see Provision). It never
// equals a run ID the hub sends.
const ProvisionOwnerPrefix = "provision-"

// readAgentInfoRunID returns the runId recorded in the agent-info.json at
// path, or "" if the file is missing, unreadable or carries none.
func readAgentInfoRunID(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var info struct {
		RunID string `json:"runId"`
	}
	if json.Unmarshal(data, &info) != nil {
		return ""
	}
	return info.RunID
}

// GetSavedRunID returns the run ID recorded in the agent's agent-info.json:
// the run that owns the agent's files (ptone/scion#2675). It is "" for an
// agent provisioned before run IDs were recorded, one provisioned but never
// started, or one whose agent-info.json is missing or unreadable.
func GetSavedRunID(agentName string, projectPath string) string {
	if info := getSavedAgentInfo(agentName, projectPath); info != nil {
		return info.RunID
	}
	return ""
}

// SetSavedRunID records runID in the agent's agent-info.json as the run that
// owns the agent's files (ptone/scion#2675). It is a no-op when
// agent-info.json does not exist yet.
func SetSavedRunID(agentName string, projectPath string, runID string) error {
	return updateSavedAgentInfo(agentName, projectPath, func(info *api.AgentInfo) {
		info.RunID = runID
	})
}

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

	newData, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}

	return writeAgentInfoFile(agentInfoPath, newData, mode)
}

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
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
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

// AgentDeleteState is the part of agent-info.json a soft delete marks
// (Phase and DeletedAt), captured so the mark can be undone.
type AgentDeleteState struct {
	Phase     string
	DeletedAt time.Time
}

// GetAgentDeleteState reads the Phase and DeletedAt of agent-info.json.
// ok is false when the file cannot be read.
func GetAgentDeleteState(agentName string, projectPath string) (AgentDeleteState, bool) {
	info := getSavedAgentInfo(agentName, projectPath)
	if info == nil {
		return AgentDeleteState{}, false
	}
	return AgentDeleteState{Phase: info.Phase, DeletedAt: info.DeletedAt}, true
}

// RestoreAgentDeleteState writes st's Phase and DeletedAt back to
// agent-info.json, undoing a soft-delete mark. It changes nothing unless the
// file still shows the mark (Phase "deleted"), so a phase a newer start has
// written since the snapshot is kept.
func RestoreAgentDeleteState(agentName string, projectPath string, st AgentDeleteState) error {
	return updateSavedAgentInfo(agentName, projectPath, func(info *api.AgentInfo) {
		if info.Phase != "deleted" {
			return
		}
		info.Phase = st.Phase
		info.DeletedAt = st.DeletedAt
	})
}

// UpdateAgentDeletedAt writes the deletedAt timestamp to agent-info.json.
func UpdateAgentDeletedAt(agentName string, projectPath string, deletedAt time.Time) error {
	return updateSavedAgentInfo(agentName, projectPath, func(info *api.AgentInfo) {
		info.DeletedAt = deletedAt
	})
}

func GetAgent(ctx context.Context, agentName string, templateName string, agentImage string, harnessConfig string, projectPath string, profileName string, optionalStatus string, branch string, workspace string, inlineConfig ...*api.ScionConfig) (string, string, string, *api.ScionConfig, error) {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return "", "", "", nil, err
	}

	util.Debugf("GetAgent: agentName=%s templateName=%q harnessConfig=%q projectPath=%q projectDir=%s",
		agentName, templateName, harnessConfig, projectPath, projectDir)

	sharedWorkspace := api.IsSharedWorkspaceFromContext(ctx)
	agentDir, err := checkAgentDirContained(projectDir, agentName, sharedWorkspace)
	if err != nil {
		return "", "", "", nil, err
	}

	agentHome := config.GetAgentHomePath(projectDir, agentName)
	var agentWorkspace string
	if !sharedWorkspace {
		agentWorkspace = filepath.Join(agentDir, "workspace")
	}

	// Check for stale/incomplete agent directory (dir exists but no config file).
	// This can happen when a previous provisioning attempt created the directory
	// but failed before writing scion-agent.json. Remove it so we re-provision.
	if _, err := os.Stat(agentDir); err == nil {
		if configPath := config.GetScionAgentConfigPath(agentDir); configPath == "" {
			util.Debugf("GetAgent: stale agent directory detected (no config file), removing: %s", agentDir)
			// chmod to ensure we can remove root-owned files left by containers.
			_ = filepath.WalkDir(agentDir, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				_ = os.Chmod(path, 0755)
				return nil
			})
			_ = os.RemoveAll(agentDir)
			// Prune worktrees so git forgets any worktree that pointed into the
			// now-deleted directory, allowing ProvisionAgent to recreate it cleanly.
			if root, rootErr := util.RepoRootDir(filepath.Dir(agentWorkspace)); rootErr == nil {
				_ = util.PruneWorktreesIn(root)
			}
		}
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
				if root, rootErr := util.RepoRootDir(filepath.Dir(agentWorkspace)); rootErr == nil {
					_ = util.PruneWorktreesIn(root)
				}
				if err := util.CreateWorktree(agentWorkspace, targetBranch); err != nil {
					util.Debugf("GetAgent: failed to recreate worktree at %s: %v, clearing workspace", agentWorkspace, err)
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
	vs, vsWarnings, err := config.LoadEffectiveSettings(projectDir)
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
				_ = util.MakeWritableRecursive(agentWorkspace)
				if err := os.RemoveAll(agentWorkspace); err != nil {
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
		resolveModelAliasForExistingAgent(ctx, agentCfg, projectPath)
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

	mergedCfg := &api.ScionConfig{}
	for _, tpl := range chain {
		tplCfg, err := tpl.LoadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to load config from template %s, skipping: %v\n", templateRef(tpl), err)
			continue
		}
		mergedCfg = config.MergeScionConfig(mergedCfg, tplCfg)
	}

	finalCfg := config.MergeScionConfig(mergedCfg, agentCfg)

	// Resolve model size aliases for existing agents.
	// Mirrors the alias resolution in ProvisionAgent (line 778-785).
	// This covers the case where scion-agent.json was written with a raw alias
	// (e.g. by applyInlineConfigUpdate before the hub-side fix) or where the
	// agent was created before the hub resolved aliases at storage time.
	resolveModelAliasForExistingAgent(ctx, finalCfg, projectPath)

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
func resolveModelAliasForExistingAgent(ctx context.Context, cfg *api.ScionConfig, projectPath string) {
	if cfg == nil || cfg.Model == "" {
		return
	}

	aliases := map[string]string{}
	hcName := cfg.HarnessConfig
	if hcName == "" {
		hcName = cfg.DefaultHarnessConfig
	}
	if hcName != "" {
		if hcDir, err := resolveHarnessConfigDir(ctx, hcName, projectPath); err == nil && hcDir != nil {
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
