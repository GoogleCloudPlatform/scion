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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
)

// DefaultSandboxBin is the path to the Cloud Run sandbox launcher binary.
// Exported so that pkg/config (which cannot import this package without
// creating a cycle) can assert equality with its own copy at test time.
const DefaultSandboxBin = "/usr/local/gcp/bin/sandbox"

// defaultScionRoot is the parent directory for all writable sandbox paths.
// Per-agent bind mounts under this directory satisfy the write-visibility
// requirement for agent home, workspace, tmux sockets, and shared dirs.
// With --rootfs /, the rootfs is READ-ONLY: writes to unmounted paths fail
// with EROFS (confirmed by diag-sbx6). Per-agent bind mounts under this
// directory provide the writable paths agents need.
const defaultScionRoot = "/scion"

// defaultStatePath is the default location for the sandbox state store.
const defaultStatePath = "/tmp/scion-sandbox-state.json"

// sandboxAgentHome is the path where the agent home is mounted INSIDE the
// sandbox. This differs from the host-side path (scionPaths.agentHome, e.g.
// /scion/agents/<slug>/home) because mountsFor maps source→destination with
// different paths. The rootfs is read-only (EROFS), so only mounted paths
// are writable.
//
// This path must match:
//   - supervisor.go:115 which hardcodes HOME="/home/<username>"
//   - The provisioner command in harness configs (e.g. /home/scion/.scion/...)
//
// When writing paths for code that runs INSIDE the sandbox, use this constant.
// When writing paths for code that runs on the HOST, use scionPaths.agentHome.
const sandboxAgentHome = "/home/scion"

// sandboxWorkspace is the path where the agent workspace is mounted INSIDE
// the sandbox. Like sandboxAgentHome, this differs from the host-side path
// (scionPaths.workspace, e.g. /scion/agents/<slug>/workspace). mountsFor
// maps the host path to this destination so that sciontool init — which
// defaults to /workspace when SCION_WORKSPACE_PATH is unset — writes to
// the writable bind mount rather than the read-only rootfs.
//
// This matches the Docker/Podman convention (common.go:210) where the
// workspace is always mounted at /workspace regardless of the host-side path.
const sandboxWorkspace = "/workspace"

// entrypointLogFile is the filename (relative to agentHome) where the
// entrypoint's stdout/stderr is captured on failure. When `exec sciontool
// init` succeeds the shell is replaced and the file contains normal init
// output; when it fails the file holds the error output that explains why.
const entrypointLogFile = ".scion-entrypoint.log"

// SandboxLauncherAvailable reports whether the Cloud Run Sandbox launcher
// binary is present on the filesystem.
func SandboxLauncherAvailable() bool {
	_, err := os.Stat(DefaultSandboxBin)
	return err == nil
}

// -----------------------------------------------------------------------
// State Store
//
// The sandbox CLI has no `list` command. The runtime maintains its own
// state in a JSON file (design doc §4.5).
// -----------------------------------------------------------------------

// sandboxStateEntry tracks a single sandbox's lifecycle.
type sandboxStateEntry struct {
	SandboxName   string            `json:"sandbox_name"`
	AgentID       string            `json:"agent_id"`
	ProjectID     string            `json:"project_id"`
	Project       string            `json:"project"`
	Labels        map[string]string `json:"labels"`
	Template      string            `json:"template"`
	HarnessConfig string            `json:"harness_config"`
	CreatedAt     time.Time         `json:"created_at"`
	AgentHome     string            `json:"agent_home"`
	Workspace     string            `json:"workspace"`
	Image         string            `json:"image"`
	Stopped       bool              `json:"stopped"`
	ExitCode      *int              `json:"exit_code,omitempty"`
	StoppedAt     *time.Time        `json:"stopped_at,omitempty"`
}

// sandboxStateStore is a thread-safe, JSON-backed store of sandbox entries.
type sandboxStateStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]*sandboxStateEntry
}

func newSandboxStateStore(path string) *sandboxStateStore {
	ss := &sandboxStateStore{
		path:    path,
		entries: make(map[string]*sandboxStateEntry),
	}
	ss.load()
	return ss
}

// add inserts or replaces an entry and persists the store.
func (s *sandboxStateStore) add(entry *sandboxStateEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[entry.SandboxName] = entry
	s.save()
}

// remove deletes an entry by sandbox name and persists the store.
func (s *sandboxStateStore) remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, name)
	s.save()
}

// get returns an entry by sandbox name, or nil if not found.
func (s *sandboxStateStore) get(name string) *sandboxStateEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[name]
}

// list returns a snapshot of all entries.
func (s *sandboxStateStore) list() []*sandboxStateEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*sandboxStateEntry, 0, len(s.entries))
	for _, e := range s.entries {
		cp := *e
		result = append(result, &cp)
	}
	return result
}

// markStopped records that a sandbox has exited.
func (s *sandboxStateStore) markStopped(name string, exitCode *int, stoppedAt *time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[name]; ok {
		e.Stopped = true
		e.ExitCode = exitCode
		e.StoppedAt = stoppedAt
		s.save()
	}
}

