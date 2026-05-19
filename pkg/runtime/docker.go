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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

type DockerRuntime struct {
	Command string
	Host    string
}

func NewDockerRuntime() *DockerRuntime {
	return &DockerRuntime{
		Command: "docker",
	}
}

func (r *DockerRuntime) Name() string {
	return "docker"
}

func (r *DockerRuntime) ExecUser() string {
	return "scion"
}

func (r *DockerRuntime) Run(ctx context.Context, config RunConfig) (string, error) {
	// Stage file and variable secrets before building args
	var secretMountSpecs []string
	if config.HomeDir != "" && len(config.ResolvedSecrets) > 0 {
		mounts, err := writeFileSecrets(config.HomeDir, util.GetHomeDir(config.UnixUsername), config.ResolvedSecrets)
		if err != nil {
			return "", fmt.Errorf("failed to stage file secrets: %w", err)
		}
		secretMountSpecs = mounts

		if err := writeVariableSecrets(config.HomeDir, config.ResolvedSecrets); err != nil {
			return "", fmt.Errorf("failed to write variable secrets: %w", err)
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

	// sciontool already handles PID 1 responsibilities (zombie reaping, signal forwarding),
	// so we don't use --init to avoid competing init processes.
	newArgs := []string{"run", "-t"}

	// Apply resource constraints from config.
	if config.Resources != nil {
		if config.Resources.Limits.Memory != "" {
			bytes, err := util.ParseMemory(config.Resources.Limits.Memory)
			if err != nil {
				return "", fmt.Errorf("invalid memory limit %q: %w", config.Resources.Limits.Memory, err)
			}
			newArgs = append(newArgs, "--memory", util.FormatMemoryForDocker(bytes))
		}
		if config.Resources.Requests.Memory != "" {
			bytes, err := util.ParseMemory(config.Resources.Requests.Memory)
			if err != nil {
				return "", fmt.Errorf("invalid memory request %q: %w", config.Resources.Requests.Memory, err)
			}
			newArgs = append(newArgs, "--memory-reservation", util.FormatMemoryForDocker(bytes))
		}
		if config.Resources.Limits.CPU != "" {
			cores, err := util.ParseCPU(config.Resources.Limits.CPU)
			if err != nil {
				return "", fmt.Errorf("invalid cpu limit %q: %w", config.Resources.Limits.CPU, err)
			}
			newArgs = append(newArgs, "--cpus", util.FormatCPU(cores))
		}
	}

	newArgs = append(newArgs, args[1:]...)

	// Insert secret volume mounts before the image so they are treated as
	// docker flags rather than arguments to the container command.
	newArgs = insertVolumeFlags(newArgs, config.Image, secretMountSpecs)

	WriteRuntimeDebugFile(config, r.Command, newArgs)

	out, err := runSimpleCommand(ctx, r.Command, newArgs...)
	if err != nil {
		return "", fmt.Errorf("container run failed: %w (output: %s)", err, out)
	}

	return strings.TrimSpace(out), nil
}

func (r *DockerRuntime) Stop(ctx context.Context, id string) error {
	out, err := runSimpleCommand(ctx, r.Command, "stop", id)
	if err != nil && out != "" {
		// Include runtime's stderr output in the error so callers can match
		// on messages like "not running" or "No such container".
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	return err
}

func (r *DockerRuntime) Delete(ctx context.Context, id string) error {
	_, err := runSimpleCommand(ctx, r.Command, "rm", "-f", id)
	return err
}

type dockerListOutput struct {
	ID     string `json:"ID"`
	Names  string `json:"Names"`
	Status string `json:"Status"`
	Image  string `json:"Image"`
	Labels string `json:"Labels"`
}

func (r *DockerRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	args := []string{"ps", "-a", "--no-trunc", "--format", "{{json .}}"}
	cmd := exec.CommandContext(ctx, r.Command, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker ps failed: %w", err)
	}

	var agents []api.AgentInfo
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		var d dockerListOutput
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			continue
		}

		labels := make(map[string]string)
		for _, pair := range strings.Split(d.Labels, ",") {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 {
				labels[parts[0]] = parts[1]
			}
		}

		// Filter by labels if requested
		match := true
		for k, v := range labelFilter {
			actual := labels[k]
			// Fallback for project labels
			if actual == "" {
				switch k {
				case "scion.project":
					actual = labels["scion.grove"]
				case "scion.project_id":
					actual = labels["scion.grove_id"]
				case "scion.project_path":
					actual = labels["scion.grove_path"]
				}
			}

			if actual != v {
				match = false
				break
			}
		}

		if match {
			// Prefer the scion.name label (slugified) over Docker container name
			agentName := labels["scion.name"]
			if agentName == "" {
				agentName = d.Names
			}
			agents = append(agents, api.AgentInfo{
				ContainerID:     d.ID,
				Name:            agentName,
				ContainerStatus: d.Status,
				Phase:           phaseFromContainerStatus(d.Status),
				Image:           d.Image,
				Labels:          labels,
				Annotations:     labels,
				Template:        labels["scion.template"],
				HarnessConfig:   labels["scion.harness_config"],
				HarnessAuth:     labels["scion.harness_auth"],
				Project: func() string {
					if p := labels["scion.project"]; p != "" {
						return p
					}
					return labels["scion.grove"]
				}(),
				ProjectID: func() string {
					if p := labels["scion.project_id"]; p != "" {
						return p
					}
					return labels["scion.grove_id"]
				}(),
				ProjectPath: func() string {
					if p := labels["scion.project_path"]; p != "" {
						return p
					}
					return labels["scion.grove_path"]
				}(),
				Runtime: r.Name(),
			})
		}
	}

	return agents, nil
}

func (r *DockerRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	return runSimpleCommand(ctx, r.Command, "logs", id)
}

func (r *DockerRuntime) Attach(ctx context.Context, id string) error {
	// We need to find the container first to handle names properly
	agents, err := r.List(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		// Match by full container ID, short ID (12 chars), or name (with or without leading slash)
		if a.ContainerID == id || (len(id) >= 12 && strings.HasPrefix(a.ContainerID, id)) || (len(a.ContainerID) >= 12 && strings.HasPrefix(id, a.ContainerID)) ||
			a.Name == id || a.Name == "/"+id || strings.TrimPrefix(a.Name, "/") == id {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' container not found. It may have exited and been removed.", id)
	}

	// Check if running
	status := strings.ToLower(agent.ContainerStatus)
	if !strings.HasPrefix(status, "up") && status != "running" {
		return fmt.Errorf("agent '%s' is not running (status: %s). Use 'scion start %s' to resume it.", id, agent.ContainerStatus, id)
	}

	// Ensure tmux uses the latest client's terminal size so the session
	// redraws correctly on attach (handles containers started before the
	// window-size option was added to session creation).
	_, _ = runSimpleCommand(ctx, r.Command, "exec", "--user", "scion",
		agent.ContainerID, "tmux", "set-option", "-g", "window-size", "latest")

	return runInteractiveCommand(r.Command, "exec", "-it", "--user", "scion", agent.ContainerID, "tmux", "attach", "-t", "scion")
}

func (r *DockerRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	_, err := runSimpleCommand(ctx, r.Command, "image", "inspect", image)
	return err == nil, nil
}

func (r *DockerRuntime) PullImage(ctx context.Context, image string) error {
	return runInteractiveCommand(r.Command, "pull", image)
}

func (r *DockerRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {
	agents, err := r.List(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		// Match by full container ID, short ID (12 chars), or name (with or without leading slash)
		if a.ContainerID == id || (len(id) >= 12 && strings.HasPrefix(a.ContainerID, id)) || (len(a.ContainerID) >= 12 && strings.HasPrefix(id, a.ContainerID)) ||
			a.Name == id || a.Name == "/"+id || strings.TrimPrefix(a.Name, "/") == id {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' container not found", id)
	}

	// Check for GCS volumes
	if val, ok := agent.Labels["scion.gcs_volumes"]; ok && val != "" {
		decoded, err := base64.StdEncoding.DecodeString(val)
		if err != nil {
			return fmt.Errorf("failed to decode gcs volume info: %w", err)
		}

		type gcsVolInfo struct {
			Source string `json:"source"`
			Target string `json:"target"`
			Bucket string `json:"bucket"`
			Prefix string `json:"prefix"`
		}
		var vols []gcsVolInfo
		if err := json.Unmarshal(decoded, &vols); err != nil {
			return fmt.Errorf("failed to parse gcs volume info: %w", err)
		}

		for _, v := range vols {
			if v.Source == "" {
				continue
			}
			if direction == SyncTo {
				if err := gcp.SyncToGCS(ctx, v.Source, v.Bucket, v.Prefix); err != nil {
					return fmt.Errorf("failed to sync to GCS: %w", err)
				}
			} else if direction == SyncFrom {
				if err := gcp.SyncFromGCS(ctx, v.Bucket, v.Prefix, v.Source); err != nil {
					return fmt.Errorf("failed to sync from GCS: %w", err)
				}
			} else {
				return fmt.Errorf("sync direction must be specified for GCS volumes")
			}
		}
		return nil
	}

	// Docker runtime uses bind mounts for normal volumes, so sync is automatic/noop
	return nil
}

func (r *DockerRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	// Resolve slug/name to actual container ID (container names include the
	// grove prefix, e.g. "mygrove--agent", so the bare slug won't match).
	if agents, err := r.List(ctx, nil); err == nil {
		id = resolveContainerID(agents, id)
	}
	args := append([]string{"exec", "--user", "scion", id}, cmd...)
	return runSimpleCommand(ctx, r.Command, args...)
}

// GetWorkspacePath returns the host path to the container's /workspace mount.
func (r *DockerRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	// Use docker inspect to get mount information
	out, err := runSimpleCommand(ctx, r.Command, "inspect", "--format", "{{json .Mounts}}", id)
	if err != nil {
		return "", fmt.Errorf("failed to inspect container: %w", err)
	}

	type mountInfo struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Type        string `json:"Type"`
	}

	var mounts []mountInfo
	if err := json.Unmarshal([]byte(out), &mounts); err != nil {
		return "", fmt.Errorf("failed to parse mounts: %w", err)
	}

	// Look for /workspace mount
	for _, m := range mounts {
		if m.Destination == "/workspace" {
			return m.Source, nil
		}
	}

	return "", fmt.Errorf("no /workspace mount found for container %s", id)
}
