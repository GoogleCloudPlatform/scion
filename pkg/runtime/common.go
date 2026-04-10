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
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Well-known secret name and env var for GCP telemetry credentials.
const (
	telemetryGCPCredentialsSecretName = "scion-telemetry-gcp-credentials"
	telemetryGCPCredentialsEnvVar     = "SCION_OTEL_GCP_CREDENTIALS"
)

// findGCPTelemetryCredentialPath scans the resolved secrets for the well-known
// GCP telemetry credential file secret and returns the expanded container target
// path. Returns "" if the secret is not present or is not a file type.
func findGCPTelemetryCredentialPath(secrets []api.ResolvedSecret, containerHome string) string {
	for _, s := range secrets {
		if s.Name == telemetryGCPCredentialsSecretName && s.Type == "file" {
			return expandTildeTarget(s.Target, containerHome)
		}
	}
	return ""
}

// ResolveContainerWorkspace computes the container-side workspace path based on
// the workspace strategy. For git worktree setups where the workspace is under
// the repo root, this returns /repo-root/<relPath>. Otherwise it returns /workspace.
func ResolveContainerWorkspace(repoRoot, workspace string, gitClone *api.GitCloneConfig) string {
	if workspace == "" {
		return "/workspace"
	}
	if gitClone != nil {
		return "/workspace"
	}
	if repoRoot != "" {
		relWorkspace, err := filepath.Rel(repoRoot, workspace)
		if err == nil && !strings.HasPrefix(relWorkspace, "..") && relWorkspace != "." {
			return filepath.Join("/repo-root", relWorkspace)
		}
	}
	return "/workspace"
}