// save persists the store to disk. Caller must hold mu.
func (s *sandboxStateStore) save() {
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		runtimeLog.Warn("sandbox state store: failed to marshal", "error", err)
		return
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		runtimeLog.Warn("sandbox state store: failed to create dir", "error", err)
		return
	}
	// Atomic write: write to temp file then rename. Prevents corruption
	// from crashes during write (audit M2). 0600 permissions (audit M1).
	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		runtimeLog.Warn("sandbox state store: failed to write temp", "path", tmpPath, "error", err)
		return
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		runtimeLog.Warn("sandbox state store: failed to rename", "from", tmpPath, "to", s.path, "error", err)
	}
}

// load reads the store from disk. Caller must hold mu (or be in init).
func (s *sandboxStateStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			runtimeLog.Warn("sandbox state store: failed to read", "path", s.path, "error", err)
		}
		return
	}
	var entries map[string]*sandboxStateEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		runtimeLog.Warn("sandbox state store: failed to unmarshal", "path", s.path, "error", err)
		return
	}
	s.entries = entries
}

// reconcile checks each entry for liveness and removes confirmed-dead
// entries. Called once at startup. If the file is stale from a previous
// Instance, all entries are dead and will be pruned.
func (s *sandboxStateStore) reconcile(bin string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) == 0 {
		return
	}

	var toRemove []string
	for name, entry := range s.entries {
		if entry.Stopped {
			toRemove = append(toRemove, name)
			continue
		}
		// Probe liveness: try to exec 'true' in the sandbox.
		// If it fails, the sandbox is dead.
		// R1: absolute path required — the sandbox launcher resolves argv[0]
		// before the sandbox environment (including PATH) is in effect.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := runSimpleCommand(ctx, bin, "exec", name, "--", "/bin/true")
		cancel()
		if err != nil {
			runtimeLog.Info("sandbox state reconcile: sandbox not alive, removing",
				"name", name, "agentID", entry.AgentID)
			toRemove = append(toRemove, name)
		}
	}

	for _, name := range toRemove {
		delete(s.entries, name)
	}

	if len(toRemove) > 0 {
		s.save()
	}
}

// -----------------------------------------------------------------------
// Sandbox name helpers
// -----------------------------------------------------------------------

// sandboxNameRe matches characters NOT allowed in sandbox names.
var sandboxNameRe = regexp.MustCompile(`[^a-z0-9-]`)

// sanitizeSandboxName produces a safe sandbox name from an agent slug.
func sanitizeSandboxName(name string) string {
	s := strings.ToLower(name)
	s = sandboxNameRe.ReplaceAllString(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	s = strings.Trim(s, "-")
	if s == "" {
		s = "sandbox"
	}
	return s
}

// -----------------------------------------------------------------------
// /scion directory layout
// -----------------------------------------------------------------------

// scionPaths holds the resolved /scion paths for a single sandbox agent.
type scionPaths struct {
	root      string // e.g. /scion
	agentHome string // /scion/agents/<slug>/home
	workspace string // /scion/agents/<slug>/workspace
}

// prepareScionLayout creates the /scion directory structure for a sandbox
// agent and relocates broker-provisioned content into it.
//
// Motivation (§3.2a): --rootfs / is READ-ONLY (EROFS; no writable overlay,
// confirmed by diag-sbx6). --write only makes MOUNTED filesystems writable.
// Per-agent bind mounts under /scion make writable paths visible to the host.
func prepareScionLayout(rootDir, slug string, cfg RunConfig) (scionPaths, error) {
	p := scionPaths{
		root:      rootDir,
		agentHome: filepath.Join(rootDir, "agents", slug, "home"),
		workspace: filepath.Join(rootDir, "agents", slug, "workspace"),
	}

	// Create all directories.
	for _, d := range []string{p.agentHome, p.workspace} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return p, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// Relocate agent home to /scion and symlink back for broker readback.
	// The broker provisions agent-info.json etc. at config.HomeDir. The
	// symlink ensures the broker reads from the /scion copy where sandbox
	// writes are visible via the bind mount.
	if cfg.HomeDir != "" && !strings.HasPrefix(cfg.HomeDir, rootDir+"/") && cfg.HomeDir != rootDir {
		if err := relocateToScion(cfg.HomeDir, p.agentHome); err != nil {
			runtimeLog.Warn("could not relocate HomeDir to /scion; agent home writes may go to overlay",
				"homeDir", cfg.HomeDir, "scionHome", p.agentHome, "error", err)
		}
	}

	// Workspace: copy content from broker-provisioned workspace to /scion.
	// Do NOT remove the original — it may be shared among agents.
	if cfg.Workspace != "" && !strings.HasPrefix(cfg.Workspace, rootDir+"/") && cfg.Workspace != rootDir {
		if err := copyDirContents(cfg.Workspace, p.workspace); err != nil {
			// Non-fatal: agent can still read via rootfs; writes go to /scion.
			runtimeLog.Debug("workspace content not copied to /scion",
				"workspace", cfg.Workspace, "scionWorkspace", p.workspace, "error", err)
		}
	}

	// Create shared dirs under /scion/shared/<name>.
	for _, sd := range cfg.SharedDirs {
		sdPath := filepath.Join(rootDir, "shared", sd.Name)
		if err := os.MkdirAll(sdPath, 0755); err != nil {
			runtimeLog.Warn("failed to create shared dir in /scion layout",
				"name", sd.Name, "error", err)
		}
	}

	return p, nil
}

// relocateToScion moves the contents of src to dst and replaces src with
// a symlink to dst. If src does not exist or is already a symlink, this
// is a no-op.
func relocateToScion(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		// src doesn't exist — nothing to relocate.
		return nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Already a symlink (perhaps from a previous run).
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("relocateToScion: %s is not a directory", src)
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("readdir %s: %w", src, err)
	}

	// Move each entry from src to dst (atomic on same filesystem).
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if err := os.Rename(srcPath, dstPath); err != nil {
			// Cross-filesystem: fall back to not relocating this entry.
			runtimeLog.Debug("could not rename to /scion, skipping",
				"src", srcPath, "dst", dstPath, "error", err)
		}
	}

	// Replace original directory with symlink so broker readback works.
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("remove original dir %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(src), 0755); err != nil {
		return fmt.Errorf("ensure parent of symlink %s: %w", src, err)
	}
	if err := os.Symlink(dst, src); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", src, dst, err)
	}
	return nil
}

