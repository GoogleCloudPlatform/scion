/*
Copyright 2026 The Scion Authors.
*/
package commands

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

var (
	provisionWorkspace    string
	provisionMode         string
	provisionDepth        int
	provisionUID          int
	provisionGID          int
	provisionWaitSentinel bool
	provisionTimeout      int
	provisionPollInterval int
)

var provisionCmd = &cobra.Command{
	Use:   "provision",
	Short: "Provision an NFS workspace (clone or wait for sentinel)",
	Long: `Provision a shared workspace in an NFS-backed init container.

In default (clone) mode, reads SCION_CLONE_URL and SCION_CLONE_BRANCH from
the environment and invokes the shared provisioning function. The sentinel
file (.scion-provisioned) and the provisioning lock are kept in the
project's provisioning state directory, which the Kubernetes runtime mounts
next to the workspace and names in SCION_PROVISION_STATE_DIR, so nothing of
them is visible in the workspace. Without that variable (an older runtime)
they are kept in the workspace directory itself, as before. A sentinel
already in the workspace directory still counts as provisioned, and the
lock in the workspace directory is taken before the one in the state
directory, so older and newer images exclude each other.

In clone-per-agent mode (SCION_WORKSPACE_MODE=clone-per-agent, with
SCION_AGENT_SLUG and SCION_AGENT_BRANCH), the mounted directory is the
agent's own directory: it prepares the empty workspace directory inside it
for the agent container's clone, records the branch, and writes the
sentinel next to the workspace. It does not clone.

In empty-per-agent mode (SCION_WORKSPACE_MODE=empty-per-agent, with
SCION_AGENT_SLUG), it does the same without a branch: it makes sure the
agent's empty workspace directory exists and writes the sentinel. Nothing
is cloned and no git is run.

In --wait-for-sentinel mode, polls for the sentinel file written by the
winning node's init container and exits 0 when found or non-zero on timeout.

URL and branch are ALWAYS read from environment variables (never from flags)
to prevent shell injection via crafted values.`,
	SilenceErrors: true,
	SilenceUsage:  true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Wired locally to this one subcommand, not at the root command
		// level: a pod deleted mid-provisioning sends this init container
		// SIGTERM, and without this, cmd.Context() never observes it
		// (rootCmd.Execute() does not itself install a signal-to-context
		// handler). Cancelling the context here makes acquireFileLock's
		// wait loop return promptly instead of running out its retry budget,
		// and — since ProvisionShared's lock release is always deferred, and
		// exec.CommandContext kills the in-flight git process on
		// cancellation too — makes an in-progress holder's own defer still
		// fire and release the lock cleanly, instead of leaving it for
		// provisionLockStaleAfter to reclaim.
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()

		if provisionWaitSentinel {
			return runWaitForSentinel(ctx)
		}
		return runProvision(ctx)
	},
}

func init() {
	rootCmd.AddCommand(provisionCmd)

	provisionCmd.Flags().StringVar(&provisionWorkspace, "workspace", "/workspace",
		"Path to the workspace directory")
	provisionCmd.Flags().StringVar(&provisionMode, "mode", "shared-plain",
		"Workspace sharing mode (shared-plain, worktree-per-agent, clone-per-agent)")
	provisionCmd.Flags().IntVar(&provisionDepth, "depth", 1,
		"Git clone depth (0=full clone, >0=that depth; default 1=shallow)")
	provisionCmd.Flags().IntVar(&provisionUID, "uid", 1000,
		"UID for chown of provisioned files (0 means 1000)")
	provisionCmd.Flags().IntVar(&provisionGID, "gid", 1000,
		"GID for chown of provisioned files (0 means 1000)")
	provisionCmd.Flags().BoolVar(&provisionWaitSentinel, "wait-for-sentinel", false,
		"Poll for sentinel file instead of provisioning (lock-loser mode)")
	provisionCmd.Flags().IntVar(&provisionTimeout, "timeout", 300,
		"Timeout in seconds for --wait-for-sentinel mode")
	provisionCmd.Flags().IntVar(&provisionPollInterval, "poll-interval", 2,
		"Poll interval in seconds for --wait-for-sentinel mode")
}

