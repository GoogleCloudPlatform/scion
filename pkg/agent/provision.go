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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

func DeleteAgentFiles(agentName string, projectPath string, removeBranch bool) (bool, error) {
	var agentsDirs []string
	branchDeleted := false
	var repoRoot string
	var externalAgentDir string
	if projectDir, err := config.GetResolvedProjectDir(projectPath); err == nil {
		agentsDirs = append(agentsDirs, filepath.Join(projectDir, "agents"))
		// Determine repo root for worktree pruning and branch cleanup
		if root, err := util.RepoRootDir(filepath.Dir(projectDir)); err == nil {
			repoRoot = root
		}
		// Check for external agent home (git grove split storage)
		if extDir, err := config.GetGitProjectExternalAgentsDir(projectDir); err == nil && extDir != "" {
			externalAgentDir = filepath.Join(extDir, agentName)
		}
	}
	// Also check global just in case
	if globalDir, err := config.GetGlobalAgentsDir(); err == nil {
		agentsDirs = append(agentsDirs, globalDir)
	}

	// Phase 1: synchronous git operations (worktree removal, pruning, branch cleanup).
	// No background deletions happen here to avoid triggering macOS autofs
	// in a goroutine that could block git subprocess I/O system-wide.
	var dirsToDelete []string

	for _, dir := range agentsDirs {
		agentDir := filepath.Join(dir, agentName)
		if _, err := os.Stat(agentDir); err != nil {
			continue
		}

		agentWorkspace := filepath.Join(agentDir, "workspace")
		// Check if it's a worktree before trying to remove it
		if _, err := os.Stat(filepath.Join(agentWorkspace, ".git")); err == nil {
			util.Debugf("delete: removing workspace at %s", agentWorkspace)
			worktreeStart := time.Now()
			if deleted, err := util.RemoveWorktree(agentWorkspace, removeBranch); err == nil {
				if deleted {
					branchDeleted = true
				}
				util.Debugf("delete: worktree removal completed in %v (branch deleted: %v)", time.Since(worktreeStart), deleted)
			} else {
				util.Debugf("delete: worktree removal failed in %v: %v", time.Since(worktreeStart), err)
				// Ensure the workspace directory is gone even if worktree
				// removal only partially succeeded, so that PruneWorktreesIn
				// can detect the stale .git/worktrees entry.
				_ = util.RemoveAllSafe(agentWorkspace)
			}
		}

		dirsToDelete = append(dirsToDelete, agentDir)
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
		if removeBranch && !branchDeleted {
			branchName := api.Slugify(agentName)
			if util.DeleteBranchIn(repoRoot, branchName) {
				branchDeleted = true
				util.Debugf("delete: deleted branch %s via fallback", branchName)
			}
		}
	}

	// Phase 2: directory removal.
	for _, agentDir := range dirsToDelete {
		util.Debugf("delete: removing directory: %s", agentDir)
		removeStart := time.Now()
		if err := util.RemoveAllSafe(agentDir); err != nil {
			util.Debugf("delete: removal failed in %v: %v", time.Since(removeStart), err)
			return branchDeleted, fmt.Errorf("failed to remove agent directory: %w", err)
		}
		util.Debugf("delete: removal completed in %v", time.Since(removeStart))
	}

	// Phase 3: remove the external per-agent state directory (git grove split
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

// migrateLegacyAgentState moves prompt.md and scion-agent.json from the
// legacy in-grove location to the external (shared-workspace) location for
// agents provisioned before per-agent state was relocated. The legacy
// directory is removed if it ends up empty (it shouldn't contain anything
// else for shared-workspace agents — there is no per-agent worktree).
//
// Best-effort: errors are logged but do not abort provisioning. A miss here
// only means the in-grove copy lingers (still readable by siblings until the
// agent re-provisions); it does not corrupt the new location.
func migrateLegacyAgentState(legacyDir, externalDir string) {
	moveFile := func(name string) {
		legacyPath := filepath.Join(legacyDir, name)
		if _, err := os.Stat(legacyPath); err != nil {
			return
		}
		externalPath := filepath.Join(externalDir, name)
		if _, err := os.Stat(externalPath); err == nil {
			// External already populated — discard the in-grove residue.
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
	// Remove the legacy dir if empty (best effort; non-empty leftovers like a
	// stale workspace/ shell are left in place to avoid surprising deletes).
	_ = os.Remove(legacyDir)
}

// StopProjectContainers finds and removes containers belonging to the given project
// that match the provided agent names. This is used during project pruning to
// clean up containers before removing the project config directory.
func StopProjectContainers(ctx context.Context, mgr Manager, projectName string, agentNames []string) []string {
	containers, err := mgr.List(ctx, map[string]string{
		"scion.agent": "true",
		"scion.grove": projectName,
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
		if _, err := mgr.Delete(ctx, c.ContainerID, false, "", false); err != nil {
			util.Debugf("StopProjectContainers: failed to remove container for agent %s: %v", agentName, err)
		} else {
			stopped = append(stopped, agentName)
		}
	}
	return stopped
}

func (m *AgentManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	if opts.BrokerMode {
		ctx = api.ContextWithBrokerMode(ctx)
	}
	if opts.GitClone != nil {
		ctx = api.ContextWithGitClone(ctx, opts.GitClone)
	}
	if opts.SharedWorkspace {
		ctx = api.ContextWithSharedWorkspace(ctx)
	}
	// Inject harness auth override into inline config so it is applied
	// before harness Provision() runs (which reads auth_selectedType to
	// decide which env vars to inject into scion-agent.json).
	inlineCfg := opts.InlineConfig
	if opts.HarnessAuth != "" {
		if inlineCfg == nil {
			inlineCfg = &api.ScionConfig{}
		}
		inlineCfg.AuthSelectedType = opts.HarnessAuth
	}
	agentDir, _, _, cfg, err := GetAgent(ctx, opts.Name, opts.Template, opts.Image, opts.HarnessConfig, opts.ProjectPath, opts.Profile, "created", opts.Branch, opts.Workspace, inlineCfg)
	if err == nil {
		_ = UpdateAgentConfig(opts.Name, opts.ProjectPath, "created", m.Runtime.Name(), opts.Profile)
	}
	if err != nil {
		return cfg, err
	}

	// Persist harness auth override to the on-disk config (for sciontool).
	// The auth type was already applied via inlineConfig above, but we
	// re-write to ensure the final file reflects the override.
	if opts.HarnessAuth != "" && cfg != nil {
		cfg.AuthSelectedType = opts.HarnessAuth
		cfgData, marshalErr := json.MarshalIndent(cfg, "", "  ")
		if marshalErr == nil {
			_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), cfgData, 0644)
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

func ProvisionAgent(ctx context.Context, agentName string, templateName string, agentImage string, harnessConfig string, projectPath string, profileName string, optionalStatus string, branch string, workspace string, inlineConfig ...*api.ScionConfig) (string, string, *api.ScionConfig, error) {
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
	isGit := util.IsGitRepoDir(projectDir)
	if isGit && os.Getenv("SCION_HOST_UID") != "" {
		// Inside an agent container: treat as non-git to prevent worktree
		// creation. Container worktrees produce path-identity mismatches
		// because --relative-paths are computed against the container mount
		// layout, not the host filesystem.
		isGit = false
	}

	// Verify .gitignore if in a repo
	if isGit {
		// Find the projectDir relative to repo root if possible
		root, err := util.RepoRootDir(projectDir)
		if err == nil {
			rel, err := filepath.Rel(root, projectDir)
			if err == nil && !strings.HasPrefix(rel, "..") {
				agentsPath := filepath.Join(rel, "agents")
				if !util.IsIgnored(root, agentsPath+"/") {
					return "", "", nil, fmt.Errorf("security error: '%s/' must be in .gitignore when using a project-local grove", agentsPath)
				}
				// Note: .scion/agents/ is the security-critical path (checked above).
				// .scion/ itself is intentionally NOT fully gitignored so that
				// templates/ and other config can be committed.
			}
		}
	}
	sharedWorkspace := api.IsSharedWorkspaceFromContext(ctx)
	agentDir := config.GetAgentDir(projectDir, agentName, sharedWorkspace)
	agentHome := config.GetAgentHomePath(projectDir, agentName)
	// In worktree mode the workspace lives under agentDir so git's relative
	// worktree pointers resolve correctly. In shared-workspace mode there is
	// no per-agent workspace dir — the grove-wide checkout is mounted directly.
	var agentWorkspace string
	if !sharedWorkspace {
		agentWorkspace = filepath.Join(agentDir, "workspace")
	}

	// Migrate any pre-existing in-grove state for shared-workspace agents to
	// the external location so siblings stop seeing it via /workspace. This
	// covers agents provisioned before the shared-workspace isolation change.
	if sharedWorkspace {
		legacyDir := filepath.Join(projectDir, "agents", agentName)
		if legacyDir != agentDir {
			migrateLegacyAgentState(legacyDir, agentDir)
		}
	}

	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return "", "", nil, fmt.Errorf("failed to create agent directory: %w", err)
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

	// Check for git clone mode from context
	gitClone := api.GitCloneFromContext(ctx)

	// Workspace Resolution Logic
	if gitClone != nil {
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
		absWorkspace, err := filepath.Abs(workspace)
		if err != nil {
			return "", "", nil, fmt.Errorf("failed to resolve absolute path for workspace %s: %w", workspace, err)
		}

		if _, err := os.Stat(absWorkspace); os.IsNotExist(err) {
			return "", "", nil, fmt.Errorf("workspace path does not exist: %s", absWorkspace)
		}

		workspaceSource = absWorkspace
		agentWorkspace = "" // We are not using the managed local workspace directory

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
			}
		}

		if !usedExistingWorktree {
			shouldCreateWorktree = true
			// agentWorkspace remains set to agents/<name>/workspace
		}

	} else {
		// Case 3: Non-Git Repository (and no explicit workspace)
		if projectName == "global" {
			workspaceSource, _ = os.Getwd()
		} else if settings != nil && settings.WorkspacePath != "" {
			// Externalized grove: use workspace-path from settings
			workspaceSource = settings.WorkspacePath
		} else {
			workspaceSource = filepath.Dir(projectDir)
		}
		agentWorkspace = "" // Using external mount
	}

	// Worktree Creation (if needed)
	if shouldCreateWorktree {
		// Remove existing workspace dir if it exists to allow worktree add
		_ = util.MakeWritableRecursive(agentWorkspace)
		os.RemoveAll(agentWorkspace)
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

		// Write a .scion grove marker into the worktree so in-container CLI
		// can discover the grove context. Worktrees don't contain .scion
		// (it's gitignored), so without this marker the CLI would report
		// "not in a scion project" inside the container.
		if projectID, err := config.ReadProjectID(projectDir); err == nil && projectID != "" {
			groveSlug := api.Slugify(projectName)
			if writeErr := config.WriteWorkspaceMarker(agentWorkspace, projectID, projectName, groveSlug); writeErr != nil {
				util.Debugf("provision: failed to write workspace marker: %v", writeErr)
			}
		}
	}

	// 2. Load templates and merge configs (no home copy yet)
	chain, err := config.GetTemplateChainInProject(templateName, projectPath)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to load template: %w", err)
	}

	finalScionCfg := &api.ScionConfig{}

	for _, tpl := range chain {
		// Load scion-agent config from this template and merge it
		tplCfg, err := tpl.LoadConfig()
		if err != nil {
			return "", "", nil, fmt.Errorf("failed to load config from template %s: %w", tpl.Name, err)
		}

		// Validate: reject legacy templates that still have a 'harness' field
		if err := config.ValidateAgnosticTemplate(tplCfg); err != nil {
			return "", "", nil, fmt.Errorf("template %s: %w", tpl.Name, err)
		}

		finalScionCfg = config.MergeScionConfig(finalScionCfg, tplCfg)
	}

	// 2a-inline. Merge inline config over template config (if provided)
	var inlineCfg *api.ScionConfig
	if len(inlineConfig) > 0 && inlineConfig[0] != nil {
		inlineCfg = inlineConfig[0]
		finalScionCfg = config.MergeScionConfig(finalScionCfg, inlineCfg)
	}

	// 2b. Resolve harness-config name (unified resolution chain)
	hcResolution, err := config.ResolveHarnessConfigName(config.HarnessConfigInputs{
		CLIFlag:     harnessConfig,
		TemplateCfg: finalScionCfg,
		Settings:    settings,
		ProfileName: profileName,
	})
	if err != nil {
		return "", "", nil, err
	}
	harnessConfigName := hcResolution.Name

	// 2c. Load harness-config from disk (check template dirs first)
	var templatePaths []string
	for _, tpl := range chain {
		templatePaths = append(templatePaths, tpl.Path)
	}
	hcDir, err := config.FindHarnessConfigDir(harnessConfigName, projectPath, templatePaths...)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to find harness-config %q: %w", harnessConfigName, err)
	}
	util.Debugf("ProvisionAgent: harness-config loaded from disk: path=%s harness=%q image=%q",
		hcDir.Path, hcDir.Config.Harness, hcDir.Config.Image)
	finalScionCfg.Harness = hcDir.Config.Harness
	finalScionCfg.HarnessConfig = harnessConfigName

	// Merge harness-config scalars into finalScionCfg (harness-config is base, template overrides)
	hcCfg := &api.ScionConfig{}
	if hcDir.Config.Image != "" {
		hcCfg.Image = hcDir.Config.Image
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

	// 2d. Compose agent home directory

	// Step 1: Copy harness-config base home → agentHome
	hcHome := filepath.Join(hcDir.Path, "home")
	if info, err := os.Stat(hcHome); err == nil && info.IsDir() {
		if err := util.CopyDir(hcHome, agentHome); err != nil {
			return "", "", nil, fmt.Errorf("failed to copy harness-config home: %w", err)
		}
	}

	// Step 2: Copy template home → agentHome (overlay; template files win on conflict)
	for _, tpl := range chain {
		templateHome := filepath.Join(tpl.Path, "home")
		if info, err := os.Stat(templateHome); err == nil && info.IsDir() {
			if err := util.CopyDir(templateHome, agentHome); err != nil {
				return "", "", nil, fmt.Errorf("failed to copy template home %s: %w", tpl.Name, err)
			}
		}
	}

	// Step 3: Copy skills directories into harness-specific location
	resolved, err := harness.Resolve(ctx, harness.ResolveOptions{
		Name:          harnessConfigName,
		ProjectPath:     projectPath,
		TemplatePaths: templatePaths,
		ProfileName:   profileName,
		Settings:      settings,
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to resolve harness for %q: %w", harnessConfigName, err)
	}
	h := resolved.Harness
	util.Debugf("ProvisionAgent: harness implementation=%s for harness=%q", resolved.Implementation, finalScionCfg.Harness)
	skillsDir := h.SkillsDir()
	if skillsDir != "" {
		skillsDest := filepath.Join(agentHome, skillsDir)

		// Copy skills from harness-config base layer
		hcSkills := filepath.Join(hcDir.Path, "skills")
		if info, err := os.Stat(hcSkills); err == nil && info.IsDir() {
			if err := os.MkdirAll(skillsDest, 0755); err != nil {
				return "", "", nil, fmt.Errorf("failed to create skills dir: %w", err)
			}
			if err := util.CopyDir(hcSkills, skillsDest); err != nil {
				return "", "", nil, fmt.Errorf("failed to copy harness-config skills: %w", err)
			}
		}

		// Copy skills from each template in the chain (overlay behavior)
		for _, tpl := range chain {
			tplSkills := filepath.Join(tpl.Path, "skills")
			if info, err := os.Stat(tplSkills); err == nil && info.IsDir() {
				if err := os.MkdirAll(skillsDest, 0755); err != nil {
					return "", "", nil, fmt.Errorf("failed to create skills dir: %w", err)
				}
				if err := util.CopyDir(tplSkills, skillsDest); err != nil {
					return "", "", nil, fmt.Errorf("failed to copy template skills %s: %w", tpl.Name, err)
				}
			}
		}
	}

	// Step 4: Inject agent instructions

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
			if content != nil {
				// Conditionally append extra instruction fragments before injection
				content = appendExtraInstructions(ctx, content, isGit, settings)

				util.Debugf("ProvisionAgent: injecting agent instructions (%d bytes) into %s", len(content), agentHome)
				if err := h.InjectAgentInstructions(agentHome, content); err != nil {
					return "", "", nil, fmt.Errorf("failed to inject agent instructions: %w", err)
				}
			} else {
				util.Debugf("ProvisionAgent: agent_instructions resolved to nil, skipping injection")
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
			content = appendExtraInstructions(ctx, content, isGit, settings)
			util.Debugf("ProvisionAgent: injecting inline agent_instructions (%d bytes, no template)", len(content))
			if err := h.InjectAgentInstructions(agentHome, content); err != nil {
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
			// Template has highest priority, so it should override settings.
			// We construct a config with ONLY the settings env, then merge finalScionCfg over it.
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

	// Mount the resolved workspace if an external source was determined
	if workspaceSource != "" {
		finalScionCfg.Volumes = append(finalScionCfg.Volumes, api.VolumeMount{
			Source:   workspaceSource,
			Target:   "/workspace",
			ReadOnly: false,
		})
	}

	// Update agent-specific scion-agent.json
	if finalScionCfg == nil {
		finalScionCfg = &api.ScionConfig{}
	}

	// Create the Info object which will go into agent-info.json.
	// Use the resolved template name from the chain (human-friendly) rather
	// than the raw templateName which may be a cache path or remote URI.
	displayTemplateName := templateName
	if len(chain) > 0 {
		displayTemplateName = chain[len(chain)-1].Name
	}
	projectID, _ := config.ReadProjectID(projectDir)
	info := &api.AgentInfo{
		Project:               projectName,
		ProjectID:             projectID,
		ProjectPath:           projectDir,
		Name:                  agentName,
		Template:              displayTemplateName,
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

	// 2f. Configure git credential helper for shared-workspace groves.
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
			f.Close()
			return "", "", nil, fmt.Errorf("failed to write credential helper to .gitconfig: %w", err)
		}
		f.Close()
		util.Debugf("provision: configured git credential helper for shared workspace in %s", gitconfigPath)
	}

	// 3. Harness provisioning
	if err := h.Provision(ctx, agentName, agentDir, agentHome, agentWorkspace); err != nil {
		return "", "", nil, fmt.Errorf("harness provisioning failed: %w", err)
	}

	// Reload config to get harness updates (e.g. Env vars injected by harness)
	reloadTpl := &config.Template{Path: agentDir}
	if updatedCfg, err := reloadTpl.LoadConfig(); err == nil {
		updatedCfg.Info = finalScionCfg.Info // Re-attach info
		finalScionCfg = updatedCfg
	} else {
		fmt.Fprintf(os.Stderr, "Warning: failed to reload agent config after harness provisioning: %v\n", err)
	}

	return agentHome, agentWorkspace, finalScionCfg, nil
}

// appendExtraInstructions conditionally appends agents-git.md and/or
// agents-hub.md content from the embedded default template to the base
// agent instructions. This runs before harness-specific injection.
func appendExtraInstructions(ctx context.Context, content []byte, isGit bool, settings *config.VersionedSettings) []byte {
	if isGit {
		if extra, err := config.EmbedsFS.ReadFile("embeds/templates/default/agents-git.md"); err == nil && len(extra) > 0 {
			util.Debugf("ProvisionAgent: appending agents-git.md (%d bytes)", len(extra))
			content = append(content, '\n')
			content = append(content, extra...)
		}
	}
	hubEnabled := (settings != nil && settings.IsHubEnabled()) || api.IsBrokerModeFromContext(ctx)
	if hubEnabled {
		if extra, err := config.EmbedsFS.ReadFile("embeds/templates/default/agents-hub.md"); err == nil && len(extra) > 0 {
			util.Debugf("ProvisionAgent: appending agents-hub.md (%d bytes)", len(extra))
			content = append(content, '\n')
			content = append(content, extra...)
		}
	}
	return content
}

func GetSavedProfile(agentName string, projectPath string) string {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return ""
	}
	agentInfoPath := filepath.Join(config.GetAgentHomePath(projectDir, agentName), "agent-info.json")
	if _, err := os.Stat(agentInfoPath); err == nil {
		data, err := os.ReadFile(agentInfoPath)
		if err == nil {
			var info api.AgentInfo
			if err := json.Unmarshal(data, &info); err == nil {
				return info.Profile
			}
		}
	}
	return ""
}

func GetSavedRuntime(agentName string, projectPath string) string {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return ""
	}
	agentInfoPath := filepath.Join(config.GetAgentHomePath(projectDir, agentName), "agent-info.json")
	if _, err := os.Stat(agentInfoPath); err == nil {
		data, err := os.ReadFile(agentInfoPath)
		if err == nil {
			var info api.AgentInfo
			if err := json.Unmarshal(data, &info); err == nil {
				return info.Runtime
			}
		}
	}
	return ""
}

func GetSavedHarnessConfig(agentName string, projectPath string) string {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return ""
	}
	agentInfoPath := filepath.Join(config.GetAgentHomePath(projectDir, agentName), "agent-info.json")
	if _, err := os.Stat(agentInfoPath); err == nil {
		data, err := os.ReadFile(agentInfoPath)
		if err == nil {
			var info api.AgentInfo
			if err := json.Unmarshal(data, &info); err == nil {
				return info.HarnessConfig
			}
		}
	}
	return ""
}

func GetSavedPhase(agentName string, projectPath string) string {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return ""
	}
	agentInfoPath := filepath.Join(config.GetAgentHomePath(projectDir, agentName), "agent-info.json")
	if _, err := os.Stat(agentInfoPath); err == nil {
		data, err := os.ReadFile(agentInfoPath)
		if err == nil {
			var info api.AgentInfo
			if err := json.Unmarshal(data, &info); err == nil {
				return info.Phase
			}
		}
	}
	return ""
}

