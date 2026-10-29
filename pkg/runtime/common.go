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
	"bytes"
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
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	stagedsecrets "github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
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

// harnessCmdLineOptions holds per-runtime choices for harnessCmdLine. The
// zero value builds the plain harness command line.
type harnessCmdLineOptions struct {
	// FreshStartOnFailedResume makes a resume fall back to a fresh start of
	// the harness when the resume attempt exits non-zero within
	// resumeFallbackWindowSeconds (see resumeFallbackCmdLine). Set it only
	// where the agent home, and so the harness's saved session, does not
	// survive a restart: the Kubernetes runtime with a pod-local home.
	// Where the home persists, a fast failure has another cause (network,
	// auth) and must not silently drop the conversation.
	FreshStartOnFailedResume bool
}

// resumeFallbackWindowSeconds bounds how soon a failed resume attempt must
// exit for FreshStartOnFailedResume to start the harness fresh. A harness
// with no session to resume exits within a few seconds; a resumed session
// that fails later is not restarted fresh.
const resumeFallbackWindowSeconds = 30

// resumeFallbackNotice is printed to stderr before the fresh start.
const resumeFallbackNotice = "scion: no resumable session; starting fresh"

// harnessCmdLine builds the shell command line that runs the harness (or
// the no-auth shell), from the fields shared by every runtime's entrypoint
// builder: NoAuth/NoAuthMessage/NoAuthCommand, or Harness.GetCommand with
// Task/Resume/CommandArgs. It returns ok=false when neither is configured;
// callers decide how to handle that case, since it differs by runtime
// (error out, or fall back to a placeholder command).
func harnessCmdLine(cfg RunConfig, opts harnessCmdLineOptions) (cmdLine string, ok bool) {
	if cfg.NoAuth {
		return buildNoAuthCmdLine(cfg.NoAuthMessage, cfg.NoAuthCommand), true
	}
	if cfg.Harness != nil {
		harnessArgs := cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
		if cfg.Resume && opts.FreshStartOnFailedResume {
			freshArgs := cfg.Harness.GetCommand(cfg.Task, false, cfg.CommandArgs)
			if !slices.Equal(harnessArgs, freshArgs) {
				return resumeFallbackCmdLine(harnessArgs, freshArgs), true
			}
		}
		return quotedArgsLine(harnessArgs, nil), true
	}
	return "", false
}

// quotedArgsLine shell-quotes each argument and joins them with spaces. An
// argument that is a key of refs is replaced by its value, a quoted shell
// variable reference that expands to the argument as one word.
func quotedArgsLine(args []string, refs map[string]string) string {
	quotedArgs := make([]string, 0, len(args))
	for _, a := range args {
		if ref, ok := refs[a]; ok {
			quotedArgs = append(quotedArgs, ref)
			continue
		}
		quotedArgs = append(quotedArgs, shellQuote(a))
	}
	return strings.Join(quotedArgs, " ")
}

// resumeFallbackSharedArgMinBytes is the shortest argument common to the
// resume and fresh commands that resumeFallbackCmdLine stores in a shell
// variable. A shorter one is written in both commands, since a variable
// costs about as many bytes as it saves.
const resumeFallbackSharedArgMinBytes = 24