// buildCommonRunArgs constructs the common arguments for 'run' command across different runtimes.
func buildCommonRunArgs(config RunConfig) ([]string, error) {
	args := []string{"run", "-d", "-i"}
	addArg := func(flag string, values ...string) {
		for _, v := range values {
			args = append(args, flag, v)
		}
	}
	addEnv := func(name, value string) {
		if value != "" {
			addArg("-e", fmt.Sprintf("%s=%s", name, value))
		}
	}

	hostHome, _ := os.UserHomeDir()
	expandPath := func(path string, isTarget bool) string {
		// Expand environment variables first (e.g., ${GOPATH}, $HOME)
		expanded, _ := util.ExpandEnv(path)

		// Then handle tilde expansion
		if strings.HasPrefix(expanded, "~/") {
			if isTarget {
				return filepath.Join(util.GetHomeDir(config.UnixUsername), expanded[2:])
			}
			return filepath.Join(hostHome, expanded[2:])
		}
		if expanded == "~" {
			if isTarget {
				return util.GetHomeDir(config.UnixUsername)
			}
			return hostHome
		}
		return expanded
	}

	// Volume deduplication
	volumeMap := make(map[string]string)
	var volumeOrder []string

	registerMount := func(src, tgt string, ro bool, overwrite bool) {
		val := fmt.Sprintf("%s:%s", src, tgt)
		if ro {
			val += ":ro"
		}
		if _, exists := volumeMap[tgt]; !exists {
			volumeOrder = append(volumeOrder, tgt)
			volumeMap[tgt] = val
		} else if overwrite {
			volumeMap[tgt] = val
		}
	}

	var fuseMounts []string
	type gcsVolInfo struct {
		Source string `json:"source"`
		Target string `json:"target"`
		Bucket string `json:"bucket"`
		Prefix string `json:"prefix"`
	}
	var gcsVolumes []gcsVolInfo

	addVolume := func(v api.VolumeMount) {
		tgt := expandPath(v.Target, true)

		if v.Type == "gcs" {
			// Do not register as docker bind mount
			cmd := fmt.Sprintf("mkdir -p %q && gcsfuse ", tgt)
			if v.Prefix != "" {
				cmd += fmt.Sprintf("--only-dir %q ", v.Prefix)
			}
			if v.Mode != "" {
				cmd += fmt.Sprintf("-o %q ", v.Mode)
			}
			// Add implicit dirs for better compatibility with folder structures created via UI/API
			cmd += "--implicit-dirs "

			cmd += fmt.Sprintf("%q %q", v.Bucket, tgt)
			fuseMounts = append(fuseMounts, cmd)

			gcsVolumes = append(gcsVolumes, gcsVolInfo{
				Source: expandPath(v.Source, false),
				Target: tgt,
				Bucket: v.Bucket,
				Prefix: v.Prefix,
			})
			return
		}

		src := expandPath(v.Source, false)
		// Generic volumes from config should NOT overwrite already registered mounts (like workspace)
		registerMount(src, tgt, v.ReadOnly, false)
	}

	addArg("--name", config.Name)

	if config.NetworkMode != "" {
		addArg("--network", config.NetworkMode)
	}

	if config.HomeDir != "" {
		registerMount(config.HomeDir, util.GetHomeDir(config.UnixUsername), false, true)
	}
	fullRepoRootMounted := false
	if config.GitClone != nil {
		// Git clone mode: mount the host-side workspace directory so the
		// cloned repo is visible on the host for debugging and persistence.
		if config.Workspace != "" {
			registerMount(config.Workspace, "/workspace", false, true)
		}
		addArg("--workdir", "/workspace")
	} else if config.RepoRoot != "" && config.Workspace != "" {
		relWorkspace, err := filepath.Rel(config.RepoRoot, config.Workspace)
		if err == nil && !strings.HasPrefix(relWorkspace, "..") && relWorkspace != "." {
			// Worktree case: workspace is a subdirectory of repo root.
			// Mount .git separately and workspace at its relative path.
			registerMount(filepath.Join(config.RepoRoot, ".git"), "/repo-root/.git", false, true)
			containerWorkspace := filepath.Join("/repo-root", relWorkspace)
			registerMount(config.Workspace, containerWorkspace, false, true)
			addArg("--workdir", containerWorkspace)
		} else if relWorkspace == "." {
			// Shared workspace: workspace IS the repo root (e.g., shared git clone).
			// Mount directly to /workspace so harnesses can trust a single path.
			registerMount(config.Workspace, "/workspace", false, true)
			addArg("--workdir", "/workspace")
		} else {
			// Fallback if workspace is outside repo root or relative path is not straightforward.
			// Still mount RepoRoot so that .git worktree pointers can potentially be resolved if
			// we are clever, but for now just mount both.
			registerMount(config.RepoRoot, "/repo-root", false, true)
			registerMount(config.Workspace, "/workspace", false, true)
			addArg("--workdir", "/workspace")
			fullRepoRootMounted = true
		}
	} else if config.Workspace != "" {
		registerMount(config.Workspace, "/workspace", false, true)
		addArg("--workdir", "/workspace")
	}

	// Add generic volumes from config, deduplicating among themselves first
	// but respecting already registered mounts.
	dedupedVolumes := make(map[string]api.VolumeMount)
	var dedupedOrder []string
	for _, v := range config.Volumes {
		tgt := expandPath(v.Target, true)
		if _, exists := dedupedVolumes[tgt]; !exists {
			dedupedOrder = append(dedupedOrder, tgt)
		}
		dedupedVolumes[tgt] = v
	}
	for _, tgt := range dedupedOrder {
		addVolume(dedupedVolumes[tgt])
	}

	// If workdir was not set by the RepoRoot/Workspace logic above, check if we have an explicit
	// volume mount for /workspace and if so set workdir to it.
	workdirSet := false
	for _, arg := range args {
		if arg == "--workdir" {
			workdirSet = true
			break
		}
	}
	if !workdirSet {
		for _, v := range dedupedVolumes {
			if expandPath(v.Target, true) == "/workspace" {
				addArg("--workdir", "/workspace")
				break
			}
		}
	}

	// Use Harness for file propagation and env
	if config.Harness != nil {
		// Apply resolved auth (env vars + files) from the new auth pipeline
		if config.ResolvedAuth != nil {
			if err := applyResolvedAuth(config, addEnv, addVolume, registerMount); err != nil {
				return nil, err
			}
		}
		// Call GetEnv for non-auth env vars (system prompt, agent name, etc.)
		for k, v := range config.Harness.GetEnv(config.Name, config.HomeDir, config.UnixUsername) {
			addEnv(k, v)
		}
		if config.TelemetryEnabled {
			for k, v := range config.Harness.GetTelemetryEnv() {
				addEnv(k, v)
			}
		}
	}

	// Pass host user UID/GID for container user synchronization
	addEnv("SCION_HOST_UID", fmt.Sprintf("%d", os.Getuid()))
	addEnv("SCION_HOST_GID", fmt.Sprintf("%d", os.Getgid()))

	// Mount gcloud config if it exists on the host (local mode only).
	// In broker mode, credentials are projected via ResolvedSecrets;
	// mounting the broker operator's gcloud dir would leak credentials.
	if !config.BrokerMode {
		home, _ := os.UserHomeDir()
		gcloudConfigDir := filepath.Join(home, ".config", "gcloud")
		if _, err := os.Stat(gcloudConfigDir); err == nil {
			// Pre-create the mount-point directory inside the agent home so that
			// Docker does not create it as root (which would make the agent
			// directory undeletable by a non-root broker process).
			if config.HomeDir != "" {
				mountPoint := filepath.Join(config.HomeDir, ".config", "gcloud")
				_ = os.MkdirAll(mountPoint, 0755)
			}
			registerMount(gcloudConfigDir, fmt.Sprintf("/home/%s/.config/gcloud", config.UnixUsername), true, false)
		}
	}

	for _, e := range config.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			addArg("-e", fmt.Sprintf("%s=%s", parts[0], parts[1]))
		} else {
			addArg("-e", e)
		}
	}

	// Inject environment-type resolved secrets
	for _, s := range config.ResolvedSecrets {
		if s.Type == "environment" || s.Type == "" {
			addArg("-e", fmt.Sprintf("%s=%s", s.Target, s.Value))
		}
	}

	// Dev-mode binary override: if SCION_DEV_BINARIES points to a local
	// directory containing scion/sciontool binaries, bind-mount it to
	// /opt/scion/bin which has highest PATH priority in the container.
	// This allows rapid iteration without rebuilding images.
	if devBinDir := os.Getenv("SCION_DEV_BINARIES"); devBinDir != "" {
		if abs, err := filepath.Abs(devBinDir); err == nil {
			if info, err := os.Stat(abs); err == nil && info.IsDir() {
				registerMount(abs, "/opt/scion/bin", true, true)
			}
		}
	}

	// Add all registered volumes
	for _, tgt := range volumeOrder {
		addArg("-v", volumeMap[tgt])
	}

	// Shadow the .scion directory with a tmpfs when the full repo root is
	// mounted into the container. This prevents agents from accessing other
	// agents' home directories and secrets via the host filesystem.
	if fullRepoRootMounted {
		addArg("--mount", "type=tmpfs,destination=/repo-root/.scion")
	}

	// Add NET_ADMIN capability for iptables-based metadata server interception
	if config.MetadataInterception {
		addArg("--cap-add", "NET_ADMIN")
	}

	// Add extra /etc/hosts entries (e.g. for host.docker.internal on Linux)
	for _, h := range config.ExtraHosts {
		addArg("--add-host", h)
	}

	if len(fuseMounts) > 0 {
		addArg("--cap-add", "SYS_ADMIN")
		addArg("--device", "/dev/fuse")
		if data, err := json.Marshal(gcsVolumes); err == nil {
			encoded := base64.StdEncoding.EncodeToString(data)
			addArg("--label", fmt.Sprintf("scion.gcs_volumes=%s", encoded))
		}
	}

	for k, v := range config.Labels {
		addArg("--label", fmt.Sprintf("%s=%s", k, v))
	}
	for k, v := range config.Annotations {
		addArg("--label", fmt.Sprintf("%s=%s", k, v))
	}
	if config.Template != "" {
		addArg("--label", fmt.Sprintf("scion.template=%s", config.Template))
	}

	args = append(args, config.Image)

	// Get command from harness
	var harnessArgs []string
	if config.Harness != nil {
		harnessArgs = config.Harness.GetCommand(config.Task, config.Resume, config.CommandArgs)
	} else {
		return nil, fmt.Errorf("no harness provided")
	}

	// Build tmux-wrapped command
	var quotedArgs []string
	for _, a := range harnessArgs {
		if strings.ContainsAny(a, " \t\n\"'$") {
			quotedArgs = append(quotedArgs, fmt.Sprintf("%q", a))
		} else {
			quotedArgs = append(quotedArgs, a)
		}
	}
	cmdLine := strings.Join(quotedArgs, " ")

	// Build tmux command: create session with "agent" window running the harness,
	// then add a "shell" window and switch back to the agent window.
	tmuxCmd := fmt.Sprintf(
		"tmux new-session -d -s scion -n agent %s \\; new-window -t scion -n shell \\; select-window -t scion:agent \\; attach-session -t scion",
		cmdLine,
	)

	if len(fuseMounts) > 0 {
		mountCmds := strings.Join(fuseMounts, " && ")
		wrapped := fmt.Sprintf("%s && exec sh -c %q", mountCmds, tmuxCmd)
		args = append(args, "sh", "-c", wrapped)
	} else {
		args = append(args, "sh", "-c", tmuxCmd)
	}

	return args, nil
}

