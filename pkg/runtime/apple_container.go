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
	"os/exec"
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/api"
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

func (r *AppleContainerRuntime) Run(ctx context.Context, config RunConfig) (string, error) {
	args, err := buildCommonRunArgs(config)
	if err != nil {
		return "", err
	}

	// For Apple Container, we want to ensure -d and -t are present for 'run'
	// matching the working manual command.
	// We also increase default memory to 2G.
	newArgs := []string{"run", "-d", "-t", "-m", "2G"}
	// Skip the original 'run', '-d', and '-i' from buildCommonRunArgs (indices 0, 1, 2)
	newArgs = append(newArgs, args[3:]...)

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
			Grove:           c.Configuration.Labels["scion.grove"],
			GrovePath:       c.Configuration.Labels["scion.grove_path"],
			Labels:          c.Configuration.Labels,
			Annotations:     c.Configuration.Labels,
			ContainerStatus: c.Status,
			Status:          "created", // Default status, updated by AgentManager logic
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

	if a.Labels["scion.tmux"] == "true" {
		return runInteractiveCommand(r.Command, "exec", "-it", "--user", "scion", a.ContainerID, "tmux", "attach", "-t", "scion")
	}

	return fmt.Errorf("apple container runtime does not support 'attach' without tmux")
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
	args := append([]string{"exec", "--user", "scion", id}, cmd...)
	return runSimpleCommand(ctx, r.Command, args...)
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
			// Fall back to grove path worktree pattern
			if agent.GrovePath != "" && agent.Name != "" {
				// Worktrees are typically at: {parent}/.scion_worktrees/{grove}/{agent}
				groveName := agent.Grove
				if groveName == "" {
					groveName = "default"
				}
				return fmt.Sprintf("%s/../.scion_worktrees/%s/%s", agent.GrovePath, groveName, agent.Name), nil
			}
			break
		}
	}

	return "", fmt.Errorf("could not determine workspace path for container %s", id)
}