// copyDirContents copies files from src to dst (non-recursive, best-effort).
func copyDirContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			// Shallow: skip subdirectories for now.
			continue
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			continue
		}
		info, _ := entry.Info()
		perm := fs.FileMode(0644)
		if info != nil {
			perm = info.Mode().Perm()
		}
		_ = os.WriteFile(dstPath, data, perm)
	}
	return nil
}

// -----------------------------------------------------------------------
// Mount and environment construction
// -----------------------------------------------------------------------

// mountsFor returns the per-agent mount descriptors. Each sandbox gets
// only its own home, workspace, and entitled shared dirs --
// NOT the entire /scion root (which would expose every agent's paths).
//
// No tmux mount: gVisor runs without --host-uds, so AF_UNIX cannot cross
// the sandbox boundary (§4.4a). The tmux socket is deliberately
// sandbox-internal; tmux uses its default path inside the sandbox.
func mountsFor(paths scionPaths, sharedDirs []api.SharedDir) []string {
	// Mount the agent home at sandboxAgentHome (/home/scion). Note:
	// supervisor.go:113 only sets HOME when UID > 0 || Rootless, so when
	// the sandbox runs as root (UID 0, non-rootless) HOME is inherited
	// from the container environment. envFor() now sets HOME explicitly
	// to ensure it resolves here. The rootfs is read-only (EROFS,
	// confirmed by diag-sbx6), so the previous rm -rf / ln -sfn approach
	// fails. Mounting with a different destination avoids rootfs mutation
	// entirely and was confirmed working by diag-sbx6.
	//
	// Mount the workspace at sandboxWorkspace (/workspace) so that
	// sciontool init's git clone (which defaults to /workspace via
	// SCION_WORKSPACE_PATH) writes to the writable bind mount. Without
	// this remapping the workspace was mounted at the host-side path
	// (/scion/agents/<slug>/workspace) while /workspace remained on the
	// read-only rootfs, causing every git-linked agent to fail at init.
	mounts := []string{
		fmt.Sprintf("type=bind,source=%s,destination=%s", paths.agentHome, sandboxAgentHome),
		fmt.Sprintf("type=bind,source=%s,destination=%s", paths.workspace, sandboxWorkspace),
	}
	for _, sd := range sharedDirs {
		sdPath := filepath.Join(filepath.Dir(paths.agentHome), "..", "..", "shared", sd.Name)
		mounts = append(mounts, fmt.Sprintf("type=bind,source=%s,destination=%s", sdPath, sdPath))
	}
	return mounts
}

// envFor builds the environment variable map for a sandbox agent.
// Env vars are passed via repeatable --env flags (confirmed by AC-0 retest).
func envFor(cfg RunConfig, paths scionPaths) map[string]string {
	env := make(map[string]string)

	// PATH is empty inside the sandbox (AC-0 retest finding). Set a
	// reasonable default so the harness and its children can find binaries.
	env["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	// Parse broker-provided env vars.
	for _, e := range cfg.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}

	// Harness-specific env vars. Pass sandboxAgentHome (the path visible
	// INSIDE the sandbox) rather than paths.agentHome (the host-side mount
	// source). Any {{ .AgentHome }} expansion in env_template must resolve
	// to a path the agent can actually reach. No current harness uses
	// {{ .AgentHome }} in env_template (the authoring guide warns against
	// it), but if one did, it would silently get a nonexistent path without
	// this.
	if cfg.Harness != nil {
		for k, v := range cfg.Harness.GetEnv(cfg.Name, sandboxAgentHome, cfg.UnixUsername) {
			env[k] = v
		}
	}

	// No TMUX_TMPDIR: gVisor runs without --host-uds, so AF_UNIX cannot
	// cross the sandbox boundary (§4.4a). tmux uses its default socket
	// path inside the sandbox. Every other runtime uses the default too
	// (common.go:480, k8s_runtime.go:934).

	// Project identity.
	if cfg.Project != "" {
		env["SCION_PROJECT"] = cfg.Project
		env["SCION_GROVE"] = cfg.Project
	}
	if cfg.ProjectID != "" {
		env["SCION_PROJECT_ID"] = cfg.ProjectID
		env["SCION_GROVE_ID"] = cfg.ProjectID
	}

	// Workspace path: tell sciontool init where the writable workspace is
	// mounted inside the sandbox. mountsFor maps the host-side workspace
	// (e.g. /scion/agents/<slug>/workspace) to sandboxWorkspace (/workspace).
	// Without this, sciontool init defaults to /workspace which is correct,
	// but setting it explicitly documents the contract and insulates against
	// any future change to the default.
	env["SCION_WORKSPACE_PATH"] = sandboxWorkspace

	// Workspace backend.
	if cfg.WorkspaceBackendName != "" {
		env["SCION_WORKSPACE_BACKEND"] = cfg.WorkspaceBackendName
	}

	// Host UID/GID for container user synchronisation.
	uid, gid := os.Getuid(), os.Getgid()
	env["SCION_HOST_UID"] = strconv.Itoa(uid)
	env["SCION_HOST_GID"] = strconv.Itoa(gid)

	// Set HOME, USER and LOGNAME explicitly for the sandbox.
	// supervisor.go:113 only sets HOME when (UID > 0 || Rootless). On
	// Cloud Run the launcher runs as root (UID 0, non-rootless), so
	// HOME is inherited as /root. tmux reads ~/.tmux.conf relative to
	// HOME, so the pane-exited hook in the template home is never found.
	// Setting HOME here ensures the sandbox-mounted agent home at
	// sandboxAgentHome is used regardless of supervisor behaviour.
	env["HOME"] = sandboxAgentHome
	env["USER"] = "scion"
	env["LOGNAME"] = "scion"

	return env
}