// resumeFallbackCmdLine runs resumeArgs and, when they exit with a status
// from 1 to 127 within resumeFallbackWindowSeconds, prints
// resumeFallbackNotice and runs freshArgs.
//
// A status of 128 or more means the harness was killed by a signal (a
// stop, suspend or pod delete), which never starts the fresh run; if the
// shell itself is signalled it dies and runs nothing further. The line's
// exit status is that of the last harness run, so tmuxAgentWindowCmd
// records the real code.
//
// freshArgs is the harness's own non-resume command, so the fallback drops
// exactly the resume flag that harness adds, plus any prompt GetCommand
// adds only on resume, and carries the same task: on a wake that is no
// task, never the original prompt.
//
// Each argument of at least resumeFallbackSharedArgMinBytes that both
// commands contain (the task, an inline system prompt) is assigned once to
// a shell variable, scion_a0, scion_a1 and so on, that both commands
// reference as "$scion_aN": one word, never re-split. The line is
// therefore longer than the plain command line by a fixed few hundred
// bytes plus a few dozen per shared or short argument, not by a second
// copy of the long ones, which keeps it under tmux's command length limit
// whenever the plain line is (see QuotedTaskBytes and InlineTaskMaxBytes).
func resumeFallbackCmdLine(resumeArgs, freshArgs []string) string {
	inResume := make(map[string]bool, len(resumeArgs))
	for _, a := range resumeArgs {
		inResume[a] = true
	}
	refs := map[string]string{}
	var assigns strings.Builder
	for _, a := range freshArgs {
		if _, done := refs[a]; done || len(a) < resumeFallbackSharedArgMinBytes || !inResume[a] {
			continue
		}
		name := fmt.Sprintf("scion_a%d", len(refs))
		refs[a] = `"$` + name + `"`
		fmt.Fprintf(&assigns, "%s=%s; ", name, shellQuote(a))
	}
	return fmt.Sprintf(
		`%sscion_start=$(date +%%s); %s; scion_rc=$?; `+
			`if [ "$scion_rc" -gt 0 ] && [ "$scion_rc" -lt 128 ] && `+
			`[ $(( $(date +%%s) - scion_start )) -lt %d ]; `+
			`then printf '%%s\n' %s >&2; %s; else (exit "$scion_rc"); fi`,
		assigns.String(),
		quotedArgsLine(resumeArgs, refs),
		resumeFallbackWindowSeconds,
		shellQuote(resumeFallbackNotice),
		quotedArgsLine(freshArgs, refs),
	)
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

// QuotedTaskBytes is the number of bytes task adds to the tmux command
// that starts the harness. The harness command line quotes each argument
// for the shell (harnessCmdLine) and tmuxAgentWindowCmd quotes that line
// again, so every single quote in task grows to 13 bytes.
func QuotedTaskBytes(task string) int {
	return len(shellQuote(shellQuote(task))) - len(shellQuote(shellQuote("")))
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

// errWorktreeAdminUnresolved is returned when the broker-recorded worktree admin
// directory for a workspace cannot be resolved from the common git dir's trusted
// back-links: the start is refused rather than binding git metadata or the
// enclosing repository. This is distinct from errOutsideRootNotRegistered (a
// workspace outside the repo root that is not a registered worktree at all); this
// sentinel covers a workspace routed through a worktree layout — including a plain
// in-repo subdirectory that is not a registered worktree — for which no per-agent
// private .git surface can be built.
var errWorktreeAdminUnresolved = errors.New("cannot resolve the broker-recorded worktree admin directory for the workspace; refusing to mount git metadata")

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

	expandPath := func(path string, isTarget bool) string {
		if !isTarget {
			return expandHostSource(path)
		}
		expanded := expandVolumeEnv(path)
		if strings.HasPrefix(expanded, "~/") {
			return filepath.Join(util.GetHomeDir(config.UnixUsername), expanded[2:])
		}
		if expanded == "~" {
			return util.GetHomeDir(config.UnixUsername)
		}
		return expanded
	}

	// Volume deduplication, by target. The source of each bind is checked
	// once, when the binds are emitted below (checkBindSource).
	type bindSpec struct {
		src string
		ro  bool
	}
	volumeMap := make(map[string]bindSpec)
	var volumeOrder []string

	registerMount := func(src, tgt string, ro bool, overwrite bool) {
		spec := bindSpec{src: src, ro: ro}
		if _, exists := volumeMap[tgt]; !exists {
			volumeOrder = append(volumeOrder, tgt)
			volumeMap[tgt] = spec
		} else if overwrite {
			volumeMap[tgt] = spec
		}
	}

	var fuseMounts []string
	var gcsVolumes []gcsVolumeInfo

	// volumeErr is the first configured volume refused below; it fails the
	// whole call once the volumes have been processed.
	var volumeErr error
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
		if _, exists := volumeMap[tgt]; exists {
			// Already mounted (for example the workspace, checked on its
			// own path); registerMount below would not add this one.
			return
		}
		// No configured volume may reach a Runtime Broker control path,
		// read-only or not (checkConfiguredVolumeSource). Agents' own
		// directories under ~/.scion/project-configs hold state the host
		// relies on; no configured volume may mount them either
		// (checkVolumeSource).
		if err := checkConfiguredVolumeSource(src); err != nil {
			if volumeErr == nil {
				volumeErr = err
			}
			return
		}
		if err := checkVolumeSource(src, v.ReadOnly); err != nil {
			if volumeErr == nil {
				volumeErr = err
			}
			return
		}
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
	// Mount points created below in the agent home go through one handle
	// opened on the home (pkg/agenthome), so a linked directory component
	// in the home is refused, not traversed. Those mkdirs are best effort,
	// as before: without a handle they are skipped.
	var agentHome *agenthome.Home
	if config.HomeDir != "" {
		if h, err := agenthome.OpenCreate(config.HomeDir, 0755); err == nil {
			agentHome = h
			defer func() { _ = agentHome.Close() }()
		}
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

	// sharedWorkspaceAgentsMask is the container path of the in-project
	// agents root of a shared workspace mount, shadowed with a tmpfs below
	// (sharedWorkspaceAgentsMaskTarget); empty when no mask applies.
	sharedWorkspaceAgentsMask := ""
	// nestedMasks collects the no-create agents-dir masks requested by the
	// nested and fallback own-.scion paths (requireExistingAgentsMask). They are
	// emitted as tmpfs mounts AFTER the bind mounts, below, so a mask lands over
	// its read-only .scion bind rather than before it.
	var nestedMasks []string
	addMask := func(maskTarget string) {
		nestedMasks = append(nestedMasks, maskTarget)
	}
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
			// Worktree case: workspace is a subdirectory of repo root. Mount
			// .git with the read-only / private narrowing (every runtime
			// and base, incl. linked checkouts), then the workspace at its
			// relative path. registerWorktreeGitMounts refuses the start if the
			// per-agent private .git/config cannot be bound safely.
			if err := registerWorktreeGitMounts(registerMount, config); err != nil {
				return nil, err
			}
			containerWorkspace := filepath.Join("/repo-root", relWorkspace)
			registerMount(config.Workspace, containerWorkspace, false, true)
			addArg("--workdir", containerWorkspace)
			// A workspace that is not the agent's own (a project kept in
			// a subdirectory of the repository) holds that project's
			// .scion: it gets the read-only bind and the agents mask of the
			// shared layout below. The agent's own worktree's .scion is its
			// branch checkout, which the broker does not read. Which one
			// this is comes from the broker (AgentOwnWorkspace), never from
			// the workspace's contents.
			if !config.AgentOwnWorkspace {
				src, tgt, ok, err := workspaceProjectConfigMount(config.Workspace, containerWorkspace, config.RuntimeName != appleContainerRuntimeName)
				if err != nil {
					return nil, err
				}
				if ok {
					registerMount(src, tgt, true, true)
					if err := ensureAgentsMaskPoint(config, src); err != nil {
						return nil, err
					}
					if isDir(filepath.Join(src, "agents")) {
						sharedWorkspaceAgentsMask = path.Join(tgt, "agents")
					}
				}
				// A project kept in a SUBDIRECTORY of this non-own workspace (its
				// .scion is <ws>/<sub>/.scion) would otherwise stay writable under the
				// read-write workspace bind: mount the nested .scion read-only and mask
				// its agents/ (same helper as the shared/config-only nested cases).
				if err := applyNestedProjectConfig(config, containerWorkspace, registerMount, addMask); err != nil {
					return nil, err
				}
			}
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
			// Agent state for shared-workspace projects is always resolved
			// from the broker-side agent dir (pkg/agent agentStateDir), never
			// from the in-project agents root under this mount; that is the
			// control on every runtime. The in-project agents root is
			// additionally kept out of the container's view (below):
			// shadowed with a tmpfs on Docker/Podman whenever .scion is a
			// directory, and on Apple when the agents dir already exists.
			//
			// The project configuration directory <workspace>/.scion
			// (settings, templates, harness-configs), which the broker
			// reads when it starts agents in this project, is mounted
			// read-only (workspaceProjectConfigMount), as is a .scion
			// project marker file on runtimes that can bind a file. Since it is
			// read-only, the container runtime cannot create the agents
			// mask's mount point in it, so on Docker/Podman the broker
			// creates the (git-excluded) agents dir first, as the runtime
			// itself used to; the mask then also hides an agent dir the
			// broker creates there after the start. Apple keeps masking
			// only an existing agents dir.
			registerMount(config.Workspace, "/workspace", false, true)
			addArg("--workdir", "/workspace")
			// Narrow the shared checkout's .git admin surface and bind a
			// per-agent private .git/config (#5865). The .git root itself stays
			// read-write (the main worktree's HEAD/index live there); a root
			// commondir/gitdir redirect is refused host-side
			// (refuseRootCommonDirRedirect) and config.worktree is bound read-only;
			// a root-level `shallow` remains a documented residual. Refuses on Apple
			// (cannot bind single files).
			if err := registerSharedPlainGitMounts(registerMount, config); err != nil {
				return nil, err
			}
			// Mount the project's .scion read-only and mask its agents dir (the
			// shared-workspace project-config protection). A git-backed shared workspace
			// was already refused above on Apple; a non-git shared workspace reaches here
			// on every runtime. On docker/podman the broker creates the (git-excluded)
			// agents dir first so the read-only mount leaves a mount point for the mask;
			// Apple masks only an existing agents dir.
			src, tgt, ok, err := workspaceProjectConfigMount(config.Workspace, "/workspace", config.RuntimeName != appleContainerRuntimeName)
			if err != nil {
				return nil, err
			}
			if ok {
				registerMount(src, tgt, true, true)
				if err := ensureAgentsMaskPoint(config, src); err != nil {
					return nil, err
				}
			}
			if target, ok := sharedWorkspaceAgentsMaskTarget(config, "/workspace", true); ok {
				sharedWorkspaceAgentsMask = target
			}
			// A project kept in a SUBDIRECTORY of this shared workspace (its .scion
			// is <ws>/<sub>/.scion, not <ws>/.scion) would otherwise stay writable
			// under the read-write /workspace bind: mount the nested .scion
			// read-only and mask its agents/.
			if err := applyNestedProjectConfig(config, "/workspace", registerMount, addMask); err != nil {
				return nil, err
			}
		} else {
			// Fallback: workspace is outside the repo root. Only a
			// registered worktree of RepoRoot may use this path, and then only
			// <main>/.git (narrowed) plus the workspace are mounted — never the
			// whole main checkout read-write. Anything else is refused.
			registered, regErr := util.IsRegisteredWorktree(config.RepoRoot, config.Workspace)
			if regErr != nil || !registered {
				return nil, fmt.Errorf("agent %q: workspace %q against repository root %q: %w", config.Name, config.Workspace, config.RepoRoot, errOutsideRootNotRegistered)
			}
			gitDir := filepath.Join(config.RepoRoot, ".git")
			// Derive the read-write own-worktree overlay target from the common
			// dir's trusted back-links, not the agent-writable gitfile; refuse if
			// it cannot be resolved.
			name, ok := util.TrustedWorktreeAdminName(gitDir, config.Workspace)
			if !ok {
				return nil, fmt.Errorf("agent %q: cannot resolve the broker-recorded worktree admin directory for workspace %q against %q; refusing to mount git metadata: %w", config.Name, config.Workspace, gitDir, errWorktreeAdminUnresolved)
			}
			// The .git root is read-only (no root-level commondir/other honoured
			// file can be created); objects/refs/logs/own-worktree are read-write.
			// docker/podman additionally bind the per-agent private config; Apple
			// reads the repository config directly (documented).
			if err := registerReadOnlyBaseGitMounts(registerMount, gitDir, "/repo-root/.git", name); err != nil {
				return nil, err
			}
			if config.RuntimeName == appleContainerRuntimeName {
				if err := refuseAppleRepoConfigWithSecrets(config.Name, gitDir); err != nil {
					return nil, err
				}
			} else {
				if err := registerPrivateGitConfig(registerMount, config, "/repo-root/.git"); err != nil {
					return nil, err
				}
			}
			registerMount(config.Workspace, "/workspace", false, true)
			addArg("--workdir", "/workspace")
			// A non-own registered worktree may hold a project's .scion the broker
			// reads; the workspace itself is bound read-write at /workspace, so its
			// project config must be protected exactly as in the other not-own
			// layouts: bind <ws>/.scion read-only and mask its agents/ when it
			// exists (never created here — the worktree is the user's checkout), and
			// mount a nested <ws>/<sub>/.scion read-only with its agents/ masked.
			// This stays below /workspace; the fallback still mounts only
			// /repo-root/.git and /workspace, never /repo-root, so nothing above the
			// project root is exposed and the negative /repo-root test holds.
			if !config.AgentOwnWorkspace {
				src, tgt, ok, err := workspaceProjectConfigMount(config.Workspace, "/workspace", config.RuntimeName != appleContainerRuntimeName)
				if err != nil {
					return nil, err
				}
				if ok {
					registerMount(src, tgt, true, true)
					if err := requireExistingAgentsMask(config, src, tgt, addMask); err != nil {
						return nil, err
					}
				}
				if err := applyNestedProjectConfig(config, "/workspace", registerMount, addMask); err != nil {
					return nil, err
				}
			}
		}
	} else if config.Workspace != "" {
		registerMount(config.Workspace, "/workspace", false, true)
		addArg("--workdir", "/workspace")
		// A workspace that holds a project configuration directory (a
		// project that keeps its .scion directory in place, or an explicit
		// workspace pointing at one) gets it read-only, with its agents
		// root masked, as in the shared-workspace layout above: on
		// Docker/Podman the broker creates the agents dir first (the
		// read-only mount leaves the runtime no way to create the mount
		// point), so the mask is always present; Apple masks only an
		// existing agents dir. The agent's own workspace
		// (AgentOwnWorkspace) is left as is.
		if !config.AgentOwnWorkspace {
			src, tgt, ok, err := workspaceProjectConfigMount(config.Workspace, "/workspace", config.RuntimeName != appleContainerRuntimeName)
			if err != nil {
				return nil, err
			}
			if ok {
				registerMount(src, tgt, true, true)
				if err := ensureAgentsMaskPoint(config, src); err != nil {
					return nil, err
				}
				if isDir(filepath.Join(src, "agents")) {
					sharedWorkspaceAgentsMask = path.Join(tgt, "agents")
				}
			}
		}
		// A project whose .scion is in a SUBDIRECTORY of this workspace (for
		// example a settings workspace_path pointing at an enclosing directory)
		// gets the nested .scion read-only and its agents/ masked too.
		if err := applyNestedProjectConfig(config, "/workspace", registerMount, addMask); err != nil {
			return nil, err
		}
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
	if volumeErr != nil {
		return nil, volumeErr
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
	uid, gid := AdvertisedHostOwnerIDs(config.WorkspaceBackendName, config.NFSUID, config.NFSGID)
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
			mkdirUnderHome(agentHome, ".config/gcloud")
			registerMount(gcloudConfigDir, fmt.Sprintf("/home/%s/.config/gcloud", config.UnixUsername), true, false)
		}
	}

	for _, e := range config.Env {
		parts := strings.SplitN(e, "=", 2)
		if parts[0] == SupplementalGIDsEnvVar {
			// Broker-owned: only appendSharedDirGroupArgs sets it
			// (ptone/scion#3155), never template or user env.
			continue
		}
		if parts[0] == MetadataInterceptionEnvVar {
			// Runtime-owned: set below from config.MetadataInterception.
			continue
		}
		if len(parts) == 2 {
			addArg("-e", fmt.Sprintf("%s=%s", parts[0], parts[1]))
		} else if !util.IsAgentRefusedHostEnvName(e) {
			// A bare name makes the container CLI copy the value from
			// this process's env; names refused for agents never are.
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
		if _, collides := envKeys[s.Target]; collides || s.Target == SupplementalGIDsEnvVar || s.Target == MetadataInterceptionEnvVar {
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
				resolved, err := checkHostFileSource(abs)
				if err != nil {
					return nil, err
				}
				registerMount(resolved, "/opt/scion/bin", true, true)
			}
		}
	}

	// Pre-create parent directories on the host for any volume targeting a
	// path under the container home directory.  When the home dir is itself a
	// bind mount, the container runtime (crun/runc) will try to mkdir the
	// mount-point inside that mount.  On rootless Podman with VirtioFS (macOS)
	// this fails with "Permission denied" unless the directory already exists.
	if agentHome != nil {
		containerHome := util.GetHomeDir(config.UnixUsername)
		for _, tgt := range volumeOrder {
			if strings.HasPrefix(tgt, containerHome+"/") {
				mkdirUnderHome(agentHome, strings.TrimPrefix(tgt, containerHome+"/"))
			}
		}
	}

	// Add all registered volumes. Every bind source, whatever produced it
	// (workspace, home, configured volume, shared dir, git, auth file), is
	// checked here against the Runtime Broker control paths, and the
	// resolved source is what is mounted.
	var emittedBinds []BindMount
	for _, tgt := range volumeOrder {
		spec := volumeMap[tgt]
		src, err := checkBindSource(spec.src)
		if err != nil {
			return nil, err
		}
		val := fmt.Sprintf("%s:%s", src, tgt)
		if spec.ro {
			val += ":ro"
		}
		addArg("-v", val)
		if !isNamedVolume(src) {
			emittedBinds = append(emittedBinds, BindMount{Source: src, Destination: tgt})
		}
	}
	// The record of the binds (LabelBindSources) is emitted after every
	// other label, below.
	bindSourcesLabel, err := encodeBindSourcesLabel(emittedBinds)
	if err != nil {
		return nil, fmt.Errorf("recording container binds: %w", err)
	}

	// (The whole repo root is never mounted read-write any more: the
	// outside-repo-root fallback now mounts only <main>/.git plus the workspace
	// for a registered worktree, and refuses otherwise, so there is
	// no /repo-root/.scion on the host to shadow.)
	// Shadow the in-project agents root of a shared workspace mount with a
	// tmpfs, so it is empty in the container. The Apple runtime drops
	// --mount arguments (stripUnsupportedAppleFlags), so it gets the
	// equivalent --tmpfs flag. Kubernetes keeps the path out of its pods
	// itself (k8sWorkspaceAgentsExcluded).
	if sharedWorkspaceAgentsMask != "" {
		if config.RuntimeName == appleContainerRuntimeName {
			addArg("--tmpfs", sharedWorkspaceAgentsMask)
		} else {
			addArg("--mount", "type=tmpfs,destination="+sharedWorkspaceAgentsMask)
		}
	}

	// No-create agents masks requested by the nested and fallback own-.scion
	// paths (requireExistingAgentsMask), emitted AFTER the bind mounts so each
	// tmpfs lands over its read-only .scion bind. The helper adds a mask only
	// for an agents dir that already exists on the broker (the start is
	// otherwise refused on Docker/Podman), so nothing is created here. Deduped
	// against the shared-workspace mask and each other.
	emittedMask := map[string]bool{sharedWorkspaceAgentsMask: true}
	for _, mask := range nestedMasks {
		if mask == "" || emittedMask[mask] {
			continue
		}
		emittedMask[mask] = true
		if config.RuntimeName == appleContainerRuntimeName {
			addArg("--tmpfs", mask)
		} else {
			addArg("--mount", "type=tmpfs,destination="+mask)
		}
	}

	// Add NET_ADMIN capability for iptables-based metadata server interception,
	// and tell sciontool whether that interception is required. The marker
	// always comes from config.MetadataInterception, never from config.Env, a
	// secret or the template fallback (all of which skip this key), so it
	// cannot disagree with the capability. The Apple runtime drops --cap-add
	// (stripUnsupportedAppleFlags), so it never reports the interception as
	// required.
	if config.MetadataInterception {
		addArg("--cap-add", "NET_ADMIN")
	}
	if config.MetadataInterception && config.RuntimeName != appleContainerRuntimeName {
		addArg("-e", MetadataInterceptionEnvVar+"="+MetadataInterceptionRequired)
	} else {
		addArg("-e", MetadataInterceptionEnvVar+"="+MetadataInterceptionOff)
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

	// LabelBindSources is the broker's own record of the binds: no other
	// label source may set it.
	for k, v := range config.Labels {
		if k == LabelBindSources {
			continue
		}
		addArg("--label", fmt.Sprintf("%s=%s", k, v))
	}
	for k, v := range config.Annotations {
		if k == LabelBindSources {
			continue
		}
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

	// The record of the binds goes last, so no earlier label can stand in
	// for it (LabelBindSources).
	addArg("--label", LabelBindSources+"="+bindSourcesLabel)

	// Get command from harness.
	// No-auth mode builds a raw shell command string that is embedded
	// directly into the sh -c used by the tmux agent window. This avoids
	// the double-sh-c wrapping that previously caused the no-auth command
	// to be injected as terminal input instead of running standalone.
	cmdLine, ok := harnessCmdLine(config, harnessCmdLineOptions{})
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

	// The template env fallback (ptone/scion#4524) is the LAST env step:
	// only keys no source above emitted, never a suppressed key. Docker
	// keeps the last -e for a key, so nothing may be added after this.
	for _, e := range FallbackEnvEntries(config.FallbackEnv, config.SuppressedFallbackKeys, envKeysOfArgs(args)) {
		addArg("-e", e)
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

// legacyProjectsDirName is the pre-rename name of the global
// ~/.scion/projects directory (pkg/config's own legacyProjectsDirName,
// unexported, is this same literal — duplicated here rather than imported so
// this file's dependency surface stays unchanged; see
// hack/check-project-compat-literals.sh's allowlist entry for this file).
const legacyProjectsDirName = "groves"

// isHubManagedWorktreeBase reports whether repoRoot is the broker-managed
// shared base clone for a hub-native worktree-per-agent project, i.e. it
// lives under the broker's conventional ~/.scion/projects/<slug> (or legacy
// ~/.scion/groves/<slug>) path.
//
// This mirrors the hub's own linked-vs-hub-native distinction: per
// pkg/hub/httpdispatcher.go (resolveDispatchProjectInfo), a project
// provider's LocalPath — set when a project is linked to an existing local
// checkout — takes precedence over hub-native slug resolution; only in its
// absence does the broker (buildStartContext) resolve the project path via
// the ~/.scion/projects/<slug> convention. A linked project's RepoRoot
// therefore points at the user's own checkout, outside this tree, so this
// check is false for it by construction. The narrowed .git admin-dir mount
// (narrowGitAdminMounts) must apply only when this returns true — a linked
// project's own hooks are host-trusted and must keep running unmodified.
//
// Any error resolving the broker's global dir (e.g. no home directory) fails
// OPEN with respect to this mount narrowing — it is skipped for
// that agent — rather than risk misclassifying a linked project as
// hub-managed and breaking its legitimate hooks. This is a deliberate
// trade-off given the two failure modes are asymmetric (a skipped mount
// narrowing leaves the pre-existing exposure; a wrong positive breaks a
// user's own repo), not a claim that it is the safer default in general.
func isHubManagedWorktreeBase(repoRoot string) bool {
	if repoRoot == "" {
		return false
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil || globalDir == "" {
		return false
	}
	repoAbs, err := filepath.Abs(repoRoot)
	if err != nil {
		return false
	}
	// RepoRoot is symlink-resolved by Start and again in buildCommonRunArgs
	// before this call, whatever its source, and this function resolves
	// repoAbs itself; os.UserHomeDir() may instead be reached through a
	// symlink (e.g. /home -> /var/home). Resolve both sides the same way
	// before comparing so a symlinked home directory doesn't cause a
	// hub-native base to be misclassified as linked (and so miss the mount
	// narrowing).
	if resolved, evalErr := filepath.EvalSymlinks(repoAbs); evalErr == nil {
		repoAbs = resolved
	}
	// Check both the canonical ~/.scion/projects/<slug> name and the
	// pre-migration ~/.scion/groves/<slug> name: config.MigrateLegacyGlobalLayout
	// renames the latter to the former, but runs separately from (and is not
	// a precondition of) this check, so a not-yet-migrated hub-native base can
	// still be live here. legacyProjectsDirName is a local literal, not
	// imported from pkg/config's own (unexported) copy, mirroring
	// pkg/config/paths.go's own ProjectsDir/legacy pairing — see
	// hack/check-project-compat-literals.sh's allowlist entry for this file.
	for _, sub := range []string{config.ProjectsDir, legacyProjectsDirName} {
		base := filepath.Clean(filepath.Join(globalDir, sub))
		if resolved, evalErr := filepath.EvalSymlinks(base); evalErr == nil {
			base = resolved
		}
		if repoAbs == base || strings.HasPrefix(repoAbs, base+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// narrowGitAdminMounts layers read-only bind mounts over the shared base
// repo's admin surface — .git/config, .git/hooks/, .git/info/, and (if
// present) the per-worktree .git/worktrees/<name>/config.worktree — on top of
// the already-registered read-write /repo-root/.git mount. This is Part A of
// the broker-git worktree containment change: it removes a container's
// ability to write the files host-side git later honors (hooks,
// core.hooksPath/fsmonitor/sshCommand, smudge/clean filter selection via
// info/attributes) when the broker runs git against this same base — see
// pkg/provision/provision.go's SafeGitCommand for the invocation-level
// additional check.
//
// Objects, refs, packed-refs, and the per-worktree HEAD/index/logs/ORIG_HEAD
// are deliberately left writable: a worktree shares the common object store,
// so in-container `git commit` must still be able to write loose objects and
// update refs. Only the admin subpaths above are narrowed — the container
// never legitimately needs to write them (identity and credentials are
// supplied via $HOME/.gitconfig, not .git/config, and there is no
// in-container `git config --local` write).
//
// objects/ stays writable on purpose (in-container commit) and is shared
// across the project's agents, so this read-only narrowing (here and in
// narrowSharerRegistryMounts) is not, on its own, complete hardening of the
// shared store (ptone/scion#4611). The durable fix is a read-only shared
// store as an alternate plus a writable per-container
// store.
//
// Each subpath is mounted read-only only if it currently exists, so a
// freshly-cloned base without a config.worktree (the common case —
// extensions.worktreeConfig is opt-in) does not cause Docker to create an
// empty file/dir on the host for a nonexistent bind-mount source.
func narrowGitAdminMounts(registerMount func(string, string, bool, bool), gitDir, containerGitDir string) error {
	// .git/hooks and .git/info are present after a default `git init`, but
	// an empty/custom init.templateDir (or `git init --template=`) can
	// produce a base without one or both. Skipping the mount in that case
	// would fail OPEN: the container could then create its own writable
	// hooks/ or info/ directly in the rw .git root. Create them on the host
	// first (an empty read-only directory is harmless) so the mount always
	// applies.
	for _, dir := range []string{"hooks", "info"} {
		// Create-first through the component-checked path (fail closed on a symlink
		// or non-directory) so the read-only mount always applies and a container
		// cannot create its own writable hooks/ or info/ in the rw .git root.
		if err := bindChecked(registerMount, gitDir, containerGitDir, true, bindDirCreate, dir); err != nil {
			return err
		}
	}

	// .git/config is bound read-only (a per-agent redacted copy; see
	// registerPrivateGitConfig), so repository config is read-only to the agent.

	// The common .git/config.worktree is bound read-only too (create-first): git
	// consults it when extensions.worktreeConfig is set. The agent's own
	// worktrees/<name>/config.worktree is handled in narrowWorktreesDir.
	if err := bindChecked(registerMount, gitDir, containerGitDir, true, bindFileCreate, "config.worktree"); err != nil {
		return err
	}

	// .git/modules/<sub>/{config,hooks,info}: a superproject's submodule admin
	// dirs are also honored by host-side git and by the agent's own in-container
	// git for that submodule. Narrow each that exists at start (dirs are
	// create-first like hooks/info above; config is a file bound only if present).
	return narrowModuleAdminDirs(registerMount, gitDir, containerGitDir)
}

// bindSourceMode selects how checkedBindSource treats the leaf of a git-metadata
// bind source.
type bindSourceMode int

const (
	bindDirRequire  bindSourceMode = iota // leaf must be an existing real directory; missing -> absent
	bindDirCreate                         // create the directory (and missing parents) if absent; -> present
	bindFileRequire                       // leaf must be an existing regular file; missing -> absent
	bindFileCreate                        // create the regular file (no-follow) if absent; -> present
)

// bindSourceRaceHook, when non-nil, is invoked by checkedBindSource in the window
// after every component has been verified and immediately before the leaf is
// created. It is nil in production (the guarded call then costs nothing); it exists
// only so a test can deterministically stand in for a concurrent swap of an
// already-verified component — replacing a verified real directory with a symlink
// in exactly the create window the post-create re-walk is there to close.
var bindSourceRaceHook func(leaf string)

// reverifyChain re-walks gitRoot/comps with os.Lstat after a component has been
// created, confirming that no component became a symlink and that the leaf still
// has the expected type. The forward walk verifies each component one at a time,
// but a component verified as a real directory can be swapped for a symlink before
// the leaf is created (O_NOFOLLOW guards only the final path component, never an
// intermediate one), which would land the created leaf outside the repository. This
// re-walk runs after the create and fails closed on any such swap, so the caller
// can refuse the start and remove what it created.
func reverifyChain(gitRoot string, comps []string, leafWantDir bool) error {
	cur := gitRoot
	last := len(comps) - 1
	for i, c := range comps {
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		if err != nil {
			return fmt.Errorf("re-check git metadata bind source %s: %w", cur, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("git metadata bind source %s became a symlink", cur)
		}
		if i < last {
			if !fi.IsDir() {
				return fmt.Errorf("git metadata bind source parent %s is not a directory", cur)
			}
			continue
		}
		if leafWantDir {
			if !fi.IsDir() {
				return fmt.Errorf("git metadata bind source %s is not a directory", cur)
			}
		} else if !fi.Mode().IsRegular() {
			return fmt.Errorf("git metadata bind source %s is not a regular file", cur)
		}
	}
	return nil
}

// bindChecked validates (and for the *Create modes creates) the git-metadata bind
// source gitRoot/comps via checkedBindSource and, when present, registers it over
// containerRoot/comps with the given read-only flag. Every git-metadata bind
// source goes through here (or through ensureOverlayDir, which also calls
// checkedBindSource), so none resolves via os.Stat/fileExists/MkdirAll/OpenFile
// unchecked.
func bindChecked(registerMount func(string, string, bool, bool), gitRoot, containerRoot string, readOnly bool, mode bindSourceMode, comps ...string) error {
	src, present, err := checkedBindSource(gitRoot, mode, comps...)
	if err != nil {
		return err
	}
	if present {
		registerMount(src, filepath.Join(append([]string{containerRoot}, comps...)...), readOnly, true)
	}
	return nil
}

// ensureOverlayDir inspects a single read-write overlay source directly under a
// trusted parent (objects/, refs/, logs/, lfs/, rr-cache/ under the .git root)
// through the same checkedBindSource fail-closed path. createIfMissing selects
// bindDirCreate vs bindDirRequire; a symlink or non-directory refuses.
func ensureOverlayDir(src string, createIfMissing bool) (present bool, err error) {
	mode := bindDirRequire
	if createIfMissing {
		mode = bindDirCreate
	}
	_, present, err = checkedBindSource(filepath.Dir(src), mode, filepath.Base(src))
	return present, err
}

// bindReadWriteOverlayDir binds src read-write over dst through ensureOverlayDir.
func bindReadWriteOverlayDir(registerMount func(string, string, bool, bool), src, dst string, createIfMissing bool) error {
	present, err := ensureOverlayDir(src, createIfMissing)
	if err != nil {
		return err
	}
	if present {
		registerMount(src, dst, false, true)
	}
	return nil
}

// narrowModuleAdminDirs binds read-only every .git/modules/<sub>/{hooks,info}
// directory and <sub>/config file that exists at start, under containerGitDir.
// Submodule admin dirs are honored by host-side git and by the agent's own
// in-container git for that submodule, so they get the same treatment as the
// top-level hooks/info/config. Submodules created in-container after start are
// out of scope (not present at mount time).
//
// The admin dirs come from util.SubmoduleAdminDirs, the same discovery the seed
// checks, so the set bound equals the set checked. Its unsafe and err results
// are ignored here, as the walk ignored them before; the seed refuses on them.
func narrowModuleAdminDirs(registerMount func(string, string, bool, bool), gitDir, containerGitDir string) error {
	dirs, _, _ := util.SubmoduleAdminDirs(gitDir)
	for _, rel := range dirs {
		// Each bound source below is resolved through checkedBindSource
		// (component-checked, symlink refused).
		relComps := strings.Split(rel, "/")
		leaf := func(name string) []string {
			c := make([]string, 0, len(relComps)+1)
			c = append(c, relComps...)
			return append(c, name)
		}
		for _, dir := range []string{"hooks", "info"} {
			if e := bindChecked(registerMount, gitDir, containerGitDir, true, bindDirRequire, leaf(dir)...); e != nil {
				return e
			}
		}
		if e := bindChecked(registerMount, gitDir, containerGitDir, true, bindFileRequire, leaf("config")...); e != nil {
			return e
		}
	}
	return nil
}

// narrowSharerRegistryMounts layers read-only bind mounts over two further
// paths in the shared base repo's .git, on top of the already-registered
// read-write /repo-root/.git mount: the sharer-registry marker directory
// (.git/scion-sharers/) and this worktree's admin back-link file
// (.git/worktrees/<name>/gitdir). This is Part B of the broker-git worktree
// containment change, and — unlike narrowGitAdminMounts above — applies on
// Docker regardless of isHubManagedWorktreeBase: neither path is ever
// legitimately written by anything other than the broker itself, on either
// base type, so the hub-native-only carve-out that exists for the broader
// admin surface (a linked project's own host-trusted hooks/config) does not
// apply here.
//
// pkg/provision's read/JOIN/teardown boundary validation (sharers.go's
// readMarker and worktree_validate.go's ValidateWorktreeForBase) is the
// primary, base-type-independent control: it already makes an invalid
// marker or a rewritten back-link harmless regardless of whether this mount
// narrowing applies. This function is an additional layer on top of that —
// it prevents the write from inside a container in the first place.
//
// The sharer registry directory may not exist yet (no branch has been
// shared) — it is created on the host first so the read-only mount always
// applies, the same fail-open concern narrowGitAdminMounts' hooks/info
// handling documents: otherwise a container could create its own writable
// one directly in the still-read-write .git root. The admin back-link file,
// by contrast, is written once by `git worktree add` before this worktree's
// container ever starts, so — like config.worktree above — it keeps
// skip-if-missing: there is no create-first fail-open gap to close, only a
// path that legitimately might not exist for a base with no worktrees yet.
func narrowSharerRegistryMounts(registerMount func(string, string, bool, bool), gitDir, containerGitDir string) error {
	// Create-first through the component-checked path (fail closed on a symlink or
	// non-directory) so the read-only mount always applies and a container cannot
	// create its own writable scion-sharers/ in the rw .git root.
	// The per-worktree admin back-link (worktrees/<name>/gitdir) is pinned
	// read-only in narrowWorktreesDir, using the broker-trusted admin name.
	return bindChecked(registerMount, gitDir, containerGitDir, true, bindDirCreate, sharerRegistryDirName)
}

// narrowObjectsInfo binds .git/objects/info read-only under containerGitDir.
// The object store itself must stay writable (in-container commit writes loose
// objects), but objects/info holds `alternates`/`http-alternates`, which host
// git honors to read objects from another repository path — a cross-repo read
// primitive. It is a directory, so this works as a bind on every runtime,
// including Apple. Created first (like hooks/info) so the mount always applies.
func narrowObjectsInfo(registerMount func(string, string, bool, bool), gitDir, containerGitDir string) error {
	// Create-first through the component-checked path: the intermediate objects/ and
	// the leaf objects/info are each Lstat-checked (symlink/non-dir refused).
	return bindChecked(registerMount, gitDir, containerGitDir, true, bindDirCreate, "objects", "info")
}

// narrowWorktreesDir binds .git/worktrees read-only, then re-mounts the agent's
// own worktrees/<name>/ read-write on top. Sibling worktree admin dirs
// (HEAD, index, commondir, gitdir, config.worktree) are then not writable from
// this container, closing sibling HEAD modification and commondir/gitdir
// redirection — and a sibling's config.worktree, which that sibling's
// in-container git honors (#5733 covers only host-side git). The agent's own
// worktrees/<name>/ stays writable for its HEAD/index/logs; its gitdir, commondir
// and config.worktree are pinned read-only here (file binds, below). The whole
// worktrees/ dir is a directory bind, so the sibling protection works on every
// runtime.
// worktreeName must be the broker-trusted admin name (util.TrustedWorktreeAdminName),
// or "" for a layout with no own per-agent worktree (shared-plain).
func narrowWorktreesDir(registerMount func(string, string, bool, bool), gitDir, containerGitDir, worktreeName string) error {
	// The whole worktrees/ dir is read-only so a container cannot write any
	// sibling's admin files (HEAD, index, commondir, gitdir, config.worktree).
	wtSrc, present, err := checkedBindSource(gitDir, bindDirRequire, "worktrees")
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	registerMount(wtSrc, filepath.Join(containerGitDir, "worktrees"), true, true)
	if worktreeName == "" {
		return nil
	}
	ownSrc, ownPresent, err := checkedBindSource(gitDir, bindDirRequire, "worktrees", worktreeName)
	if err != nil {
		return err
	}
	if !ownPresent {
		return nil
	}
	// The agent's own worktree admin dir is read-write for HEAD/index/logs/ORIG_HEAD.
	registerMount(ownSrc, filepath.Join(containerGitDir, "worktrees", worktreeName), false, true)
	// …but its back-link (gitdir), its commondir pointer, and its config.worktree
	// stay read-only (create-first through the component-checked path), so the agent
	// cannot redirect its own gitdir/commondir or write worktree-scoped config.
	// These sit inside the read-write own-admin overlay, so a failed create would
	// fail open; refuse the start.
	for _, f := range []string{"gitdir", "commondir", "config.worktree"} {
		if e := bindChecked(registerMount, gitDir, containerGitDir, true, bindFileCreate, "worktrees", worktreeName, f); e != nil {
			return e
		}
	}
	return nil
}

// registerPrivateGitConfig binds the per-agent private .git/config copy
// (config.PrivateGitConfigSource) READ-ONLY as a single file over
// containerGitDir/config. The repository config is therefore read-only to the
// agent on every runtime: the agent reads a redacted, per-agent copy, and any
// write (git config --local, remote add) fails because the mount point cannot be
// renamed over. The copy is pre-seeded with the agent's own branch upstream and
// push.autoSetupRemote so a bare `git push` finds the upstream already set and
// exits 0 without a config write; `git push -u` also exits 0 but cannot persist
// the upstream and prints an error. Personal preferences go in the per-agent
// writable global git config in the agent home.
//
// Immediately before the bind the source is re-checked: it must be a regular
// file, not a symlink, with link count 1, owned by this (broker) process.
// A missing or unsafe source returns an error so the caller refuses the start —
// the config must never fall back to the shared, host-writable base file. The
// broker never reads this file back.
func registerPrivateGitConfig(registerMount func(string, string, bool, bool), config RunConfig, containerGitDir string) error {
	src := config.PrivateGitConfigSource
	if src == "" {
		return fmt.Errorf("agent %q: a private .git/config source is required for this git layout but was not seeded", config.Name)
	}
	if err := checkPrivateGitConfigSource(src); err != nil {
		return fmt.Errorf("agent %q: refusing to bind the private .git/config: %w", config.Name, err)
	}
	registerMount(src, filepath.Join(containerGitDir, "config"), true, true)
	return nil
}

// registerWorktreeGitMounts registers the .git mounts for an in-repo worktree
// layout (workspace is a subdirectory of the repo root). On every runtime the
// base .git is bound read-only (registerReadOnlyBaseGitMounts), with read-write
// overlays only for the paths an in-container git must write, so no root-level
// file git honours can be created. On docker/podman a per-agent redacted private
// config is then bound read-only over .git/config; Apple cannot bind a single
// file, so there the agent reads the repository config directly (documented).
func registerWorktreeGitMounts(registerMount func(string, string, bool, bool), config RunConfig) error {
	gitDir := filepath.Join(config.RepoRoot, ".git")
	const containerGit = "/repo-root/.git"
	// The read-write own-worktree overlay must target the admin dir the broker's
	// `git worktree add` created, identified from the common dir's trusted
	// back-links — never the agent-writable <workspace>/.git gitfile. If it
	// cannot be resolved, refuse and mount nothing.
	name, ok := util.TrustedWorktreeAdminName(gitDir, config.Workspace)
	if !ok {
		return fmt.Errorf("agent %q: cannot resolve the broker-recorded worktree admin directory for workspace %q against %q; refusing to mount git metadata: %w", config.Name, config.Workspace, gitDir, errWorktreeAdminUnresolved)
	}
	// The .git root is read-only on every runtime (so no root-level commondir or
	// other honoured file can be created); objects/refs/logs/own-worktree are
	// overlaid read-write. On docker/podman a per-agent redacted private config is
	// then bound read-only over .git/config; Apple cannot bind a single file, so
	// there the agent reads the repository config directly (documented).
	if err := registerReadOnlyBaseGitMounts(registerMount, gitDir, containerGit, name); err != nil {
		return err
	}
	if config.RuntimeName == appleContainerRuntimeName {
		return refuseAppleRepoConfigWithSecrets(config.Name, gitDir)
	}
	return registerPrivateGitConfig(registerMount, config, containerGit)
}

// refuseAppleRepoConfigWithSecrets refuses the start when, on the Apple runtime,
// the repository's .git/config holds credential-bearing settings. Apple cannot
// bind a single file, so the agent reads the repository config directly (no
// redacted per-agent copy); a read-only mount keeps the agent from changing the
// config but still lets it read the values. The check runs
// util.RedactGitConfigSecrets on the config and refuses if anything would be
// stripped, naming only the sections (never the values). docker/podman are
// unaffected: they bind a redacted private copy.
func refuseAppleRepoConfigWithSecrets(agentName, gitDir string) error {
	raw, err := readSmallConfigFile(filepath.Join(gitDir, "config"))
	if err != nil {
		return fmt.Errorf("agent %q: cannot read the repository git config to vet it for the Apple runtime: %w", agentName, err)
	}
	if len(raw) == 0 {
		return nil
	}
	_, stripped, err := util.RedactGitConfigSecrets(raw)
	if err != nil {
		return fmt.Errorf("agent %q: cannot parse the repository git config to vet it for the Apple runtime: %w", agentName, err)
	}
	if len(stripped) == 0 {
		return nil
	}
	return fmt.Errorf("agent %q: the repository's .git/config holds credentials in section(s) %s; remove them, or use a runtime that supports a private git config", agentName, strings.Join(credentialSections(stripped), ", "))
}

// credentialSections returns the sorted, unique top-level sections of the given
// config keys (the part before the first "."), so a refusal can name the
// sections without ever quoting a credential value.
func credentialSections(keys []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, k := range keys {
		sec := k
		if i := strings.IndexByte(k, '.'); i >= 0 {
			sec = k[:i]
		}
		if _, ok := seen[sec]; !ok {
			seen[sec] = struct{}{}
			out = append(out, sec)
		}
	}
	slices.Sort(out)
	return out
}

// registerRwBaseGitNarrowing narrows a read-write .git base
// (docker/podman/k8s-style single-file binds supported). It layers the
// read-only admin surface and the per-agent read-only private config over an
// already read-write containerGitDir, and returns an error (refusing the start)
// if the private config cannot be bound. worktreeName is the broker-trusted
// admin name, or "" for a layout with no own per-agent worktree (shared-plain).
func registerRwBaseGitNarrowing(registerMount func(string, string, bool, bool), config RunConfig, gitDir, containerGitDir, worktreeName string) error {
	if err := narrowGitAdminMounts(registerMount, gitDir, containerGitDir); err != nil {
		return err
	}
	if err := narrowSharerRegistryMounts(registerMount, gitDir, containerGitDir); err != nil {
		return err
	}
	if err := narrowObjectsInfo(registerMount, gitDir, containerGitDir); err != nil {
		return err
	}
	if err := narrowWorktreesDir(registerMount, gitDir, containerGitDir, worktreeName); err != nil {
		return err
	}
	return registerPrivateGitConfig(registerMount, config, containerGitDir)
}

// registerReadOnlyBaseGitMounts implements the read-only-.git-root layout used by
// the worktree and outside-root fallback layouts on every runtime. The whole .git
// is bound read-only as a directory, so no new root-level entry (commondir,
// gitdir, or any other file git would honour) can be created. Only the specific
// paths an in-container git must write are re-opened read-write on top:
//   - objects/, refs/, logs/ (commit/fetch/push write loose objects, refs, reflogs);
//   - lfs/ (the in-container git-lfs object store) and rr-cache/ (rerere) — these
//     are directories, so writes land in their own subtrees, not the root;
//   - the agent's own worktrees/<name>/ (its HEAD/index/logs).
//
// objects/info/ is pinned read-only again on top of the read-write objects/
// overlay (alternates), as are the agent's own worktrees/<name>/{gitdir,commondir,
// config.worktree}. hooks/, info/, the root config and every other root entry
// stay read-only by inheritance from the read-only base. Overlay source dirs are
// created host-side first, failing closed (a symlink or non-directory source
// refuses the start).
//
// Operations that write a lock or state file directly in the .git root fail
// cleanly rather than silently, and are documented limitations of this layout
// (the same ones the Apple layout has): deleting a branch or fetch --prune
// (packed-refs.lock), git gc / git pack-refs, and a shallow fetch (shallow.lock)
// all need a writable .git root; objects under modules/<name>/ (submodules) are
// read-only too. Commit, branch creation, fetch, pull and push work normally.
// worktreeName must be the broker-trusted admin name (util.TrustedWorktreeAdminName),
// or "" when the layout has no own per-agent worktree.
func registerReadOnlyBaseGitMounts(registerMount func(string, string, bool, bool), gitDir, containerGitDir, worktreeName string) error {
	registerMount(gitDir, containerGitDir, true, true) // read-only base
	// objects/refs/logs are always present and needed; lfs is created so the
	// in-container git-lfs store works (harmless when absent). Each is a writable
	// overlay (created host-side, fail-closed).
	for _, dir := range []string{"objects", "refs", "logs", "lfs"} {
		if err := bindReadWriteOverlayDir(registerMount, filepath.Join(gitDir, dir), filepath.Join(containerGitDir, dir), true); err != nil {
			return err
		}
	}
	// rr-cache is overlaid read-write only if it already exists (createIfMissing
	// false): creating it host-side would turn on rerere in the user's own checkout
	// (git enables rerere automatically when .git/rr-cache exists and rerere.enabled
	// is unset). It passes the same ensureOverlayDir refusal, so a symlink or a
	// non-directory there is refused.
	if err := bindReadWriteOverlayDir(registerMount, filepath.Join(gitDir, "rr-cache"), filepath.Join(containerGitDir, "rr-cache"), false); err != nil {
		return err
	}
	// objects/info read-only again (alternates), on top of the rw objects/.
	if err := narrowObjectsInfo(registerMount, gitDir, containerGitDir); err != nil {
		return err
	}
	// The agent's own worktree admin dir must be writable (HEAD/index/logs), but
	// its gitdir, commondir and config.worktree stay read-only. The rw overlay and
	// each file go through the same checkedBindSource refusal, component by component;
	// a missing own dir means no own worktree (skip the admin binds), a symlink or
	// non-directory is refused.
	if worktreeName == "" {
		return nil
	}
	ownSrc, present, err := checkedBindSource(gitDir, bindDirRequire, "worktrees", worktreeName)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	registerMount(ownSrc, filepath.Join(containerGitDir, "worktrees", worktreeName), false, true)
	for _, f := range []string{"gitdir", "commondir", "config.worktree"} {
		if e := bindChecked(registerMount, gitDir, containerGitDir, true, bindFileCreate, "worktrees", worktreeName, f); e != nil {
			return e
		}
	}
	return nil
}

// registerSharedPlainGitMounts narrows the .git admin surface of a shared-plain
// workspace (the workspace IS the repo root, already bound read-write at
// /workspace). The .git root stays read-write — the main worktree's HEAD/index
// live there and the agent legitimately writes them — so only the admin surface
// (hooks/info/objects-info/sibling-worktrees, modules) is pinned read-only and a
// per-agent private .git/config is bound over the shared one. A root commondir/gitdir
// redirect is refused host-side (refuseRootCommonDirRedirect) and config.worktree is
// bound read-only (create-first); a root-level `shallow` an agent creates is a
// documented residual (no host-side control for it). The Apple runtime is refused: it cannot bind
// single files to make .git/config private, and a read-only .git would break the
// main worktree's HEAD/index writes.
func registerSharedPlainGitMounts(registerMount func(string, string, bool, bool), config RunConfig) error {
	gitDir := filepath.Join(config.Workspace, ".git")
	fi, err := os.Stat(gitDir)
	if err != nil || !fi.IsDir() {
		// A non-git (old-style) shared workspace, or .git is a file: nothing to
		// narrow here.
		return nil
	}
	if config.RuntimeName == appleContainerRuntimeName {
		return fmt.Errorf("agent %q: %w", config.Name, errAppleSharedWorkspaceUnsupported)
	}
	// Shared-plain has no per-agent worktree admin dir (agents share the main
	// checkout), so no own-worktree read-write overlay — pass "".
	return registerRwBaseGitNarrowing(registerMount, config, gitDir, "/workspace/.git", "")
}

// sharerRegistryDirName is the marker directory pkg/provision's sharer
// registry stores under a shared base's .git — see the unexported sharerDir
// constant in pkg/provision/sharers.go, which this must match. Duplicated as
// a literal because that constant is unexported (an internal name for
// pkg/provision's own registry-path helper, not part of its public API).
const sharerRegistryDirName = "scion-sharers"

// mkdirUnderHome creates the directory rel (relative to the agent home) and
// its missing parents through home, best effort: a nil home, an invalid rel
// (an absolute path or one with a ".." component) or a linked directory
// component is skipped.
func mkdirUnderHome(home *agenthome.Home, rel string) {
	if home == nil {
		return
	}
	_ = home.MkdirAll(rel, 0755)
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

// runtimeLog returns the structured logger for the runtime subsystem.
//
// It resolves slog.Default() at each call rather than once at package init:
// a package-level logger would capture the init-time handler, so a later
// slog.SetDefault (logging setup, --debug, SCION_LOG_LEVEL) would never reach
// runtime log lines, and those lines would lose their structured attributes
// (#1339).
func runtimeLog() *slog.Logger {
	return logging.Subsystem("runtime")
}

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
	runtimeLog().Debug("Executing command", "cmd", command, "argc", len(args))
	start := time.Now()
	cmd := exec.CommandContext(ctx, command, args...)
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if err != nil {
		logCommandFailure(ctx, command, len(args), elapsed, out)
		return string(out), fmt.Errorf("%s failed: %w", command, err)
	}
	runtimeLog().Debug("Command completed", "cmd", command, "argc", len(args), "duration", elapsed)
	return strings.TrimSpace(string(out)), nil
}

// stdoutCommandTimeout bounds runStdoutCommand. A variable so tests can
// shorten it.
var stdoutCommandTimeout = 30 * time.Second

// runStdoutCommand runs command and returns its standard output only, for
// callers that parse it (container inspect, daemon endpoint resolution): a
// warning the CLI writes to standard error must not corrupt the parse.
// Standard error is captured separately and logged with the output on
// failure. The run is bounded by stdoutCommandTimeout, and WaitDelay stops
// a child that keeps the output pipes open after the kill, so a hung CLI
// returns an error instead of blocking its caller. Logging otherwise
// follows runSimpleCommand.
func runStdoutCommand(ctx context.Context, command string, args ...string) (string, error) {
	runtimeLog().Debug("Executing command", "cmd", command, "argc", len(args))
	ctx, cancel := context.WithTimeout(ctx, stdoutCommandTimeout)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	elapsed := time.Since(start)
	if err != nil {
		logCommandFailure(ctx, command, len(args), elapsed, append(out, stderr.Bytes()...))
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("%s failed: %w (%v)", command, err, ctxErr)
		}
		return "", fmt.Errorf("%s failed: %w", command, err)
	}
	runtimeLog().Debug("Command completed", "cmd", command, "argc", len(args), "duration", elapsed)
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
		runtimeLog().Debug("Command failed", "cmd", command, "argc", argc, "duration", elapsed)
		return
	}
	runtimeLog().Debug("Command failed", "cmd", command, "argc", argc, "duration", elapsed, "output", strings.TrimSpace(string(out)))
}

// runSimpleCommandWithStdin is runSimpleCommand's counterpart for callers
// that need to deliver data to the child process over stdin rather than
// argv (see #1355 — secrets embedded in argv leak via /proc/<pid>/cmdline
// for the lifetime of the exec). It never logs the piped content.
func runSimpleCommandWithStdin(ctx context.Context, stdin io.Reader, command string, args ...string) (string, error) {
	// Log the command name and argument count only — see runSimpleCommand
	// comment above. The stdin payload is never logged.
	runtimeLog().Debug("Executing command with stdin", "cmd", command, "argc", len(args))
	start := time.Now()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if err != nil {
		logCommandFailure(ctx, command, len(args), elapsed, out)
		return string(out), fmt.Errorf("%s failed: %w", command, err)
	}
	runtimeLog().Debug("Command completed", "cmd", command, "argc", len(args), "duration", elapsed)
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
// runID is the cancelled create's scion.run_id label. When it is set, only
// containers carrying that label are removed, found by listing; the name is
// not used, so a container another run has since created under the same
// name is left alone (ptone/scion#2550). If the listing fails or finds
// nothing, nothing is removed. Without a run ID (a RunConfig built outside
// pkg/agent) the container is removed by name, as before run IDs existed.
//
// The Apple "container" CLI's "rm" does not support "-f" and fails if the
// container is still running (see AppleContainerRuntime.Delete), so for that
// runtime we kill first, then retry a plain "rm" a few times — kill is
// asynchronous and the container may not be immediately ready for removal.
// Docker/Podman support "rm -f" directly.
func rollbackCancelledCreate(command, containerName, runID string) {
	if containerName == "" && runID == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	apple := filepath.Base(command) == "container"

	targets := []string{containerName}
	if runID != "" {
		ids, err := containersWithRunID(cleanupCtx, command, apple, runID)
		if err != nil {
			runtimeLog().Warn("Failed to roll back cancelled create: could not list the run's containers; nothing removed",
				"cmd", command, "container", containerName, "run_id", runID, "error", err)
			return
		}
		if len(ids) == 0 {
			runtimeLog().Debug("Cancelled create left no container of its run", "cmd", command, "container", containerName, "run_id", runID)
			return
		}
		targets = ids
	}

	for _, target := range targets {
		if apple {
			appleKillAndRemove(cleanupCtx, command, target)
			continue
		}
		if out, err := runSimpleCommand(cleanupCtx, command, "rm", "-f", target); err != nil {
			runtimeLog().Warn("Failed to roll back cancelled create", "cmd", command, "container", target, "run_id", runID, "error", err, "output", strings.TrimSpace(out))
		}
	}
}

// containersWithRunID lists every container (running or not) labelled with
// scion.run_id=runID and returns their IDs: engine IDs on Docker and Podman,
// container names on Apple.
func containersWithRunID(ctx context.Context, command string, apple bool, runID string) ([]string, error) {
	if apple {
		raw, err := appleListContainers(ctx, command)
		if err != nil {
			return nil, err
		}
		var ids []string
		for _, c := range raw {
			if c.Configuration.Labels[api.LabelRunID] == runID && c.Configuration.ID != "" {
				ids = append(ids, c.Configuration.ID)
			}
		}
		return ids, nil
	}
	out, err := runSimpleCommand(ctx, command, "ps", "-a", "-q", "--no-trunc", "--filter", "label="+api.LabelRunID+"="+runID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// appleKillAndRemove kills and removes the Apple container id, retrying the
// rm a few times since kill is asynchronous. Failures are logged.
func appleKillAndRemove(ctx context.Context, command, id string) {
	_, _ = runSimpleCommand(ctx, command, "kill", id)

	var out string
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		out, err = runSimpleCommand(ctx, command, "rm", id)
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			runtimeLog().Warn("Failed to roll back cancelled create", "cmd", command, "container", id, "error", ctx.Err(), "output", strings.TrimSpace(out))
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	runtimeLog().Warn("Failed to roll back cancelled create", "cmd", command, "container", id, "error", err, "output", strings.TrimSpace(out))
}

func runInteractiveCommand(command string, args ...string) error {
	// Log command name and argument count only — see runSimpleCommand comment
	// and #127 / P5 for rationale.
	runtimeLog().Debug("Executing interactive command", "cmd", command, "argc", len(args))
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
		runtimeLog().Debug("Failed to write runtime debug file", "path", debugPath, "error", err)
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
		runtimeLog().Debug("Unable to probe Docker server version for host-gateway support", "error", err)
		return true
	}
	major, minor, ok := parseDockerServerVersion(strings.TrimSpace(out))
	if !ok {
		runtimeLog().Debug("Unable to parse Docker server version for host-gateway support", "version", out)
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
	var home *agenthome.Home
	defer func() {
		if home != nil {
			_ = home.Close()
		}
	}()
	for _, f := range ra.Files {
		if f.SourcePath == "" {
			// In broker mode, file content is projected from staged secrets
			// (SCION_STAGED_SECRETS), not copied from host paths. SourcePath is
			// intentionally cleared by run.go. Skip the copy/mount.
			continue
		}
		// No auth file may come from a Runtime Broker control path or from
		// the Scion global directory, whether it is copied or bound.
		sourcePath, err := checkHostFileSource(f.SourcePath)
		if err != nil {
			return err
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
			if home == nil {
				h, err := agenthome.OpenCreate(config.HomeDir, 0755)
				if err != nil {
					return fmt.Errorf("failed to open the agent home for auth file %s: %w", dst, err)
				}
				home = h
			}
			if err := copyFileIntoHome(sourcePath, home, relPath); err != nil {
				return fmt.Errorf("failed to copy auth file %s → %s: %w", f.SourcePath, dst, err)
			}
		} else {
			// Volume mount mode: register a read-only bind mount
			registerMount(sourcePath, containerPath, true, false)
		}
	}

	return nil
}

// copyFileIntoHome copies the host file src to rel in the agent home with
// src's permission bits less group and other write, creating missing parent directories (0755). Every
// write is confined to the home (pkg/agenthome): a link at rel is replaced,
// never written through, and a linked directory component is refused.
func copyFileIntoHome(src string, home *agenthome.Home, rel string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(filepath.Clean(rel)); dir != "." {
		if err := home.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory for auth file: %w", err)
		}
	}
	// Home.WriteFile sets the mode exactly (no umask). The old copy
	// (os.WriteFile) had the process umask applied to a new file, so strip
	// group and other write as the default umask (022) did: a group- or
	// other-writable source must not give a writable copy in the home.
	return home.WriteFile(rel, data, info.Mode().Perm()&^0o022)
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
// A secret that does not fit limit fails the call with an error naming it
// rather than being dropped. Values are never included in the error.
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

// AdvertisedHostOwnerIDs returns the uid and gid the runtime advertises
// to an agent container as SCION_HOST_UID and SCION_HOST_GID, which
// sciontool init realigns the container's scion user (and so the harness)
// to: the agent runtime process's own ids for every workspace backend but
// nfs, and the stable NFS owner ids (nfsOwnerIDs) for nfs. Container
// runtimes and the agent home ownership repair both use it, so the two
// never disagree.
func AdvertisedHostOwnerIDs(workspaceBackendName string, nfsUID, nfsGID int) (int, int) {
	if workspaceBackendName == "nfs" {
		return nfsOwnerIDs(nfsUID, nfsGID)
	}
	return os.Getuid(), os.Getgid()
}

// nfsOwnerIDs returns the stable uid and gid for an NFS workspace: each
// id as configured, or the default 1000 when it is 0 (unset).
func nfsOwnerIDs(uid, gid int) (int, int) {
	return provision.DefaultOwnerID(uid), provision.DefaultOwnerID(gid)
}

// SupplementalGIDsEnvVar lists the nfs shared-dir groups the runtime
// granted. sciontool keeps only ids that are also in its own supplementary
// groups (what the runtime actually granted), keeps them when it drops from
// root to the agent user (Go's privilege drop otherwise clears them), and
// clears the group bits of the umask (077 becomes 007) when any remain.
// Both runtimes set it and own it:
// Docker/Podman via appendSharedDirGroupArgs (buildCommonRunArgs drops
// template or user values), Kubernetes via withSupplementalGIDsEnv with
// the leaf gids the pod holds (supplementalGroups plus one equal to
// fsGroup). Mirrored in
// pkg/sciontool/suppgroups (EnvVar) (ptone/scion#3155).
const SupplementalGIDsEnvVar = "SCION_SUPPLEMENTAL_GIDS"

// MetadataInterceptionEnvVar tells sciontool inside a Docker/Podman agent
// whether the in-container GCP metadata interception is required
// (MetadataInterceptionRequired) or not (MetadataInterceptionOff). It is set
// only by buildCommonRunArgs, from RunConfig.MetadataInterception, the same
// field that grants NET_ADMIN; template, user and secret values are dropped.
// Mirrored in pkg/sciontool/metadata (InterceptionEnvVar).
const MetadataInterceptionEnvVar = "SCION_METADATA_INTERCEPTION"

// Values of MetadataInterceptionEnvVar.
const (
	MetadataInterceptionRequired = "required"
	MetadataInterceptionOff      = "off"
)

// sharedDirGroups returns the guarded shared-dir leaf groups for config,
// or nil when the agent mounts no shared dir from a shared backend.
func sharedDirGroups(config RunConfig) []int64 {
	if !config.SharedDirStorage.Shared() || len(config.SharedDirs) == 0 {
		return nil
	}
	var out []int64
	for _, gid := range config.SharedDirStorage.SupplementalGroups {
		if gid > 0 {
			out = append(out, gid)
		}
	}
	return out
}

// appendSharedDirGroupArgs adds one --group-add per nfs shared-dir leaf
// group, plus SupplementalGIDsEnvVar so sciontool keeps them after its
// privilege drop (ptone/scion#3155). When the runtime cannot add groups
// (supported false: rootless Podman, whose user namespace does not map the
// leaf gid, or Apple's container CLI), it logs a warning and returns args
// unchanged, so the agent starts as before.
func appendSharedDirGroupArgs(args []string, config RunConfig, runtimeName string, supported bool) []string {
	groups := sharedDirGroups(config)
	if len(groups) == 0 {
		return args
	}
	if !supported {
		runtimeLog().Warn("shared dir groups are not supported on this runtime; the agent starts without them "+
			"and may be unable to modify files other agents create in its nfs shared dirs",
			"runtime", runtimeName, "gids", groups)
		return args
	}
	ids := make([]string, 0, len(groups))
	for _, gid := range groups {
		id := strconv.FormatInt(gid, 10)
		args = append(args, "--group-add", id)
		ids = append(ids, id)
	}
	return append(args, "-e", SupplementalGIDsEnvVar+"="+strings.Join(ids, ","))
}