func runProvision(ctx context.Context) error {
	cloneURL := os.Getenv("SCION_CLONE_URL")
	cloneBranch := os.Getenv("SCION_CLONE_BRANCH")
	projectID := os.Getenv("SCION_PROJECT_ID")
	if projectID == "" {
		projectID = "unknown"
	}

	var gc *api.GitCloneConfig
	if cloneURL != "" {
		depthVal := provisionDepth
		gc = &api.GitCloneConfig{
			URL:    cloneURL,
			Branch: cloneBranch,
			Depth:  &depthVal,
		}
	}

	// The Kubernetes runtime passes the workspace mode and the agent's
	// worktree as env vars (an older sciontool ignores them and provisions
	// the shared checkout only). SCION_WORKSPACE_MODE takes precedence over
	// --mode.
	modeLabel := provisionMode
	if envMode := os.Getenv("SCION_WORKSPACE_MODE"); envMode != "" {
		modeLabel = envMode
	}
	mode := store.ResolveWorkspaceSharingMode(modeLabel)
	// The agent's worktree directory is named after the agent's slug.
	agentSlug := os.Getenv("SCION_AGENT_SLUG")
	worktree := mode == store.SharingModeWorktreePerAgent
	// Clone-per-agent: the workspace path is the agent's directory
	// (<project>/agents/<agent name>); this step prepares its workspace
	// directory and the agent container clones into it.
	// Empty-per-agent on NFS: the same agent directory, whose workspace
	// stays empty (no branch, no clone).
	agentDir := mode == store.SharingModeClonePerAgent || mode == store.SharingModeEmptyPerAgent
	if worktree || agentDir {
		if slug, err := api.ValidateAgentName(agentSlug); err != nil || slug != agentSlug {
			return fmt.Errorf("provision: %s mode needs SCION_AGENT_SLUG set to the agent's slug (got %q)", mode, agentSlug)
		}
	}
	if worktree || agentDir {
		// Files the worktree step creates as root keep group write, like
		// the directories the broker prepares.
		defer setProvisionUmask()()
	}

	// F-111 (design §9): the k8s runtime mounts each NFS-backed shared dir
	// into this init container at its own path (mirroring the main
	// container's mounts) and passes "name=mountPath" pairs here, comma-joined
	// — mkdir+chown must reach them the same as the workspace dir, since
	// they're separate volume mounts the workspace's own chown doesn't reach.
	// Keyed explicitly by the shared dir's own name (the producer side,
	// pkg/runtime/k8s_runtime.go's nfsSharedDirMount, carries it alongside
	// the mount for exactly this reason), not derived from the path here —
	// two shared dirs could produce the same path basename through different
	// target shapes (InWorkspace vs not), so reconstructing the key from the
	// path on this side would risk a silent collision.
	sharedDirs := make(map[string]provision.ResolvedSharedDir)
	if raw := os.Getenv("SCION_SHARED_DIR_PATHS"); raw != "" {
		for i, pair := range strings.Split(raw, ",") {
			if pair == "" {
				continue
			}
			name, path, ok := strings.Cut(pair, "=")
			if !ok || name == "" || path == "" {
				log.Info("SCION_SHARED_DIR_PATHS: skipping malformed entry %q (want name=path)", pair)
				continue
			}
			key := name
			if _, dup := sharedDirs[key]; dup {
				key = fmt.Sprintf("%s-%d", name, i)
			}
			sharedDirs[key] = provision.ResolvedSharedDir{HostPath: path}
		}
	}

	// Shared-plain and worktree-per-agent: keep the sentinel and the lock
	// in the provisioning state directory when the pod mounts one. The
	// agent-directory modes keep them in the mounted agent directory,
	// outside the workspace already.
	sentinelDir := provisionWorkspace
	legacyDir := ""
	if !agentDir {
		stateDir, err := provisionStateDir(os.Getenv, provisionWorkspace)
		if err != nil {
			return err
		}
		if stateDir != "" {
			// Without the broker's preparation (no setgid and group write),
			// the node created the directory as root: give it to the
			// workspace owner, the same condition under which the workspace
			// chown stays strict. 0 means the default 1000, as for the
			// workspace.
			uid, gid := provision.DefaultOwnerID(provisionUID), provision.DefaultOwnerID(provisionGID)
			if err := prepareStateDir(stateDir, uid, gid, provisionRequireChownSuccess(os.Getenv)); err != nil {
				return fmt.Errorf("provision: %w", err)
			}
			sentinelDir = stateDir
			legacyDir = provisionWorkspace
		}
	}

	in := provision.ProvisionInput{
		Ctx: ctx,
		Resolved: provision.ResolvedWorkspace{
			HostPath:   provisionWorkspace,
			SharedDirs: sharedDirs,
		},
		ProjectID:   projectID,
		Mode:        mode,
		GitClone:    gc,
		Locker:      nil, // no advisory locker in init container
		NFSUID:      provisionUID,
		NFSGID:      provisionGID,
		SentinelDir: sentinelDir,
		LegacyDir:   legacyDir,
		// F-111: this command's entire purpose is the chown. A silent
		// failure here would reproduce the "workspace stuck root:root" bug
		// invisibly — the sentinel would still get written, and every future
		// pod for this project would see it and skip provisioning forever.
		// Failing the init container (non-zero exit, pod doesn't start) is
		// the correct, loud failure mode. The one exception is a workspace
		// the broker prepared before the pod existed, by creating it or
		// finding it with setgid and group write (see
		// provision.ChownBestEffortEnv): agents reach it through its group,
		// so a chown the export does not allow is logged and the sentinel is
		// still written.
		RequireChownSuccess: provisionRequireChownSuccess(os.Getenv),
	}
	if worktree {
		setWorktreeInput(&in, agentSlug, os.Getenv("SCION_AGENT_BRANCH"), provisionTimeout)
		for key, value := range worktreeSafeDirectoryEnv(os.Getenv, provisionWorkspace, agentSlug) {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("provision: set %s: %w", key, err)
			}
		}
	}
	if agentDir {
		branch := os.Getenv("SCION_AGENT_BRANCH")
		if mode == store.SharingModeEmptyPerAgent {
			branch = ""
		}
		setAgentDirInput(&in, agentSlug, branch, provisionTimeout)
	}
	if !in.RequireChownSuccess {
		log.Info("Best-effort chown requested (workspace directory prepared by the broker); a failed chown is logged and provisioning continues")
	}

	log.Info("Provisioning workspace at %s (mode=%s, project=%s, shared_dirs=%d)",
		provisionWorkspace, mode, projectID, len(sharedDirs))
	if worktree {
		log.Info("Adding the worktree for agent %s at %s", agentSlug, provision.WorktreePath(provisionWorkspace, agentSlug))
	}
	if agentDir {
		log.Info("Preparing the workspace of agent %s at %s", agentSlug, filepath.Join(provisionWorkspace, provision.AgentWorkspaceDir))
		if err := provision.ProvisionAgentDir(in); err != nil {
			return fmt.Errorf("provision failed: %w", err)
		}
		log.Info("Agent workspace prepared")
		return nil
	}
	if err := provision.ProvisionShared(in); err != nil {
		return fmt.Errorf("provision failed: %w", err)
	}
	log.Info("Workspace provisioned successfully")
	return nil
}