// envArgs converts an env map to a sorted list of KEY=VALUE strings
// suitable for --env flag values.
func envArgs(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	args := make([]string, 0, len(keys))
	for _, k := range keys {
		args = append(args, fmt.Sprintf("%s=%s", k, env[k]))
	}
	return args
}

// -----------------------------------------------------------------------
// Entrypoint construction
// -----------------------------------------------------------------------

// buildEntrypoint constructs the process tree that runs inside the sandbox:
//
//	sciontool init -- sh -c '<tmux-command>'
//
// The tmux command follows the pattern from common.go:469-482, adapted for
// sandboxes which have no TTY (sandbox run --detach does not allocate one):
//
//	tmux new-session -d -s scion -n agent <agent-window-cmd> \;
//	    set-option -g window-size latest \;
//	    new-window -t scion -n shell \;
//	    select-window -t scion:agent;
//	    while tmux has-session -t scion 2>/dev/null; do sleep 2; done
func buildEntrypoint(cfg RunConfig) ([]string, error) {
	// Build the harness command line.
	var cmdLine string
	if cfg.NoAuth {
		cmdLine = buildNoAuthCmdLine(cfg.NoAuthMessage, cfg.NoAuthCommand)
	} else if cfg.Harness != nil {
		harnessArgs := cfg.Harness.GetCommand(cfg.Task, cfg.Resume, cfg.CommandArgs)
		var quotedArgs []string
		for _, a := range harnessArgs {
			quotedArgs = append(quotedArgs, shellQuote(a))
		}
		cmdLine = strings.Join(quotedArgs, " ")
	} else {
		return nil, fmt.Errorf("cloudrun-sandbox: no harness provided")
	}

	// Wrap the harness in a shell that records its real exit code (see
	// common.go:469-475 for the pattern). Use absolute path for sh —
	// this runs as a tmux window command where PATH is available, but
	// absolute paths are used throughout buildEntrypoint for consistency.
	agentWindowCmd := "/bin/sh -c " + shellQuote(cmdLine+"; echo $? > "+state.HarnessExitCodeFile)

	// Build tmux command (common.go:479-482 pattern, adapted for sandbox).
	//
	// Finding #12: `sandbox run --detach` provides no TTY. Docker allocates
	// one with `docker run -t` (docker.go:76), but sandboxes do not.
	// `tmux attach-session` fails immediately with "open terminal failed:
	// not a terminal" (rc=1), killing PID 1 and destroying the sandbox.
	//
	// Fix: replace `attach-session` with a poll loop that tracks the tmux
	// session's lifetime without needing a terminal. PID 1 exits when the
	// session ends (all windows closed), providing the same lifecycle
	// semantics as attach-session did in the Docker case.
	//
	// Note the boundary between tmux subcommands and the poll loop:
	// `\;` is a tmux command separator (parsed by tmux), while the bare
	// `;` after select-window ends the tmux invocation and starts the
	// shell's while loop.
	tmuxCmd := fmt.Sprintf(
		"tmux new-session -d -s scion -n agent %s \\; set-option -g window-size latest \\; new-window -t scion -n shell \\; select-window -t scion:agent; while tmux has-session -t scion 2>/dev/null; do sleep 2; done",
		agentWindowCmd,
	)

	// CRITICAL: argv[0] must be an absolute path. The sandbox launcher resolves
	// argv[0] BEFORE the PATH env var (set by envFor) is in effect, so bare "sh"
	// silently fails (Finding #10).
	//
	// No symlink chain: agent home is bind-mounted directly at /home/scion
	// by mountsFor(). HOME is set by envFor() to sandboxAgentHome
	// (/home/scion) so it resolves to the writable mount regardless of
	// whether supervisor.go sets it. The rootfs is read-only (EROFS,
	// confirmed by diag-sbx6).
	//
	// Entrypoint output capture (#22): wrap the command in a group whose
	// stdout/stderr are redirected to a log file. On the happy path `exec`
	// replaces the shell with sciontool (which inherits the redirected fds
	// — acceptable because tmux manages its own pty). On failure the log
	// captures the error output.
	//
	// IMPORTANT: paths here are INSIDE the sandbox. Use sandboxAgentHome
	// (/home/scion, the mount destination), NOT agentHome (the host-side
	// mount source). Writing to agentHome inside the sandbox hits the
	// read-only rootfs (EROFS). The DOA probe in Run() reads from
	// agentHome on the HOST, where the bind mount makes the same files
	// visible at the source path.
	//
	// No .rc file: `exec` replaces the shell on success (echo never runs),
	// and `sandbox wait` already provides the exit code on the host side.
	logPath := filepath.Join(sandboxAgentHome, entrypointLogFile)
	wrappedCmd := fmt.Sprintf("{ exec sciontool init -- /bin/sh -c %s; } > %s 2>&1",
		shellQuote(tmuxCmd), logPath)
	return []string{"/bin/sh", "-c", wrappedCmd}, nil
}

