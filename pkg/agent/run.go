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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/harness"
	"github.com/ptone/scion-agent/pkg/runtime"
	"github.com/ptone/scion-agent/pkg/util"
)

func (m *AgentManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	// 0. Check if container already exists
	agents, err := m.Runtime.List(ctx, nil)
	if err == nil {
		for _, a := range agents {
			if a.ContainerID == opts.Name || a.Name == opts.Name || strings.TrimPrefix(a.Name, "/") == opts.Name {
				status := strings.ToLower(a.ContainerStatus)
				isRunning := strings.HasPrefix(status, "up") || status == "running"
				if isRunning {
					// If a new task is provided, we might want to recreate even if running
					// but if no task provided, we just return the running one
					if opts.Task == "" {
						a.Detached = true
						if opts.Detached != nil {
							a.Detached = *opts.Detached
						}
						return &a, nil
					}
				}
				// If it exists but not running (or we have a new task), we delete it so we can recreate it
				if err := m.Runtime.Delete(ctx, a.ContainerID); err != nil {
					return nil, fmt.Errorf("failed to cleanup existing container: %w", err)
				}
			}
		}
	}

	projectDir, err := config.GetResolvedProjectDir(opts.GrovePath)
	if err != nil {
		return nil, err
	}
	groveName := config.GetGroveName(projectDir)

	// If resuming, verify the agent exists before proceeding
	if opts.Resume {
		agentDir := filepath.Join(projectDir, "agents", opts.Name)
		if _, err := os.Stat(agentDir); os.IsNotExist(err) {
			return nil, fmt.Errorf("cannot resume agent '%s': agent does not exist. Use 'scion start' to create a new agent", opts.Name)
		}
	}

	agentDir, agentHome, agentWorkspace, finalScionCfg, err := GetAgent(ctx, opts.Name, opts.Template, opts.Image, opts.GrovePath, opts.Profile, "", opts.Branch, opts.Workspace)
	if err != nil {
		return nil, err
	}

	promptFile := filepath.Join(agentDir, "prompt.md")
	promptFileContent := ""
	if content, err := os.ReadFile(promptFile); err == nil {
		promptFileContent = strings.TrimSpace(string(content))
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read prompt file %s: %w", promptFile, err)
	}

	task := opts.Task
	// If we are explicitly attaching or resuming, we allow starting without a task
	isAttaching := opts.Detached != nil && !*opts.Detached
	if task == "" && promptFileContent == "" && !isAttaching && !opts.Resume {
		return nil, fmt.Errorf("no task provided: prompt.md is empty at %s and no task was given in options", promptFile)
	}

	if !opts.Resume && task != "" && promptFileContent != "" && task != promptFileContent {
		return nil, fmt.Errorf("task conflict: both prompt.md and start options provide a task")
	}

	if task == "" && !opts.Resume {
		task = promptFileContent
	} else if promptFileContent == "" && task != "" {
		_ = os.WriteFile(promptFile, []byte(task), 0644)
	}

	// Load settings for registry resolution
	settings, err := config.LoadSettings(projectDir)
	if err != nil {
		// Fallback to defaults or log?
	}

	harnessName := ""
	if finalScionCfg != nil {
		harnessName = finalScionCfg.Harness
	}

	// Default values
	resolvedImage := "gemini-cli-sandbox"
	unixUsername := "root"
	useTmux := false
	profileName := opts.Profile

	if settings != nil && harnessName != "" {
		hConfig, err := settings.ResolveHarness(opts.Profile, harnessName)
		if err == nil {
			resolvedImage = hConfig.Image
			unixUsername = hConfig.User
		}
	}

	if settings != nil {
		if profileName == "" {
			profileName = settings.ActiveProfile
		}
		if p, ok := settings.Profiles[profileName]; ok {
			// 1. Start with runtime default if available
			if rCfg, ok := settings.Runtimes[p.Runtime]; ok && rCfg.Tmux != nil {
				useTmux = *rCfg.Tmux
			}
			// 2. Override with profile setting if explicitly set
			if p.Tmux != nil {
				useTmux = *p.Tmux
			}
		}
	}

	var warnings []string
	if m.Runtime.Name() == "container" && !useTmux {
		warnings = append(warnings, "Warning: Apple container runtime does not support 'attach' without tmux. Sessions will be non-interactive after start.")
	}

	if finalScionCfg != nil && finalScionCfg.Image != "" {
		resolvedImage = finalScionCfg.Image
	}

	// CLI Overrides
	if opts.Image != "" {
		resolvedImage = opts.Image
	}

	h := harness.New(harnessName)

	// 3. Propagate credentials
	var auth api.AuthConfig
	if !opts.NoAuth {
		if opts.Auth != nil {
			auth, err = opts.Auth.GetAuthConfig(ctx)
			if err != nil {
				return nil, fmt.Errorf("failed to get auth: %w", err)
			}
		} else {
			// Fallback to legacy discovery if no harness given
			auth = h.DiscoverAuth(agentHome)
		}
	}

	// 4. Launch container
	detached := true

	if finalScionCfg != nil {
		detached = finalScionCfg.IsDetached()
	}

	if opts.Detached != nil {
		detached = *opts.Detached
	}

	if useTmux {
		// We no longer automatically append -tmux to the image tag.
		// We launch optimistically and provide a clear error if tmux is missing.
	}

	exists, err := m.Runtime.ImageExists(ctx, resolvedImage)
	if err != nil || !exists {
		if err := m.Runtime.PullImage(ctx, resolvedImage); err != nil {
			return nil, fmt.Errorf("failed to pull image '%s': %w", resolvedImage, err)
		}
	}

	template := ""
	if finalScionCfg != nil && finalScionCfg.Info != nil {
		template = finalScionCfg.Info.Template
	}

	if opts.Env == nil {
		opts.Env = make(map[string]string)
	}
	opts.Env["SCION_AGENT_NAME"] = opts.Name
	if template != "" {
		opts.Env["SCION_TEMPLATE_NAME"] = template
	} else {
		opts.Env["SCION_TEMPLATE_NAME"] = "custom"
	}
	if _, ok := opts.Env["SCION_BROKER_NAME"]; !ok {
		opts.Env["SCION_BROKER_NAME"] = "local"
	}

	agentEnv, envWarnings := buildAgentEnv(finalScionCfg, opts.Env)
	warnings = append(warnings, envWarnings...)

	// Determine the effective workspace path. If agentWorkspace is empty but we have
	// a volume mounted to /workspace (e.g., shared worktree case), use that source path.
	effectiveWorkspace := agentWorkspace
	if effectiveWorkspace == "" && finalScionCfg != nil {
		effectiveWorkspace = extractWorkspaceFromVolumes(finalScionCfg.Volumes)
	}

	repoRoot := ""
	if effectiveWorkspace != "" && util.IsGitRepoDir(effectiveWorkspace) {
		commonDir, err := util.GetCommonGitDir(effectiveWorkspace)
		if err == nil {
			repoRoot = filepath.Dir(commonDir)
		}
	} else if util.IsGitRepoDir(projectDir) {
		repoRoot, _ = util.RepoRootDir(projectDir)
	}

	runCfg := runtime.RunConfig{
		Name:         opts.Name,
		Template:     template,
		UnixUsername: unixUsername,
		Image:        resolvedImage,
		HomeDir:      agentHome,
		Workspace:    effectiveWorkspace,
		RepoRoot:     repoRoot,
		Auth:         auth,
		Harness:      h,
		UseTmux:      useTmux,
		Task:         task,
		CommandArgs: func() []string {
			var args []string
			if finalScionCfg != nil {
				args = finalScionCfg.CommandArgs
				if finalScionCfg.Model != "" {
					// Prepend model flag so it appears before user args but is passed in baseArgs
					args = append([]string{"--model", finalScionCfg.Model}, args...)
				}
			}
			return args
		}(),
		Env:          agentEnv,
		Volumes: func() []api.VolumeMount {
			if finalScionCfg == nil {
				return nil
			}
			// If we extracted effectiveWorkspace from a /workspace volume mount,
			// filter it out to avoid a duplicate mount (the buildCommonRunArgs
			// will handle the workspace mount properly with worktree support).
			if effectiveWorkspace != "" && effectiveWorkspace != agentWorkspace {
				return filterWorkspaceVolume(finalScionCfg.Volumes)
			}
			return finalScionCfg.Volumes
		}(),
		Kubernetes: func() *api.KubernetesConfig {
			if finalScionCfg != nil {
				return finalScionCfg.Kubernetes
			}
			return nil
		}(),
		Resume: opts.Resume,
		Labels: map[string]string{
			"scion.agent":    "true",
			"scion.name":     opts.Name,
			"scion.grove":    groveName,
			"scion.template": template,
		},
		Annotations: map[string]string{
			"scion.grove_path": projectDir,
		},
	}
	id, err := m.Runtime.Run(ctx, runCfg)
	if err != nil {
		if useTmux && (strings.Contains(err.Error(), "executable file not found") ||
			strings.Contains(err.Error(), "tmux: command not found") ||
			strings.Contains(err.Error(), "tmux: not found")) {
			return nil, fmt.Errorf("failed to launch container with tmux: tmux binary not found in image '%s'. "+
				"Please ensure the image has tmux installed, use an image with a :tmux tag, or disable tmux in settings. Error: %w", resolvedImage, err)
		}
		return nil, fmt.Errorf("failed to launch container: %w", err)
	}

	status := "running"
	if opts.Resume {
		status = "resumed"
	}
	_ = UpdateAgentConfig(opts.Name, opts.GrovePath, status, m.Runtime.Name(), profileName, "ACTIVE")

	// Fetch fresh info
	allAgents, err := m.Runtime.List(ctx, map[string]string{"scion.name": opts.Name})
	if err == nil {
		for _, a := range allAgents {
			if a.ContainerID == id || a.Name == opts.Name {
				a.Detached = detached
				a.Warnings = warnings
				return &a, nil
			}
		}
	}

	return &api.AgentInfo{ID: id, Name: opts.Name, Status: status, Detached: detached, Warnings: warnings}, nil
}