// setAgentDirInput fills in the clone-per-agent part of in: the agent's
// slug and branch, and a lock wait of at least the --timeout value. The
// clone itself is left to the agent container: ProvisionAgentDir does not
// use clone settings.
func setAgentDirInput(in *provision.ProvisionInput, agentSlug, branch string, timeoutSeconds int) {
	in.AgentID = agentSlug
	in.AgentName = branch
	in.LockWait = time.Duration(timeoutSeconds) * time.Second
}

// setWorktreeInput fills in the worktree-per-agent part of in. The branch
// is used as the broker passes it, as for worktrees on the local runtimes,
// and ProvisionShared checks that it is a valid branch name. In this mode
// every pod of the project takes the provisioning file lock, so the lock
// wait is at least the --timeout value, the same time a pod waiting for
// the sentinel would wait.
func setWorktreeInput(in *provision.ProvisionInput, agentSlug, branch string, timeoutSeconds int) {
	in.AgentID = agentSlug
	in.AgentName = branch
	in.MountedWorktree = true
	in.LockWait = time.Duration(timeoutSeconds) * time.Second
}

// worktreeSafeDirectoryEnv returns the environment entries that list the
// shared checkout and the agent's worktree, and only those two paths, as
// git safe.directory entries for the git commands this process runs.
// The init container runs as root while both directories belong to the
// agents' user, and git does not work in a repository owned by another
// user unless it is listed. The entries are appended to any GIT_CONFIG_*
// entries already in the environment.
func worktreeSafeDirectoryEnv(getenv func(string) string, workspace, agentSlug string) map[string]string {
	base := 0
	if raw := getenv("GIT_CONFIG_COUNT"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			base = n
		}
	}
	paths := []string{workspace, provision.WorktreePath(workspace, agentSlug)}
	env := map[string]string{"GIT_CONFIG_COUNT": strconv.Itoa(base + len(paths))}
	for i, p := range paths {
		env[fmt.Sprintf("GIT_CONFIG_KEY_%d", base+i)] = "safe.directory"
		env[fmt.Sprintf("GIT_CONFIG_VALUE_%d", base+i)] = p
	}
	return env
}

