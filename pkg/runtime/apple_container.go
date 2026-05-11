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

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

type AppleContainerRuntime struct {
	Command string
}

func NewAppleContainerRuntime() *AppleContainerRuntime {
	return &AppleContainerRuntime{
		Command: "container",
	}
}

func (r *AppleContainerRuntime) Name() string {
	return "container"
}

func (r *AppleContainerRuntime) ExecUser() string {
	return "scion"
}

func (r *AppleContainerRuntime) Run(ctx context.Context, config RunConfig) (string, error) {
	// Stage file, variable, and secret-map secrets before building args
	if config.HomeDir != "" && len(config.ResolvedSecrets) > 0 {
		containerHome := util.GetHomeDir(config.UnixUsername)
		if _, err := writeFileSecrets(config.HomeDir, containerHome, config.ResolvedSecrets); err != nil {
			return "", fmt.Errorf("failed to stage file secrets: %w", err)
		}
		if err := writeVariableSecrets(config.HomeDir, config.ResolvedSecrets); err != nil {
			return "", fmt.Errorf("failed to write variable secrets: %w", err)
		}
		if err := writeSecretMap(config.HomeDir, containerHome, config.ResolvedSecrets); err != nil {
			return "", fmt.Errorf("failed to write secret map: %w", err)
		}
	}

	// Inject GCP telemetry credential path if the well-known secret is present
	if credPath := findGCPTelemetryCredentialPath(config.ResolvedSecrets, util.GetHomeDir(config.UnixUsername)); credPath != "" {
		config.Env = append(config.Env, telemetryGCPCredentialsEnvVar+"="+credPath)
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		return "", err
	}

	// For Apple Container, we want to ensure -d and -t are present for 'run'
	// matching the working manual command.
	newArgs := []string{"run", "-d", "-t"}

	// Apply resource constraints from config, falling back to defaults.
	memFlag := "2G" // default
	if config.Resources != nil {
		mem := config.Resources.Limits.Memory
		if mem == "" {
			mem = config.Resources.Requests.Memory
		}
		if mem != "" {
			bytes, err := util.ParseMemory(mem)
			if err != nil {
				return "", fmt.Errorf("invalid memory resource %q: %w", mem, err)
			}
			memFlag = util.FormatMemoryForApple(bytes)
		}
	}
	newArgs = append(newArgs, "-m", memFlag)

	if config.Resources != nil {
		cpuStr := config.Resources.Limits.CPU
		if cpuStr == "" {
			cpuStr = config.Resources.Requests.CPU
		}
		if cpuStr != "" {
			cores, err := util.ParseCPU(cpuStr)
			if err != nil {
				return "", fmt.Errorf("invalid cpu resource %q: %w", cpuStr, err)
			}
			newArgs = append(newArgs, "-c", util.FormatCPU(cores))
		}
	}

	// Skip the original 'run', '-d', and '-i' from buildCommonRunArgs (indices 0, 1, 2)
	// then strip flags that the Apple container CLI does not support.
	newArgs = append(newArgs, stripUnsupportedAppleFlags(args[3:])...)

	// Insert secrets staging directory volume before the image so it is treated
	// as a container flag rather than an argument to the container command.
	if config.HomeDir != "" && len(config.ResolvedSecrets) > 0 {
		secretsDir := filepath.Join(filepath.Dir(config.HomeDir), "secrets")
		if _, err := os.Stat(secretsDir); err == nil {
			newArgs = insertVolumeFlags(newArgs, config.Image, []string{secretsDir + ":/run/scion-secrets:ro"})
		}
	}

	WriteRuntimeDebugFile(config, r.Command, newArgs)

	out, err := runSimpleCommand(ctx, r.Command, newArgs...)
	if err != nil {
		return "", fmt.Errorf("container run failed: %w (output: %s)", err, out)
	}

	// The output of 'container run -d' is the container ID
	return strings.TrimSpace(out), nil
}

func (r *AppleContainerRuntime) Stop(ctx context.Context, id string) error {
	_, err := runSimpleCommand(ctx, r.Command, "stop", id)
	return err
}

func (r *AppleContainerRuntime) Delete(ctx context.Context, id string) error {
	// Apple's `container rm` doesn't support -f and fails on running containers,
	// so kill first (ignoring errors if already stopped) then remove.
	_, _ = runSimpleCommand(ctx, r.Command, "kill", id)

	// Retry rm with short delays since kill is asynchronous and the container
	// may not be immediately ready for removal.
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		_, err = runSimpleCommand(ctx, r.Command, "rm", id)
		if err == nil {
			return nil
		}
		// Check if context is cancelled before sleeping
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
			// Continue to next attempt
		}
	}
	return err
}

type containerListOutput struct {
	Status        string `json:"status"`
	Configuration struct {
		ID     string            `json:"id"`
		Labels map[string]string `json:"labels"`
		Image  struct {
			Reference string `json:"reference"`
		} `json:"image"`
	} `json:"configuration"`
}