func UpdateAgentConfig(agentName string, projectPath string, status string, runtime string, profile string) error {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return err
	}
	agentHome := config.GetAgentHomePath(projectDir, agentName)
	agentInfoPath := filepath.Join(agentHome, "agent-info.json")

	// If agent-info.json doesn't exist, we can't update it.
	// This might happen if provisioning failed or hasn't finished.
	if _, err := os.Stat(agentInfoPath); os.IsNotExist(err) {
		return nil
	}

	data, err := os.ReadFile(agentInfoPath)
	if err != nil {
		return err
	}

	var info api.AgentInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return err
	}

	if status != "" {
		info.Phase = status
	}
	if runtime != "" {
		info.Runtime = runtime
	}
	if profile != "" {
		info.Profile = profile
	}

	newData, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(agentInfoPath, newData, 0644); err != nil {
		return err
	}

	return nil
}

// UpdateAgentDeletedAt writes the deletedAt timestamp to agent-info.json.
func UpdateAgentDeletedAt(agentName string, projectPath string, deletedAt time.Time) error {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return err
	}
	agentInfoPath := filepath.Join(config.GetAgentHomePath(projectDir, agentName), "agent-info.json")

	if _, err := os.Stat(agentInfoPath); os.IsNotExist(err) {
		return nil
	}

	data, err := os.ReadFile(agentInfoPath)
	if err != nil {
		return err
	}

	var info api.AgentInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return err
	}

	info.DeletedAt = deletedAt

	newData, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(agentInfoPath, newData, 0644)
}