// runtimeLog is the structured logger for runtime command execution.
var runtimeLog = slog.Default().With(slog.String("subsystem", "runtime"))

func runSimpleCommand(ctx context.Context, command string, args ...string) (string, error) {
	cmdStr := command + " " + strings.Join(args, " ")
	runtimeLog.Debug("Executing command", "cmd", cmdStr)
	start := time.Now()
	cmd := exec.CommandContext(ctx, command, args...)
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if err != nil {
		runtimeLog.Debug("Command failed", "cmd", cmdStr, "duration", elapsed, "output", strings.TrimSpace(string(out)))
		return string(out), fmt.Errorf("%s %s failed: %w", command, strings.Join(args, " "), err)
	}
	runtimeLog.Debug("Command completed", "cmd", cmdStr, "duration", elapsed)
	return strings.TrimSpace(string(out)), nil
}

func runInteractiveCommand(command string, args ...string) error {
	runtimeLog.Debug("Executing interactive command", "cmd", command+" "+strings.Join(args, " "))
	cmd := exec.Command(command, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// insertVolumeFlags inserts -v flags for the given mount specs before the image
// in an args slice. This ensures volume mounts appear as runtime flags rather
// than being appended after the image and container command.
func insertVolumeFlags(args []string, image string, mountSpecs []string) []string {
	if len(mountSpecs) == 0 {
		return args
	}

	// Find the image position (search from end since it's near the tail)
	imageIdx := -1
	for i := len(args) - 1; i >= 0; i-- {
		if args[i] == image {
			imageIdx = i
			break
		}
	}

	var mountArgs []string
	for _, spec := range mountSpecs {
		mountArgs = append(mountArgs, "-v", spec)
	}

	if imageIdx < 0 {
		// Fallback: shouldn't happen, but append before end as best-effort
		return append(args, mountArgs...)
	}

	result := make([]string, 0, len(args)+len(mountArgs))
	result = append(result, args[:imageIdx]...)
	result = append(result, mountArgs...)
	result = append(result, args[imageIdx:]...)
	return result
}

// WriteRuntimeDebugFile writes the full runtime execution command to a debug
// file inside the agent directory for diagnostic purposes. The command is
// formatted with one argument per line using backslash continuation characters
// for readability. The file is written to <agentDir>/runtime-exec-debug.
// This is a no-op if config.Debug is false or HomeDir is empty.
func WriteRuntimeDebugFile(config RunConfig, command string, args []string) {
	if !config.Debug || config.HomeDir == "" {
		return
	}
	agentDir := filepath.Dir(config.HomeDir)
	debugPath := filepath.Join(agentDir, "runtime-exec-debug")

	var buf strings.Builder
	buf.WriteString(command)
	for _, arg := range args {
		buf.WriteString(" \\\n  ")
		buf.WriteString(arg)
	}
	buf.WriteString("\n")

	if err := os.WriteFile(debugPath, []byte(buf.String()), 0644); err != nil {
		runtimeLog.Debug("Failed to write runtime debug file", "path", debugPath, "error", err)
	}
}

// expandTildeTarget expands a ~/ prefix in a target path to the container user's
// home directory. Paths without ~/ are returned unchanged.
func expandTildeTarget(target, containerHome string) string {
	if strings.HasPrefix(target, "~/") {
		return filepath.Join(containerHome, target[2:])
	}
	return target
}

// ResolveDockerNetworking checks whether Docker host networking should be used
// to allow containers to reach services on the host's loopback interface.
// When the hub endpoint is localhost or was translated to a Docker bridge
// hostname (host.docker.internal), it returns "host" and rewrites any bridge
// hostnames back to localhost in the env map. This avoids the need for the
// server to bind to 0.0.0.0.
//
// For non-Docker runtimes or non-localhost endpoints, returns "" (no override).
func ResolveDockerNetworking(runtimeName string, env map[string]string) string {
	if runtimeName != "docker" {
		return ""
	}

	ep := env["SCION_HUB_ENDPOINT"]
	if ep == "" {
		ep = env["SCION_HUB_URL"]
	}
	if ep == "" {
		return ""
	}

	// If endpoint uses the Docker bridge hostname (translated from localhost),
	// rewrite back to localhost since host networking makes it reachable directly.
	if strings.Contains(ep, "host.docker.internal") {
		for _, key := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL"} {
			if v, ok := env[key]; ok {
				env[key] = strings.Replace(v, "host.docker.internal", "localhost", 1)
			}
		}
		return "host"
	}

	// If endpoint is localhost, containers need host networking to reach it.
	u, err := url.Parse(ep)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return "host"
	}

	return ""
}

