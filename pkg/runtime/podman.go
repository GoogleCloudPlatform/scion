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
	"os"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

type PodmanRuntime struct {
	Command  string
	Host     string
	Rootless bool // true when Podman is running in rootless mode
}

// NewPodmanRuntime creates a new PodmanRuntime after verifying that the podman
// binary exists and meets the minimum version requirement (4.x+).
// Returns an ErrorRuntime if validation fails.
func NewPodmanRuntime() Runtime {
	command := "podman"

	// Verify podman is on PATH
	path, err := exec.LookPath(command)
	if err != nil {
		return &ErrorRuntime{Err: fmt.Errorf("podman not found on PATH: %w", err)}
	}
	_ = path

	// Check version: minimum 4.x
	out, err := exec.Command(command, "--version").Output()
	if err != nil {
		return &ErrorRuntime{Err: fmt.Errorf("failed to get podman version: %w", err)}
	}

	version := parsePodmanVersion(strings.TrimSpace(string(out)))
	major, err := parseMajorVersion(version)
	if err != nil {
		return &ErrorRuntime{Err: fmt.Errorf("failed to parse podman version %q: %w", version, err)}
	}
	if major < 4 {
		return &ErrorRuntime{Err: fmt.Errorf("podman version %s is below the minimum supported version 4.x", version)}
	}

	// Detect and store rootless mode
	rootless := detectRootlessMode(command)

	return &PodmanRuntime{
		Command:  command,
		Rootless: rootless,
	}
}

// parsePodmanVersion extracts the version string from "podman version X.Y.Z" output.
func parsePodmanVersion(output string) string {
	// Output format: "podman version X.Y.Z"
	parts := strings.Fields(output)
	if len(parts) >= 3 {
		return parts[len(parts)-1]
	}
	return output
}

// parseMajorVersion extracts the major version number from a semver string.
func parseMajorVersion(version string) (int, error) {
	parts := strings.Split(version, ".")
	if len(parts) < 1 {
		return 0, fmt.Errorf("unexpected version format: %s", version)
	}
	return strconv.Atoi(parts[0])
}

// detectRootlessMode checks whether Podman is running in rootless mode.
// Returns true when rootless, false otherwise.
func detectRootlessMode(command string) bool {
	out, err := exec.Command(command, "info", "--format", "{{.Host.Security.Rootless}}").Output()
	if err != nil {
		util.Debugf("podman: failed to detect rootless mode: %v", err)
		return false
	}
	rootless := strings.TrimSpace(string(out))
	util.Debugf("podman: rootless mode = %s", rootless)
	return rootless == "true"
}

func (r *PodmanRuntime) Name() string {
	return "podman"
}

// ExecUser implements Runtime. Always returns "scion" because sciontool
// init drops privileges to the scion user (via SCION_HOST_UID) before
// launching the tmux session — even in rootless mode. The exec user must
// match the tmux socket owner (UID 1000 = scion), not the container's
// initial PID 1 user.
//
// The previous behaviour returned "root" for rootless podman, assuming
// the host UID mapped to container UID 0 and no privilege drop occurred.
// This was incorrect when the image starts as root (USER root) and
// sciontool drops to scion, placing the tmux socket at /tmp/tmux-1000/
// while the broker looked at /tmp/tmux-0/.
func (r *PodmanRuntime) ExecUser() string {
	return "scion"
}

func (r *PodmanRuntime) Run(ctx context.Context, config RunConfig) (string, error) {
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
	// podman flags rather than arguments to the container command.
	newArgs = insertVolumeFlags(newArgs, config.Image, secretMountSpecs)

	WriteRuntimeDebugFile(config, r.Command, newArgs)

	out, err := runSimpleCommand(ctx, r.Command, newArgs...)
	if err != nil {
		return "", fmt.Errorf("container run failed: %w (output: %s)", err, out)
	}

	return strings.TrimSpace(out), nil
}