// -----------------------------------------------------------------------
// CloudRunSandboxRuntime
// -----------------------------------------------------------------------

// CloudRunSandboxRuntime implements the Runtime interface for Cloud Run
// Sandboxes. Sandboxes are nested isolated workloads launched from inside
// a Cloud Run Instance via the `sandbox` CLI binary.
//
// Architecture:
//   - Each sandbox gets per-agent bind mounts (home, workspace, tmux, shared dirs).
//   - Environment is injected via repeatable --env flags.
//   - State is tracked locally in a JSON file (no `sandbox list` command).
//   - A watcher goroutine runs `sandbox wait` to detect exits.
type CloudRunSandboxRuntime struct {
	bin           string             // path to sandbox binary
	state         *sandboxStateStore // local state (sandbox CLI has no list command)
	rootDir       string             // /scion by default; overridable for testing
	deleteTimeout time.Duration      // timeout for sandbox delete --force; 0 = default

	watchMu      sync.Mutex                    // guards watchCancels
	watchCancels map[string]context.CancelFunc // per-sandbox watcher cancel functions
}

// NewCloudRunSandboxRuntime returns a new CloudRunSandboxRuntime.
func NewCloudRunSandboxRuntime(cfg *config.V1CloudRunSandboxConfig) *CloudRunSandboxRuntime {
	bin := DefaultSandboxBin
	if cfg != nil && cfg.SandboxBin != "" {
		bin = cfg.SandboxBin
	}

	ss := newSandboxStateStore(defaultStatePath)
	ss.reconcile(bin)

	return &CloudRunSandboxRuntime{
		bin:          bin,
		state:        ss,
		rootDir:      defaultScionRoot,
		watchCancels: make(map[string]context.CancelFunc),
	}
}

func (r *CloudRunSandboxRuntime) Name() string { return "cloudrun-sandbox" }

func (r *CloudRunSandboxRuntime) ExecUser() string { return "scion" }

// Run launches an agent inside a sandbox. See brief §Deliverable 2.
// waitForSandboxLiveness polls with backoff until probe returns nil or ctx is
// cancelled. Returns nil on success, ctx.Err() on cancellation, or the last
// probe error if all attempts fail.
func waitForSandboxLiveness(ctx context.Context, delays []time.Duration, probe func(ctx context.Context) error, name string) error {
	var probeErr error
	for i, delay := range delays {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
		probeErr = probe(probeCtx)
		probeCancel()
		if probeErr == nil {
			runtimeLog.Info("sandbox liveness probe passed", "name", name, "attempt", i+1)
			return nil
		}
		runtimeLog.Debug("sandbox liveness probe failed, retrying",
			"name", name, "attempt", i+1, "delay", delay, "error", probeErr)
	}
	return probeErr
}