// BridgeExtraHosts returns the --add-host entries needed for the given runtime
// when a bridge hostname (e.g. host.docker.internal) is used in environment
// variables. On Linux, Docker does not automatically resolve
// host.docker.internal; the "host-gateway" special address must be mapped
// explicitly. Podman resolves host.containers.internal natively, so no extra
// hosts are needed.
func BridgeExtraHosts(runtimeName string, env []string) []string {
	if runtimeName != "docker" {
		return nil
	}
	// Check if any env value references host.docker.internal
	for _, e := range env {
		if strings.Contains(e, "host.docker.internal") {
			return []string{"host.docker.internal:host-gateway"}
		}
	}
	return nil
}

// applyResolvedAuth injects ResolvedAuth env vars and files into the container
// args. For files, it either copies into HomeDir (file copy mode) or registers
// a read-only bind mount (volume mount mode).
func applyResolvedAuth(config RunConfig, addEnv func(string, string), addVolume func(api.VolumeMount), registerMount func(string, string, bool, bool)) error {
	ra := config.ResolvedAuth

	// Inject env vars
	for k, v := range ra.EnvVars {
		addEnv(k, v)
	}

	// Inject files
	containerHome := util.GetHomeDir(config.UnixUsername)
	for _, f := range ra.Files {
		containerPath := expandTildeTarget(f.ContainerPath, containerHome)

		if config.HomeDir != "" {
			// File copy mode: copy SourcePath into HomeDir at the relative
			// path derived from containerPath within the container home.
			var relPath string
			if strings.HasPrefix(containerPath, containerHome+"/") {
				relPath = strings.TrimPrefix(containerPath, containerHome+"/")
			} else {
				relPath = strings.TrimPrefix(containerPath, "/")
			}
			dst := filepath.Join(config.HomeDir, relPath)
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return fmt.Errorf("failed to create directory for auth file %s: %w", dst, err)
			}
			if err := util.CopyFile(f.SourcePath, dst); err != nil {
				return fmt.Errorf("failed to copy auth file %s → %s: %w", f.SourcePath, dst, err)
			}
		} else {
			// Volume mount mode: register a read-only bind mount
			registerMount(f.SourcePath, containerPath, true, false)
		}
	}

	return nil
}

