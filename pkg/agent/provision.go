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
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/harness"
	"github.com/ptone/scion-agent/pkg/util"
)

func DeleteAgentFiles(agentName string, grovePath string, removeBranch bool) (bool, error) {
	var agentsDirs []string
	branchDeleted := false
	var repoRoot string
	if projectDir, err := config.GetResolvedProjectDir(grovePath); err == nil {
		agentsDirs = append(agentsDirs, filepath.Join(projectDir, "agents"))
		// Determine repo root for worktree pruning and branch cleanup
		if root, err := util.RepoRootDir(filepath.Dir(projectDir)); err == nil {
			repoRoot = root
		}
	}
	// Also check global just in case
	if globalDir, err := config.GetGlobalAgentsDir(); err == nil {
		agentsDirs = append(agentsDirs, globalDir)
	}

	for _, dir := range agentsDirs {
		agentDir := filepath.Join(dir, agentName)
		if _, err := os.Stat(agentDir); err != nil {
			continue
		}

		agentWorkspace := filepath.Join(agentDir, "workspace")
		// Check if it's a worktree before trying to remove it
		if _, err := os.Stat(filepath.Join(agentWorkspace, ".git")); err == nil {
			util.Debugf("delete: removing git worktree at %s", agentWorkspace)
			worktreeStart := time.Now()
			if deleted, err := util.RemoveWorktree(agentWorkspace, removeBranch); err == nil {
				if deleted {
					branchDeleted = true
				}
				util.Debugf("delete: worktree removal completed in %v (branch deleted: %v)", time.Since(worktreeStart), deleted)
			} else {
				util.Debugf("delete: worktree removal failed in %v: %v", time.Since(worktreeStart), err)
			}
		}

		util.Debugf("delete: making directory writable: %s", agentDir)
		chmodStart := time.Now()
		_ = util.MakeWritableRecursive(agentDir)
		util.Debugf("delete: chmod completed in %v", time.Since(chmodStart))

		util.Debugf("delete: removing directory: %s", agentDir)
		removeStart := time.Now()
		if err := os.RemoveAll(agentDir); err != nil {
			util.Debugf("delete: RemoveAll failed in %v: %v", time.Since(removeStart), err)
			return branchDeleted, fmt.Errorf("failed to remove agent directory: %w", err)
		}
		util.Debugf("delete: RemoveAll completed in %v", time.Since(removeStart))
	}

	// Prune stale worktree records from the repo. This handles cases where the
	// workspace directory was removed (e.g. by os.RemoveAll above, or a previous
	// incomplete cleanup) but the git worktree record was not properly unregistered.
	if repoRoot != "" {
		util.Debugf("delete: pruning stale worktrees in %s", repoRoot)
		_ = util.PruneWorktreesIn(repoRoot)

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

	return branchDeleted, nil
}

func (m *AgentManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	agentDir, _, _, cfg, err := GetAgent(ctx, opts.Name, opts.Template, opts.Image, opts.HarnessConfig, opts.GrovePath, opts.Profile, "created", opts.Branch, opts.Workspace)
	if err == nil {
		_ = UpdateAgentConfig(opts.Name, opts.GrovePath, "created", m.Runtime.Name(), opts.Profile, "")
	}
	if err != nil {
		return cfg, err
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

func ProvisionAgent(ctx context.Context, agentName string, templateName string, agentImage string, harnessConfig string, grovePath string, profileName string, optionalStatus string, branch string, workspace string) (string, string, *api.ScionConfig, error) {
	// 1. Prepare agent directories
	projectDir, err := config.GetResolvedProjectDir(grovePath)
	if err != nil {
		return "", "", nil, err
	}

	settings, warnings, _ := config.LoadEffectiveSettings(projectDir)
	config.PrintDeprecationWarnings(warnings)
	if profileName == "" && settings != nil {
		profileName = settings.ActiveProfile
	}

	groveName := config.GetGroveName(projectDir)
	isGit := util.IsGitRepoDir(projectDir)

	// Verify .gitignore if in a repo
	if isGit {
		// Find the projectDir relative to repo root if possible
		root, err := util.RepoRootDir(projectDir)
		if err == nil {
			rel, err := filepath.Rel(root, projectDir)
			if err == nil && !strings.HasPrefix(rel, "..") {
				agentsPath := filepath.Join(rel, "agents")
				if !util.IsIgnored(agentsPath + "/") {
					return "", "", nil, fmt.Errorf("security error: '%s/' must be in .gitignore when using a project-local grove", agentsPath)
				}
			}
		}
	}
	agentsDir := filepath.Join(projectDir, "agents")

	agentDir := filepath.Join(agentsDir, agentName)
	agentHome := filepath.Join(agentDir, "home")
	agentWorkspace := filepath.Join(agentDir, "workspace")

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

	// Workspace Resolution Logic
	if workspace != "" {
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
		if groveName == "global" {
			workspaceSource, _ = os.Getwd()
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
	}

	// 2. Load templates and merge configs (no home copy yet)
	chain, err := config.GetTemplateChainInGrove(templateName, grovePath)
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

	// 2b. Resolve harness-config name (full chain)
	harnessConfigName := harnessConfig // CLI --harness-config flag (highest priority)
	if harnessConfigName == "" {
		harnessConfigName = finalScionCfg.DefaultHarnessConfig // template's default
	}
	if harnessConfigName == "" && settings != nil {
		// Profile's DefaultHarnessConfig
		effectiveProfile := profileName
		if effectiveProfile == "" {
			effectiveProfile = settings.ActiveProfile
		}
		if p, ok := settings.Profiles[effectiveProfile]; ok && p.DefaultHarnessConfig != "" {
			harnessConfigName = p.DefaultHarnessConfig
		}
	}
	if harnessConfigName == "" && settings != nil {
		harnessConfigName = settings.DefaultHarnessConfig // top-level settings
	}
	if harnessConfigName == "" {
		return "", "", nil, fmt.Errorf("no harness-config resolved. Specify --harness-config, set default_harness_config in the template, or set default_harness_config in settings")
	}

	// 2c. Load harness-config from disk
	hcDir, err := config.FindHarnessConfigDir(harnessConfigName, grovePath)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to find harness-config %q: %w", harnessConfigName, err)
	}
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
	if hcDir.Config.Env != nil {
		hcCfg.Env = hcDir.Config.Env
	}
	if hcDir.Config.Volumes != nil {
		hcCfg.Volumes = hcDir.Config.Volumes
	}
	if hcDir.Config.AuthSelectedType != "" {
		hcCfg.Gemini = &api.GeminiConfig{
			AuthSelectedType: hcDir.Config.AuthSelectedType,
		}
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

	// Step 3: Inject agent instructions
	h := harness.New(finalScionCfg.Harness)
	if len(chain) > 0 {
		lastTpl := chain[len(chain)-1]
		if finalScionCfg.AgentInstructions != "" {
			content, err := lastTpl.ResolveContent(finalScionCfg.AgentInstructions)
			if err != nil {
				return "", "", nil, fmt.Errorf("failed to resolve agent_instructions: %w", err)
			}
			if content != nil {
				if err := h.InjectAgentInstructions(agentHome, content); err != nil {
					return "", "", nil, fmt.Errorf("failed to inject agent instructions: %w", err)
				}
			}
		}

		// Step 4: Inject system prompt
		if finalScionCfg.SystemPrompt != "" {
			content, err := lastTpl.ResolveContent(finalScionCfg.SystemPrompt)
			if err != nil {
				return "", "", nil, fmt.Errorf("failed to resolve system_prompt: %w", err)
			}
			if content != nil {
				if err := h.InjectSystemPrompt(agentHome, content); err != nil {
					return "", "", nil, fmt.Errorf("failed to inject system prompt: %w", err)
				}
			}
		}
	}

	// Step 5: Copy common files (.tmux.conf, .zshrc)
	if err := config.SeedCommonFilesToHome(agentHome, false); err != nil {
		return "", "", nil, fmt.Errorf("failed to seed common files: %w", err)
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
				settingsCfg.Gemini = &api.GeminiConfig{
					AuthSelectedType: hConfig.AuthSelectedType,
				}
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

	// Create the Info object which will go into agent-info.json
	info := &api.AgentInfo{
		Grove:         groveName,
		Name:          agentName,
		Template:      templateName,
		HarnessConfig: harnessConfigName,
		Profile:       profileName,
		SessionStatus: "CREATED",
	}
	if optionalStatus != "" {
		info.Status = optionalStatus
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

	// 3. Harness provisioning
	if err := h.Provision(ctx, agentName, agentHome, agentWorkspace); err != nil {
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

func GetSavedProfile(agentName string, grovePath string) string {
	projectDir, err := config.GetResolvedProjectDir(grovePath)
	if err != nil {
		return ""
	}
	agentInfoPath := filepath.Join(projectDir, "agents", agentName, "home", "agent-info.json")
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

func GetSavedRuntime(agentName string, grovePath string) string {
	projectDir, err := config.GetResolvedProjectDir(grovePath)
	if err != nil {
		return ""
	}
	agentInfoPath := filepath.Join(projectDir, "agents", agentName, "home", "agent-info.json")
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

func UpdateAgentConfig(agentName string, grovePath string, status string, runtime string, profile string, sessionStatus string) error {
	projectDir, err := config.GetResolvedProjectDir(grovePath)
	if err != nil {
		return err
	}
	agentsDir := filepath.Join(projectDir, "agents")
	agentDir := filepath.Join(agentsDir, agentName)
	agentHome := filepath.Join(agentDir, "home")
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
		info.Status = status
	}
	if runtime != "" {
		info.Runtime = runtime
	}
	if profile != "" {
		info.Profile = profile
	}
	if sessionStatus != "" {
		info.SessionStatus = sessionStatus
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

func GetAgent(ctx context.Context, agentName string, templateName string, agentImage string, harnessConfig string, grovePath string, profileName string, optionalStatus string, branch string, workspace string) (string, string, string, *api.ScionConfig, error) {
	projectDir, err := config.GetResolvedProjectDir(grovePath)
	if err != nil {
		return "", "", "", nil, err
	}
	agentsDir := filepath.Join(projectDir, "agents")
	agentDir := filepath.Join(agentsDir, agentName)
	agentHome := filepath.Join(agentDir, "home")
	agentWorkspace := filepath.Join(agentDir, "workspace")

	// If we are resuming, and it's not a git repo, the physical workspace dir might not exist.
	if _, err := os.Stat(filepath.Join(agentWorkspace, ".git")); err != nil {
		if _, err := os.Stat(agentWorkspace); os.IsNotExist(err) {
			agentWorkspace = ""
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
		home, ws, cfg, err := ProvisionAgent(ctx, agentName, templateName, agentImage, harnessConfig, grovePath, profileName, optionalStatus, branch, workspace)
		return agentDir, home, ws, cfg, err
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

	chain, err := config.GetTemplateChainInGrove(effectiveTemplate, grovePath)
	if err != nil {
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

	return agentDir, agentHome, agentWorkspace, finalCfg, nil
}