func GetAgent(ctx context.Context, agentName string, templateName string, agentImage string, harnessConfig string, projectPath string, profileName string, optionalStatus string, branch string, workspace string, inlineConfig ...*api.ScionConfig) (string, string, string, *api.ScionConfig, error) {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return "", "", "", nil, err
	}

	util.Debugf("GetAgent: agentName=%s templateName=%q harnessConfig=%q projectPath=%q projectDir=%s",
		agentName, templateName, harnessConfig, projectPath, projectDir)

	sharedWorkspace := api.IsSharedWorkspaceFromContext(ctx)
	agentDir := config.GetAgentDir(projectDir, agentName, sharedWorkspace)
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
			util.Debugf("GetAgent: agent dir exists but no config file found, removing stale directory")
			os.RemoveAll(agentDir)
			// Prune worktrees so git forgets any worktree that pointed into the
			// now-deleted directory, allowing ProvisionAgent to recreate it cleanly.
			if root, rootErr := util.RepoRootDir(filepath.Dir(agentWorkspace)); rootErr == nil {
				_ = util.PruneWorktreesIn(root)
			}
		}
	}

	// If the managed workspace directory doesn't exist, try to recreate it.
	// Only do this for existing, fully-provisioned agents (config file present).
	// For new agents or stale directories, ProvisionAgent handles worktree creation.
	// Skipped for shared-workspace agents (agentWorkspace == "") because they
	// share the grove-wide checkout and have no per-agent worktree.
	if agentWorkspace != "" && config.GetScionAgentConfigPath(agentDir) != "" {
		if _, err := os.Stat(agentWorkspace); os.IsNotExist(err) {
			if util.IsGitRepoDir(projectDir) {
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
		// Just log or ignore
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

	// When git clone is configured (hub-dispatched create), clear the workspace
	// so sciontool performs a fresh clone. The agent directory may be left over
	// from a previous agent with the same name that was deleted via the hub but
	// whose local files were not cleaned up. Without this, sciontool sees the
	// old clone as "already populated" and skips cloning.
	if gitClone := api.GitCloneFromContext(ctx); gitClone != nil {
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

	if infoData, err := os.ReadFile(agentInfoPath); err == nil {
		if err := json.Unmarshal(infoData, &agentInfo); err == nil {
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

	chain, err := config.GetTemplateChainInProject(effectiveTemplate, projectPath)
	if err != nil {
		util.Debugf("GetAgent: template chain for %q not found: %v, returning agentCfg only (harness=%q image=%q)",
			effectiveTemplate, err, agentCfg.Harness, agentCfg.Image)
		return agentDir, agentHome, agentWorkspace, agentCfg, nil
	}

	mergedCfg := &api.ScionConfig{}
	for _, tpl := range chain {
		tplCfg, err := tpl.LoadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to load config from template %s, skipping: %v\n", tpl.Name, err)
			continue
		}
		mergedCfg = config.MergeScionConfig(mergedCfg, tplCfg)
	}

	finalCfg := config.MergeScionConfig(mergedCfg, agentCfg)

	// Ensure Info is populated from agent-info.json if available
	if agentInfo != nil {
		finalCfg.Info = agentInfo
	}

	util.Debugf("GetAgent: existing agent config loaded, harness=%q harnessConfig=%q image=%q defaultHarnessConfig=%q",
		finalCfg.Harness, finalCfg.HarnessConfig, finalCfg.Image, finalCfg.DefaultHarnessConfig)

	return agentDir, agentHome, agentWorkspace, finalCfg, nil
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