// extractWorkspaceFromVolumes finds a volume mounted to /workspace and returns its source path.
// This is used when an agent shares an existing worktree from another agent.
func extractWorkspaceFromVolumes(volumes []api.VolumeMount) string {
	for _, v := range volumes {
		if v.Target == "/workspace" {
			return v.Source
		}
	}
	return ""
}

// filterWorkspaceVolume removes volumes targeting /workspace from the list.
// This is used when the workspace will be handled by the RepoRoot/Workspace logic
// in buildCommonRunArgs instead of as a generic volume mount.
func filterWorkspaceVolume(volumes []api.VolumeMount) []api.VolumeMount {
	var filtered []api.VolumeMount
	for _, v := range volumes {
		if v.Target != "/workspace" {
			filtered = append(filtered, v)
		}
	}
	return filtered
}

func buildAgentEnv(scionCfg *api.ScionConfig, extraEnv map[string]string) ([]string, []string) {
	combined := make(map[string]string)
	var warnings []string

	if scionCfg != nil && scionCfg.Env != nil {
		for k, v := range scionCfg.Env {
			// Support variable substitution in keys and values
			expandedKey, _ := util.ExpandEnv(k)
			expandedValue, warned := util.ExpandEnv(v)

			if expandedKey == "" {
				continue
			}
			// If the value is empty and we warned about a missing variable,
			// skip adding it to combined to avoid a redundant warning later.
			if expandedValue == "" && warned {
				continue
			}
			combined[expandedKey] = expandedValue
		}
	}
	// Add extraEnv
	for k, v := range extraEnv {
		combined[k] = v
	}

	agentEnv := []string{}
	for k, v := range combined {
		if v == "" {
			warnings = append(warnings, fmt.Sprintf("Warning: Environment variable '%s' has no value and will be omitted.", k))
			continue
		}
		agentEnv = append(agentEnv, fmt.Sprintf("%s=%s", k, v))
	}
	return agentEnv, warnings
}