// prepareStateDir is provision.PrepareStateDir; a variable so tests can
// observe the owner it is given.
var prepareStateDir = provision.PrepareStateDir

// provisionRequireChownSuccess keeps a chown failure fatal unless the
// Kubernetes runtime marked the workspace directory as prepared by the
// broker, created or found with setgid and group write
// (provision.ChownBestEffortEnv set to exactly "1").
func provisionRequireChownSuccess(getenv func(string) string) bool {
	return !provision.ChownBestEffortRequested(getenv)
}

// provisionStateDirEnv names the provisioning state directory the
// Kubernetes runtime mounts into the init container (shared-plain and
// worktree-per-agent modes). An environment variable rather than a flag, so
// an older sciontool ignores it and keeps the sentinel in the workspace.
const provisionStateDirEnv = "SCION_PROVISION_STATE_DIR"

// provisionStateDir returns the provisioning state directory from
// SCION_PROVISION_STATE_DIR, or "" when it is unset. It must be a clean
// absolute path other than /, and neither inside the workspace nor
// containing it.
func provisionStateDir(getenv func(string) string, workspace string) (string, error) {
	dir := getenv(provisionStateDirEnv)
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" {
		return "", fmt.Errorf("provision: %s=%q must be a clean absolute path other than /", provisionStateDirEnv, dir)
	}
	ws := filepath.Clean(workspace)
	if within(dir, ws) || within(ws, dir) {
		return "", fmt.Errorf("provision: %s=%q must be outside the workspace %s", provisionStateDirEnv, dir, ws)
	}
	return dir, nil
}

// within reports whether path is dir or below it (both clean).
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
}

func runWaitForSentinel(ctx context.Context) error {
	// The provisioning container writes the sentinel in the state directory
	// when the pod mounts one; an older one (or an earlier provisioning)
	// wrote it in the workspace, which is still accepted.
	stateDir, err := provisionStateDir(os.Getenv, provisionWorkspace)
	if err != nil {
		return err
	}
	dirs := []string{provisionWorkspace}
	sentinelPath := filepath.Join(provisionWorkspace, provision.ProvisionSentinelFile)
	if stateDir != "" {
		dirs = []string{stateDir, provisionWorkspace}
		sentinelPath = filepath.Join(stateDir, provision.ProvisionSentinelFile)
	}
	timeout := time.Duration(provisionTimeout) * time.Second
	interval := time.Duration(provisionPollInterval) * time.Second
	start := time.Now()
	deadline := start.Add(timeout)

	log.Info("Waiting for sentinel %s (timeout=%s, interval=%s)", sentinelPath, timeout, interval)

	for {
		// Any error (including EACCES on a state directory the node just
		// created as root) means "not yet".
		if provision.SentinelPresent(dirs...) {
			log.Info("Sentinel found after %s", time.Since(start).Truncate(time.Second))
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for sentinel %s after %s", sentinelPath, timeout)
		}

		// Sleep for the poll interval, but wake immediately on cancellation
		// (SIGTERM/SIGINT) so the init container exits promptly.
		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled while waiting for sentinel %s: %w", sentinelPath, ctx.Err())
		case <-time.After(interval):
		}
	}
}