func (r *CloudRunSandboxRuntime) Run(ctx context.Context, cfg RunConfig) (string, error) {
	slug := sanitizeSandboxName(cfg.Name)

	// OQ-14 (§11.12) proved that Vertex AI and gcloud-adc auth modes work
	// on this runtime via the metadata emulator. The emulator runs INSIDE
	// each sandbox on loopback (GCE_METADATA_HOST=localhost:18380), not on
	// the launcher's link-local address (reverted in 9877da59). It mints
	// no tokens locally — it delegates to the hub via SCION_HUB_ENDPOINT.
	// This path works for all callers that honour the GCE_METADATA_HOST
	// environment variable — which includes all five shipped harnesses and
	// the standard Google auth SDKs (google-auth-library for Node,
	// google-auth for Python, cloud.google.com/go/compute/metadata for Go).
	//
	// Callers that hardcode 169.254.169.254 and ignore GCE_METADATA_HOST
	// will fail outright: iptables -t nat does not exist in gVisor, so
	// there is no transparent interception fallback (§4.10).
	if cfg.ResolvedAuth != nil {
		method := cfg.ResolvedAuth.Method
		if method == "vertex-ai" || strings.Contains(strings.ToLower(method), "gcloud") {
			runtimeLog.Info("cloudrun-sandbox: GCP auth mode in sandbox — "+
				"credentials provided via in-sandbox metadata emulator on loopback",
				"method", method, "agent", cfg.Name)
		}
	}

	// Prepare the /scion directory layout (§3.2a: rootfs is read-only;
	// only mounted paths are writable).
	paths, err := prepareScionLayout(r.rootDir, slug, cfg)
	if err != nil {
		return "", fmt.Errorf("cloudrun-sandbox: prepare layout: %w", err)
	}

	// Build environment.
	env := envFor(cfg, paths)

	// Build entrypoint command.
	entrypoint, err := buildEntrypoint(cfg)
	if err != nil {
		return "", err
	}

	// Build the sandbox run command.
	// C1: No `create` verb. Use `sandbox run <name> --detach --rootfs / --write --allow-egress`.
	// C3: --allow-egress is MANDATORY.
	args := []string{
		"run", slug,
		"--detach",
		"--rootfs", "/",
		"--write",
		"--allow-egress",
	}

	// Per-agent mounts -- each sandbox gets only its own paths (not /scion root).
	for _, m := range mountsFor(paths, cfg.SharedDirs) {
		args = append(args, "--mount", m)
	}

	// Environment variables via --env flags (repeatable, confirmed by AC-0 retest).
	for _, e := range envArgs(env) {
		args = append(args, "--env", e)
	}

	args = append(args, "--")
	args = append(args, entrypoint...)

	WriteRuntimeDebugFile(cfg, r.bin, args)

	out, err := runSimpleCommand(ctx, r.bin, args...)
	if err != nil {
		// #1342: `out` is the sandbox binary's combined stdout+stderr, and a CLI
		// that reports the offending invocation on an argument error will echo
		// its own argv -- which carries every --env KEY=VALUE pair. This error
		// reaches an HTTP response body via handlers.go RuntimeError(), so the
		// values are redacted before they are spliced in. Measured, not assumed:
		// see TestCloudRunSandboxRun_ErrorDoesNotLeakEnvValues.
		//
		// `err` itself is safe to wrap as-is on this base: #127 P5 reduced
		// runSimpleCommand's error to `%s failed: %w` with no argv. The silent
		// case of that test is the guard on it -- if P5 is ever reverted, the
		// leak returns through THIS %w and that case goes red.
		//
		// %w is deliberately preserved. handlers.go calls errors.Is(err,
		// agent.ErrContainerNameInUse) at :899 and :1422, and
		// classifyLaunchRuntimeError calls errors.Is(err, exec.ErrNotFound);
		// rebuilding this error from a string would sever that chain silently.
		safeOut := redactEnvValues(out, externalEnvValues(cfg, env))
		return "", fmt.Errorf("cloudrun-sandbox: run failed: %w (output: %s)", err, safeOut)
	}

	// Post-run liveness probe: `sandbox run --detach` returns rc=0 even for
	// sandboxes that die immediately (e.g. because the entrypoint binary
	// cannot be resolved — see R1 absolute-path fix above). The exit code
	// carries no information about whether the sandbox is actually alive.
	// Probe with `sandbox exec` to confirm liveness before reporting success.
	// Retry with backoff to give the sandbox time to initialize its control
	// server.
	//
	// Finding from §1 E2E walk (2026-08-26): 4 of 6 test sandboxes returned
	// rc=0 from `run --detach` but were dead within 5 seconds. Without this
	// probe, dead-on-arrival sandboxes are permanently reported as running.
	//
	// The retry delays are a best-guess bounded by observation: sandboxes
	// that die do so in under 5 seconds (diag-sbx3 matrix). There is no
	// precise measurement of sandbox startup latency. If this probe proves
	// flaky in practice, lengthen the ladder rather than removing the probe.
	probeDelays := []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second}
	probeErr := waitForSandboxLiveness(ctx, probeDelays, func(probeCtx context.Context) error {
		_, err := runSimpleCommand(probeCtx, r.bin, "exec", slug, "--", "/bin/true")
		return err
	}, slug)
	if probeErr != nil {
		// A cancelled context is not a dead sandbox — the caller asked us
		// to stop. Return the context error without dead-on-arrival
		// diagnostics or cleanup.
		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		// Read entrypoint diagnostics from the host filesystem.
		// paths.agentHome is the HOST-SIDE mount source; the sandbox writes
		// to sandboxAgentHome (/home/scion, the mount destination). The bind
		// mount makes them the same file. We cannot use `sandbox exec` here
		// — the probe just proved the sandbox is dead.
		var diagInfo string
		logPath := filepath.Join(paths.agentHome, entrypointLogFile)
		if logData, readErr := os.ReadFile(logPath); readErr == nil {
			logStr := string(logData)
			// Truncate to the last 2000 bytes to keep error messages readable.
			if len(logStr) > 2000 {
				logStr = "...(truncated)\n" + logStr[len(logStr)-2000:]
			}
			diagInfo += fmt.Sprintf("\nentrypoint log (%s):\n%s", logPath, logStr)
		}

		runtimeLog.Error("sandbox dead on arrival: all liveness probes failed",
			"name", slug, "agentID", cfg.Name, "error", probeErr, "diagnostics", diagInfo)
		// Attempt cleanup — sandbox may be in a broken state.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = runSimpleCommand(cleanupCtx, r.bin, "delete", "--force", slug)
		cleanupCancel()

		errMsg := fmt.Sprintf("cloudrun-sandbox: sandbox dead on arrival after run returned rc=0 — "+
			"all liveness probes failed: %v", probeErr)
		if diagInfo != "" {
			errMsg += diagInfo
		}
		return "", errors.New(errMsg)
	}

	// Record in state store.
	entry := &sandboxStateEntry{
		SandboxName:   slug,
		AgentID:       cfg.Name,
		ProjectID:     cfg.ProjectID,
		Project:       cfg.Project,
		Labels:        cfg.Labels,
		Template:      cfg.Template,
		HarnessConfig: labelValue(cfg.Labels, "scion.harness_config"),
		CreatedAt:     time.Now(),
		AgentHome:     paths.agentHome,
		Workspace:     paths.workspace,
		Image:         cfg.Image,
	}
	r.state.add(entry)

	// Start a watcher goroutine that blocks on `sandbox wait` and records
	// the exit in the state store when the sandbox terminates.
	// The context is cancelled by deleteOrWorkaround to unblock the watcher
	// when the sandbox is force-deleted.
	watchCtx, watchCancel := context.WithCancel(context.Background())
	r.watchMu.Lock()
	r.watchCancels[slug] = watchCancel
	r.watchMu.Unlock()
	go r.watchSandbox(watchCtx, slug)

	return slug, nil
}

