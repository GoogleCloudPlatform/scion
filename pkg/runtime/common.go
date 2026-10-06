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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	stagedsecrets "github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Well-known secret name and env var for GCP telemetry credentials.
const (
	telemetryGCPCredentialsSecretName = "scion-telemetry-gcp-credentials"
	telemetryGCPCredentialsEnvVar     = "SCION_OTEL_GCP_CREDENTIALS"
)

type gcsVolumeInfo struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix"`
}

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

// buildNoAuthCmdLine returns a shell command string for no-auth mode.
// The result is embedded directly into the sh -c command line used by the
// tmux agent window, avoiding a redundant nested sh -c that previously
// caused the no-auth command to be fed as terminal input instead of
// running as a standalone command.
func buildNoAuthCmdLine(noAuthMsg, noAuthCmd string) string {
	if noAuthCmd != "" {
		msgPart := ""
		if noAuthMsg != "" {
			msgPart = fmt.Sprintf("printf '%%s\\n' %s; ", shellQuote(noAuthMsg))
		}
		return fmt.Sprintf("%s%s; exec bash", msgPart, noAuthCmd)
	}
	if noAuthMsg != "" {
		return fmt.Sprintf("printf '%%s\\n' %s; exec bash", shellQuote(noAuthMsg))
	}
	return "exec bash"
}

// harnessCmdLine builds the shell command line that runs the harness (or
// the no-auth shell), from the fields shared by every runtime's entrypoint
// builder: NoAuth/NoAuthMessage/NoAuthCommand, or Harness.GetCommand with
// Task/Resume/CommandArgs. It returns ok=false when neither is configured;
// callers decide how to handle that case, since it differs by runtime
// (error out, or fall back to a placeholder command).
func harnessCmdLine(cfg RunConfig) (cmdLine string, ok bool) {
	if cfg.NoAuth {
		return buildNoAuthCmdLine(cfg.NoAuthMessage, cfg.NoAuthCommand), true
	}
	if cfg.Harness != nil {
		harnessArgs := cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
		quotedArgs := make([]string, 0, len(harnessArgs))
		for _, a := range harnessArgs {
			quotedArgs = append(quotedArgs, shellQuote(a))
		}
		return strings.Join(quotedArgs, " "), true
	}
	return "", false
}

// tmuxAgentWindowCmd wraps cmdLine so the harness's real exit code is
// recorded to state.HarnessExitCodeFile once it exits. The harness runs as
// a tmux grandchild, so its exit code is otherwise invisible to the
// `sciontool init` supervisor (which only sees the sh/container exit
// code); reading the file lets init report crashes correctly. The whole
// wrapper is single-quoted again so tmux's command parser treats it as one
// word. shellPath is the shell binary to invoke ("sh" or "/bin/sh",
// depending on what the runtime guarantees is resolvable for argv[0]).
func tmuxAgentWindowCmd(shellPath, cmdLine string) string {
	return shellPath + " -c " + shellQuote(cmdLine+"; echo $? > "+state.HarnessExitCodeFile)
}

// tmuxSessionEnd selects how buildTmuxStartCmd keeps the tmux session's
// lifetime visible to the container/pod's PID 1 once the session is
// created.
type tmuxSessionEnd int

const (
	// tmuxAttachSession runs `attach-session -t scion`, a foreground
	// attach bound to a TTY. Valid only where the runtime provides a TTY
	// for PID 1 (Docker, Podman, Kubernetes).
	tmuxAttachSession tmuxSessionEnd = iota
	// tmuxPollSession polls tmux for the session's liveness in a shell
	// loop instead of attaching. Required where PID 1 has no TTY (Cloud
	// Run, Cloud Run sandbox): `attach-session` fails immediately with
	// "open terminal failed: not a terminal" there.
	tmuxPollSession
)