// writeFileSecrets writes file-type secrets to a staging directory and returns
// bind-mount specs that should be added to the container run command.
// The staging directory is created as a sibling of homeDir: <parent>/secrets/<name>/
// The containerHome parameter is the container user's home directory (e.g., /home/gemini)
// and is used to expand ~/ prefixes in target paths.
func writeFileSecrets(homeDir string, containerHome string, secrets []api.ResolvedSecret) ([]string, error) {
	secretsDir := filepath.Join(filepath.Dir(homeDir), "secrets")

	// Deduplicate by container target path (defense-in-depth — the secret
	// backend should already have resolved target conflicts, but if multiple
	// file secrets still share a target we keep only the last one, which
	// corresponds to the highest-scope entry when the list is ordered).
	type fileEntry struct {
		secret api.ResolvedSecret
		index  int
	}
	targetMap := make(map[string]fileEntry)
	var targetOrder []string
	idx := 0
	for _, s := range secrets {
		if s.Type != "file" {
			continue
		}
		containerTarget := expandTildeTarget(s.Target, containerHome)
		if _, exists := targetMap[containerTarget]; !exists {
			targetOrder = append(targetOrder, containerTarget)
		} else {
			slog.Warn("writeFileSecrets: duplicate container target, keeping later entry",
				"target", containerTarget,
				"kept", s.Name,
				"replaced", targetMap[containerTarget].secret.Name,
			)
		}
		targetMap[containerTarget] = fileEntry{secret: s, index: idx}
		idx++
	}

	var mountSpecs []string
	for _, containerTarget := range targetOrder {
		entry := targetMap[containerTarget]
		s := entry.secret

		// Decode base64-encoded file content
		data, err := base64.StdEncoding.DecodeString(s.Value)
		if err != nil {
			// Fall back to raw value if not base64-encoded
			data = []byte(s.Value)
		}

		// Write to staging dir using the secret name as filename
		hostPath := filepath.Join(secretsDir, s.Name)
		if err := os.MkdirAll(filepath.Dir(hostPath), 0700); err != nil {
			return nil, fmt.Errorf("failed to create secret directory: %w", err)
		}
		if err := os.WriteFile(hostPath, data, 0600); err != nil {
			return nil, fmt.Errorf("failed to write secret file %s: %w", s.Name, err)
		}

		// Pre-create the parent directory of the mount target inside the
		// agent home so that Docker/Podman does not create it as root
		// (which would make the agent directory undeletable by a non-root
		// broker process).
		if homeDir != "" && strings.HasPrefix(containerTarget, containerHome+"/") {
			rel := strings.TrimPrefix(containerTarget, containerHome+"/")
			parentDir := filepath.Dir(rel)
			if parentDir != "." {
				_ = os.MkdirAll(filepath.Join(homeDir, parentDir), 0755)
			}
		}

		// Bind-mount from host staging path to container target path (read-only)
		mountSpecs = append(mountSpecs, fmt.Sprintf("%s:%s:ro", hostPath, containerTarget))
	}

	return mountSpecs, nil
}