func (r *CloudRunSandboxRuntime) Stop(ctx context.Context, id string) error {
	// sandbox delete requires --force for running sandboxes.
	// There is no stop/pause verb; Stop == Delete.
	return r.deleteOrWorkaround(ctx, id)
}

func (r *CloudRunSandboxRuntime) Delete(ctx context.Context, id string) error {
	// Always use --force: sandbox delete without it silently fails for
	// running sandboxes. NEVER fall back to plain delete (without --force) --
	// it refuses AND kills the sandbox anyway, leaving orphaned
	// runsc-gofer/runsc-sandbox processes behind a CLI that reports "not
	// running". This is the more dangerous defect. See
	// defect-sandbox-delete-hang.md.
	return r.deleteOrWorkaround(ctx, id)
}

// deleteOrWorkaround dispatches to the workaround or the plain path based
// on the SCION_CLOUDRUN_DELETE_WORKAROUND kill switch. The watcher cancel
// is performed here so neither downstream path needs it -- and removing
// the workaround file does not lose the watcher cancel logic.
func (r *CloudRunSandboxRuntime) deleteOrWorkaround(ctx context.Context, id string) error {
	// Cancel the watcher goroutine so `sandbox wait` doesn't hang after
	// the sandbox is deleted.
	r.watchMu.Lock()
	if cancel, ok := r.watchCancels[id]; ok {
		cancel()
		delete(r.watchCancels, id)
	}
	r.watchMu.Unlock()

	if deleteWorkaroundEnabled {
		return r.deleteWithTimeout(ctx, id)
	}
	return r.deletePlain(ctx, id)
}

// deletePlain is the non-workaround path: plain `sandbox delete --force`.
// Used when SCION_CLOUDRUN_DELETE_WORKAROUND=off.
func (r *CloudRunSandboxRuntime) deletePlain(ctx context.Context, id string) error {
	cmd := exec.CommandContext(ctx, r.bin, "delete", "--force", id)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cloudrun-sandbox: delete --force failed: %w", err)
	}
	r.state.remove(id)
	return nil
}

// List returns agent info for all tracked sandboxes, applying an optional
// label filter.
func (r *CloudRunSandboxRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	entries := r.state.list()

	var agents []api.AgentInfo
	for _, entry := range entries {
		// Label filtering (same pattern as docker.go:182-201).
		match := true
		for k, v := range labelFilter {
			actual := entry.Labels[k]
			if actual == "" {
				switch k {
				case projectcompat.LabelProject:
					actual = projectcompat.ProjectNameFromLabels(entry.Labels)
				case projectcompat.LabelProjectID:
					actual = projectcompat.ProjectIDFromLabels(entry.Labels)
				}
			}
			if actual != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}

		statusStr := "running"
		phase := "running"
		if entry.Stopped {
			statusStr = "stopped"
			phase = "stopped"
		}

		// STOPGAP (§9.1b): Report ExitCode=nil for stopped sandboxes.
		// Non-zero ExitCode is hardcoded to PhaseError at handlers_runtime_brokers.go:719,
		// and Instance teardown (our normal shutdown) SIGKILLs every sandbox simultaneously.
		// Reporting the real code would put the entire fleet into PhaseError on every
		// normal teardown. This stopgap can be removed once BOTH conditions hold:
		//
		//   1. state.ExitReason gains a platform_eviction-style value so that
		//      routine Instance teardown can be distinguished from a real crash.
		//   2. The phase-promotion logic in handlers_runtime_brokers.go consults
		//      ExitReason before promoting PhaseStopped → PhaseError (condition 2
		//      is load-bearing — adding the enum value alone changes nothing).
		//
		// The state store tracks the real exit code internally (in sandboxStateEntry)
		// so it is available when the above conditions are met.
		agents = append(agents, api.AgentInfo{
			ContainerID:     entry.SandboxName,
			Name:            entry.AgentID,
			ContainerStatus: statusStr,
			Phase:           phase,
			Image:           entry.Image,
			Labels:          entry.Labels,
			Template:        entry.Template,
			HarnessConfig:   entry.HarnessConfig,
			Project:         entry.Project,
			ProjectID:       entry.ProjectID,
			Runtime:         r.Name(),
		})
	}

	return agents, nil
}