// buildTmuxStartCmd builds the tmux invocation shared by every runtime: it
// creates the "scion" session with an "agent" window running
// agentWindowCmd, adds a "shell" window, selects back to "agent", and then
// keeps PID 1 alive per end. end is an explicit parameter rather than
// something this function infers, because the TTY-vs-no-TTY difference
// between runtimes is a genuine behavioural difference, not one to
// normalise away.
func buildTmuxStartCmd(agentWindowCmd string, end tmuxSessionEnd) string {
	cmd := fmt.Sprintf(
		"tmux new-session -d -s scion -n agent %s \\; set-option -g window-size latest \\; new-window -t scion -n shell \\; select-window -t scion:agent",
		agentWindowCmd,
	)
	switch end {
	case tmuxPollSession:
		return cmd + "; while tmux has-session -t scion 2>/dev/null; do sleep 2; done"
	default:
		return cmd + " \\; attach-session -t scion"
	}
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
	var gcsVolumes []gcsVolumeInfo

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

			gcsVolumes = append(gcsVolumes, gcsVolumeInfo{
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

	// Reject a home directory that is not an allowed agent-home path before
	// it becomes a bind mount, the same way config.Workspace is checked just
	// below: this is the Docker/Podman/Apple-container counterpart to the
	// Kubernetes runtime's Run(), which already validates HomeDir this way.
	// No root is passed: unlike config.Workspace, a home directory has no
	// per-project root to check it against at this call site at all, so the
	// fixed deny-set and the named ~/.scion allow list are the only checks,
	// the same as every other rootless caller of ValidateAgentHomeSource.
	if config.HomeDir != "" {
		resolvedHomeDir, err := ValidateAgentHomeSource(config.HomeDir, "")
		if err != nil {
			return nil, err
		}
		config.HomeDir = resolvedHomeDir
		registerMount(config.HomeDir, util.GetHomeDir(config.UnixUsername), false, true)
	}
	// Reject a workspace source that is not an allowed workspace path before
	// any of the branches below turn it into a bind mount, and act on the
	// resolved, symlink-free path it returns rather than the original value.
	// This mirrors the same check at the pkg/agent Start() call site, as a
	// second gate at the actual point config.Workspace becomes a mount —
	// covering any caller that builds a RunConfig without going through
	// Start().
	//
	// No root is passed here, even though config.RepoRoot is often set: a
	// workspace outside the repo root is an intentional, supported shape at
	// this layer (see the "Fallback if workspace is outside repo root"
	// branch below, and an explicit --workspace generally), and RunConfig
	// carries no flag this function could use to tell that apart from a bad
	// value. Only the fixed deny-set ('/', $HOME, ~/.scion, and its named
	// ~/.scion allow list) applies here; per-project root containment is
	// enforced upstream, at the pkg/agent Start() call site, which does have
	// that context.
	resolvedWorkspace, err := ValidateWorkspaceSource(config.Workspace, "")
	if err != nil {
		return nil, err
	}
	config.Workspace = resolvedWorkspace

	// Resolve RepoRoot through any symlinks too, the same way Workspace just
	// was: the branches below compare the two with filepath.Rel to decide
	// the mount layout, and a symlinked repo path that only one of the two
	// still carries silently changes which branch fires (in-repo worktree
	// vs. shared-workspace vs. the outside-repo-root fallback) depending on
	// which side of the comparison the symlink survives on.
	if config.RepoRoot != "" {
		resolvedRepoRoot, err := filepath.EvalSymlinks(config.RepoRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve repo root %s: %w", config.RepoRoot, err)
		}
		config.RepoRoot = resolvedRepoRoot
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
	addEnv("SCION_PROJECT_ID", config.ProjectID)

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

	// Inject environment-type resolved secrets. config.Env, emitted above,
	// always wins: skip any secret whose target collides with a key already
	// present there, so a secret can never re-decide a value the caller (the
	// broker or hub that built config.Env) already set for that name. This is
	// explicit and symmetric with the k8s runtime's equivalent check.
	envKeys := make(map[string]struct{}, len(config.Env))
	for _, e := range config.Env {
		key, _, _ := strings.Cut(e, "=")
		envKeys[key] = struct{}{}
	}
	for _, s := range config.ResolvedSecrets {
		if s.Type != "environment" && s.Type != "" {
			continue
		}
		if _, collides := envKeys[s.Target]; collides {
			continue
		}
		addArg("-e", fmt.Sprintf("%s=%s", s.Target, s.Value))
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
		addArg("--label", fmt.Sprintf("%s=%s", projectkeys.LabelProject, config.Project))
	}
	if config.ProjectID != "" {
		addArg("--label", fmt.Sprintf("%s=%s", projectkeys.LabelProjectID, config.ProjectID))
	}

	if config.Template != "" {
		addArg("--label", fmt.Sprintf("scion.template=%s", config.Template))
	}

	// Get command from harness.
	// No-auth mode builds a raw shell command string that is embedded
	// directly into the sh -c used by the tmux agent window. This avoids
	// the double-sh-c wrapping that previously caused the no-auth command
	// to be injected as terminal input instead of running standalone.
	cmdLine, ok := harnessCmdLine(config)
	if !ok {
		return nil, fmt.Errorf("no harness provided")
	}

	// Wrap the harness in a shell that records its real exit code to a fixed
	// file, then build the tmux command: create session with "agent" window
	// running the harness, add a "shell" window, and attach (Docker/Podman
	// provide PID 1 a TTY). See tmuxAgentWindowCmd/buildTmuxStartCmd for the
	// shared definition across runtimes.
	agentWindowCmd := tmuxAgentWindowCmd("sh", cmdLine)
	tmuxCmd := buildTmuxStartCmd(agentWindowCmd, tmuxAttachSession)

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
	if agent := findContainerAgent(agents, id); agent != nil {
		return agent.ContainerID
	}
	return id
}

func findContainerAgent(agents []api.AgentInfo, id string) *api.AgentInfo {
	for i := range agents {
		agent := &agents[i]
		if agent.ContainerID == id ||
			(len(id) >= 12 && strings.HasPrefix(agent.ContainerID, id)) ||
			(len(agent.ContainerID) >= 12 && strings.HasPrefix(id, agent.ContainerID)) ||
			agent.Name == id || agent.Name == "/"+id || strings.TrimPrefix(agent.Name, "/") == id {
			return agent
		}
	}
	return nil
}

func syncGCSVolumes(ctx context.Context, encoded string, direction SyncDirection) error {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("failed to decode gcs volume info: %w", err)
	}

	var volumes []gcsVolumeInfo
	if err := json.Unmarshal(decoded, &volumes); err != nil {
		return fmt.Errorf("failed to parse gcs volume info: %w", err)
	}

	for _, volume := range volumes {
		if volume.Source == "" {
			continue
		}
		// volume.Source is read back from a persisted label on every sync,
		// not just checked once when the agent started: validate it the
		// same way every other workspace source is, since nothing upstream
		// of this decode re-derives or re-checks it. No per-project root is
		// available at this call site, so only the universal floors apply
		// (refusing '/', $HOME, ~/.scion outside its own allow list, and
		// the other critical-system-path and ancestor refusals).
		resolvedSource, err := ValidateWorkspaceSource(volume.Source, "")
		if err != nil {
			return fmt.Errorf("invalid GCS volume source: %w", err)
		}
		volume.Source = resolvedSource
		switch direction {
		case SyncTo:
			if err := gcp.SyncToGCS(ctx, volume.Source, volume.Bucket, volume.Prefix); err != nil {
				return fmt.Errorf("failed to sync to GCS: %w", err)
			}
		case SyncFrom:
			if err := gcp.SyncFromGCS(ctx, volume.Bucket, volume.Prefix, volume.Source); err != nil {
				return fmt.Errorf("failed to sync from GCS: %w", err)
			}
		default:
			return fmt.Errorf("sync direction must be specified for GCS volumes")
		}
	}
	return nil
}

func appendContainerResourceArgs(args []string, resources *api.ResourceSpec) ([]string, error) {
	if resources == nil {
		return args, nil
	}
	if resources.Limits.Memory != "" {
		bytes, err := util.ParseMemory(resources.Limits.Memory)
		if err != nil {
			return nil, fmt.Errorf("invalid memory limit %q: %w", resources.Limits.Memory, err)
		}
		args = append(args, "--memory", util.FormatMemoryForDocker(bytes))
	}
	if resources.Requests.Memory != "" {
		bytes, err := util.ParseMemory(resources.Requests.Memory)
		if err != nil {
			return nil, fmt.Errorf("invalid memory request %q: %w", resources.Requests.Memory, err)
		}
		args = append(args, "--memory-reservation", util.FormatMemoryForDocker(bytes))
	}
	if resources.Limits.CPU != "" {
		cores, err := util.ParseCPU(resources.Limits.CPU)
		if err != nil {
			return nil, fmt.Errorf("invalid cpu limit %q: %w", resources.Limits.CPU, err)
		}
		args = append(args, "--cpus", util.FormatCPU(cores))
	}
	return args, nil
}

// runtimeLog is the structured logger for runtime command execution.
var runtimeLog = slog.Default().With(slog.String("subsystem", "runtime"))

// isExitError reports whether err (possibly wrapped by runSimpleCommand's
// fmt.Errorf) contains an *exec.ExitError, meaning the command executed but
// returned a non-zero exit code.  This distinguishes "command ran but failed"
// from fundamental failures like a missing binary or a cancelled context.
func isExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// isImageNotFoundOutput checks whether the combined stdout/stderr output from
// an `image inspect` command indicates that the image was simply not found, as
// opposed to some other failure (daemon unreachable, permission denied, etc.).
//
// When the container daemon is down, the CLI binary still exits with a non-zero
// code (exec.ExitError), but the output contains a connection error rather than
// a "not found" message. Callers should only treat the result as "image not
// found" when this function returns true.
//
// Known patterns:
//   - Docker:          "No such image"
//   - Podman:          "image not known"
//   - Apple Container: "image not found"
func isImageNotFoundOutput(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "no such image") ||
		strings.Contains(lower, "image not known") ||
		strings.Contains(lower, "image not found")
}

func runSimpleCommand(ctx context.Context, command string, args ...string) (string, error) {
	// Log the command name and argument count only — never the argument values.
	// On this tier, secrets travel in argv (--env KEY=VALUE), so logging the
	// full command string would write credentials to logs.  See #127 / P5.
	runtimeLog.Debug("Executing command", "cmd", command, "argc", len(args))
	start := time.Now()
	cmd := exec.CommandContext(ctx, command, args...)
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if err != nil {
		logCommandFailure(ctx, command, len(args), elapsed, out)
		return string(out), fmt.Errorf("%s failed: %w", command, err)
	}
	runtimeLog.Debug("Command completed", "cmd", command, "argc", len(args), "duration", elapsed)
	return strings.TrimSpace(string(out)), nil
}

// logCommandFailure logs a failed command's combined stdout/stderr, unless
// ctx is marked via WithSensitiveExec — in which case the output is omitted
// (never even truncated or hashed) because it may carry caller-supplied
// content that must not reach logs at all. The command name, argument count
// and duration are always logged regardless: only the output value itself is
// sensitive. See WithSensitiveExec's doc comment for why this suppression
// must live here rather than only at the broker layer.
func logCommandFailure(ctx context.Context, command string, argc int, elapsed time.Duration, out []byte) {
	if IsSensitiveExec(ctx) {
		// No "output" key at all — matches this function's doc comment
		// ("omitted"), not a placeholder value that would still need its own
		// audit for leaking anything.
		runtimeLog.Debug("Command failed", "cmd", command, "argc", argc, "duration", elapsed)
		return
	}
	runtimeLog.Debug("Command failed", "cmd", command, "argc", argc, "duration", elapsed, "output", strings.TrimSpace(string(out)))
}

// runSimpleCommandWithStdin is runSimpleCommand's counterpart for callers
// that need to deliver data to the child process over stdin rather than
// argv (see #1355 — secrets embedded in argv leak via /proc/<pid>/cmdline
// for the lifetime of the exec). It never logs the piped content.
func runSimpleCommandWithStdin(ctx context.Context, stdin io.Reader, command string, args ...string) (string, error) {
	// Log the command name and argument count only — see runSimpleCommand
	// comment above. The stdin payload is never logged.
	runtimeLog.Debug("Executing command with stdin", "cmd", command, "argc", len(args))
	start := time.Now()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if err != nil {
		logCommandFailure(ctx, command, len(args), elapsed, out)
		return string(out), fmt.Errorf("%s failed: %w", command, err)
	}
	runtimeLog.Debug("Command completed", "cmd", command, "argc", len(args), "duration", elapsed)
	return strings.TrimSpace(string(out)), nil
}

// rollbackCancelledCreate best-effort removes a container that the daemon
// may have already created/started before the CLI process (e.g. a detached
// "run -d") was killed by context cancellation. exec.CommandContext only
// kills the local CLI subprocess — it does not tell the daemon to undo work
// it already began server-side. Without this, a create whose context is
// cancelled because the caller gave up (e.g. the Hub's dispatch timeout
// elapsed, or the original request was itself cancelled) can leak a running
// container that nothing else knows to reap. See ptone/scion#1886.
//
// It intentionally uses a fresh, short-lived context rather than the
// (already cancelled) caller context, since the caller has already given up
// and this cleanup must still be allowed to run.
//
// The Apple "container" CLI's "rm" does not support "-f" and fails if the
// container is still running (see AppleContainerRuntime.Delete), so for that
// runtime we kill first, then retry a plain "rm" a few times — kill is
// asynchronous and the container may not be immediately ready for removal.
// Docker/Podman support "rm -f" directly.
func rollbackCancelledCreate(command, containerName string) {
	if containerName == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if filepath.Base(command) == "container" {
		_, _ = runSimpleCommand(cleanupCtx, command, "kill", containerName)

		var out string
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			out, err = runSimpleCommand(cleanupCtx, command, "rm", containerName)
			if err == nil {
				return
			}
			select {
			case <-cleanupCtx.Done():
				runtimeLog.Warn("Failed to roll back cancelled create", "cmd", command, "container", containerName, "error", cleanupCtx.Err(), "output", strings.TrimSpace(out))
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		runtimeLog.Warn("Failed to roll back cancelled create", "cmd", command, "container", containerName, "error", err, "output", strings.TrimSpace(out))
		return
	}
	if out, err := runSimpleCommand(cleanupCtx, command, "rm", "-f", containerName); err != nil {
		runtimeLog.Warn("Failed to roll back cancelled create", "cmd", command, "container", containerName, "error", err, "output", strings.TrimSpace(out))
	}
}

func runInteractiveCommand(command string, args ...string) error {
	// Log command name and argument count only — see runSimpleCommand comment
	// and #127 / P5 for rationale.
	runtimeLog.Debug("Executing interactive command", "cmd", command, "argc", len(args))
	cmd := exec.Command(command, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// WriteRuntimeDebugFile writes a redacted summary of the runtime execution
// command to a debug file inside the agent directory. The file records the
// command name and argument count but NOT the argument values, because on the
// sandbox tier secrets travel in argv (--env KEY=VALUE) and writing them to a
// host-filesystem file creates an uncontrolled secret-at-rest.  See #127 / P5.
//
// The file is written to <agentDir>/runtime-exec-debug.
// This is a no-op if config.Debug is false or HomeDir is empty.
func WriteRuntimeDebugFile(config RunConfig, command string, args []string) {
	if !config.Debug || config.HomeDir == "" {
		return
	}
	agentDir := filepath.Dir(config.HomeDir)
	debugPath := filepath.Join(agentDir, "runtime-exec-debug")

	var buf strings.Builder
	buf.WriteString(command)
	fmt.Fprintf(&buf, " [%d args redacted — may contain credentials, see #127]\n", len(args))

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

// ResolveHostNetworking reports whether a container should use host networking
// to reach services on the host's loopback interface, returning "host" (the
// network mode) or "" (no override). When the hub endpoint is localhost or was
// translated to a bridge hostname (host.docker.internal), it returns "host" and
// rewrites any bridge hostnames back to localhost in the env map. This avoids
// the need for the server to bind to 0.0.0.0.
//
// When the SCION_FORCE_HOST_NETWORK escape hatch is set, host networking is
// forced for any runtime whenever a hub endpoint is configured, reverting to
// the legacy behavior.
//
// For non-Docker runtimes (absent the escape hatch) or non-localhost endpoints,
// returns "" (no override).
func ResolveHostNetworking(runtimeName string, env map[string]string) string {
	ep := env["SCION_HUB_ENDPOINT"]
	if ep == "" {
		ep = env["SCION_HUB_URL"]
	}
	if ep == "" {
		return ""
	}

	// Escape hatch: force host networking regardless of the endpoint value, for
	// any runtime, so a deployment can revert to the legacy behavior without a
	// redeploy. Runtime-agnostic, so evaluated before the Docker-only endpoint
	// heuristics below.
	if forceHostNetworking() {
		rewriteBridgeHostToLocalhost(env)
		return "host"
	}

	if runtimeName != "docker" {
		return ""
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
			v = strings.Replace(v, "host.docker.internal", "localhost", 1)
			// host.containers.internal does not resolve under --network=host.
			v = strings.Replace(v, "host.containers.internal", "localhost", 1)
			env[key] = v
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
		if f.SourcePath == "" {
			// In broker mode, file content is projected from staged secrets
			// (SCION_STAGED_SECRETS), not copied from host paths. SourcePath is
			// intentionally cleared by run.go. Skip the copy/mount.
			continue
		}
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

// stagedSecretWarnThreshold is the size (in bytes) above which a warning is
// logged for the serialized secret blob. Container runtimes typically cap
// the combined environment at ~128KB.
const stagedSecretWarnThreshold = 100 * 1024

// prepareContainerSecretEnv adds the shared secret-related environment needed
// by local container runtimes.
func prepareContainerSecretEnv(config *RunConfig) error {
	if len(config.ResolvedSecrets) == 0 {
		return nil
	}

	containerHome := util.GetHomeDir(config.UnixUsername)
	encoded, err := serializeSecrets(containerHome, config.ResolvedSecrets)
	if err != nil {
		return fmt.Errorf("failed to serialize secrets: %w", err)
	}
	if encoded != "" {
		config.Env = append(config.Env, stagedsecrets.EnvVar+"="+encoded)
	}

	if credPath := findGCPTelemetryCredentialPath(config.ResolvedSecrets, containerHome); credPath != "" {
		config.Env = append(config.Env, telemetryGCPCredentialsEnvVar+"="+credPath)
	}
	return nil
}

// envSizeLimit is the largest environment entry a runtime accepts.
// maxBytes 0 means no check. When includeKey is set the limit covers the
// whole "KEY=VALUE" string (plus its NUL terminator), as for an argv
// string; otherwise it covers the value alone.
type envSizeLimit struct {
	maxBytes   int
	includeKey bool
}

// size returns the bytes an entry counts against the limit.
func (l envSizeLimit) size(key, value string) int {
	if l.includeKey {
		return len(key) + 1 + len(value) + 1
	}
	return len(value)
}

// exceeds reports whether key=value does not fit.
func (l envSizeLimit) exceeds(key, value string) bool {
	return l.maxBytes > 0 && l.size(key, value) > l.maxBytes
}

// unit names what the limit measures, for error messages.
func (l envSizeLimit) unit() string {
	if l.includeKey {
		return "environment entry (KEY=VALUE)"
	}
	return "environment variable value"
}

// applyResolvedSecretsToEnv folds config.ResolvedSecrets into config.Env for
// runtimes that hand the container a flat environment list (Cloud Run
// instances and cloudrun-sandbox) rather than building it through
// buildCommonRunArgs.
//
// Environment-type secrets become KEY=VALUE entries. As in Docker, a key
// already present in config.Env wins and the secret is skipped; among
// secrets sharing a target the later one wins (Docker keeps the last -e
// flag). Keys this function adds itself (SCION_STAGED_SECRETS,
// SCION_OTEL_GCP_CREDENTIALS) and any reservedKeys the runtime sets after
// config.Env are treated the same way, so no env name is emitted twice.
// File and variable secrets go into the SCION_STAGED_SECRETS blob via
// prepareContainerSecretEnv, which sciontool init writes out in the
// container.
//
// A secret that does not fit limit fails the call with an error naming it,
// so no secret is ever dropped silently. Values are never included in the
// error.
//
// It returns the env-type secret keys it added, in order. config.Env is
// clipped before appending, so a caller's backing array is never written.
func applyResolvedSecretsToEnv(config *RunConfig, limit envSizeLimit, reservedKeys ...string) ([]string, error) {
	if len(config.ResolvedSecrets) == 0 {
		return nil, nil
	}

	staged := *config
	staged.Env = nil
	if err := prepareContainerSecretEnv(&staged); err != nil {
		return nil, err
	}
	for _, e := range staged.Env {
		key, val, _ := strings.Cut(e, "=")
		if key == stagedsecrets.EnvVar && limit.exceeds(key, val) {
			return nil, fmt.Errorf("file/variable secrets do not fit: %s is %d bytes, over the %d-byte limit for one %s on this runtime (largest secret: %q)",
				stagedsecrets.EnvVar, limit.size(key, val), limit.maxBytes, limit.unit(), largestStagedSecretName(config.ResolvedSecrets))
		}
	}

	envKeys := make(map[string]struct{}, len(config.Env)+len(reservedKeys)+2)
	for _, e := range config.Env {
		key, _, _ := strings.Cut(e, "=")
		envKeys[key] = struct{}{}
	}
	// The staged keys are reserved even when this call does not set them:
	// sciontool init reads them, so a secret must not supply them.
	envKeys[stagedsecrets.EnvVar] = struct{}{}
	envKeys[telemetryGCPCredentialsEnvVar] = struct{}{}
	for _, key := range reservedKeys {
		envKeys[key] = struct{}{}
	}

	// Collect env-type secrets in order, later duplicates replacing earlier ones.
	var order []string
	values := make(map[string]api.ResolvedSecret)
	for _, s := range config.ResolvedSecrets {
		if s.Type != "environment" && s.Type != "" {
			continue
		}
		if _, collides := envKeys[s.Target]; collides {
			continue
		}
		if _, seen := values[s.Target]; !seen {
			order = append(order, s.Target)
		}
		values[s.Target] = s
	}
	for _, target := range order {
		s := values[target]
		if limit.exceeds(target, s.Value) {
			return nil, fmt.Errorf("secret %q (env %s) is %d bytes, over the %d-byte limit for one %s on this runtime",
				s.Name, s.Target, limit.size(target, s.Value), limit.maxBytes, limit.unit())
		}
	}

	config.Env = slices.Clip(config.Env)
	for _, target := range order {
		config.Env = append(config.Env, target+"="+values[target].Value)
	}
	config.Env = append(config.Env, staged.Env...)
	return order, nil
}

// largestStagedSecretName returns the name of the largest file or variable
// secret, i.e. the one most responsible for an oversized staged blob.
func largestStagedSecretName(secrets []api.ResolvedSecret) string {
	name, size := "", -1
	for _, s := range secrets {
		if s.Type != "file" && s.Type != "variable" {
			continue
		}
		if len(s.Value) > size {
			name, size = s.Name, len(s.Value)
		}
	}
	return name
}

// serializeSecrets collects file and variable secrets into a single JSON blob,
// base64-encodes it, and returns the encoded string suitable for injection as
// an environment variable. Returns "" when there are no file or variable secrets.
// The containerHome parameter is used to expand ~/ prefixes in file target paths.
func serializeSecrets(containerHome string, secrets []api.ResolvedSecret) (string, error) {
	var staged stagedsecrets.Staged

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

		entry := stagedsecrets.FileSecret{
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

// Deprecated: ExitCodeFromContainerStatus parses exit codes from container
// status strings like "Exited (137)". New code should use the structured
// ExitCode field on AgentInfo/AgentHeartbeat instead. This function remains
// for the hub's legacy heartbeat fallback path (old brokers without ExitCode).
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