// writeVariableSecrets writes variable-type secrets to ~/.scion/secrets.json
// inside the agent's home directory for programmatic access.
func writeVariableSecrets(homeDir string, secrets []api.ResolvedSecret) error {
	vars := make(map[string]string)
	for _, s := range secrets {
		if s.Type != "variable" {
			continue
		}
		vars[s.Target] = s.Value
	}

	if len(vars) == 0 {
		return nil
	}

	scionDir := filepath.Join(homeDir, ".scion")
	if err := os.MkdirAll(scionDir, 0700); err != nil {
		return fmt.Errorf("failed to create .scion directory: %w", err)
	}

	data, err := json.Marshal(vars)
	if err != nil {
		return fmt.Errorf("failed to marshal secrets.json: %w", err)
	}

	return os.WriteFile(filepath.Join(scionDir, "secrets.json"), data, 0600)
}

// secretMapEntry describes a file secret for the Apple runtime's secret-map.json.
type secretMapEntry struct {
	Name   string `json:"name"`
	Target string `json:"target"`
	Source string `json:"source"` // relative path within the secrets volume
}

// writeSecretMap writes a secret-map.json manifest that the Apple container runtime
// uses to copy file secrets from the shared volume to their target paths.
// The containerHome parameter is used to expand ~/ prefixes in target paths.
func writeSecretMap(homeDir string, containerHome string, secrets []api.ResolvedSecret) error {
	// Deduplicate by container target (mirrors writeFileSecrets logic)
	seen := make(map[string]bool)
	var entries []secretMapEntry
	for _, s := range secrets {
		if s.Type != "file" {
			continue
		}
		target := expandTildeTarget(s.Target, containerHome)
		if seen[target] {
			continue
		}
		seen[target] = true
		entries = append(entries, secretMapEntry{
			Name:   s.Name,
			Target: target,
			Source: s.Name, // filename within secrets/ volume
		})
	}

	if len(entries) == 0 {
		return nil
	}

	secretsDir := filepath.Join(filepath.Dir(homeDir), "secrets")
	if err := os.MkdirAll(secretsDir, 0700); err != nil {
		return fmt.Errorf("failed to create secrets directory: %w", err)
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("failed to marshal secret-map.json: %w", err)
	}

	return os.WriteFile(filepath.Join(secretsDir, "secret-map.json"), data, 0600)
}

// phaseFromContainerStatus derives an agent phase from a container status string.
// Container runtimes (podman, docker, apple) report status strings like
// "Up 5 minutes", "Exited (0) 3 hours ago", or "Created". This function
// maps those to lifecycle phases so heartbeats report accurate state
// regardless of whether agent-info.json has been updated.
func phaseFromContainerStatus(status string) string {
	lower := strings.ToLower(status)
	switch {
	case strings.HasPrefix(lower, "up") || lower == "running":
		return "running"
	case strings.HasPrefix(lower, "exited") || lower == "stopped":
		return "stopped"
	default:
		return "created"
	}
}