func (r *CloudRunSandboxRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	// Use tmux capture-pane to get the scrollback buffer.
	// Absolute paths required -- PATH is empty inside a sandbox.
	return runSimpleCommand(ctx, r.bin, "exec", id, "--", "/usr/bin/tmux", "capture-pane", "-p", "-t", "scion", "-S", "-1000")
}

func (r *CloudRunSandboxRuntime) Attach(ctx context.Context, id string) error {
	// Look up the sandbox to verify it exists and is running.
	entry := r.state.get(id)
	if entry == nil {
		return fmt.Errorf("cloudrun-sandbox: sandbox %q not found", id)
	}
	// Set tmux window-size to latest for proper resize behavior.
	_, _ = runSimpleCommand(ctx, r.bin, "exec", id, "--",
		"/usr/bin/tmux", "set-option", "-g", "window-size", "latest")
	// Interactive attach via sandbox exec.
	// TERM=xterm-256color is load-bearing: without it tmux sees TERM=dumb
	// and exits with "terminal does not support clear" -- which looks like
	// a PTY failure but is not one (design doc section 4.4a-rev).
	return runInteractiveCommand(r.bin, "exec", id,
		"--env", "TERM=xterm-256color", "--",
		"/usr/bin/tmux", "attach-session", "-t", "scion")
}

// ImageExists returns true — the omni-image is always present.
func (r *CloudRunSandboxRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	return true, nil
}

// ImageID returns a fixed identifier — the omni-image is always present.
func (r *CloudRunSandboxRuntime) ImageID(ctx context.Context, image string) (string, error) {
	return "omni-image", nil
}

// RemoveImage is a no-op — the omni-image cannot be removed.
func (r *CloudRunSandboxRuntime) RemoveImage(ctx context.Context, image string) error {
	return nil
}

// PullImage is a no-op — the omni-image is already present.
func (r *CloudRunSandboxRuntime) PullImage(ctx context.Context, image string) error {
	return nil
}

// Sync is a no-op — the filesystem is shared via bind mounts. The
// sandbox uses --rootfs / (the launcher's filesystem) and the /scion
// bind mount makes writes visible to both sides. There is nothing
// additional to synchronise.
func (r *CloudRunSandboxRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {
	return nil
}

func (r *CloudRunSandboxRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	// sandbox exec has no --user flag; the process runs as the sandbox's
	// configured user (which is the scion user via the omni-image entrypoint).
	// Absolute paths: PATH is empty inside a sandbox, so callers must provide
	// absolute paths or the command must be on a bind-mounted path.
	args := append([]string{"exec", id, "--"}, cmd...)
	return runSimpleCommand(ctx, r.bin, args...)
}

// GetWorkspacePath returns the HOST-side workspace path from the state
// store (e.g. /scion/agents/<slug>/workspace). This is the path on the
// launcher filesystem, not the path inside the sandbox — mountsFor
// remaps the workspace to /workspace inside the sandbox, so the two
// differ. The interface contract (interface.go:121) defines the return
// value as the host path.
func (r *CloudRunSandboxRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	entry := r.state.get(id)
	if entry == nil {
		return "", fmt.Errorf("cloudrun-sandbox: sandbox %q not found in state store", id)
	}
	return entry.Workspace, nil
}

// -----------------------------------------------------------------------
// Watcher goroutine
// -----------------------------------------------------------------------

// watchSandbox blocks on `sandbox wait <name>` and updates the state
// store when the sandbox exits. The context allows deleteOrWorkaround to
// cancel the watcher when the sandbox is force-deleted.
func (r *CloudRunSandboxRuntime) watchSandbox(ctx context.Context, name string) {
	// sandbox wait blocks until the sandbox exits.
	cmd := exec.CommandContext(ctx, r.bin, "wait", name)
	out, err := cmd.CombinedOutput()

	// If the context was cancelled (sandbox was force-deleted), don't
	// update the state store — deleteOrWorkaround handles cleanup.
	if ctx.Err() != nil {
		runtimeLog.Debug("watcher cancelled for deleted sandbox", "name", name)
		return
	}

	now := time.Now()
	var exitCode *int

	if err != nil {
		// C5 (§4.3d): sandbox wait exit-code semantics are undocumented.
		// Treat ExitCode=nil as normal ("nil means unknown"), not an error.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code := exitErr.ExitCode()
			exitCode = &code
		}
		// Non-ExitError (e.g. binary not found) is logged but not fatal.
	} else {
		// Try to parse exit code from stdout if provided.
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			if code, parseErr := strconv.Atoi(trimmed); parseErr == nil {
				exitCode = &code
			}
		}
	}

	// Log at info level — sandbox exit is normal (C5).
	runtimeLog.Info("sandbox exited", "name", name, "exitCode", exitCode)
	r.state.markStopped(name, exitCode, &now)

	// Clean up the cancel function from the map.
	r.watchMu.Lock()
	delete(r.watchCancels, name)
	r.watchMu.Unlock()
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

// labelValue safely retrieves a label value, returning "" if the map is
// nil or the key is absent.
func labelValue(labels map[string]string, key string) string {
	if labels == nil {
		return ""
	}
	return labels[key]
}