func (r *PodmanRuntime) Stop(ctx context.Context, id string) error {
	out, err := runSimpleCommand(ctx, r.Command, "stop", id)
	if err != nil && out != "" {
		// Include podman's stderr output in the error so callers can match
		// on messages like "not running" (which runSimpleCommand's error
		// wrapping would otherwise discard).
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	return err
}

func (r *PodmanRuntime) Delete(ctx context.Context, id string) error {
	_, err := runSimpleCommand(ctx, r.Command, "rm", "-f", id)
	return err
}

// podmanListOutput represents the JSON structure returned by "podman ps --format json".
// Unlike Docker which returns newline-separated JSON objects with string fields,
// Podman returns a JSON array with different field names and types:
//   - Id (not ID)
//   - Names is an array (not a single string)
//   - Labels is a map (not CSV)
type podmanListOutput struct {
	Id     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Status string            `json:"Status"`
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

func (r *PodmanRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	args := []string{"ps", "-a", "--no-trunc", "--format", "json"}
	cmd := exec.CommandContext(ctx, r.Command, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("podman ps failed: %w", err)
	}

	trimmed := strings.TrimSpace(string(out))

	// Podman returns a JSON array (possibly empty or "null"/"[]")
	var containers []podmanListOutput
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(trimmed), &containers); err != nil {
		return nil, fmt.Errorf("failed to parse podman ps output: %w", err)
	}

	var agents []api.AgentInfo
	for _, c := range containers {
		labels := c.Labels
		if labels == nil {
			labels = make(map[string]string)
		}

		// Filter by labels if requested
		match := true
		for k, v := range labelFilter {
			if labels[k] != v {
				match = false
				break
			}
		}

		if match {
			name := ""
			if len(c.Names) > 0 {
				name = c.Names[0]
			}

			agents = append(agents, api.AgentInfo{
				ContainerID:     c.Id,
				Name:            name,
				ContainerStatus: c.Status,
				Phase:           "created",
				Image:           c.Image,
				Labels:          labels,
				Annotations:     labels,
				Template:        labels["scion.template"],
				HarnessConfig:   labels["scion.harness_config"],
				HarnessAuth:     labels["scion.harness_auth"],
				Grove:           labels["scion.grove"],
				GroveID:         labels["scion.grove_id"],
				GrovePath:       labels["scion.grove_path"],
				Runtime:         r.Name(),
			})
		}
	}

	return agents, nil
}

func (r *PodmanRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	return runSimpleCommand(ctx, r.Command, "logs", id)
}

func (r *PodmanRuntime) Attach(ctx context.Context, id string) error {
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

	return runInteractiveCommand(r.Command, "exec", "-it", "--user", r.ExecUser(), agent.ContainerID, "tmux", "attach", "-t", "scion")
}

func (r *PodmanRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	_, err := runSimpleCommand(ctx, r.Command, "image", "inspect", image)
	return err == nil, nil
}

func (r *PodmanRuntime) PullImage(ctx context.Context, image string) error {
	return runInteractiveCommand(r.Command, "pull", image)
}

func (r *PodmanRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {
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

	// Podman runtime uses bind mounts for normal volumes, so sync is automatic/noop
	return nil
}

func (r *PodmanRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	args := append([]string{"exec", "--user", r.ExecUser(), id}, cmd...)
	return runSimpleCommand(ctx, r.Command, args...)
}

// GetWorkspacePath returns the host path to the container's /workspace mount.
func (r *PodmanRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	// Use podman inspect to get mount information
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

// validatePodmanMachineMounts checks that the workspace path is within the user's
// home directory when running on macOS with Podman Machine. Podman Machine exposes
// $HOME via virtiofs by default; paths outside $HOME won't be accessible in the VM.
func validatePodmanMachineMounts(workspace string) error {
	if goruntime.GOOS != "darwin" {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil // Can't determine home dir, skip validation
	}

	if !strings.HasPrefix(workspace, home) {
		return fmt.Errorf(
			"workspace path %q is outside your home directory (%s). "+
				"Podman Machine on macOS exposes $HOME via virtiofs by default. "+
				"Either move your workspace under $HOME or configure additional "+
				"Podman Machine mounts with: podman machine init --volume <path>:<path>",
			workspace, home,
		)
	}

	return nil
}