func (r *AppleContainerRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	args := []string{"list", "-a", "--format", "json"}

	cmd := exec.CommandContext(ctx, r.Command, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("container list failed: %w (output: %s)", err, string(out))
	}

	var raw []containerListOutput
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse container list output: %w (output: %s)", err, string(out))
	}

	var agents []api.AgentInfo
	for _, c := range raw {
		// Filter by labels if requested
		if len(labelFilter) > 0 {
			match := true
			for k, v := range labelFilter {
				if lv, ok := c.Configuration.Labels[k]; !ok || lv != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}

		agents = append(agents, api.AgentInfo{
			ContainerID:     c.Configuration.ID,
			Name:            c.Configuration.Labels["scion.name"],
			Template:        c.Configuration.Labels["scion.template"],
			HarnessConfig:   c.Configuration.Labels["scion.harness_config"],
			HarnessAuth:     c.Configuration.Labels["scion.harness_auth"],
			Project:         c.Configuration.Labels["scion.grove"],
			ProjectID:       c.Configuration.Labels["scion.grove_id"],
			ProjectPath:     c.Configuration.Labels["scion.grove_path"],
			Labels:          c.Configuration.Labels,
			Annotations:     c.Configuration.Labels,
			ContainerStatus: c.Status,
			Phase:           phaseFromContainerStatus(c.Status),
			Image:           c.Configuration.Image.Reference,
			Runtime:         r.Name(),
		})
	}

	return agents, nil
}

func (r *AppleContainerRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	return runSimpleCommand(ctx, r.Command, "logs", id)
}

func (r *AppleContainerRuntime) Attach(ctx context.Context, id string) error {
	// 1. Find container to check for tmux label
	agents, err := r.List(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	var a *api.AgentInfo
	for _, agent := range agents {
		// Match by full container ID, or name
		if agent.ContainerID == id || agent.Name == id || strings.TrimPrefix(agent.Name, "/") == id {
			a = &agent
			break
		}
	}

	if a == nil {
		return fmt.Errorf("agent '%s' container not found. It may have exited and been removed.", id)
	}

	// Check if running
	status := strings.ToLower(a.ContainerStatus)
	if !strings.HasPrefix(status, "up") && status != "running" {
		return fmt.Errorf("agent '%s' is not running (status: %s). Use 'scion start %s' to resume it.", id, a.ContainerStatus, id)
	}

	// Ensure tmux uses the latest client's terminal size so the session
	// redraws correctly on attach (handles containers started before the
	// window-size option was added to session creation).
	_, _ = runSimpleCommand(ctx, r.Command, "exec", "--user", "scion",
		a.ContainerID, "tmux", "set-option", "-g", "window-size", "latest")

	return runInteractiveCommand(r.Command, "exec", "-it", "--user", "scion", a.ContainerID, "tmux", "attach", "-t", "scion")
}

func (r *AppleContainerRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	_, err := runSimpleCommand(ctx, r.Command, "image", "inspect", image)
	return err == nil, nil
}

func (r *AppleContainerRuntime) PullImage(ctx context.Context, image string) error {
	return runInteractiveCommand(r.Command, "image", "pull", image)
}

func (r *AppleContainerRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {

	// Apple container runtime uses bind mounts (if configured), so sync is likely automatic/noop

	return nil

}

func (r *AppleContainerRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	// Resolve slug/name to actual container ID (container names include the
	// grove prefix, e.g. "mygrove--agent", so the bare slug won't match).
	if agents, err := r.List(ctx, nil); err == nil {
		id = resolveContainerID(agents, id)
	}
	args := append([]string{"exec", "--user", "scion", id}, cmd...)
	return runSimpleCommand(ctx, r.Command, args...)
}

// stripUnsupportedAppleFlags removes flag-value pairs that the Apple
// `container` CLI does not recognise (e.g. --cap-add, --device, --mount,
// --add-host, --network). These are Docker/Podman-specific and cause the
// Apple runtime to exit with "Unknown option".
func stripUnsupportedAppleFlags(args []string) []string {
	unsupported := map[string]bool{
		"--cap-add":  true,
		"--device":   true,
		"--mount":    true,
		"--add-host": true,
		"--network":  true,
	}
	var out []string
	for i := 0; i < len(args); i++ {
		if unsupported[args[i]] {
			slog.Warn("stripping unsupported flag for Apple container runtime", "flag", args[i], "value", args[i+1])
			i++ // skip the value
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// GetWorkspacePath returns the host path to the container's /workspace mount.
func (r *AppleContainerRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	// Apple container runtime doesn't expose mount inspection in the same way as Docker.
	// We need to rely on the labels stored when the container was created.
	agents, err := r.List(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to list containers: %w", err)
	}

	for _, agent := range agents {
		if agent.ContainerID == id || agent.Name == id {
			// Check for workspace path in labels
			if workspacePath, ok := agent.Labels["scion.workspace_path"]; ok && workspacePath != "" {
				return workspacePath, nil
			}
			// Fall back to project path worktree pattern
			if agent.ProjectPath != "" && agent.Name != "" {
				// Worktrees are typically at: {parent}/.scion_worktrees/{project}/{agent}
				projectName := agent.Project
				if projectName == "" {
					projectName = "default"
				}
				return fmt.Sprintf("%s/../.scion_worktrees/%s/%s", agent.ProjectPath, projectName, agent.Name), nil
			}
			break
		}
	}

	return "", fmt.Errorf("could not determine workspace path for container %s", id)
}
