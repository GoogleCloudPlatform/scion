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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	stagedsecrets "github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
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

// shellQuote returns s quoted for safe embedding in a POSIX shell command.
// It uses single quotes, which prevent all shell interpretation (variable
// expansion, command substitution via backticks or $(), globbing, etc.).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
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
			//
			// Sibling agents share this exact mount, so per-agent state must not
			// live under <workspace>/.scion/agents/ on the broker — that path
			// would be visible to every container in the project. Provisioning
			// relocates prompt.md and scion-agent.json to
			// ~/.scion/project-configs/<slug>__<uuid>/.scion/agents/<name>/
			// (config.GetAgentDir with sharedWorkspace=true), so there is
			// nothing to leak through this mount. See
			// .design/hub-shared-workspace-isolation.md (defense by absence).
			// If the threat model ever requires in-container shadowing, mirror
			// the /repo-root/.scion tmpfs pattern below at
			// /workspace/.scion/agents.
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

	// Pass host user UID/GID for container user synchronization.
	// N1-5: branch on workspace backend — NFS needs a stable, node-independent
	// UID/GID (default 1000:1000) so files written by agents on different nodes
	// have consistent ownership on the shared filesystem. The local backend
	// continues to use the broker's host UID/GID (today's behavior, unchanged).
	uid, gid := os.Getuid(), os.Getgid()
	if config.WorkspaceBackendName == "nfs" {
		uid, gid = config.NFSUID, config.NFSGID
		if uid == 0 {
			uid = 1000 // default stable NFS UID
		}
		if gid == 0 {
			gid = 1000 // default stable NFS GID
		}
	}
	addEnv("SCION_HOST_UID", fmt.Sprintf("%d", uid))
	addEnv("SCION_HOST_GID", fmt.Sprintf("%d", gid))

	// Expose the workspace backend to the container so sciontool init can
	// skip the per-start recursive chown when backend=nfs (slow/racy over
	// the network; ownership is set once by operator + provisioner).
	if config.WorkspaceBackendName != "" {
		addEnv("SCION_WORKSPACE_BACKEND", config.WorkspaceBackendName)
	}

	// Phase 3 & 5: Project identity injection
	addEnv("SCION_PROJECT", config.Project)
	addEnv("SCION_GROVE", config.Project)
	addEnv("SCION_PROJECT_ID", config.ProjectID)
	addEnv("SCION_GROVE_ID", config.ProjectID)

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

	// Pre-create parent directories on the host for any volume targeting a
	// path under the container home directory.  When the home dir is itself a
	// bind mount, the container runtime (crun/runc) will try to mkdir the
	// mount-point inside that mount.  On rootless Podman with VirtioFS (macOS)
	// this fails with "Permission denied" unless the directory already exists.
	if config.HomeDir != "" {
		containerHome := util.GetHomeDir(config.UnixUsername)
		for _, tgt := range volumeOrder {
			if strings.HasPrefix(tgt, containerHome+"/") {
				rel := strings.TrimPrefix(tgt, containerHome+"/")
				hostDir := filepath.Join(config.HomeDir, rel)
				_ = os.MkdirAll(hostDir, 0755)
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

	// Phase 5: Standard project labels
	if config.Project != "" {
		addArg("--label", fmt.Sprintf("%s=%s", projectcompat.LabelProject, config.Project))
		addArg("--label", fmt.Sprintf("%s=%s", projectcompat.LabelGrove, config.Project))
	}
	if config.ProjectID != "" {
		addArg("--label", fmt.Sprintf("%s=%s", projectcompat.LabelProjectID, config.ProjectID))
		addArg("--label", fmt.Sprintf("%s=%s", projectcompat.LabelGroveID, config.ProjectID))
	}

	if config.Template != "" {
		addArg("--label", fmt.Sprintf("scion.template=%s", config.Template))
	}

	// Get command from harness
	var harnessArgs []string
	if config.NoAuth {
		if config.NoAuthMessage != "" {
			harnessArgs = []string{"sh", "-c", fmt.Sprintf("printf '%%s\\n' %s; exec bash", shellQuote(config.NoAuthMessage))}
		} else {
			harnessArgs = []string{"bash"}
		}
	} else if config.Harness != nil {
		harnessArgs = config.Harness.GetCommand(config.Task, config.Resume, config.CommandArgs)
	} else {
		return nil, fmt.Errorf("no harness provided")
	}

	// Build tmux-wrapped command — use POSIX single-quote escaping so that
	// shell metacharacters (backticks, $, etc.) in the task prompt are not
	// interpreted by sh -c.
	var quotedArgs []string
	for _, a := range harnessArgs {
		quotedArgs = append(quotedArgs, shellQuote(a))
	}
	cmdLine := strings.Join(quotedArgs, " ")

	// Wrap the harness in a shell that records its real exit code to a fixed
	// file. The harness runs as a tmux grandchild, so its exit code is
	// otherwise invisible to the `sciontool init` supervisor (which only sees
	// the sh/container exit code). Writing $? lets init read the authoritative
	// harness exit code and report crashes correctly. The whole wrapper is
	// single-quoted again so tmux's command parser treats it as one word.
	agentWindowCmd := "sh -c " + shellQuote(cmdLine+"; echo $? > "+state.HarnessExitCodeFile)

	// Build tmux command: create session with "agent" window running the harness,
	// then add a "shell" window and switch back to the agent window.
	tmuxCmd := fmt.Sprintf(
		"tmux new-session -d -s scion -n agent %s \\; set-option -g window-size latest \\; new-window -t scion -n shell \\; select-window -t scion:agent \\; attach-session -t scion",
		agentWindowCmd,
	)

	if len(fuseMounts) > 0 {
		// Pass tmuxCmd via env var to avoid double-shell quoting issues.
		// The env var value is set by Docker/Podman without shell
		// interpretation, then safely expanded by sh via "$SCION_START_CMD".
		// Must be added before the image name — Docker/Podman require all
		// flags before the image argument.
		addArg("-e", fmt.Sprintf("SCION_START_CMD=%s", tmuxCmd))
	}

	args = append(args, config.Image)

	if len(fuseMounts) > 0 {
		mountCmds := strings.Join(fuseMounts, " && ")
		wrapped := fmt.Sprintf(`%s && exec sh -c "$SCION_START_CMD"`, mountCmds)
		args = append(args, "sh", "-c", wrapped)
	} else {
		args = append(args, "sh", "-c", tmuxCmd)
	}

	return args, nil
}

// resolveContainerID maps an agent identifier (slug, name, or partial
// container ID) to the authoritative container ID used by the runtime.
// When no match is found the original id is returned so callers can fall
// back to the raw value (which may itself be a valid container name).
func resolveContainerID(agents []api.AgentInfo, id string) string {
	for _, a := range agents {
		if a.ContainerID == id ||
			(len(id) >= 12 && strings.HasPrefix(a.ContainerID, id)) ||
			(len(a.ContainerID) >= 12 && strings.HasPrefix(id, a.ContainerID)) ||
			a.Name == id || a.Name == "/"+id || strings.TrimPrefix(a.Name, "/") == id {
			return a.ContainerID
		}
	}
	return id
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

// TODO(#284): remove once Phase 2 eliminates remaining bind-mount usage
//
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

// ForceHostNetworkEnvVar, when set to a non-empty value in the broker's
// environment, forces colocated Docker agents back onto host networking. It is
// the escape hatch that reverts to the pre-bridge behavior without a redeploy.
const ForceHostNetworkEnvVar = "SCION_FORCE_HOST_NETWORK"

// forceHostNetworking reports whether the host-networking escape hatch is set.
func forceHostNetworking() bool {
	return os.Getenv(ForceHostNetworkEnvVar) != ""
}

// ResolveDockerNetworking checks whether Docker host networking should be used
// to allow containers to reach services on the host's loopback interface.
// When the hub endpoint is localhost or was translated to a Docker bridge
// hostname (host.docker.internal), it returns "host" and rewrites any bridge
// hostnames back to localhost in the env map. This avoids the need for the
// server to bind to 0.0.0.0.
//
// When the SCION_FORCE_HOST_NETWORK escape hatch is set, host networking is
// forced regardless of the endpoint, reverting to the legacy behavior.
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

	// Escape hatch: force host networking regardless of endpoint so a
	// deployment can revert to the legacy behavior without a redeploy.
	if forceHostNetworking() {
		rewriteBridgeHostToLocalhost(env)
		return "host"
	}

	// If endpoint uses the Docker bridge hostname (translated from localhost),
	// rewrite back to localhost since host networking makes it reachable directly.
	if strings.Contains(ep, "host.docker.internal") {
		rewriteBridgeHostToLocalhost(env)
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

// rewriteBridgeHostToLocalhost rewrites any host.docker.internal references in
// the hub endpoint env vars back to localhost, since host networking makes the
// host loopback reachable directly.
func rewriteBridgeHostToLocalhost(env map[string]string) {
	for _, key := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL"} {
		if v, ok := env[key]; ok {
			env[key] = strings.Replace(v, "host.docker.internal", "localhost", 1)
		}
	}
}

// DockerSupportsHostGateway reports whether the Docker daemon supports the
// special "host-gateway" address used by --add-host. Support was added in
// Docker Engine 20.10; older daemons cannot map a domain to the host, so
// colocated bridge networking would be unable to reach Caddy and the broker
// must fall back to host networking. On any probe failure we conservatively
// assume support is present (the common case on modern hosts) so we don't
// needlessly disable the fix; a genuinely old daemon will surface the missing
// host-gateway when the container fails to start, which is rare in practice.
func DockerSupportsHostGateway(ctx context.Context, command string) bool {
	if command == "" {
		command = "docker"
	}
	// Bound the probe so an unresponsive Docker daemon cannot hang server
	// startup indefinitely; on timeout we fall through to the conservative
	// "assume support" path below.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := runSimpleCommand(probeCtx, command, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		runtimeLog.Debug("Unable to probe Docker server version for host-gateway support", "error", err)
		return true
	}
	major, minor, ok := parseDockerServerVersion(strings.TrimSpace(out))
	if !ok {
		runtimeLog.Debug("Unable to parse Docker server version for host-gateway support", "version", out)
		return true
	}
	// host-gateway requires Docker Engine >= 20.10.
	if major > 20 || (major == 20 && minor >= 10) {
		return true
	}
	return false
}

// parseDockerServerVersion parses the leading "major.minor" of a Docker server
// version string (e.g. "24.0.7" or "20.10.21"). It tolerates a leading "v"/"V"
// prefix and scans line-by-line so daemon warnings or other noise mixed into the
// command output (runSimpleCommand combines stdout and stderr) do not defeat the
// probe.
func parseDockerServerVersion(v string) (major, minor int, ok bool) {
	for _, line := range strings.Split(v, "\n") {
		line = strings.TrimLeft(strings.TrimSpace(line), "vV")
		parts := strings.SplitN(line, ".", 3)
		if len(parts) < 2 {
			continue
		}
		major, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		minor, err = strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		return major, minor, true
	}
	return 0, 0, false
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

// StagedSecretEnvVar is the environment variable used to pass serialized
// file and variable secrets from the broker to the container.
const StagedSecretEnvVar = stagedsecrets.EnvVar

// stagedSecretWarnThreshold is the size (in bytes) above which a warning is
// logged for the serialized secret blob. Container runtimes typically cap
// the combined environment at ~128KB.
const stagedSecretWarnThreshold = 100 * 1024

// StagedFileSecret is an alias for the type in pkg/stagedsecrets.
type StagedFileSecret = stagedsecrets.FileSecret

// StagedSecrets is an alias for the type in pkg/stagedsecrets.
type StagedSecrets = stagedsecrets.Staged

// serializeSecrets collects file and variable secrets into a single JSON blob,
// base64-encodes it, and returns the encoded string suitable for injection as
// an environment variable. Returns "" when there are no file or variable secrets.
// The containerHome parameter is used to expand ~/ prefixes in file target paths.
func serializeSecrets(containerHome string, secrets []api.ResolvedSecret) (string, error) {
	var staged StagedSecrets

	// Collect file secrets, deduplicating by container target path (last wins).
	targetIndex := make(map[string]int) // target → index in FileSecrets
	for _, s := range secrets {
		if s.Type != "file" {
			continue
		}
		target := expandTildeTarget(s.Target, containerHome)

		// Normalize to deterministic base64: decode first (to get raw bytes),
		// then always re-encode. This guarantees uniform base64 on the container side.
		var data []byte
		if decoded, err := base64.StdEncoding.DecodeString(s.Value); err == nil {
			data = decoded
		} else {
			data = []byte(s.Value)
		}

		entry := StagedFileSecret{
			Name:   s.Name,
			Target: target,
			Value:  base64.StdEncoding.EncodeToString(data),
		}

		if idx, exists := targetIndex[target]; exists {
			slog.Warn("serializeSecrets: duplicate container target, keeping later entry",
				"target", target, "kept", s.Name,
				"replaced", staged.FileSecrets[idx].Name)
			staged.FileSecrets[idx] = entry
		} else {
			targetIndex[target] = len(staged.FileSecrets)
			staged.FileSecrets = append(staged.FileSecrets, entry)
		}
	}

	// Collect variable secrets.
	for _, s := range secrets {
		if s.Type != "variable" {
			continue
		}
		if staged.VariableSecrets == nil {
			staged.VariableSecrets = make(map[string]string)
		}
		staged.VariableSecrets[s.Target] = s.Value
	}

	if len(staged.FileSecrets) == 0 && len(staged.VariableSecrets) == 0 {
		return "", nil
	}

	jsonData, err := json.Marshal(staged)
	if err != nil {
		return "", fmt.Errorf("failed to serialize staged secrets: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(jsonData)
	if len(encoded) > stagedSecretWarnThreshold {
		slog.Warn("serialized secrets blob exceeds size warning threshold",
			"size_bytes", len(encoded), "threshold_bytes", stagedSecretWarnThreshold)
	}

	return encoded, nil
}

// DecodeStagedSecrets decodes the SCION_STAGED_SECRETS env var value.
// Deprecated: Use stagedsecrets.Decode directly.
func DecodeStagedSecrets(encoded string) (*StagedSecrets, error) {
	return stagedsecrets.Decode(encoded)
}

// WriteStagedSecrets writes decoded staged secrets to the filesystem.
// Deprecated: Use stagedsecrets.Write directly.
func WriteStagedSecrets(homeDir string, staged *StagedSecrets) error {
	return stagedsecrets.Write(homeDir, staged)
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

// exitedStatusRe matches the exit code in container-runtime status strings such
// as "Exited (137) 2 minutes ago" (Docker/Podman) or "exited (0)".
var exitedStatusRe = regexp.MustCompile(`(?i)exited\s*\((\d+)\)`)

// ExitCodeFromContainerStatus extracts the exit code from a container status
// string like "Exited (137) 2 minutes ago". It returns (code, true) when an
// exited status with a parseable code is present, otherwise (0, false). A plain
// "stopped" (no embedded code) yields (0, false).
func ExitCodeFromContainerStatus(status string) (int, bool) {
	m := exitedStatusRe.FindStringSubmatch(status)
	if m == nil {
		return 0, false
	}
	code, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return code, true
}
