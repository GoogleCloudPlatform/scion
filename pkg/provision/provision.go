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

// Package provision implements Tier-1 universal workspace provisioning.
// It is a config-free leaf package that depends only on stdlib, pkg/api,
// pkg/store, pkg/util, pkg/util/fsutil (itself stdlib-only) and
// golang.org/x/sys/unix (for no-follow file operations) — deliberately
// avoiding pkg/config so that lean binaries (e.g. sciontool) can invoke
// provisioning without pulling in filesystem-based project path resolution.
package provision

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/fsutil"
	"golang.org/x/sys/unix"
)

// ProvisionSentinelFile is the name of the sentinel file written atomically
// after a successful workspace clone/setup. Its presence short-circuits
// subsequent ProvisionShared calls — the workspace is already ready.
const ProvisionSentinelFile = ".scion-provisioned"

// ProvisionStateDirName is the name of the directory that holds the
// provisioning bookkeeping (the sentinel, and the file lock with its
// satellites) for an NFS workspace on Kubernetes in shared-plain and
// worktree-per-agent modes: <subPathRoot>/<projectID>/provision, a sibling of
// the project's workspace directory, so none of it is visible inside the
// workspace. The provisioning init container mounts it by subPath (see
// pkg/runtime NFSProvisionStateSubPath); the broker reaches it through its own
// mount of the export (ProjectStateDir).
const ProvisionStateDirName = "provision"

// ProjectStateDir returns the provisioning state directory of the workspace
// at workspacePath, as seen through a mount of the whole export:
// <project dir>/provision, next to <project dir>/workspace.
func ProjectStateDir(workspacePath string) string {
	return filepath.Join(filepath.Dir(workspacePath), ProvisionStateDirName)
}

// SentinelPresent reports whether ProvisionSentinelFile exists in any of
// dirs, checked in order; empty entries are skipped. Callers list the
// current location first and legacy locations after it (for example the
// workspace root, where Kubernetes init containers wrote the sentinel before
// the state directory existed), so a workspace provisioned under an older
// layout is not provisioned again.
func SentinelPresent(dirs ...string) bool {
	_, ok := findSentinel(dirs...)
	return ok
}

// findSentinel returns the first of dirs holding ProvisionSentinelFile.
func findSentinel(dirs ...string) (string, bool) {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, ProvisionSentinelFile)); err == nil {
			return dir, true
		}
	}
	return "", false
}

// provisionLockRetries is the number of times to retry acquiring the
// per-project advisory lock before giving up. Each retry sleeps briefly
// (provisionLockRetryDelay) to allow the current holder to finish.
const provisionLockRetries = 30

// provisionLockRetryDelay is the sleep between advisory lock acquisition
// retries. Provisioning (git clone) is typically short (seconds), so a
// short retry cadence is appropriate.
const provisionLockRetryDelay = 1 * time.Second

// provisionFileLockName is the lock directory used to serialize provisioning
// when no store.AdvisoryLocker is available (e.g. a k8s init container,
// which has no Hub/DB connection by design — see the package doc above).
// It is a distinct name from ProvisionSentinelFile: the sentinel is written
// only AFTER provisioning succeeds and so cannot gate the provisioning work
// itself, but this lock is acquired BEFORE mkdir/clone/chown run.
const provisionFileLockName = ".scion-provision.lock"

// provisionLockStagingPattern is the os.MkdirTemp pattern tryCreateFileLock
// uses for its staging directory. Defined once and shared with tests that
// need to construct or recognize a staging directory, rather than each
// duplicating the literal pattern — a test using its own copy would not
// notice if tryCreateFileLock's own pattern ever drifted from it.
const provisionLockStagingPattern = provisionFileLockName + ".stage-*"

// provisionLockStaleAfter bounds how long a file lock may go without a
// heartbeat refresh (see startLockHeartbeat) before a waiter treats its
// owner as dead (crashed pod, killed init container) and reclaims it.
// Because the holder refreshes its heartbeat marker every
// provisionLockHeartbeatInterval for as long as it is actually alive — not
// just once at acquisition — this threshold bounds detection latency for a
// genuinely dead holder, not how long provisioning itself is allowed to
// take: a clone or chown that runs for an hour is fine, since each
// heartbeat resets the clock.
//
// This comparison is always done against another NFS-server-assigned
// timestamp (see serverNow), never this process's own clock — see
// lockLooksAbandoned's doc for why mixing clocks across nodes is unsafe and
// how this avoids it. The residual assumption is that the export server's
// mtime attribute reaches every node within its NFS attribute-cache window
// (acregmax/acdirmax, both default up to ~60s — the heartbeat and probe are
// regular files, governed by acregmax, but lastHeartbeat's fallback reads
// the lock DIRECTORY's own mtime, governed by acdirmax, and a reclaimer's
// visibility of a newly created heartbeat file at all depends on the
// directory's lookup cache too) — not that any two nodes' wall clocks
// agree with each other or with the server.
//
// These are vars, not consts, solely so tests can shrink them (with
// t.Cleanup to restore the defaults) instead of waiting out multi-minute
// real-time windows — production code never reassigns them.
var (
	provisionLockStaleAfter = 3 * time.Minute

	// provisionLockHeartbeatInterval is how often a held file lock's
	// heartbeat marker is refreshed (see startLockHeartbeat). Set well below
	// provisionLockStaleAfter so a live holder tolerates several missed
	// beats (e.g. NFS latency, a paused goroutine under load) before ever
	// looking abandoned. A test that overrides provisionLockStaleAfter must
	// also override this explicitly — it is not recomputed from the
	// (possibly now-stale) ratio below.
	provisionLockHeartbeatInterval = provisionLockStaleAfter / 6

	// fileLockRetries and fileLockRetryDelay bound how long acquireFileLock
	// waits for a contended lock before giving up — as a wall-clock deadline
	// (fileLockRetries * fileLockRetryDelay ≈ 3m40s) computed once at the
	// start of the wait, not an attempt counter: a counter can be exhausted
	// in milliseconds by a run of non-sleeping "checked, not actually stale,
	// back off normally" iterations if it weren't for every such iteration
	// going through the same sleep-or-ctx select below regardless. The
	// deadline must comfortably exceed provisionLockStaleAfter: a waiter has
	// to still be retrying, not already failed, by the time a genuinely
	// abandoned lock becomes reclaimable — otherwise every concurrent start
	// for that project fails outright for the entire stale window, which is
	// worse than having no lock at all. The init container runs with
	// RestartPolicy: Never (pkg/runtime/k8s_runtime.go), so a waiter that
	// gives up fails the pod, not just this one attempt.
	//
	// Known, accepted limitation: this is still a fixed budget, not "wait
	// indefinitely while the holder keeps heartbeating" — a clone or chown
	// genuinely slower than ~3m40s still makes a concurrent waiter give up
	// even though the holder is alive and making progress. Accepted, rather
	// than the fuller fix (unbounded wait bounded only by ctx/an external
	// deadline), to avoid
	// callers with no ctx deadline of their own waiting forever on a
	// genuinely wedged (not crashed, not progressing) holder.
	//
	// This is deliberately a separate, longer budget from
	// provisionLockRetries/provisionLockRetryDelay below, which govern the
	// store.AdvisoryLocker path only: a Postgres advisory lock has no staleness
	// concept (it releases automatically when the holding connection drops), so
	// that path has no equivalent "outlive the stale window" requirement and
	// keeps its original, shorter budget.
	fileLockRetries    = 110
	fileLockRetryDelay = 2 * time.Second
)

// provisionHeartbeatMissTolerance is how many consecutive *transient* failed
// heartbeat beats (a write error, or a momentarily unreadable owner marker)
// a holder tolerates before concluding it has lost the lock and cancelling
// its own in-flight provisioning work. This tolerance applies only to
// failures that don't themselves prove loss — an owner marker that reads
// back with a *different* id is definitive, immediate loss (a lost holder
// must never keep waiting out this tolerance once it has actual proof) and
// is never counted against it.
const provisionHeartbeatMissTolerance = 3

// ResolvedWorkspace holds the deterministic path resolution result.
type ResolvedWorkspace struct {
	// HostPath is the absolute host-side path for the workspace.
	// For localBackend this is the existing project path (e.g.
	// ~/.scion.projects/<slug>/). For nfsBackend this is
	// <MountRoot>/<shareID>/<ServerRelativePath>.
	HostPath string

	// ServerRelativePath is the path relative to the NFS export root.
	// Empty for localBackend. For nfsBackend, e.g. "projects/<pid>/workspace".
	ServerRelativePath string

	// HostBase is the host mount prefix for NFS-backed workspaces
	// (<MountRoot>/<shareID>). Empty for localBackend.
	HostBase string

	// SharedDirs maps shared-dir name → resolved path info.
	SharedDirs map[string]ResolvedSharedDir

	// Backend identifies which backend produced this resolution: "local",
	// "nfs", "cloudrun-volume" or "gke-shared-volume".
	Backend string
}

// ResolvedSharedDir holds path resolution for a single shared directory.
type ResolvedSharedDir struct {
	// HostPath is the absolute host path for this shared dir.
	HostPath string

	// ServerRelativePath is the NFS export-relative path (empty for local).
	ServerRelativePath string
}

// ProvisionInput holds parameters for workspace provisioning.
type ProvisionInput struct {
	// Ctx is the context for cancellation and timeouts. Optional: when nil,
	// ProvisionShared falls back to context.Background(). Keeping it as a struct
	// field (rather than a ProvisionShared parameter) preserves the existing
	// function signature for callers.
	Ctx context.Context

	// Resolved is the output of a prior Resolve call.
	Resolved ResolvedWorkspace

	// ProjectID is the project's stable UUID.
	ProjectID string

	// AgentID is the agent's stable UUID.
	AgentID string

	// AgentName is a human-readable agent name (used for worktree branch names).
	AgentName string

	// Mode is the workspace sharing mode.
	Mode store.WorkspaceSharingMode

	// GitClone holds git-clone config when the project is git-backed; nil otherwise.
	GitClone *api.GitCloneConfig

	// Locker provides the per-project advisory lock for the NFS first-access
	// provisioning guard (design §7, risk RN1). On Postgres-backed deployments
	// this uses pg_try_advisory_lock(classid, objid) for cross-node mutual
	// exclusion; on SQLite it's a no-op (single-writer serializes already).
	//
	// May be nil — ProvisionShared then falls back to acquireFileLock, a
	// filesystem-based mutex on the shared NFS mount itself (see its doc).
	// This is the common case: the k8s init container has no Hub/DB
	// connection at all, so Locker is always nil there, and in practice no
	// caller anywhere sets it today (see acquireFileLock's doc).
	Locker store.AdvisoryLocker

	// NFSUID and NFSGID are the stable NFS ownership values (default 1000:1000).
	// Used for one-time chown of newly provisioned workspace directories.
	NFSUID int
	NFSGID int

	// SentinelDir overrides the directory where the provisioning sentinel file
	// (.scion-provisioned) is written and checked. When empty, defaults to
	// filepath.Dir(Resolved.HostPath) — the project root parent of the workspace
	// dir. The k8s init container, which cannot see that parent, sets it to the
	// project's provisioning state directory mounted next to the workspace
	// (ProvisionStateDirName), or, when its pod has no such mount (an older
	// runtime), to the workspace dir itself.
	SentinelDir string

	// LegacyDir is the directory where an older layout kept the sentinel and
	// the file lock: the workspace directory itself, for the Kubernetes init
	// container before the provisioning state directory existed. When set
	// and different from the sentinel directory:
	//   - a sentinel found there also counts as provisioned (the new sentinel
	//     is only ever written in the sentinel directory), and
	//     "/.scion-provisioned" is added to the workspace's git excludes;
	//   - with no Locker, the file lock in LegacyDir is taken BEFORE the one
	//     in the sentinel directory and released after it, so a pod running
	//     an older sciontool (which takes only the legacy lock) and a newer
	//     one always exclude each other, and the fixed order rules out a
	//     deadlock. The legacy lock is transient and git-excluded.
	LegacyDir string

	// RequireChownSuccess makes a chown failure fatal (returns an error,
	// before the sentinel is written) instead of the default warn-and-continue
	// behavior (F-111, design §9). The default tolerates "an operator may
	// have pre-chowned" for the broker's own host-side worktree-per-agent
	// flow. The k8s init container (cmd/sciontool/commands/provision.go) sets
	// this true: its entire purpose IS the chown, so a silent failure there
	// reproduces the exact "workspace stuck root:root" bug this mechanism
	// exists to fix — and does so invisibly, since without this flag the
	// sentinel would still be written, masking the failure from every future
	// pod that starts for this project (they'd all see the sentinel and skip
	// provisioning, forever).
	RequireChownSuccess bool

	// MountedWorktree is set by the Kubernetes provisioning init container
	// for WorktreePerAgent mode, where the agent container mounts
	// <workspace>/worktrees/<AgentID> by path. It changes the worktree step
	// as follows:
	//   - the branch is AgentName exactly as given (AgentID when empty),
	//     not sanitized, and must be a valid git branch name;
	//   - an existing worktree at the agent's path is reused only while
	//     git's entry for it points back to it, and only on the requested
	//     branch (or on another branch after the agent switched branches in
	//     it); a worktree git has no entry for is moved aside;
	//   - an existing empty directory at the agent's worktree path (created
	//     by the broker so the pod can mount it) is filled with the worktree;
	//   - an existing standalone checkout there (a directory with its own
	//     .git directory, cloned by the agent container of an older image) is
	//     kept as it is;
	//   - a shared checkout cloned in this mode has its HEAD detached; one
	//     provisioned earlier in another mode gets the worktrees/ exclude
	//     and gc.auto 0, and otherwise keeps its contents and branch;
	//   - the agent's branch must not already be checked out in another
	//     directory, since the pod can only mount its own path;
	//   - with a git too old for relative worktree paths, the agent's
	//     directory is created empty, so the agent container clones its own
	//     checkout into it;
	//   - afterwards, what this step added or rewrote is chowned to
	//     NFSUID:NFSGID (subject to RequireChownSuccess), since the worktree
	//     is added after the shared checkout's one-time chown (see
	//     ensureMountedWorktree).
	MountedWorktree bool

	// LockWait raises how long ProvisionShared waits for the file lock
	// (when Locker is nil). Values below the default budget (about 3m40s)
	// are ignored. The Kubernetes provisioning init container sets it in
	// worktree-per-agent mode, where every pod of the project takes the
	// lock: a pod that starts while another one clones the shared checkout
	// then waits as long as a pod that only waits for the sentinel.
	LockWait time.Duration
}

// heldLock represents a successfully acquired provisioning lock — either a
// store.AdvisoryLocker-backed lock or the filesystem fallback — plus the
// hooks ProvisionShared needs to detect and react to losing it mid-provision:
// a holder must not simply assume it holds the lock forever once acquired.
type heldLock struct {
	// release releases the lock. ProvisionShared calls it exactly once, via
	// a single deferred call.
	release func() error

	// ctx is derived from the ctx passed to the acquire call. For the
	// filesystem fallback, it is cancelled early if the heartbeat
	// determines this holder no longer owns the lock (a reclaimer beat it
	// to it), in addition to normal parent cancellation. Provisioning work
	// (git clone, chown) must run under this ctx, not the original one, so
	// a detected loss kills any in-flight subprocess instead of letting it
	// run to completion under a lock that no longer belongs to it. For the
	// store.AdvisoryLocker path, which has no independent loss-detection
	// signal, this is just the original ctx.
	ctx context.Context

	// stillOwned reports, via a fresh synchronous check (not merely the
	// heartbeat's last-observed state), whether this holder still owns the
	// lock right now. Callers must check this immediately before an
	// irreversible step (finalizing a clone move, writing the completion
	// sentinel) — the background heartbeat's periodic check alone leaves a
	// gap between "last confirmed" and the exact instant of that step; this
	// closes it. Always true for the store.AdvisoryLocker path.
	stillOwned func() bool
}

// resolveSentinelDir returns the directory used for both the completion
// sentinel and (when no Locker is available) the fallback file lock: an
// explicit override, or the workspace directory's parent by default. Shared
// by ProvisionShared and gitCloneWorkspace so the two never disagree about
// where the lock actually lives.
//
// For the broker and Cloud Run (no override), this is the workspace dir's
// PARENT — a sibling of, not inside, the git working tree. The Kubernetes
// init container (shared-plain and worktree-per-agent) overrides it with the
// project's provisioning state directory (ProvisionStateDirName), which the
// pod mounts by its own subPath next to the workspace, so the sentinel and
// the lock's litter stay out of the workspace there too. Only an init
// container whose pod has no such mount (an older runtime) still falls back
// to the workspace directory itself; there the lock's litter is excluded
// from chownProjectTree's walk (isLockArtifactPath) and from git's view
// (appendGitExclude), and is garbage-collected once old enough. The legacy
// lock taken through ProvisionInput.LegacyDir lives in the workspace too and
// is handled the same way.
func resolveSentinelDir(in ProvisionInput) string {
	if in.SentinelDir != "" {
		return in.SentinelDir
	}
	// The project root is the parent of the workspace dir:
	// <MountRoot>/<shareID>/<SubPathRoot>/<projectID>/ contains workspace/ + shared-dirs/.
	return filepath.Dir(in.Resolved.HostPath)
}

// SentinelDirs returns the directories ProvisionShared looks in for the
// sentinel, in order: the sentinel directory (SentinelDir, or the
// workspace's parent by default), then LegacyDir when set and different.
// A caller that must agree with ProvisionShared about whether the shared
// checkout is provisioned checks exactly these (see SentinelPresent).
func (in ProvisionInput) SentinelDirs() []string {
	sentinelDir := resolveSentinelDir(in)
	if legacy := legacyDir(in, sentinelDir); legacy != "" {
		return []string{sentinelDir, legacy}
	}
	return []string{sentinelDir}
}

// legacyDir returns in.LegacyDir when it names a directory other than
// sentinelDir, and "" otherwise.
func legacyDir(in ProvisionInput, sentinelDir string) string {
	if in.LegacyDir == "" || filepath.Clean(in.LegacyDir) == filepath.Clean(sentinelDir) {
		return ""
	}
	return in.LegacyDir
}

// excludeLegacySentinel adds "/.scion-provisioned" to the workspace's git
// excludes when the legacy layout is in use (LegacyDir is the workspace) and
// a sentinel file is in the workspace root, so it does not show in git
// status. That is checked independently of where the sentinel that ended
// provisioning was found: a pod running an older sciontool writes the
// legacy sentinel back into the workspace root even after the state
// directory has its own. Best effort: a workspace without a .git directory,
// or a failed write, is left alone.
func excludeLegacySentinel(in ProvisionInput) {
	ws := in.Resolved.HostPath
	if in.LegacyDir == "" || filepath.Clean(in.LegacyDir) != filepath.Clean(ws) {
		return
	}
	if _, err := os.Lstat(filepath.Join(ws, ProvisionSentinelFile)); err != nil {
		return
	}
	if fi, err := os.Lstat(filepath.Join(ws, ".git")); err != nil || !fi.IsDir() {
		return
	}
	if err := appendGitExclude(ws, "/"+ProvisionSentinelFile); err != nil {
		slog.Warn("ProvisionShared: failed to exclude the legacy sentinel from git status (non-fatal)",
			"project_id", in.ProjectID, "path", ws, "error", err)
	}
}

// ProvisionShared is the universal, vendor-agnostic workspace provisioning
// function (Tier 1). It ensures the workspace directory exists and is ready
// for use. For git projects this includes cloning/worktree setup. Idempotent.
//
// The flow implements the first-access provisioning guard:
//
//  1. If sentinel <projectRoot>/.scion-provisioned already exists and mode is
//     SharedPlain, done — return without ever taking the lock: an
//     already-provisioned project must not contend the lock at all,
//     including a file lock that a crashed holder elsewhere is still
//     sitting on.
//  2. Acquire per-project advisory lock (try with retry).
//  3. Re-check the sentinel now that the lock is held, in case another node
//     finished between step 1 and step 2.
//  4. Else: mkdir -p, git clone, chown 1000:1000, mode 0770, write sentinel.
//  5. WorktreePerAgent only: create/attach this agent's worktree (still under
//     the lock — worktree add/remove touches shared .git metadata).
//  6. Release lock.
//
// ClonePerAgent mode MUST NOT reach this path — it is node-local and handled
// by localBackend. An assert guards this.
//
// The flow is idempotent and safe under concurrency: two agents for the same
// project starting on two different nodes contend on the advisory lock;
// exactly one clones, the second sees the sentinel and reuses the workspace.
func ProvisionShared(in ProvisionInput) error {
	// Guard: ClonePerAgent must never use the NFS path. SelectWorkspaceBackend
	// already routes it to localBackend, but assert here as defense in depth.
	if in.Mode == store.SharingModeClonePerAgent {
		return fmt.Errorf("ProvisionShared: ClonePerAgent mode must not use NFS backend " +
			"(should be routed to localBackend by SelectWorkspaceBackend)")
	}
	// Guard: EmptyPerAgent has no shared project workspace to provision. On
	// NFS its private agent directory is prepared by ProvisionAgentDir
	// (design #2703 P3), so reaching here is a routing bug.
	if in.Mode == store.SharingModeEmptyPerAgent {
		return fmt.Errorf("ProvisionShared: EmptyPerAgent mode has no shared workspace to provision " +
			"(its agent directory is prepared by ProvisionAgentDir)")
	}

	if in.Resolved.HostPath == "" {
		return fmt.Errorf("ProvisionShared: Resolved.HostPath is required")
	}
	if in.ProjectID == "" {
		return fmt.Errorf("ProvisionShared: ProjectID is required")
	}

	sentinelDir := resolveSentinelDir(in)
	sentinelPath := filepath.Join(sentinelDir, ProvisionSentinelFile)

	ctx := in.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	// --- Step 0: Pre-lock sentinel fast path (SharedPlain only) ---
	// WorktreePerAgent always needs the lock below regardless of this check
	// — ensureWorktree must run under it even when the base clone is already
	// done — so only SharedPlain can skip locking entirely here.
	if in.Mode != store.SharingModeWorktreePerAgent {
		if dir, ok := findSentinel(in.SentinelDirs()...); ok && !markedWorkspaceNeedsClone(in) {
			slog.Debug("ProvisionShared: workspace already provisioned (sentinel exists, pre-lock)",
				"project_id", in.ProjectID, "sentinel_dir", dir)
			excludeLegacySentinel(in)
			return nil
		}
	}

	// --- Step 1: Acquire per-project advisory lock ---
	held, err := acquireProvisionLock(ctx, in, sentinelDir)
	if err != nil {
		return fmt.Errorf("ProvisionShared: failed to acquire lock for project %s: %w", in.ProjectID, err)
	}
	defer func() {
		if releaseErr := held.release(); releaseErr != nil {
			slog.Warn("ProvisionShared: failed to release advisory lock",
				"project_id", in.ProjectID, "error", releaseErr)
		}
	}()
	// From here on, use the lock-aware ctx: for the filesystem fallback it
	// is cancelled early if the heartbeat detects this holder lost the lock,
	// which kills any in-flight git/chown subprocess below instead of
	// letting it run to completion under a lock that no longer belongs to
	// it.
	ctx = held.ctx

	// --- Step 2: Check sentinel (under the lock: closes the gap between
	// the pre-lock check above and actually acquiring the lock) ---
	if dir, ok := findSentinel(in.SentinelDirs()...); ok {
		// Already provisioned — skip to worktree setup if needed.
		slog.Debug("ProvisionShared: workspace already provisioned (sentinel exists)",
			"project_id", in.ProjectID, "sentinel_dir", dir)
		excludeLegacySentinel(in)
		if markedWorkspaceNeedsClone(in) {
			return cloneIntoMarkedWorkspace(ctx, in, held.stillOwned, sentinelDir)
		}
		if in.MountedWorktree && in.Mode == store.SharingModeWorktreePerAgent {
			// The shared checkout may have been provisioned in another mode:
			// add what worktrees need, leaving its contents and HEAD alone.
			if err := prepareExistingBaseForWorktrees(ctx, in.Resolved.HostPath); err != nil {
				return fmt.Errorf("ProvisionShared: prepare base: %w", err)
			}
		}
		return ensureMountedWorktree(ctx, in)
	}

	// --- Step 3: Provision (mkdir + clone + chown + sentinel) ---
	slog.Info("ProvisionShared: provisioning workspace",
		"project_id", in.ProjectID, "host_path", in.Resolved.HostPath)

	// Create workspace directory.
	if err := os.MkdirAll(in.Resolved.HostPath, 0770); err != nil {
		return fmt.Errorf("ProvisionShared: mkdir workspace %s: %w", in.Resolved.HostPath, err)
	}

	// Create shared-dir directories.
	for name, sd := range in.Resolved.SharedDirs {
		if err := os.MkdirAll(sd.HostPath, 0770); err != nil {
			return fmt.Errorf("ProvisionShared: mkdir shared-dir %q %s: %w", name, sd.HostPath, err)
		}
	}

	// Git clone if project is git-backed.
	if in.GitClone != nil && in.GitClone.URL != "" {
		if err := gitCloneWorkspace(ctx, in, held.stillOwned); err != nil {
			return fmt.Errorf("ProvisionShared: git clone: %w", err)
		}
	}

	// For worktree-per-agent: detach HEAD, disable gc, exclude worktrees/.
	if in.Mode == store.SharingModeWorktreePerAgent {
		if err := prepareBaseForWorktrees(ctx, in.Resolved.HostPath); err != nil {
			return fmt.Errorf("ProvisionShared: prepare base: %w", err)
		}
	}

	// Chown to stable NFS UID/GID (design §9.1). This is a ONE-TIME operation
	// under the advisory lock — per-start chown is skipped for NFS (see N1-5).
	// chown -R on an existing, differently-owned directory (e.g. one kubelet
	// auto-created as root:root before this mechanism ran) re-owns it and
	// everything already inside it — self-healing on the next start needs no
	// separate repair step, as long as no sentinel was ever written for it
	// (F-111, design §9).
	chownRoot := chownTarget(in.Resolved.HostPath)
	uid, gid := resolveUID(in), resolveGID(in)
	if err := chownProjectTreeExcluding(ctx, chownRoot, []string{sentinelDir, legacyDir(in, sentinelDir)}, uid, gid); err != nil {
		if in.RequireChownSuccess {
			return fmt.Errorf("ProvisionShared: chown %s to %d:%d: %w", chownRoot, uid, gid, err)
		}
		slog.Warn("ProvisionShared: chown failed (non-fatal, may lack privileges)",
			"project_id", in.ProjectID, "path", chownRoot, "uid", uid, "gid", gid, "error", err)
		// Non-fatal: operator may have pre-chowned. Continue to write sentinel.
	}

	// Shared dirs are siblings of the workspace dir under the project root
	// on the broker's own host-side flow, so chownRoot above (the project
	// root there) already recurses into them — this loop is a no-op there
	// beyond a second, redundant chown -R. In the k8s init container,
	// chownRoot is scoped to the workspace subPath mount alone (chownTarget's
	// "/" fallback), which does NOT reach a shared dir mounted at its own,
	// separate subPath (F-111, design §9) — chown each one explicitly so it
	// isn't missed there.
	for name, sd := range in.Resolved.SharedDirs {
		if err := chownProjectTree(ctx, sd.HostPath, "", uid, gid); err != nil {
			if in.RequireChownSuccess {
				return fmt.Errorf("ProvisionShared: chown shared-dir %q %s to %d:%d: %w", name, sd.HostPath, uid, gid, err)
			}
			slog.Warn("ProvisionShared: chown shared-dir failed (non-fatal, may lack privileges)",
				"project_id", in.ProjectID, "name", name, "path", sd.HostPath, "uid", uid, "gid", gid, "error", err)
		}
	}

	// Re-verify ownership immediately before the irreversible sentinel
	// write: the chown above can take a long time on a large tree over NFS,
	// long enough that this holder could have silently lost the lock to a
	// reclaimer since the heartbeat's last check. Writing the sentinel while
	// a second provisioner also believes it owns the lock would let both
	// finish and contend over the write — a synchronous check right here
	// closes that gap rather than trusting the background heartbeat's
	// last-observed state alone.
	if !held.stillOwned() {
		return fmt.Errorf("ProvisionShared: lost the provisioning lock before writing the sentinel for project %s", in.ProjectID)
	}

	// Write sentinel atomically.
	if err := writeSentinel(sentinelPath); err != nil {
		return fmt.Errorf("ProvisionShared: write sentinel: %w", err)
	}

	slog.Info("ProvisionShared: workspace provisioned successfully",
		"project_id", in.ProjectID, "host_path", in.Resolved.HostPath)

	// --- Step 4: Worktree setup (if WorktreePerAgent) ---
	return ensureMountedWorktree(ctx, in)
}

// ensureMountedWorktree runs the worktree step and, for MountedWorktree,
// chowns to NFSUID:NFSGID what that step created or rewrote as root: on
// every start the sharer marker and .git/config (rewritten by git config),
// plus .git/info/exclude, the worktree's start-branch record and any other
// sharer marker the step rewrote; and, when a worktree was added, the agent's
// worktree, its admin directory under .git/worktrees, the branch ref and
// its reflog (with any directories the branch name adds) and the
// worktrees/ directories. Everything else in the shared checkout was
// chowned when it was first provisioned, and a reused worktree is left as
// it is. The files rewritten on every start are chowned even when the step
// fails, so a failed start does not leave them owned by root.
func ensureMountedWorktree(ctx context.Context, in ProvisionInput) error {
	outcome, stepErr := ensureWorktree(ctx, in)
	if !in.MountedWorktree || in.Mode != store.SharingModeWorktreePerAgent {
		return stepErr
	}
	base := in.Resolved.HostPath
	gitDir := filepath.Join(base, ".git")
	uid, gid := resolveUID(in), resolveGID(in)
	chown := func(path string, recursive bool) error {
		if _, err := os.Lstat(path); err != nil {
			return nil
		}
		var err error
		if recursive {
			err = chownProjectTree(ctx, path, "", uid, gid)
		} else {
			err = lchownFile(path, uid, gid)
		}
		if err != nil {
			if in.RequireChownSuccess {
				return fmt.Errorf("ProvisionShared: chown %s to %d:%d: %w", path, uid, gid, err)
			}
			slog.Warn("ProvisionShared: chown failed (continuing)",
				"project_id", in.ProjectID, "path", path, "uid", uid, "gid", gid, "error", err)
		}
		return nil
	}
	// chownChain chowns each existing directory and file from root down to
	// root/rel, so directories git created for a branch name with slashes
	// are included.
	chownChain := func(root, rel string) error {
		cur := root
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			cur = filepath.Join(cur, part)
			if err := chown(cur, false); err != nil {
				return err
			}
		}
		return nil
	}

	paths := []string{filepath.Join(gitDir, "config"), filepath.Join(gitDir, "info"), filepath.Join(gitDir, "info", "exclude")}
	if outcome.branch != "" {
		marker := sharerPath(base, outcome.branch)
		paths = append(paths, filepath.Dir(marker), marker)
	}
	paths = append(paths, outcome.rewritten...)
	if outcome.added || outcome.emptyDir {
		paths = append(paths, filepath.Join(base, "worktrees"))
	}
	for _, p := range paths {
		if err := chown(p, false); err != nil && stepErr == nil {
			return err
		}
	}
	if stepErr != nil {
		return stepErr
	}
	worktreePath := WorktreePath(base, in.AgentID)
	if outcome.emptyDir {
		return chown(worktreePath, false)
	}
	if !outcome.added {
		return nil
	}
	if err := chown(worktreePath, true); err != nil {
		return err
	}
	if admin, ok := worktreeAdminDir(worktreePath, base); ok {
		if err := chown(filepath.Dir(admin), false); err != nil {
			return err
		}
		if err := chown(admin, true); err != nil {
			return err
		}
	}
	if err := chownChain(gitDir, filepath.Join("refs", "heads", outcome.branch)); err != nil {
		return err
	}
	return chownChain(gitDir, filepath.Join("logs", "refs", "heads", outcome.branch))
}

// ErrSeparateCheckout is returned by RemoveMountedWorktree when the agent's
// directory holds its own clone (made by the agent container of an older
// image) instead of a worktree of the shared checkout. The directory is
// left in place.
var ErrSeparateCheckout = errors.New("the agent's directory holds a separate clone, not a worktree of the shared checkout")

const (
	// removedWorktreePrefix names an agent's worktree directory once
	// RemoveMountedWorktree has moved it aside; PurgeRemovedWorktrees
	// deletes such directories. A leading dot keeps it apart from agent
	// slugs.
	removedWorktreePrefix = ".removing-"
	// staleWorktreePrefix names an agent's directory that the worktree
	// step moved aside because git no longer knew the worktree. It is
	// kept for the user to inspect.
	staleWorktreePrefix = ".stale-"
)

// RemoveMountedWorktree removes the worktree of the agent named name from
// the shared checkout at base (base/worktrees/<name>), as added by
// ProvisionShared with MountedWorktree. It runs under the same per-project
// file lock as provisioning, which lives in base itself, so it never runs
// at the same time as a provisioning init container of the same project.
// lockWait bounds only the wait for that lock; the steps under the lock
// run to completion.
//
// name must be an agent slug, and base/worktrees must be a plain
// directory. Only base/worktrees/<name> is touched, and only when it is a
// worktree of this checkout (or one git no longer has an entry for): it
// is renamed to a .removing- name inside worktrees/, git worktree prune
// drops its entry, and the agent is dropped from the sharer registry. The
// rename is a single step, so a removal that stops partway never leaves a
// half-deleted worktree at the agent's path. PurgeRemovedWorktrees deletes
// the renamed directory afterwards. The agent's branch is kept. An empty
// directory is removed. A missing directory is not an error. A separate
// clone is left in place (ErrSeparateCheckout), and so is anything else,
// with an error.
//
// stateDir is the project's provisioning state directory (ProjectStateDir)
// when the init containers keep their lock there, or "" for the legacy
// layout. With stateDir set, the legacy lock in base is taken first and the
// lock in stateDir second, in the same order as ProvisionShared with
// ProvisionInput.LegacyDir, so a removal excludes both older and newer
// provisioning init containers.
func RemoveMountedWorktree(ctx context.Context, base, stateDir, name string, lockWait time.Duration) error {
	if slug, err := api.ValidateAgentName(name); err != nil || slug != name {
		return fmt.Errorf("remove worktree: agent name %q is not an agent slug", name)
	}
	path := WorktreePath(base, name)
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("remove worktree %s: %w", path, err)
	}

	// lockWait bounds the wait for the lock; the caller's cancellation is
	// ignored so the steps under the lock always finish.
	lockDirs := []string{base}
	if stateDir != "" && filepath.Clean(stateDir) != filepath.Clean(base) {
		// Legacy lock first, then the state-directory lock (see
		// ProvisionInput.LegacyDir). The legacy lock goes away with
		// ptone/scion#2974.
		lockDirs = append(lockDirs, stateDir)
	}
	held, err := acquireOrderedFileLocks(context.WithoutCancel(ctx), lockDirs, lockWait)
	if err != nil {
		return fmt.Errorf("remove worktree %s: %w", path, err)
	}
	defer func() {
		if releaseErr := held.release(); releaseErr != nil {
			slog.Warn("remove worktree: failed to release the provisioning lock", "path", base, "error", releaseErr)
		}
	}()
	ctx = held.ctx

	// Check again under the lock.
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	worktreesDir := filepath.Join(base, "worktrees")
	if fi, err := os.Lstat(worktreesDir); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("remove worktree %s: %s is not a plain directory; left in place", path, worktreesDir)
	}
	if !IsRealWorktreeDir(path, base) {
		switch mountedDirState(path) {
		case mountedDirEmpty:
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove worktree %s: %w", path, err)
			}
			return nil
		case mountedDirStandalone:
			return fmt.Errorf("remove worktree %s: %w; left in place", path, ErrSeparateCheckout)
		default:
			return fmt.Errorf("remove worktree %s: not a worktree of the shared checkout at %s; left in place", path, base)
		}
	}
	if linkedWorktreeState(base, path) == linkedWorktreeMismatch {
		return fmt.Errorf("remove worktree %s: git's entry for this worktree points elsewhere; left in place", path)
	}

	if _, err := moveWorktreeAside(base, name, removedWorktreePrefix); err != nil {
		return fmt.Errorf("remove worktree %s: %w", path, err)
	}
	pruneCtx, cancel := context.WithTimeout(ctx, removalPruneTimeout)
	defer cancel()
	if out, err := gitInSharedCheckout(pruneCtx, base, path, "worktree", "prune"); err != nil {
		slog.Warn("remove worktree: git worktree prune failed", "path", base, "error", err, "output", out)
	}
	if _, err := UnregisterSharerElsewhere(base, name, ""); err != nil {
		slog.Warn("remove worktree: could not update the sharer registry", "path", base, "error", err)
	}
	return nil
}

// removalPruneTimeout bounds the git worktree prune RemoveMountedWorktree
// runs under the lock.
const removalPruneTimeout = 2 * time.Minute

// PurgeRemovedWorktrees deletes the directories RemoveMountedWorktree
// moved aside under base/worktrees, including any left by an earlier
// removal that stopped partway. Nothing refers to them any more, so it
// runs without the provisioning lock and without a deadline. base/worktrees
// must be a plain directory; the deletes run inside that directory as
// opened, so they stay there even if the path is replaced meanwhile.
func PurgeRemovedWorktrees(base string) error {
	return purgePrefixedEntries(filepath.Join(base, "worktrees"), removedWorktreePrefix)
}

// moveWorktreeAside renames base/worktrees/<name> to a unique
// <prefix><name>-<suffix> entry in the same directory and returns the new
// path. The moved directory's .git file is renamed to .git.moved, so the
// directory is plain files: git no longer treats it as a worktree, and its
// link cannot reach the git entry of a new worktree with the same name.
func moveWorktreeAside(base, name, prefix string) (string, error) {
	from := WorktreePath(base, name)
	to := filepath.Join(base, "worktrees", prefix+name+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := os.Rename(from, to); err != nil {
		return "", fmt.Errorf("move %s aside: %w", from, err)
	}
	link := filepath.Join(to, ".git")
	if fi, err := os.Lstat(link); err == nil && !fi.IsDir() {
		if err := os.Rename(link, link+".moved"); err != nil {
			if rmErr := os.Remove(link); rmErr != nil {
				slog.Warn("could not unlink the moved directory from git", "path", to, "rename_error", err, "remove_error", rmErr)
			}
		}
	}
	return to, nil
}

// gitInSharedCheckout runs git in the shared checkout at base. The shared
// checkout and the agent's worktree belong to the agents' user rather than
// the caller, so both are listed in safe.directory for this command only.
func gitInSharedCheckout(ctx context.Context, base, worktree string, args ...string) (string, error) {
	full := append([]string{"-c", "safe.directory=" + base, "-c", "safe.directory=" + worktree, "-C", base}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// fileLockWait is how long ProvisionShared waits for the file lock:
// in.LockWait when it is longer than the default budget, otherwise the
// default.
func fileLockWait(in ProvisionInput) time.Duration {
	if def := defaultFileLockWait(); in.LockWait <= def {
		return def
	}
	return in.LockWait
}

// acquireProvisionLock acquires the per-project advisory lock, retrying briefly
// if another node currently holds it. Returns a heldLock.
//
// The retry loop respects context cancellation so that server shutdown is not
// blocked for up to provisionLockRetries × provisionLockRetryDelay.
//
// sentinelDir is used only by the no-Locker fallback (acquireFileLock) — it
// is the directory both a lock-winner and any concurrent waiters already see
// identically (the NFS-mounted project root), so it doubles as the shared
// medium for a filesystem-based mutex when no store.AdvisoryLocker is wired
// up. This is the common case for the k8s init container: it has no Hub/DB
// connection at all (see the package doc above), so in.Locker is always nil
// there — see cmd/sciontool/commands/provision.go's ProvisionInput.
func acquireProvisionLock(ctx context.Context, in ProvisionInput, sentinelDir string) (heldLock, error) {
	if in.Locker == nil {
		if legacy := legacyDir(in, sentinelDir); legacy != "" {
			// Legacy lock first, then the state-directory lock, always in
			// this order (see ProvisionInput.LegacyDir). The legacy lock can
			// be dropped once no supported sciontool image takes only the
			// legacy lock; tracked in ptone/scion#2974.
			return acquireOrderedFileLocks(ctx, []string{legacy, sentinelDir}, fileLockWait(in))
		}
		return acquireFileLockWithin(ctx, sentinelDir, fileLockWait(in))
	}

	objID := store.StableProjectHash(in.ProjectID)
	ticker := time.NewTicker(provisionLockRetryDelay)
	defer ticker.Stop()

	for attempt := 0; attempt < provisionLockRetries; attempt++ {
		acquired, release, err := in.Locker.TryAdvisoryLockObject(ctx, store.LockWorkspaceProvision, objID)
		if err != nil {
			return heldLock{}, fmt.Errorf("advisory lock attempt %d: %w", attempt, err)
		}
		if acquired {
			// No independent loss-detection signal on this path (a Postgres
			// advisory lock releases automatically if the holding
			// connection drops, but nothing here observes that
			// independently) — ctx is just the caller's own, and stillOwned
			// is trivially true.
			return heldLock{release: release, ctx: ctx, stillOwned: func() bool { return true }}, nil
		}
		// Another node holds the lock — it's provisioning this project.
		// Wait briefly and retry, but honour context cancellation.
		slog.Debug("ProvisionShared: lock held by another node, retrying",
			"project_id", in.ProjectID, "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return heldLock{}, fmt.Errorf("context cancelled while waiting for provisioning lock (project %s): %w",
				in.ProjectID, ctx.Err())
		case <-ticker.C:
		}
	}

	return heldLock{}, fmt.Errorf("failed to acquire provisioning lock after %d attempts (project %s)",
		provisionLockRetries, in.ProjectID)
}

// acquireOrderedFileLocks takes the file lock in each of dirs, in order,
// each within wait, and returns a heldLock covering all of them: release
// releases them in reverse order, ctx is cancelled when any of them is lost
// (each acquisition derives from the previous one's ctx), and stillOwned
// requires every one of them. If an acquisition fails, the locks already
// taken are released.
func acquireOrderedFileLocks(ctx context.Context, dirs []string, wait time.Duration) (heldLock, error) {
	held := make([]heldLock, 0, len(dirs))
	releaseAll := func() error {
		var firstErr error
		for i := len(held) - 1; i >= 0; i-- {
			if err := held[i].release(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
	cur := ctx
	for _, dir := range dirs {
		h, err := acquireFileLockWithin(cur, dir, wait)
		if err != nil {
			if relErr := releaseAll(); relErr != nil {
				slog.Warn("acquireOrderedFileLocks: failed to release a lock after a later acquisition failed",
					"dir", dir, "error", relErr)
			}
			return heldLock{}, err
		}
		held = append(held, h)
		cur = h.ctx
	}
	return heldLock{
		release: releaseAll,
		ctx:     cur,
		stillOwned: func() bool {
			for _, h := range held {
				if !h.stillOwned() {
					return false
				}
			}
			return true
		},
	}, nil
}

// provisionLockOwnerFile is written inside the staging directory BEFORE the
// publish rename that makes a lock visible at lockPath at all (see
// tryCreateFileLock), carrying a random owner id unique to that one
// acquisition. A published lock therefore always already has this file —
// lockPath is never observed in a state where the directory exists but this
// file does not. It is how every operation here tells "this is still the
// same lock instance I think it is" apart from "this name now refers to a
// different (older evicted, or newer) generation" — the identity check that
// makes every rename-based claim below safe to act on.
const provisionLockOwnerFile = "owner"

// provisionLockFileMode is the mode of the files inside a lock directory
// (owner, heartbeat, evicted-at). Together with shareLockDirWithGroup it
// lets another user in the directory's group, such as the broker and the
// provisioning init container on an NFS export, read the owner id and the
// heartbeat, so either one can reclaim a lock the other left behind.
const provisionLockFileMode = 0o660

// shareLockDirWithGroup gives a new lock directory group read, write and
// search, keeping the setgid bit it inherited from a setgid parent, so new
// files in it keep the parent's group. os.MkdirTemp creates it 0700.
func shareLockDirWithGroup(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	return os.Chmod(dir, info.Mode()&os.ModeSetgid|0o770)
}

// provisionLockHeartbeatFile is written inside the lock directory on every
// heartbeat beat (see startLockHeartbeat), carrying the holder's owner id.
// Its mtime — an ordinary content write, so the timestamp the NFS
// server assigns it is the server's own clock, never the writer's (see
// serverNow's doc) — is what lockLooksAbandoned actually measures staleness
// against. A lock that has never beaten yet has no such file; lastHeartbeat
// falls back to the lock directory's own (also server-assigned) mtime for
// that case. This covers three situations: the publish-to-synchronous-beat
// window itself (the brief gap between the publish rename and the
// synchronous first beat that follows it, before either has run); that
// synchronous first beat failing transiently, with the periodic beat not
// yet landed either; or an NFSv3 RENAME retransmit making its own
// successful publish look like a failure to its creator — see
// acquireFileLock's doc.
const provisionLockHeartbeatFile = "heartbeat"

// provisionClockProbeFile is the name PREFIX for the small marker serverNow
// writes (and immediately reads back and removes) to obtain a timestamp
// assigned by the NFS export server's own clock. Kept in the same directory
// as the lock — and, deliberately, named UNDER the lock's own prefix
// (provisionFileLockName+".clock-probe", not an unrelated name) so that in
// the k8s configuration, where that directory is the workspace itself, it
// is covered by the same git-exclude glob and stray-content-clear exemption
// as the lock and its stage/evict entries. Its content is irrelevant, only
// its mtime is ever read.
//
// Each call appends its own random suffix (see serverNow) rather than
// sharing one fixed name: two concurrent callers acting on the same tick
// would otherwise write, stat and remove the identical path, and one caller's
// remove can land between another's write and its own stat, making that
// stat fail with ENOENT for no reason related to actual staleness — on a
// crashed holder with several waiters polling the same tick, this knocks
// every one of them out of that tick's reclaim attempt at once. A leftover
// probe from a removal that failed is still harmless, inert litter under
// the same prefix, swept by garbageCollectLockLitter like any other.
const provisionClockProbeFile = provisionFileLockName + ".clock-probe"

// provisionLockEvictedAtFile is written inside an ".evict-<id>" directory
// immediately after the rename that creates it, via an ordinary content
// write — so, like the heartbeat marker, its mtime is assigned by the NFS
// server's own clock, not the writer's. garbageCollectLockLitter ages an
// evict entry from THIS marker's mtime, not the entry's own: rename(2) does
// not change the mtime of the directory it moves, so an evicted directory's
// own mtime is still whenever its last entry was created before eviction —
// for a lock first published long ago, that is far in the past the instant
// it is evicted, not "just now". Ageing from the directory's own mtime would
// let a generation held longer than roughly provisionLockStaleAfter get
// swept by the very next acquireFileLock, immediately after eviction,
// re-opening the exact vacancy the deterministic destination exists to
// close: see garbageCollectLockLitter's doc.
const provisionLockEvictedAtFile = "evicted-at"

// writeLockFile and renameFile are indirections over os.WriteFile/os.Rename
// used throughout the lock machinery below, so tests can simulate a failure
// at exactly the owner-marker-write point — a real disk-full/read-only-mount
// fault isn't something a hermetic unit test can portably trigger otherwise.
// moveRenameFile is the same idea for gitCloneWorkspace's clone-move step
// specifically, kept as its own variable so a test simulating a move
// failure can't also perturb the lock's own rename calls running around it.
// Production code never reassigns any of these.
var (
	writeLockFile  = os.WriteFile
	renameFile     = os.Rename
	moveRenameFile = renameNoReplace
)

// lchownFile is an indirection over os.Lchown, used by chownProjectTree, so
// a test can simulate ownership having been lost mid-walk (a reclaimer
// evicting this holder's lock while the walk is still in flight over NFS)
// at a specific, controlled point, without needing a real multi-second walk
// over a real filesystem to interleave with. Production code never
// reassigns it.
var lchownFile = os.Lchown

// timeNow is time.Now, indirected so tests can simulate the passage of time
// for the elapsed-time pause guards in tryReclaimIfAbandoned,
// tryCreateFileLock, and releaseFileLock (see each one's doc) without
// actually sleeping for minutes of wall-clock time. Production code never
// reassigns it; it is deliberately separate from the server-timestamp
// machinery above (serverNow, lastHeartbeat) — those measure staleness from
// server-assigned timestamps shared across nodes, while this measures only
// how long THIS process's own local attempt has been running, which is
// meaningful even to a single node with no shared clock reference.
// time.Since (and Sub, used here) subtracts the monotonic reading time.Now
// embeds when both values carry one, so a wall-clock step — NTP, a manual
// change — does not affect these guards. See acquireFileLock's
// "Per-participant bounds" for the one caveat this does not cover: a host
// suspend/resume does not advance Go's monotonic clock on Linux either.
var timeNow = time.Now

// acquireFileLock acquires a mutual-exclusion lock backed by the shared
// filesystem itself, for use when no store.AdvisoryLocker is available —
// which today is every caller: RunConfig.Locker is never populated by any
// runtime (k8s, docker, Cloud Run), so this is not just the k8s init
// container's fallback, it is the only lock any of them ever actually gets.
// Every concurrent provisioner for the same project already sees the same
// NFS-mounted dir identically — that shared filesystem, not a DB
// connection, is the coordination medium actually available in that context.
//
// dir is created (MkdirAll) if it does not yet exist: this lock must be
// acquired before ProvisionShared's own mkdir of the workspace directory
// runs, so on a project's first-ever start, dir may not exist yet.
//
// # Correctness argument
//
// lockPath (dir/provisionFileLockName) is always in exactly one of two
// states: EMPTY (no lock) or OCCUPIED-BY-T (a fully-formed lock directory
// carrying owner id T sits there — see tryCreateFileLock's doc for why it
// is never observed half-built). The only transitions are:
//
//  1. EMPTY → OCCUPIED-BY-T: tryCreateFileLock's publish, a single rename of
//     a fully-built staging directory onto lockPath. Of any number of
//     concurrent publish attempts, exactly one rename can win an empty
//     destination — that is what makes T unique per occupancy.
//  2. OCCUPIED-BY-T → EMPTY: exactly one rename, by EITHER a reclaimer
//     (tryReclaimIfAbandoned) or T's own holder releasing
//     (releaseFileLock), of the form rename(lockPath, lockPath+".evict-"+T).
//     The destination name is DETERMINISTIC in T, not a fresh name per
//     attempt: every caller that ever wants to remove generation T from
//     lockPath — whoever they are, however many of them there are, however
//     stale or fresh their own view of T is — targets that exact same
//     destination.
//
// # The invariant this depends on
//
// rename(2) does not inspect what it moves: `rename(lockPath, X)` relocates
// WHATEVER currently sits at lockPath, not specifically generation T. The
// only thing that stops a late remover of T — one still acting on a
// stale/cached read of T, interleaved with transition 1's publish of a live
// successor — from relocating that live successor is that its destination,
// ".evict-T", is already occupied by T's own, earlier removal. So: **from
// the instant T leaves lockPath until no observer of T can still attempt to
// remove it, ".evict-T" must exist.** Only while that holds can at most one
// rename ever succeed for a given T, and only then is it true that a
// rename's success proves lockPath still held that exact id at that
// instant. garbageCollectLockLitter is the one piece of code responsible
// for not breaking this invariant — see its doc for how it ages an
// ".evict-T" entry to stay within it, and tryReclaimIfAbandoned's doc for
// what happens in the remaining, narrow window where a late rename does
// land on a live successor (it is detected after the fact, not prevented).
//
// A stale/cached observer of a since-replaced T2 cannot evict T2 instead,
// because its rename targets ".evict-T" (the OLD generation it observed),
// not ".evict-T2" — those are different destination names by construction,
// so a remover of T can never collide with T2's own eventual removal. What
// the invariant above adds is the other half: T's OWN destination name must
// still be reserved for long enough that a late remover of T collides with
// it too, instead of finding it vacant and relocating whatever generation
// has since taken T's place.
//
// This holds regardless of how many nodes are involved or how stale any
// individual node's cached view of a prior generation is, PROVIDED every
// participant that can touch lockPath — not just a holder between
// heartbeats, but also a reclaimer between its staleness read and its
// rename, and a publisher between starting to build its staging directory
// and publishing it — stays within its own stated bound. See
// "Per-participant bounds" below.
//
// # Per-participant bounds
//
// All three bounds below are derived from the same underlying fact: an
// NFS-server-assigned timestamp can lag the event it records by up to the
// attribute-cache window (acregmax/acdirmax, ~60s by default) by the time
// another node observes it. Call the stale window S (provisionLockStaleAfter),
// the heartbeat interval I (provisionLockHeartbeatInterval), and the
// attribute-cache window A.
//
//   - Holder: pause + I + A < S. A reclaimer's view of the last heartbeat
//     can already lag by A, and the last beat it sees can itself be up to
//     one interval old, before the holder's own pause between beats is even
//     counted. At production defaults (S=3min, I=30s, A≈60s) this bounds a
//     tolerable holder pause to roughly 90s, not the full 3 minutes. A
//     genuinely live holder that keeps heartbeating faster than this is
//     never evicted — lockLooksAbandoned keeps judging it fresh, and a
//     reclaimer's own claim targets exactly the T it just read as stale, so
//     a legitimately alive holder is only evicted once it has gone silent
//     for longer than this bound. This bound is inherent to a lease with no
//     fencing mechanism and is not enforced in code, only documented and
//     accepted (see the PARTITIONED paragraph below).
//   - Reclaimer: the elapsed time in tryReclaimIfAbandoned from reading an
//     owner id to renaming it away is bounded at one S by a monotonic
//     elapsed-time guard, checked immediately before its rename —
//     comfortably under the garbage-collection cutoff of 2S (see "Why the
//     garbage-collection cutoff is 2S" below), leaving room for the rename
//     RPC itself to complete.
//   - Publisher: the elapsed time in tryCreateFileLock from starting to
//     build its staging directory to publishing it, plus the margin needed
//     for a cached reclaimer to still see this generation as fresh once
//     published. acquireFileLock beats once synchronously immediately after
//     a successful publish; WHEN THAT BEAT SUCCEEDS, it lands the first
//     heartbeat marker before this function ever returns, so the remaining
//     requirement is pre-publish-pause + A < S. If that beat instead fails
//     transiently, the periodic beat lands within one interval I, so the
//     requirement becomes pre-publish-pause + I + A < S — still satisfied by
//     the bound below at production defaults, so the bound does not depend
//     on the synchronous beat's success. (A DEFINITE loss from that beat is
//     a different case entirely — see its call site — and is never treated
//     as a successful acquisition.) tryCreateFileLock enforces a bound of
//     S/4 (at defaults, 45+60=105 < 180 with the synchronous beat, or
//     45+30+60=135 < 180 without it, both leaving real margin for the
//     publish rename RPC itself) via the same kind of monotonic
//     elapsed-time guard, checked immediately before its publish rename.
//   - Release: the elapsed time in releaseFileLock between its ownership
//     check and its eviction rename has the identical shape as the
//     reclaimer's window and is bounded the same way, at one S.
//
// # Why the garbage-collection cutoff is 2S
//
// A late remover of generation T — a reclaimer still acting on a cached
// read of T, or T's own holder releasing through a cached lookup — can act
// until roughly: the instant of T's eviction, plus the attribute-cache
// window A (how stale its own cached view of T can be before it even starts
// acting), plus its own elapsed-time guard's bound of one S, plus whatever
// the rename RPC itself adds on top (the one residual neither guard can
// see). The cutoff of 2S is therefore sufficient exactly when that residual
// stays under S − A (120s at production defaults). That residual covers
// ordinary RPC latency only: a partition or hang that begins AFTER the
// elapsed-time guard's own check but DURING the rename RPC itself is bounded
// by neither this margin nor any guard here — on a hard mount (the default)
// the client retransmits the same rename once the partition heals, and by
// then ".evict-T" may already be gone if more than 2S − A has elapsed since
// T's eviction. This is the same accepted partition/hang class described
// below, not a new gap this cutoff closes; a GC pause or an ordinary node
// stall BEFORE the rename is already caught by the elapsed-time guards
// above, well before a comparison against this cutoff would even be
// reached. garbageCollectLockLitter is the only place this cutoff is
// enforced; see its own doc for how it ages an entry against it.
//
// A crashed or killed participant is the intended failure mode here. One
// that is merely PARTITIONED or hung — still running, but unable to reach
// the NFS server for longer than its own bound above — is not: once the
// pause exceeds the bound, another participant may legitimately act on the
// resulting state, and the original one, on healing, continues running
// (its next heartbeat beat, its own elapsed-time guard on its next
// operation, or a stillOwned() check, is what would tell a holder
// specifically that it no longer holds the lock) until one of those
// actually happens. stillOwned()
// is checked before the sentinel write and before the clone-move step in
// gitCloneWorkspace; the clone itself and ensureWorktree's
// git calls run under the lock-derived ctx (cancelled once the heartbeat
// notices) but do not re-verify ownership synchronously mid-operation. A
// holder partition longer than its bound, healing after one of those steps
// has already run, can therefore yield two concurrent writers into one
// workspace; this is accepted for the same reason the fixed wait budget
// below is (see its doc), not something this change closes. The
// reclaimer, publisher, and release guards above turn a pause that happens
// BEFORE their own rename into an aborted or retried attempt for those
// three participants — but a partition or hang that begins DURING the
// rename RPC itself, after the guard's own check has already passed, is
// bounded by none of them (see "Why the garbage-collection cutoff is 2S"
// above for why): it falls into this same accepted partition/hang class,
// detected after the fact by the reclaimer's own defensive post-rename
// owner check once it resumes, and not detected at all for release, which
// has no equivalent check.
//
// None of these bounds requires any two nodes' wall clocks to agree with
// each other or with the server: the holder/reclaimer comparison is
// between two server-assigned timestamps, and the reclaimer/publisher/
// release elapsed-time guards each measure only their own process's local
// monotonic clock across a single short operation, needing no shared
// reference at all. The one caveat is that a monotonic clock reading does
// not advance across a host suspend/resume (e.g. suspend-to-RAM, or a
// suspended VM) on Linux, so a node suspended between a guard's start and
// end wakes up with a small elapsed reading and acts on what is by then
// stale state — a suspend is treated the same as any other
// longer-than-bound pause: not measured, not prevented, and no worse than
// the accepted holder-partition case above.
//
// The overall wait is a wall-clock deadline (fileLockRetries *
// fileLockRetryDelay), not an attempt counter: tryReclaimIfAbandoned itself
// only performs a destructive rename after its own non-destructive
// staleness read says stale, so a fresh/live lock is never touched by it at
// all, and every outcome that isn't an actual eviction falls through to the
// same sleep-or-ctx select as plain contention — an attempt counter could
// otherwise be exhausted in milliseconds by a run of such outcomes with
// nothing to slow it down.
//
// One caveat neither this nor the staleness comparison below can fully
// close: NFSv3 write-class operations (MKDIR, RENAME, CREATE) are not
// required to be idempotent under retransmission. If a client's publish
// rename (in tryCreateFileLock) succeeds on the server but the reply is
// lost, the client's RPC layer may retransmit the same rename, and the
// retransmit can come back as a failure — to the very client that just
// published the lock. That client would then treat its own successful
// acquisition as a failure and back off; the lock it actually holds
// self-heals via the normal stale-reclaim path once no further heartbeats
// arrive (it thinks nobody owns it, so it never releases it either — an
// orphaned lock, not a corrupted one). This is a known NFS protocol
// wrinkle, not something a client-side retry loop can fully distinguish
// from genuine contention; documented here rather than solved.
func acquireFileLock(ctx context.Context, dir string) (heldLock, error) {
	return acquireFileLockWithin(ctx, dir, defaultFileLockWait())
}

// defaultFileLockWait is acquireFileLock's overall wait budget.
func defaultFileLockWait() time.Duration {
	return time.Duration(fileLockRetries) * fileLockRetryDelay
}

// acquireFileLockWithin is acquireFileLock with an explicit overall wait
// budget instead of defaultFileLockWait().
func acquireFileLockWithin(ctx context.Context, dir string, wait time.Duration) (heldLock, error) {
	if err := os.MkdirAll(dir, 0770); err != nil {
		return heldLock{}, fmt.Errorf("acquireFileLock: mkdir %s: %w", dir, err)
	}
	// Best-effort, once per acquisition attempt (not per retry tick, to
	// keep the extra NFS round trips proportional to actual contention
	// rather than to how long a single wait runs) — see its doc.
	garbageCollectLockLitter(dir)

	lockPath := filepath.Join(dir, provisionFileLockName)

	deadline := time.Now().Add(wait)
	ticker := time.NewTicker(fileLockRetryDelay)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		token, err := tryCreateFileLock(lockPath)
		if err == nil {
			// A synchronous first beat, right here, before this function
			// returns to its caller, lands the first heartbeat marker as
			// soon as the publish rename itself completes, instead of
			// waiting up to one full heartbeat interval for the periodic
			// beat below — see tryCreateFileLock's elapsed-time guard and
			// "Per-participant bounds" in this function's own doc for the
			// bound this closes WHEN THIS BEAT SUCCEEDS. If it fails
			// transiently, the periodic beat below still lands within one
			// interval, and the bound (S/4 + I + A < S at defaults) already
			// accounts for that case. A definite loss here is different in
			// kind from a transient failure: it means this generation was
			// already evicted before this process could even confirm its
			// own publish — reachable only if the publish rename, or the
			// gap between it and this beat, stalled past the publisher's
			// own bound (the residual neither that guard nor this beat can
			// see) — and must not be returned to the caller as a successful
			// acquisition. That eviction shows up two ways: a successor has
			// already published, so beatOnce reads back a mismatched owner
			// id directly (beatDefiniteLoss); or no successor has published
			// yet, so lockPath is simply vacant and beatOnce's own read
			// reports ENOENT — ordinarily tolerated as beatTransientMiss for
			// the periodic heartbeat's benefit (an owner file can be
			// momentarily absent mid-eviction on a lock this process still
			// holds), but this call just published this exact generation
			// moments ago, so a missing owner file here is proof this
			// generation is already gone, not ordinary transient noise, and
			// is treated the same as a definite loss below. A third outcome
			// of the identical residual: beatOnce can also report a
			// transient miss for a reason OTHER than ENOENT (its heartbeat
			// write hitting ESTALE, for instance, precisely because this
			// generation was just evicted) and a successor can publish in
			// the gap before the follow-up read below runs -- that read
			// then succeeds, but returns the SUCCESSOR's id, not ours; a
			// different id on a successful read is exactly as much proof of
			// loss as ENOENT is, and must be upgraded the same way. A
			// genuinely transient error that is neither ENOENT nor a
			// mismatched id (EIO, an ESTALE that resolves on its own) stays
			// a transient miss, as documented above.
			res := beatOnce(lockPath, token)
			if res == beatTransientMiss {
				if tok, oerr := readOwnerToken(lockPath); os.IsNotExist(oerr) || (oerr == nil && tok != token) {
					res = beatDefiniteLoss
				}
			}
			if res == beatDefiniteLoss {
				slog.Warn("acquireFileLock: publish stalled past the publisher bound before the first heartbeat; lockPath no longer reflects this generation",
					"path", lockPath)
				select {
				case <-ctx.Done():
					return heldLock{}, fmt.Errorf("context cancelled while waiting for file lock %s: %w", lockPath, ctx.Err())
				case <-ticker.C:
				}
				continue
			}

			lockCtx, cancelLoss := context.WithCancel(ctx)
			stopHeartbeat := startLockHeartbeat(lockPath, token, cancelLoss)
			release := releaseFileLock(lockPath, token)
			return heldLock{
				release: func() error {
					stopHeartbeat()
					cancelLoss() // unregister from the parent ctx even on a normal release, not just on detected loss.
					return release()
				},
				ctx:        lockCtx,
				stillOwned: func() bool { return stillOwnsLockFile(lockPath, token) },
			}, nil
		}
		if errors.Is(err, errPublishPausedTooLong) {
			// Transient by nature (see errPublishPausedTooLong's doc):
			// nothing was published, so there is nothing to reclaim here —
			// just wait out one tick, the same as plain contention, and try
			// again within the normal wait budget instead of failing this
			// caller outright. Worth a log line regardless: it means this
			// node itself paused long enough to trip a bound, which an
			// operator will want visibility into even though this process
			// recovers on its own.
			slog.Warn("acquireFileLock: publisher pause guard tripped; retrying", "path", lockPath, "error", err)
			select {
			case <-ctx.Done():
				return heldLock{}, fmt.Errorf("context cancelled while waiting for file lock %s: %w", lockPath, ctx.Err())
			case <-ticker.C:
			}
			continue
		}
		if !os.IsExist(err) {
			return heldLock{}, fmt.Errorf("acquireFileLock: create %s: %w", lockPath, err)
		}

		if tryReclaimIfAbandoned(dir, lockPath) {
			continue // made real progress (evicted a dead lock) -- retry the publish immediately
		}

		select {
		case <-ctx.Done():
			return heldLock{}, fmt.Errorf("context cancelled while waiting for file lock %s: %w", lockPath, ctx.Err())
		case <-ticker.C:
		}
	}

	return heldLock{}, fmt.Errorf("failed to acquire file lock %s: deadline exceeded after ~%s",
		lockPath, wait)
}

// garbageCollectLockLitter best-effort removes this project's stage/evict
// entries (see tryCreateFileLock and tryReclaimIfAbandoned) once they are
// old enough that keeping them no longer matters — this IS a correctness
// boundary, not just tidiness: see acquireFileLock's correctness argument,
// "Why the garbage-collection cutoff is 2S", for the derivation of the
// cutoff below. Errors are swallowed: a failed
// sweep just leaves litter a little longer, which is always safe, only ever
// slower to tidy up.
//
// ".evict-*" entries are aged from provisionLockEvictedAtFile's mtime — the
// moment of eviction — never the entry's own mtime: rename(2) does not
// change the mtime of the directory it moves, so an evicted directory's own
// mtime is whatever it was before eviction (its creation time, or its first
// heartbeat, whichever wrote to it last) — for a lock that was held for a
// long time, that can already be far in the past the instant it is evicted.
// Ageing from the directory's own mtime would make GC sweep a long-held
// generation's evict entry on the very next call, immediately after
// eviction, breaking the invariant above well within the window a stale
// observer could still be acting on it. If the marker is missing (the write
// failed partway), the entry is left alone rather than guessed at —
// correctness here matters more than reclaiming the space promptly.
//
// Other litter (currently only "<provisionFileLockName>.stage-*" staging
// directories left behind by a tryCreateFileLock that crashed mid-sequence)
// is aged from its own mtime, which IS accurate for it: nothing ever renames
// an existing, older directory into a staging name, so a staging entry's
// mtime is genuinely its creation time.
//
// Known gap: a project that reaches its sentinel and then stays on the
// SharedPlain pre-lock fast path (see ProvisionShared) never calls this
// again, so litter from that project's initial contended provisioning is
// not revisited. Accepted: it does not accumulate further once contention
// stops, and it is already git-excluded.
func garbageCollectLockLitter(dir string) {
	now, err := serverNow(dir)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := 2 * provisionLockStaleAfter
	prefix := provisionFileLockName + "."
	for _, e := range entries {
		name := e.Name()
		if name == provisionFileLockName || !strings.HasPrefix(name, prefix) {
			continue // the live lock name itself, or not one of ours
		}
		path := filepath.Join(dir, name)
		if strings.Contains(name, ".evict-") {
			info, err := os.Stat(filepath.Join(path, provisionLockEvictedAtFile))
			if err != nil {
				continue // no confirmed eviction time yet; never guess (see doc)
			}
			if now.Sub(info.ModTime()) > cutoff {
				_ = os.RemoveAll(path)
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > cutoff {
			_ = os.RemoveAll(path)
		}
	}
}

// tryCreateFileLock atomically publishes a fully-formed, already-owned lock
// directory at lockPath and returns the owner id identifying this one
// acquisition. The returned error satisfies os.IsExist when another holder
// currently owns the lock (or, for a non-empty stray directory already
// sitting at lockPath outside this mechanism, a similar "occupied" error).
//
// The lock directory is built in a temporary staging location — with its
// owner marker already written inside it — and only then moved to lockPath
// via a single atomic rename, rather than creating an empty directory at
// lockPath first and writing the owner marker into it as a second step.
// This closes a real window a two-step version would have: a concurrent
// tryReclaimIfAbandoned or release elsewhere doesn't care whether the
// occupant it finds is fully formed — it would happily act on a directory
// that was Mkdir'd a microsecond ago and hasn't gotten its owner file yet.
// Publishing the complete directory in one atomic step means no other
// caller — reclaim, release, or another concurrent create — ever observes
// lockPath in a partially-formed state at all: from their perspective it
// goes directly from "absent" to "a fully owned lock", never through
// "present but ownerless".
//
// Acquisition either fully succeeds or leaves nothing behind: any failure
// along the way removes the staging directory before returning — a stray
// "<provisionFileLockName>.stage-*" directory left behind by a mid-sequence
// crash is inert litter, never mistaken for the lock itself (it's never at
// lockPath), and doesn't block anything; garbageCollectLockLitter sweeps it
// once old enough.
//
// The staging directory's name deliberately starts with provisionFileLockName
// (not some unrelated prefix): gitCloneWorkspace's pre-clone workspace
// check (unexpectedWorkspaceEntries) accepts anything under that prefix
// because the live lock and its evicted/staging siblings sit next to the
// clone — a staging directory with a different prefix would make a
// concurrently-running caller's clone refuse the workspace. Sharing the
// prefix closes that gap for free. provisionLockStagingPattern is the single
// source of truth for that prefix, shared with tests that need to construct
// or recognize a staging directory the same way this function does.
func tryCreateFileLock(lockPath string) (string, error) {
	start := timeNow()
	token := newFileLockToken()
	tmpDir, err := os.MkdirTemp(filepath.Dir(lockPath), provisionLockStagingPattern)
	if err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}
	if err := shareLockDirWithGroup(tmpDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", fmt.Errorf("set staging dir mode: %w", err)
	}
	if err := writeLockFile(filepath.Join(tmpDir, provisionLockOwnerFile), []byte(token), provisionLockFileMode); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", fmt.Errorf("write owner marker: %w", err)
	}

	// Discard rather than publish if building this staging directory itself
	// already took too long: rename(2) does not refresh a directory's own
	// mtime, and the caller's own synchronous first beat (see
	// acquireFileLock) is the only thing standing between this publish and
	// looking stale. This is the publisher-side counterpart to
	// tryReclaimIfAbandoned's own elapsed-time guard — see the
	// "Per-participant bounds" section of acquireFileLock's correctness
	// argument for the bound this stays under and the margin it leaves for
	// the publish rename RPC itself, which this check cannot see.
	// errPublishPausedTooLong lets the caller retry this as ordinary
	// contention instead of failing outright — see its doc.
	if elapsed := timeNow().Sub(start); elapsed > provisionLockStaleAfter/4 {
		_ = os.RemoveAll(tmpDir)
		return "", fmt.Errorf("%w: %s pause building the staging directory", errPublishPausedTooLong, elapsed)
	}

	if err := renameFile(tmpDir, lockPath); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", err
	}
	return token, nil
}

// errPublishPausedTooLong is returned by tryCreateFileLock when its own
// elapsed-time guard trips (see there). It is a distinct sentinel, not an
// os.IsExist-style filesystem error, specifically so acquireFileLock can
// tell "this attempt paused too long to safely publish" apart from "lockPath
// is genuinely occupied by someone else" and retry the former within the
// normal wait budget instead of failing the caller outright: the condition
// this guard catches (a GC pause, CPU starvation, or a local NFS stall) is
// transient by nature, and the whole point of catching it is to avoid
// publishing a lock that starts out looking stale — not to turn a passing
// local hiccup into a hard failure for whatever is waiting on this
// acquisition.
var errPublishPausedTooLong = errors.New("tryCreateFileLock: paused too long before publish")

// newFileLockToken returns a random identifier unique to one lock
// acquisition attempt, used as that acquisition's owner id: 32 lowercase
// hex characters (see isHexLockToken, which readOwnerToken's callers use to
// recognize this exact shape). Its eventual eviction destination (see
// tryReclaimIfAbandoned and releaseFileLock) is derived deterministically
// from this id, not a further fresh random name per removal attempt —
// that determinism is what makes eviction identity-bound.
func newFileLockToken() string {
	var b [16]byte
	// crypto/rand.Read is documented to never return an error as of Go 1.24
	// (it terminates the program instead, on an unrecoverable entropy-source
	// failure), so its error is intentionally ignored rather than given a
	// fallback path: a fallback id would in any case be unacceptable here
	// — the correctness argument above requires T to be unique per
	// occupancy, and a timestamp-derived id has none of the collision
	// resistance a real acquisition's identity depends on.
	_, _ = cryptorand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// isHexLockToken reports whether s has the exact shape newFileLockToken
// produces: 32 lowercase hex characters. readOwnerToken's callers use this
// to decide whether an owner file's content is trustworthy enough to use
// verbatim as part of a filesystem path. Owner files are written only by
// tryCreateFileLock, so in normal operation this is always true for a
// non-empty owner file — but a stray or truncated file, or content from an
// incompatible version, is still only ever READ by this code, never
// validated at write time by whatever put it there. Using such content
// verbatim as a path segment is itself a hazard: a value containing "/"
// would turn the intended sibling path lockPath+".evict-"+content into a
// path inside a different, likely non-existent, directory, so the rename
// that is supposed to evict it would fail with ENOENT forever and the lock
// could never be reclaimed. Content that doesn't pass this check is treated
// the same as a missing owner file — see markerlessOwnerToken.
func isHexLockToken(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// evictPathFor returns the deterministic destination a removal of the
// generation identified by `token` from lockPath must target — see
// acquireFileLock's correctness argument for why this must be a function of
// the owner id alone, never a fresh name per removal attempt.
func evictPathFor(lockPath, token string) string {
	return lockPath + ".evict-" + token
}

// markerlessOwnerToken stands in for the owner id of a lock directory
// that exists but has no readable, valid owner id at all. A lock
// published by tryCreateFileLock always already has a complete, valid owner
// file — the staging directory is fully built, owner file included, before
// the one publish rename — so this is not a state normal operation produces;
// it covers a directory placed at lockPath some other way (a foreign tool,
// an operator, or a test fixture), with no owner file at all, or with an
// owner file whose content fails isHexLockToken (a stray write, truncation,
// or content from an incompatible version). It lets such a directory still
// be evicted through the same deterministic destination-naming scheme as a
// normal owner id, rather than being permanently stuck because there is no
// real id to name a destination after.
//
// Every markerless generation shares this one literal value, so two
// DIFFERENT markerless generations occurring at the same lockPath (an
// already rare, degenerate case) would collide on the same destination name
// if used verbatim — not unsafely (the rename simply fails, the same as any
// other destination collision), but the second one would be stuck
// unreclaimable until the first's evict entry is garbage-collected.
// markerlessEvictSuffix adds a disambiguator for this reason.
const markerlessOwnerToken = "(none)"

// markerlessEvictSuffix returns the evictPathFor suffix to use for a
// markerless generation at lockPath: markerlessOwnerToken plus the lock
// directory's own mtime, read non-destructively at the same moment as the
// staleness judgment that is about to authorize its removal. This does not
// need to be globally unique the way a real owner id is — it only needs to
// usually differ between two markerless generations that are never actually
// live at the same time (one must have been evicted before the other is
// ever published), which the creation-time mtime does in practice. If the
// stat fails, it falls back to the plain, non-disambiguated value — the
// same single-shared-name behavior as before: never unsafe (a collision
// just makes the second generation's rename fail with EEXIST, the same as
// any other destination collision), but a second, later markerless
// generation that collides with an EARLIER one's still-present evict entry
// stays stuck — unable to be reclaimed itself — until that first entry ages
// out (2x provisionLockStaleAfter after ITS OWN eviction, not the second
// generation's), or indefinitely if that first entry's eviction marker was
// never successfully written at all (see provisionLockEvictedAtFile's doc on
// garbageCollectLockLitter never guessing at an unconfirmed eviction time).
func markerlessEvictSuffix(lockPath string) string {
	info, err := os.Stat(lockPath)
	if err != nil {
		return markerlessOwnerToken
	}
	return markerlessOwnerToken + "-" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

// readOwnerToken reads lockPath's current owner id without modifying
// anything.
func readOwnerToken(lockPath string) (string, error) {
	b, err := os.ReadFile(filepath.Join(lockPath, provisionLockOwnerFile))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// stillOwnsLockFile reports whether `token` is still the recorded owner of
// lockPath, via a fresh synchronous read (not any previously cached state).
func stillOwnsLockFile(lockPath, token string) bool {
	current, err := os.ReadFile(filepath.Join(lockPath, provisionLockOwnerFile))
	return err == nil && string(current) == token
}

// serverNow returns a timestamp assigned by the NFS export server itself —
// never this process's own clock — by writing a small probe file into dir
// and reading back its resulting mtime.
//
// This is what makes staleness comparisons safe across nodes with no
// assumption that their wall clocks agree: an ordinary content write
// (os.WriteFile, or any other operation that implicitly bumps mtime as a
// side effect — create, write, rename) is timestamped by the SERVER as it
// processes the request. This is categorically different from
// os.Chtimes(path, explicitTime, explicitTime): that sends NFSv3 SETATTR
// with SET_TO_CLIENT_TIME, embedding whichever node called it as the new
// mtime verbatim — which would make a waiter's time.Since(lockMtime)
// actually mean "holder's clock vs. waiter's clock", not "how long has this
// been stale by any single shared reference".
//
// Two timestamps obtained this way — the lock's own heartbeat marker
// (lastHeartbeat) and a probe created right now by whichever node is
// judging staleness — are both assigned by the same server clock, so their
// difference is meaningful regardless of which nodes produced them or how
// badly their own clocks disagree. The only residual assumption is that the
// server's mtime attribute reaches every node within its NFS attribute
// cache window (acregmax/acdirmax, both default up to ~60s) — see
// provisionLockStaleAfter's doc.
func serverNow(dir string) (time.Time, error) {
	// Each call uses its own name, under the shared prefix: two concurrent
	// callers sharing one fixed probe name could otherwise have one caller's
	// remove land between another's write and its own stat, failing that
	// stat with ENOENT for a reason unrelated to actual staleness — see
	// provisionClockProbeFile's doc.
	probePath := filepath.Join(dir, provisionClockProbeFile+"-"+newFileLockToken())
	// Clean up on every path out of this function, not only the success
	// path: a short write (ENOSPC, EDQUOT) can still create the file before
	// failing, and a stat error leaves whatever the write produced in
	// place. Either way the file is harmless, inert litter under the lock's
	// own prefix regardless — garbageCollectLockLitter would sweep it like
	// any other stray entry after its own cutoff — but there is no reason
	// to wait that long when an immediate best-effort remove is free.
	defer func() { _ = os.Remove(probePath) }()
	if err := writeLockFile(probePath, []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0600); err != nil {
		return time.Time{}, fmt.Errorf("serverNow: write probe %s: %w", probePath, err)
	}
	info, err := os.Stat(probePath)
	if err != nil {
		return time.Time{}, fmt.Errorf("serverNow: stat probe %s: %w", probePath, err)
	}
	return info.ModTime(), nil
}

// lastHeartbeat returns the most recent server-assigned timestamp available
// for the lock at lockPath: its heartbeat marker's mtime if one has ever
// been written, or the lock directory's own mtime (set at staging-directory
// creation, unchanged by the publish rename) otherwise — see
// provisionLockHeartbeatFile's doc for when that fallback
// applies.
//
// The fallback is taken ONLY when the heartbeat file does not exist — never
// for any other stat error (EIO/ETIMEDOUT on a soft or interruptible mount,
// EACCES, ESTALE after a server failover). Falling back on any error would
// make a holder that has been beating normally, but whose heartbeat file
// happens to be momentarily unreadable for an unrelated reason, look
// abandoned by the directory's old (first-beat) mtime instead — exactly a
// false positive against a live holder, not the dead-holder case this
// mechanism exists to detect. Any other error is returned as-is;
// lockLooksAbandoned already treats "can't establish a timestamp" as "don't
// guess, assume not abandoned".
func lastHeartbeat(lockPath string) (time.Time, error) {
	info, err := os.Stat(filepath.Join(lockPath, provisionLockHeartbeatFile))
	switch {
	case err == nil:
		return info.ModTime(), nil
	case os.IsNotExist(err):
		// No heartbeat has ever been written for this generation yet.
	default:
		return time.Time{}, err
	}
	info, err = os.Stat(lockPath)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// startLockHeartbeat refreshes lockPath's heartbeat marker every
// provisionLockHeartbeatInterval for as long as the returned stop func has
// not been called, so a live holder's lock never looks abandoned to a
// waiter's staleness check no matter how long the actual provisioning work
// (a large clone, a slow chown -R over NFS) takes.
//
// Every beat also re-verifies that `token` is still the recorded owner: a
// holder must detect losing its own lock, not assume it holds it forever
// once acquired — it can lose it if this process paused long enough, an
// NFS hang outlasted the stale window, or the
// heartbeat write itself kept failing (EACCES, ESTALE). After
// provisionHeartbeatMissTolerance consecutive misses (a failed write, a
// missing or mismatched owner marker), it logs at Warn and cancels
// cancelLoss (so the ctx returned to the caller's provisioning work is
// cancelled, killing any in-flight git/chown subprocess rather than letting
// it run to completion under a lock this holder no longer has). release
// does not need a separate signal from this goroutine: it re-reads current
// ownership itself before ever renaming anything (see its doc), which is
// both necessary (this goroutine's last beat could always be one tick
// behind an actual loss) and sufficient on its own.
func startLockHeartbeat(lockPath, token string, cancelLoss context.CancelFunc) (stop func()) {
	// Snapshot synchronously, in the calling goroutine, before spawning:
	// see the stop() doc below for why the goroutine must never read a
	// package-level var directly once running.
	interval := provisionLockHeartbeatInterval
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		misses := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				switch beatOnce(lockPath, token) {
				case beatOK:
					misses = 0
				case beatDefiniteLoss:
					// An owner id other than ours is on record: a successor
					// already exists. This is proof, not a guess, so it is
					// never tolerated the way a transient miss is.
					slog.Warn("acquireFileLock: heartbeat found a different owner on record; this holder has definitely lost the lock",
						"path", lockPath)
					cancelLoss()
					return
				case beatTransientMiss:
					misses++
					slog.Warn("acquireFileLock: heartbeat failed to confirm ownership",
						"path", lockPath, "consecutive_misses", misses)
					if misses >= provisionHeartbeatMissTolerance {
						slog.Warn("acquireFileLock: heartbeat declares this holder has lost the lock after repeated failures; cancelling in-flight provisioning work",
							"path", lockPath)
						cancelLoss()
						return
					}
				}
			}
		}
	}()
	var once sync.Once
	// stop closes done AND waits for the goroutine to actually observe it
	// and return (via exited), rather than merely signalling and hoping.
	// Closing done wakes a goroutine blocked in the select immediately
	// (select reacts to done becoming ready, not to ticker.C's remaining
	// time), so the only possible delay is however long one in-flight
	// beatOnce call takes to finish (fast local/NFS I/O) — not a full
	// heartbeat interval. Waiting here is what makes it safe for beatOnce
	// to read package-level vars like writeLockFile at all: without it, a
	// test could reassign such a var for its own scenario while a PRIOR
	// test's heartbeat goroutine was still alive (release() had returned,
	// but the goroutine simply hadn't been scheduled yet to notice done),
	// concurrently with that reassignment — exactly the kind of data
	// condition go test -race is built to catch.
	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}

// beatResult classifies one heartbeat attempt.
type beatResult int

const (
	// beatOK: the owner marker still matches `token`, and the heartbeat
	// marker was refreshed.
	beatOK beatResult = iota
	// beatTransientMiss: something didn't work (a write failed, or the
	// owner marker was briefly unreadable) but does not itself prove loss
	// — tolerated up to provisionHeartbeatMissTolerance times.
	beatTransientMiss
	// beatDefiniteLoss: the owner marker exists and names a DIFFERENT id.
	// This proves a successor already holds the lock; it is never
	// tolerated, regardless of the miss counter.
	beatDefiniteLoss
)

// beatOnce checks ownership FIRST, and only writes the heartbeat marker if
// the check passes — never the other order — because writing unconditionally
// and checking ownership only afterward would let a holder that has already
// lost the lock keep refreshing its SUCCESSOR's heartbeat marker for as long
// as it keeps running (up to provisionHeartbeatMissTolerance beats, since
// the write would always "succeed" and only a post-write check could catch
// the mismatch — by which point the damage, a freshened successor marker,
// is already done). Checking first closes that for every beat except one
// narrow window: if this holder's own generation is evicted and a successor
// publishes AFTER the pre-write check passes but BEFORE the write lands, the
// write still goes into the successor's now-current lockPath, once. The
// write-then-reread below still detects this immediately afterward and
// reports beatDefiniteLoss, so the effect is bounded to a single stray write
// per loss event, never a lost holder that keeps refreshing a successor's
// marker beat after beat.
//
// After a successful write, ownership is verified again, in case it changed
// while the write was in flight — this is what catches the one-write window
// described above.
func beatOnce(lockPath, token string) beatResult {
	current, err := os.ReadFile(filepath.Join(lockPath, provisionLockOwnerFile))
	switch {
	case err == nil:
		if string(current) != token {
			return beatDefiniteLoss
		}
	case os.IsNotExist(err):
		return beatTransientMiss // owner marker itself is gone -- could be mid-eviction; tolerate briefly
	default:
		return beatTransientMiss // I/O error reading it -- tolerate briefly
	}

	if err := writeLockFile(filepath.Join(lockPath, provisionLockHeartbeatFile), []byte(token), provisionLockFileMode); err != nil {
		return beatTransientMiss
	}

	current, err = os.ReadFile(filepath.Join(lockPath, provisionLockOwnerFile))
	if err != nil {
		return beatTransientMiss
	}
	if string(current) != token {
		return beatDefiniteLoss
	}
	return beatOK
}

// releaseFileLock returns the release function for a lock this process
// acquired with the given owner id. It re-reads lockPath's CURRENT owner
// synchronously, right before ever renaming anything, and does nothing
// further if it no longer matches — this is the ONLY ownership guard
// release needs. Without it, a holder that had already been silently
// superseded would rename WHATEVER currently occupies lockPath — a live
// successor's directory, if one has since been published — to this
// generation's own deterministic destination, mislabeling it and vacating
// lockPath out from under that successor.
//
// release does not consult the heartbeat's own loss signal: a transient-miss
// loss can coexist with this holder still genuinely owning lockPath (a
// write that failed for an unrelated reason, not an actual owner-id
// mismatch), and short-circuiting on it would make release do nothing at
// all in that case — orphaning this holder's own, still-valid lock until it
// eventually goes stale and some other node reclaims it, rather than
// releasing it immediately as every other code path assumes release does.
// The ownership re-check above is both necessary and sufficient on its own.
//
// Once that check passes, release removes this generation from lockPath the
// same way tryReclaimIfAbandoned does: a single rename to the DETERMINISTIC
// destination evictPathFor(lockPath, token) — never a fresh name per call.
// See acquireFileLock's correctness argument, "The invariant this depends
// on", for why a rename that fails always means "already gone or already
// being removed by someone else" — it never means this call touched a
// successor — and "Per-participant bounds" for the bound the window between
// the ownership check above and this rename must stay under, enforced the
// same way the reclaimer's identical window is, by the elapsed-time guard
// below.
func releaseFileLock(lockPath, token string) func() error {
	return func() error {
		start := timeNow()
		if !stillOwnsLockFile(lockPath, token) {
			// Already superseded: nothing here belongs to us any more, so
			// there is nothing to release.
			return nil
		}

		// Abort rather than rename if this call has itself already taken
		// too long since the check above — see tryReclaimIfAbandoned's
		// identical guard for why this specific pause is otherwise
		// unbounded. Skipping the rename here is always safe: the lock
		// simply goes stale and is reclaimed through the normal path. It is
		// not silent, though: until that happens, this generation stays
		// owned and non-heartbeating, blocking every peer for up to a full
		// stale window — worth an operator's attention.
		if elapsed := timeNow().Sub(start); elapsed > provisionLockStaleAfter {
			slog.Warn("releaseFileLock: aborting release after a long pause between the ownership check and the rename; the lock will be reclaimed once it goes stale",
				"path", lockPath, "elapsed", elapsed)
			return nil
		}

		evictPath := evictPathFor(lockPath, token)
		if err := renameFile(lockPath, evictPath); err != nil {
			if os.IsNotExist(err) || os.IsExist(err) {
				// ENOENT: lockPath no longer holds this generation (a
				// reclaimer already evicted it, or it was already
				// released). EEXIST/ENOTEMPTY: a reclaimer already moved
				// this exact generation to this exact destination before we
				// got here. Either way, our rename never took the source,
				// so it never touched whatever (if anything) occupies
				// lockPath now.
				return nil
			}
			return fmt.Errorf("releaseFileLock: %w", err)
		}
		// Moved successfully; leave it for garbageCollectLockLitter rather
		// than deleting it immediately. Record the eviction instant via an
		// ordinary (server-timestamped) content write — see
		// provisionLockEvictedAtFile's doc for why GC needs this rather than
		// evictPath's own mtime. Best-effort: if this write fails,
		// garbageCollectLockLitter simply leaves this entry alone rather
		// than guessing at its age.
		_ = writeLockFile(filepath.Join(evictPath, provisionLockEvictedAtFile), []byte{}, provisionLockFileMode)
		return nil
	}
}

// ownerLooksMarkerless reports whether path's owner file is absent, or
// present but not a valid owner id (see isHexLockToken) — the two situations
// tryReclaimIfAbandoned treats identically as "no real id to work with".
func ownerLooksMarkerless(path string) bool {
	raw, err := readOwnerToken(path)
	if err != nil {
		return os.IsNotExist(err)
	}
	return !isHexLockToken(raw)
}

// tryReclaimIfAbandoned reads the CURRENT owner id non-destructively,
// judges staleness on lockPath itself (no rename yet), and only if it looks
// abandoned, removes that exact generation the same way releaseFileLock
// does: a single rename to the deterministic destination
// evictPathFor(lockPath, id). Returns true only when it actually evicted an
// abandoned lock.
//
// This never claims speculatively: it reads the owner id BEFORE doing
// anything destructive, and its rename targets that SPECIFIC id's
// deterministic destination — so even if lockPath has already been replaced
// by a different generation by the time the rename runs, the rename either
// fails outright (source no longer matches by the time the syscall executes
// against a destination some other, earlier remover of THIS id has already
// taken), or it succeeds because lockPath still held that exact id OR
// because a live successor happened to be renamed in error (see the
// defensive check inside, and acquireFileLock's correctness argument for
// the invariant that keeps this case rare — bounded by
// garbageCollectLockLitter correctly preserving ".evict-<id>" for the full
// detection window, not "effectively unreachable"). The defensive check
// inside is what turns "rename succeeded" into "safe to treat as this
// eviction", rather than the rename's own semantics alone.
func tryReclaimIfAbandoned(dir, lockPath string) bool {
	// Monotonic: bounds how long THIS attempt may spend between its
	// non-destructive read and its rename — see the elapsed-time check
	// below and acquireFileLock's correctness argument for why an
	// unbounded reclaimer pause is its own, separate way to break the
	// ".evict-T must exist" invariant, independent of a holder's own pause.
	start := timeNow()

	rawToken, err := readOwnerToken(lockPath)
	var matchToken, destSuffix string
	switch {
	case err == nil && isHexLockToken(rawToken):
		matchToken = rawToken
		destSuffix = rawToken
	case err == nil:
		// Content is present but does not look like a real owner id (a stray
		// write, truncation, or incompatible-version content — see
		// isHexLockToken's doc for why such content is never used verbatim
		// as a path segment). Treat this degenerate state as its own
		// pseudo-generation so it can still be reclaimed once it looks old
		// enough, going through the same deterministic-eviction path as a
		// normal owner id. See markerlessOwnerToken's doc.
		matchToken = markerlessOwnerToken
		destSuffix = markerlessEvictSuffix(lockPath)
	case os.IsNotExist(err):
		// ENOENT here covers two different situations: the owner file is
		// missing (a markerless lock directory IS still there), or
		// lockPath itself is gone. Only the first is a markerless
		// generation to reclaim — the second has nothing to reclaim at
		// all, and treating it as markerless anyway would compute the
		// SHARED fallback suffix from nothing, so a generation published
		// moments later (by the time the staleness check below runs) would
		// be evicted under a destination bound to no real generation.
		if _, statErr := os.Lstat(lockPath); statErr != nil {
			return false // nothing at lockPath to reclaim
		}
		matchToken = markerlessOwnerToken
		destSuffix = markerlessEvictSuffix(lockPath)
	default:
		return false // unreadable for some other reason; do not guess
	}
	if !lockLooksAbandoned(dir, lockPath) {
		return false
	}

	// Abort rather than rename if this attempt itself has already taken too
	// long: a reclaimer that stalls between its read above and here — a GC
	// pause, CPU starvation, or an NFS hang on this node specifically,
	// while lockPath's actual occupant and the server are both healthy —
	// is not bounded by the holder's own heartbeat assumption at all, and
	// garbageCollectLockLitter cannot distinguish "still legitimately
	// needed" from "this reclaimer gave up ages ago" once this much time
	// has passed. This does not make the window provably zero (the rename
	// RPC itself could still stall), but it closes the common case cheaply.
	// See acquireFileLock's correctness argument ("Per-participant bounds")
	// for the quantitative bound this is meant to stay well under.
	if elapsed := timeNow().Sub(start); elapsed > provisionLockStaleAfter {
		slog.Warn("tryReclaimIfAbandoned: aborting reclaim after a long pause between the staleness read and the rename",
			"path", lockPath, "elapsed", elapsed)
		return false
	}

	evictPath := evictPathFor(lockPath, destSuffix)
	if err := renameFile(lockPath, evictPath); err != nil {
		// lockPath no longer holds this id (someone else already removed
		// it, to this same destination if they also read this id, or to a
		// different one if it's already a different generation), or this
		// destination is already taken by an earlier removal of the same
		// id. Either way, this rename never touched whatever currently
		// occupies lockPath.
		return false
	}

	// Defensive: rename(2) does not inspect what it moves — see
	// acquireFileLock's correctness argument for the invariant this relies
	// on, and for the narrow window (a live successor published between the
	// staleness read above and this rename) where that invariant can still
	// be violated if ".evict-<destSuffix>" had already been vacated early.
	// If the thing just moved does not actually carry the id being evicted,
	// it is not safe to treat it as this eviction — leave it exactly where
	// it landed, for garbage collection, rather than guessing: never
	// restore it (lockPath may since have been reclaimed by someone else
	// for a reason that no longer applies to what we are holding), and
	// never delete it (it may be a live generation that was moved here by
	// mistake).
	var matches bool
	if matchToken == markerlessOwnerToken {
		matches = ownerLooksMarkerless(evictPath)
	} else {
		current, cerr := readOwnerToken(evictPath)
		matches = cerr == nil && current == matchToken
	}
	if !matches {
		slog.Warn("tryReclaimIfAbandoned: claimed directory's owner did not match the id being evicted; leaving it for garbage collection",
			"path", lockPath, "evicted_to", evictPath)
		return false
	}

	// Record the eviction instant via an ordinary (server-timestamped)
	// content write — see provisionLockEvictedAtFile's doc for why GC needs
	// this rather than evictPath's own mtime. Best-effort: if this write
	// fails, garbageCollectLockLitter simply leaves this entry alone rather
	// than guessing at its age.
	_ = writeLockFile(filepath.Join(evictPath, provisionLockEvictedAtFile), []byte{}, provisionLockFileMode)

	slog.Warn("acquireFileLock: reclaimed apparently-abandoned lock", "path", lockPath, "owner", matchToken)
	// Leave evictPath for garbageCollectLockLitter rather than deleting it
	// immediately — keep evicted generations around for a while so a stale
	// observer's own removal attempt still has something to collide with
	// (see garbageCollectLockLitter's doc).
	return true
}

// lockLooksAbandoned judges whether the lock at path has gone stale, by
// comparing two NFS-server-assigned timestamps: a probe created right now
// in dir (serverNow) against the lock's own last heartbeat (lastHeartbeat).
// See serverNow's doc for why this — and never this process's own clock —
// is the only comparison that is safe across nodes with no assumption that
// their wall clocks agree with each other or with the server.
func lockLooksAbandoned(dir, path string) bool {
	now, err := serverNow(dir)
	if err != nil {
		return false // can't establish a reference time; don't guess
	}
	last, err := lastHeartbeat(path)
	if err != nil {
		return false
	}
	return now.Sub(last) > provisionLockStaleAfter
}

// markedWorkspaceNeedsClone reports whether a shared-plain git workspace that
// already has its provisioning marker still needs its clone:
//   - it holds nothing but entries that may sit next to a clone
//     (ignorableWorkspaceEntry); a workspace provisioned before the
//     Kubernetes init container received the project's clone settings has
//     the marker and no repository; or
//   - an earlier clone into it finished but was interrupted while its
//     entries were being moved into place (a scratch directory holding a
//     clone manifest, and no .git in the workspace). gitCloneWorkspace then
//     finishes the move, or refuses with the entries named.
//
// A workspace with a .git, or with any other entry, is left as it is.
//
// This relies on every start taking the provisioning file lock before it
// looks at the workspace (no store.AdvisoryLocker; see acquireProvisionLock).
// A start that only waited for the marker would not wait for this clone.
func markedWorkspaceNeedsClone(in ProvisionInput) bool {
	if in.Mode != store.SharingModeSharedPlain || in.GitClone == nil || in.GitClone.URL == "" {
		return false
	}
	dest := in.Resolved.HostPath
	if _, err := os.ReadDir(dest); err != nil {
		return false
	}
	if _, err := os.Lstat(filepath.Join(dest, ".git")); err == nil {
		return false
	}
	if scratch, err := completedCloneScratch(dest); err != nil || scratch != "" {
		// An interrupted move (or more than one finished clone, which
		// gitCloneWorkspace reports): never treat it as provisioned.
		return true
	}
	other, err := nonIgnorableWorkspaceEntries(in, dest)
	return err == nil && len(other) == 0
}

// cloneIntoMarkedWorkspace clones into a marked shared-plain workspace that
// still needs its clone (markedWorkspaceNeedsClone) and chowns the result.
// The caller holds the provisioning lock and has re-checked the marker under
// it. The marker is already present, so nothing is written for it; a failed
// clone leaves the workspace as it was, and the next start tries again.
func cloneIntoMarkedWorkspace(ctx context.Context, in ProvisionInput, stillOwned func() bool, sentinelDir string) error {
	slog.Info("ProvisionShared: workspace has a provisioning marker but no clone, cloning",
		"project_id", in.ProjectID, "host_path", in.Resolved.HostPath)
	if err := gitCloneWorkspace(ctx, in, stillOwned); err != nil {
		return fmt.Errorf("ProvisionShared: git clone: %w", err)
	}
	// The marker may be the legacy one in the workspace root; now that
	// there is a .git, keep it out of git status.
	excludeLegacySentinel(in)
	uid, gid := resolveUID(in), resolveGID(in)
	if err := chownProjectTreeExcluding(ctx, in.Resolved.HostPath, []string{sentinelDir, legacyDir(in, sentinelDir)}, uid, gid); err != nil {
		if in.RequireChownSuccess {
			return fmt.Errorf("ProvisionShared: chown %s to %d:%d: %w", in.Resolved.HostPath, uid, gid, err)
		}
		slog.Warn("ProvisionShared: chown failed (non-fatal, may lack privileges)",
			"project_id", in.ProjectID, "path", in.Resolved.HostPath, "uid", uid, "gid", gid, "error", err)
	}
	return nil
}

// cloneTempDirPrefix names the scratch directory a clone attempt creates
// under the workspace. Nothing else creates entries with this prefix.
const cloneTempDirPrefix = ".scion-clone-"

// cloneManifestFile is written into a clone's scratch directory once the
// clone has succeeded, before any entry is moved into the workspace. It
// lists the clone's top-level entries, one per line. A scratch directory
// with a manifest holds a finished clone whose move may have been
// interrupted; one without a manifest is the remains of a clone that never
// finished, and is removed.
const cloneManifestFile = ".scion-clone-entries"

// sharedVolumesDirName is where in-workspace shared dirs are mounted
// (<workspace>/.scion-volumes/<name>).
const sharedVolumesDirName = ".scion-volumes"

// ignorableWorkspaceEntry reports whether e, an entry directly in the
// workspace dir, may sit next to a clone. These are Scion's own entries:
//   - the provisioning marker file (ProvisionSentinelFile);
//   - the provisioning file lock directory (provisionFileLockName) and its
//     siblings "<lock>.stage-*", "<lock>.evict-*" and "<lock>.clock-probe";
//   - clone scratch directories (cloneTempDirPrefix);
//   - .scion-volumes, when it is a directory whose children are all
//     directories that are either empty (mount points created for
//     in-workspace shared dirs) or the mount of one of in's shared dirs;
//   - an empty worktrees directory.
//
// Anything else is workspace content. Symlinks are never ignorable.
func ignorableWorkspaceEntry(in ProvisionInput, dir string, e os.DirEntry) (bool, error) {
	name := e.Name()
	isDir := e.Type()&os.ModeType == os.ModeDir
	switch {
	case name == ProvisionSentinelFile:
		return e.Type().IsRegular(), nil
	case name == provisionFileLockName || strings.HasPrefix(name, provisionFileLockName+"."):
		return true, nil
	case strings.HasPrefix(name, cloneTempDirPrefix):
		return isDir, nil
	case name == sharedVolumesDirName && isDir:
		return onlySharedDirMounts(in, filepath.Join(dir, name))
	case name == "worktrees" && isDir:
		nonEmpty, err := dirHasEntries(filepath.Join(dir, name))
		if err != nil {
			return false, fmt.Errorf("check %s: %w", filepath.Join(dir, name), err)
		}
		return !nonEmpty, nil
	default:
		return false, nil
	}
}

// onlySharedDirMounts reports whether every entry of volumesDir is a
// directory that is empty or is the mount of one of in's shared dirs.
func onlySharedDirMounts(in ProvisionInput, volumesDir string) (bool, error) {
	entries, err := os.ReadDir(volumesDir)
	if err != nil {
		return false, fmt.Errorf("read dir %s: %w", volumesDir, err)
	}
	mounts := map[string]bool{}
	for _, sd := range in.Resolved.SharedDirs {
		if sd.HostPath != "" {
			mounts[filepath.Clean(sd.HostPath)] = true
		}
	}
	for _, e := range entries {
		if e.Type()&os.ModeType != os.ModeDir {
			return false, nil
		}
		p := filepath.Join(volumesDir, e.Name())
		if mounts[filepath.Clean(p)] {
			continue
		}
		nonEmpty, err := dirHasEntries(p)
		if err != nil {
			return false, fmt.Errorf("check %s: %w", p, err)
		}
		if nonEmpty {
			return false, nil
		}
	}
	return true, nil
}

// nonIgnorableWorkspaceEntries returns the sorted names of the entries in
// dir that are workspace content (not ignorableWorkspaceEntry).
func nonIgnorableWorkspaceEntries(in ProvisionInput, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	var other []string
	for _, e := range entries {
		ok, err := ignorableWorkspaceEntry(in, dir, e)
		if err != nil {
			return nil, err
		}
		if !ok {
			other = append(other, e.Name())
		}
	}
	return other, nil
}

// gitCloneWorkspace clones in.GitClone into in.Resolved.HostPath. The caller
// holds the provisioning lock and has already checked the provisioning
// marker.
//
// The workspace is often a mount point (a k8s PVC subPath), so it cannot be
// swapped out as a whole. The clone runs in a fresh scratch directory under
// the workspace. Once it succeeds, a manifest of its entries is written into
// the scratch directory, and the entries are renamed into place with .git
// last, never replacing an existing entry (moveDirContentsUp). The
// workspace's own content is never cleared or overwritten:
//   - a .git already in the workspace means an earlier clone finished (.git
//     is always renamed last), so it is reused;
//   - a scratch directory with a manifest is an earlier clone whose move was
//     interrupted: its remaining entries are moved, provided every other
//     entry in the workspace came from it (resumeCloneMove);
//   - other scratch directories are left over from clones that never
//     finished, and are removed;
//   - any workspace content (nonIgnorableWorkspaceEntries) makes the clone
//     fail with those entries named, both before the clone and again just
//     before the move, since something may write to the workspace while a
//     long clone runs.
//
// stillOwned reports whether the caller still holds the provisioning lock;
// nil means not applicable (the store.AdvisoryLocker path).
func gitCloneWorkspace(ctx context.Context, in ProvisionInput, stillOwned func() bool) error {
	dest := in.Resolved.HostPath

	if _, statErr := os.Stat(filepath.Join(dest, ".git")); statErr == nil {
		slog.Warn("ProvisionShared: workspace already has a .git dir, reusing prior clone",
			"project_id", in.ProjectID, "path", dest)
		return nil
	}

	scratch, err := completedCloneScratch(dest)
	if err != nil {
		return err
	}
	if scratch != "" {
		if err := resumeCloneMove(in, scratch); err != nil {
			return err
		}
		excludeProvisioningFiles(in)
		return finishCloneScratch(scratch)
	}

	if err := removeCloneTempDirs(dest); err != nil {
		return err
	}
	if other, err := nonIgnorableWorkspaceEntries(in, dest); err != nil {
		return err
	} else if len(other) > 0 {
		return fmt.Errorf("workspace %s is not empty but has no provisioning marker and no .git (found %s); "+
			"refusing to clone into it or clear it: move those entries out of the workspace and start the agent again",
			dest, strings.Join(other, ", "))
	}

	tmpDir, err := os.MkdirTemp(dest, cloneTempDirPrefix+"*")
	if err != nil {
		return fmt.Errorf("create temp clone dir under %s: %w", dest, err)
	}
	tmpIn := in
	tmpIn.Resolved.HostPath = tmpDir
	if cloneErr := runGitClone(ctx, tmpIn); cloneErr != nil {
		_ = os.RemoveAll(tmpDir)
		return cloneErr
	}
	if err := writeCloneManifest(tmpDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return err
	}

	// Re-verify ownership immediately before the irreversible move into
	// dest: the clone above can take a long time, long enough for this
	// holder to have silently lost the lock to a reclaimer since the
	// heartbeat's last check.
	if stillOwned != nil && !stillOwned() {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("gitCloneWorkspace: lost the provisioning lock during clone; refusing to move cloned content into %s", dest)
	}

	// Check the workspace again: an agent already running in it (one
	// started before it was provisioned) may have written to it during the
	// clone.
	if other, err := nonIgnorableWorkspaceEntries(in, dest); err != nil {
		_ = os.RemoveAll(tmpDir)
		return err
	} else if len(other) > 0 {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("workspace %s changed during the clone (found %s); "+
			"leaving it as it is and discarding the clone", dest, strings.Join(other, ", "))
	}

	if err := moveDirContentsUp(tmpDir, dest); err != nil {
		// moveDirContentsUp has moved its entries back; drop the clone.
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("move cloned contents from %s to %s: %w", tmpDir, dest, err)
	}
	excludeProvisioningFiles(in)
	return finishCloneScratch(tmpDir)
}

// excludeProvisioningFiles keeps Scion's provisioning files in a freshly
// cloned workspace out of git's view: leftover clone scratch directories,
// and, when the provisioning file lock (or the legacy one) lives in the
// workspace itself (the k8s init container), the lock's on-disk footprint
// and the marker. Patterns are anchored with a leading "/", so an untracked
// file elsewhere that shares a prefix stays visible. Best effort.
func excludeProvisioningFiles(in ProvisionInput) {
	dest := in.Resolved.HostPath
	patterns := []string{"/" + cloneTempDirPrefix + "*"}
	sentinelDir := resolveSentinelDir(in)
	if in.Locker == nil && (sentinelDir == dest || legacyDir(in, sentinelDir) == dest) {
		patterns = append(patterns, "/"+provisionFileLockName, "/"+provisionFileLockName+".*")
	}
	if sentinelDir == dest {
		patterns = append(patterns, "/"+ProvisionSentinelFile)
	}
	for _, p := range patterns {
		if err := appendGitExclude(dest, p); err != nil {
			slog.Warn("gitCloneWorkspace: failed to add a git exclude for provisioning files (non-fatal)",
				"project_id", in.ProjectID, "path", dest, "pattern", p, "error", err)
			return
		}
	}
}

// writeCloneManifest writes cloneManifestFile into scratch, listing its
// entries. A repository whose top level has an entry with that name cannot
// be provisioned this way.
func writeCloneManifest(scratch string) error {
	entries, err := os.ReadDir(scratch)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", scratch, err)
	}
	var b strings.Builder
	for _, e := range entries {
		if e.Name() == cloneManifestFile {
			return fmt.Errorf("the repository has a top-level entry named %s, which workspace provisioning uses itself", cloneManifestFile)
		}
		b.WriteString(e.Name())
		b.WriteByte('\n')
	}
	return writeFileAtomic(filepath.Join(scratch, cloneManifestFile), []byte(b.String()), 0o644)
}

// readCloneManifest returns the entry names listed in scratch's manifest.
func readCloneManifest(scratch string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(scratch, cloneManifestFile))
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			names[line] = true
		}
	}
	return names, nil
}

// finishCloneScratch removes a scratch directory whose entries have all been
// moved: its manifest, then the directory itself.
func finishCloneScratch(scratch string) error {
	if err := os.Remove(filepath.Join(scratch, cloneManifestFile)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", filepath.Join(scratch, cloneManifestFile), err)
	}
	return os.Remove(scratch)
}

// completedCloneScratch returns the scratch directory in dir that holds a
// clone manifest, or "" if there is none. More than one is an error: the
// lock lets only one clone run at a time, and an earlier one is always
// finished (or refused) before a new one starts.
func completedCloneScratch(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read dir %s: %w", dir, err)
	}
	var found []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), cloneTempDirPrefix) || e.Type()&os.ModeType != os.ModeDir {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if fi, err := os.Lstat(filepath.Join(p, cloneManifestFile)); err == nil && fi.Mode().IsRegular() {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("workspace %s holds more than one interrupted clone (%s); refusing to pick one: remove them and start the agent again",
			dir, strings.Join(found, ", "))
	}
}

// resumeCloneMove finishes moving the entries of an interrupted clone in
// scratch into the workspace. Every workspace content entry must be one the
// clone listed in its manifest and no longer holds, i.e. one this clone
// moved; anything else makes it fail with those entries named, leaving the
// workspace and the scratch directory as they are. Nothing is replaced.
func resumeCloneMove(in ProvisionInput, scratch string) error {
	dest := in.Resolved.HostPath
	manifest, err := readCloneManifest(scratch)
	if err != nil {
		return fmt.Errorf("read clone manifest in %s: %w", scratch, err)
	}
	remaining := map[string]bool{}
	scratchEntries, err := os.ReadDir(scratch)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", scratch, err)
	}
	for _, e := range scratchEntries {
		if e.Name() != cloneManifestFile {
			remaining[e.Name()] = true
		}
	}
	other, err := nonIgnorableWorkspaceEntries(in, dest)
	if err != nil {
		return err
	}
	var foreign []string
	moved := map[string]bool{}
	for _, name := range other {
		if manifest[name] && !remaining[name] {
			moved[name] = true
			continue
		}
		foreign = append(foreign, name)
	}
	if len(foreign) > 0 {
		return fmt.Errorf("workspace %s holds an interrupted clone (%s) and entries that did not come from it (%s); "+
			"refusing to finish the clone: move those entries out of the workspace and start the agent again",
			dest, scratch, strings.Join(foreign, ", "))
	}
	var missing []string
	for name := range manifest {
		if !moved[name] && !remaining[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("workspace %s holds an interrupted clone (%s) that is missing %s; "+
			"refusing to finish it: remove the clone's entries from the workspace and %s, then start the agent again",
			dest, scratch, strings.Join(missing, ", "), scratch)
	}
	slog.Warn("ProvisionShared: finishing an interrupted clone",
		"project_id", in.ProjectID, "path", dest, "scratch", scratch, "remaining", len(remaining))
	if err := moveDirContentsUp(scratch, dest); err != nil {
		return fmt.Errorf("finish moving cloned contents from %s to %s: %w", scratch, dest, err)
	}
	return nil
}

// removeCloneTempDirs removes the scratch directories (cloneTempDirPrefix)
// that clones which never finished left in dir: those without a clone
// manifest. Only directories are removed; a regular file or symlink with
// that name is left in place and is then reported as workspace content.
func removeCloneTempDirs(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), cloneTempDirPrefix) || e.Type()&os.ModeType != os.ModeDir {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if _, err := os.Lstat(filepath.Join(p, cloneManifestFile)); err == nil {
			continue
		}
		slog.Warn("ProvisionShared: removing scratch directory left by an unfinished clone", "path", p)
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("remove leftover clone dir %s: %w", p, err)
		}
	}
	return nil
}

// runGitClone runs git clone into in.Resolved.HostPath, which must be empty
// or absent. The clone runs under ctx via exec.CommandContext so that a
// cancelled or timed-out context stops the git process. Interactive prompts
// are disabled. The returned error names the repository without credentials,
// query string or fragment, and says when the failure is a missing or
// rejected credential.
func runGitClone(ctx context.Context, in ProvisionInput) error {
	gc := in.GitClone
	args := []string{"clone"}

	// Depth: nil/omitted = shallow depth 1, 0 = full clone (no --depth), >0 = that depth.
	depth := 1
	if gc.Depth != nil {
		depth = *gc.Depth
	}
	if depth > 0 {
		args = append(args, "--depth", fmt.Sprintf("%d", depth))
	}
	if gc.Branch != "" {
		args = append(args, "--branch", gc.Branch)
	}
	args = append(args, gc.URL, in.Resolved.HostPath)

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	return cloneError(gc.URL, string(output), err)
}

// cloneError builds the error for a failed clone of rawURL. git's output is
// stripped of the URL's credentials before it is included.
func cloneError(rawURL, output string, runErr error) error {
	safeURL := redactCloneURL(rawURL)
	detail := strings.TrimSpace(sanitizeCloneOutput(output, rawURL))
	if detail == "" && runErr != nil {
		detail = runErr.Error()
	}
	switch util.ClassifyGitError(detail).Kind {
	case util.GitErrAuth:
		return fmt.Errorf("git clone %s: the repository needs credentials, and workspace provisioning has none "+
			"(it clones without a git token, so a private repository cannot be cloned here): %s", safeURL, detail)
	case util.GitErrNotFound:
		return fmt.Errorf("git clone %s: repository or branch not found "+
			"(a private repository also reports this when no credentials are given): %s", safeURL, detail)
	default:
		return fmt.Errorf("git clone %s: %s", safeURL, detail)
	}
}

// redactCloneURL returns rawURL without userinfo, query string or fragment,
// any of which can carry a token. For an scp-style URL ("user@host:path")
// the user is dropped and host and path are kept. An unparseable URL is
// never echoed.
func redactCloneURL(rawURL string) string {
	if !strings.Contains(rawURL, "://") {
		// scp-style "user@host:path", or a local path: keep host and path,
		// drop the user.
		if at := strings.Index(rawURL, "@"); at >= 0 && strings.Contains(rawURL[at:], ":") {
			return rawURL[at+1:]
		}
		if !strings.Contains(rawURL, "@") {
			return rawURL
		}
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<unparseable clone URL>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// sanitizeCloneOutput removes rawURL's credentials, query string and
// fragment from text (git's own output can echo the URL).
func sanitizeCloneOutput(text, rawURL string) string {
	if rawURL == "" || text == "" {
		return text
	}
	out := strings.ReplaceAll(text, rawURL, redactCloneURL(rawURL))
	u, err := url.Parse(rawURL)
	if err != nil {
		return out
	}
	if u.User != nil && u.User.String() != "" {
		out = strings.ReplaceAll(out, u.User.String()+"@", "")
	}
	if u.RawQuery != "" {
		out = strings.ReplaceAll(out, "?"+u.RawQuery, "")
	}
	if u.Fragment != "" {
		out = strings.ReplaceAll(out, "#"+u.Fragment, "")
	}
	return out
}

// moveDirContentsUp moves every entry from src into dest, leaving src with
// only its clone manifest (cloneManifestFile, which is never moved) for the
// caller to remove. src must be a subdirectory of dest (it is always
// created under it via os.MkdirTemp), so every move stays on the same
// filesystem and is a plain rename.
//
// No entry in dest is ever replaced: each move goes through moveRenameFile
// (renameNoReplace by default), and a name that already exists in dest makes
// the move fail.
//
// Any ".git" entry is moved LAST, after every other entry has already
// landed in dest. If a move fails partway through, the entries this call
// already moved are moved back into src, so dest is left as it was and src
// still holds the whole clone. A process killed partway through leaves
// working-tree files but no .git in dest, never the reverse, so the
// ".git present" reuse check can never mistake it for a finished clone; the
// manifest left in src lets the next start finish the move
// (resumeCloneMove).
func moveDirContentsUp(src, dest string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}

	var moved []string
	rollback := func() {
		for i := len(moved) - 1; i >= 0; i-- {
			name := moved[i]
			if err := renameNoReplace(filepath.Join(dest, name), filepath.Join(src, name)); err != nil {
				slog.Warn("moveDirContentsUp: could not move an entry back after a failed move",
					"src", src, "dest", dest, "name", name, "error", err)
			}
		}
	}

	move := func(name string) error {
		if err := moveRenameFile(filepath.Join(src, name), filepath.Join(dest, name)); err != nil {
			return err
		}
		moved = append(moved, name)
		return nil
	}

	var gitEntryName string
	for _, e := range entries {
		switch e.Name() {
		case cloneManifestFile:
			continue
		case ".git":
			gitEntryName = e.Name()
			continue
		}
		if err := move(e.Name()); err != nil {
			rollback()
			return fmt.Errorf("move %s to %s: %w", e.Name(), dest, err)
		}
	}
	if gitEntryName != "" {
		if err := move(gitEntryName); err != nil {
			rollback()
			return fmt.Errorf("move %s to %s: %w", gitEntryName, dest, err)
		}
	}
	return nil
}

// renameNoReplaceFallback renames oldpath to newpath unless newpath exists.
// The existence check and the rename are two steps, so it is used only where
// the kernel's atomic no-replace rename is unavailable (for example on NFS).
func renameNoReplaceFallback(oldpath, newpath string) error {
	if _, err := os.Lstat(newpath); err == nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: fs.ErrExist}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(oldpath, newpath)
}

// dirHasEntries reports whether dir exists and contains at least one entry.
// A missing directory reports false with a nil error: there is nothing in
// it, by definition.
func dirHasEntries(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return len(entries) > 0, nil
}

// WorktreePath returns the canonical worktree path for a given agent within
// a shared base checkout: <hostPath>/worktrees/<agentID>.
func WorktreePath(hostPath, agentID string) string {
	return filepath.Join(hostPath, "worktrees", agentID)
}

// IsRealWorktreeDir reports whether path is a git worktree that belongs to
// the shared base checkout at base: a real directory (not a symlink)
// containing a .git file (not a directory, and not a symlink) whose
// "gitdir: " target resolves inside base's own .git/worktrees admin
// directory. Any other entry at path — a plain file, a directory without a
// matching worktree admin entry, or a symlink at any point in the chain —
// is not a worktree this checkout owns, and must never be reused, mounted,
// or removed as if it were one.
func IsRealWorktreeDir(path, base string) bool {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return false
	}

	gitFile := filepath.Join(path, ".git")
	gfi, err := os.Lstat(gitFile)
	if err != nil || gfi.IsDir() || gfi.Mode()&os.ModeSymlink != 0 {
		return false
	}

	data, err := os.ReadFile(gitFile)
	if err != nil {
		return false
	}
	content := strings.TrimSpace(string(data))
	target, ok := strings.CutPrefix(content, "gitdir: ")
	if !ok || target == "" {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(path, target)
	}
	target = filepath.Clean(target)

	// The gitdir target must be exactly one path element below the admin
	// worktrees directory (base/.git/worktrees/<name>) — not the admin
	// directory itself (rel "."), not above it (rel ".." or an ancestor),
	// and not nested any deeper.
	worktreesAdminDir := filepath.Clean(filepath.Join(base, ".git", "worktrees"))
	rel, err := filepath.Rel(worktreesAdminDir, target)
	if err != nil || rel == "." || rel == ".." || strings.ContainsRune(rel, filepath.Separator) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// isDirectChildOfWorktreesDir reports whether path, once symlinks are
// resolved, is a direct child of base's own "worktrees" directory — not the
// worktrees directory itself, and not anything nested deeper. Only direct
// entries of worktrees/ are ever mounted read-write into a container, so a
// path that is a real worktree by IsRealWorktreeDir's check but sits any
// deeper (or is the worktrees directory itself) must still be refused.
func isDirectChildOfWorktreesDir(base, path string) bool {
	worktreesDir := filepath.Join(base, "worktrees")
	// The exact string is what is stored and mounted, and downstream
	// consumers clean it lexically rather than resolving symlinks, so it
	// must already be the canonical worktrees/<name> form.
	if filepath.Clean(path) != path || filepath.Dir(path) != worktreesDir {
		return false
	}
	resolvedWorktreesDir, err := filepath.EvalSymlinks(worktreesDir)
	if err != nil {
		return false
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return resolvedPath != resolvedWorktreesDir && filepath.Dir(resolvedPath) == resolvedWorktreesDir
}

// worktreeOutcome reports what ensureWorktree did.
type worktreeOutcome struct {
	// branch is the branch the agent was registered under in the sharer
	// registry, or "" when the registry was not written.
	branch string
	// added is true when a new worktree was added at the agent's path.
	added bool
	// emptyDir is true when the agent's directory was created empty
	// because git is too old for relative worktree paths.
	emptyDir bool
	// rewritten lists other files the step wrote in the shared .git: the
	// worktree's start-branch record and sharer markers it updated.
	rewritten []string
}

// ensureWorktree creates or attaches to a per-agent worktree if the mode is
// WorktreePerAgent. For SharedPlain mode this is a no-op.
//
// Create-or-attach logic (D3 hub-join):
//   - If a worktree for the requested branch already exists (found via the
//     sharer registry or git worktree list), the agent ATTACHES to it (JOIN)
//     and registers as a sharer — no second worktree is created.
//   - Otherwise, a new worktree is created and the agent registers as its
//     first sharer.
//
// The worktree add is done under the already-held advisory lock (design §9.2:
// worktree add/remove touches shared .git metadata).
//
// It reports what it did in a worktreeOutcome.
func ensureWorktree(ctx context.Context, in ProvisionInput) (worktreeOutcome, error) {
	if in.Mode != store.SharingModeWorktreePerAgent {
		return worktreeOutcome{}, nil // SharedPlain: nothing to do
	}

	if in.AgentID == "" {
		return worktreeOutcome{}, fmt.Errorf("ProvisionShared: AgentID is required for WorktreePerAgent mode")
	}

	base := in.Resolved.HostPath
	worktreePath := WorktreePath(base, in.AgentID)

	// Derive a branch name from the agent name or ID. For MountedWorktree
	// the branch is used exactly as given, as on the local runtimes, and
	// must be a valid branch name.
	branchName := in.AgentID
	if in.MountedWorktree {
		if in.AgentName != "" {
			branchName = in.AgentName
		}
		if err := checkBranchName(ctx, base, branchName); err != nil {
			return worktreeOutcome{}, err
		}
	} else if in.AgentName != "" {
		branchName = sanitizeBranchName(in.AgentName)
	}
	registered := func(path string) (worktreeOutcome, error) {
		o := worktreeOutcome{branch: branchName}
		if in.MountedWorktree {
			// The agent's own worktree is registered under this branch
			// only.
			rewritten, err := UnregisterSharerElsewhere(base, in.AgentID, branchName)
			o.rewritten = rewritten
			if err != nil {
				return o, fmt.Errorf("ProvisionShared: update the sharer registry: %w", err)
			}
		}
		return o, RegisterSharer(base, branchName, path, in.AgentID)
	}

	// If this agent's own worktree directory already exists, reuse it only
	// if it is a real worktree of this base — never a plain file, a foreign
	// directory, or a symlink, none of which this checkout created and none
	// of which are safe to mount or to remove.
	if _, err := os.Lstat(worktreePath); err == nil {
		reuse := IsRealWorktreeDir(worktreePath, base)
		if reuse && in.MountedWorktree {
			// The directory must also be known to git: its admin entry
			// must exist and point back to it.
			switch linkedWorktreeState(base, worktreePath) {
			case linkedWorktreeOK:
				return reuseMountedWorktree(ctx, in, worktreePath, branchName)
			case linkedWorktreeNoAdmin:
				aside, err := moveWorktreeAside(base, in.AgentID, staleWorktreePrefix)
				if err != nil {
					return worktreeOutcome{}, fmt.Errorf("ProvisionShared: %w", err)
				}
				slog.Warn("ProvisionShared: git has no entry for the agent's worktree any more; moved it aside and adding a new one",
					"agent_id", in.AgentID, "moved_to", aside)
				reuse = false
			default:
				return worktreeOutcome{}, fmt.Errorf("ProvisionShared: git's entry for the agent's worktree %s points to another directory. "+
					"Move %s out of the way (or delete the agent with its files) and start the agent again",
					mountedDisplayPath(in, worktreePath), mountedDisplayPath(in, worktreePath))
			}
		}
		if reuse {
			slog.Debug("ProvisionShared: worktree already exists",
				"agent_id", in.AgentID, "path", worktreePath)
			return registered(worktreePath)
		}
	}
	if _, err := os.Lstat(worktreePath); err == nil {
		state := mountedDirState(worktreePath)
		switch {
		case in.MountedWorktree && state == mountedDirStandalone:
			slog.Info("ProvisionShared: the agent's directory already holds its own checkout; keeping it",
				"agent_id", in.AgentID, "path", worktreePath)
			return worktreeOutcome{}, nil
		case in.MountedWorktree && state == mountedDirEmpty:
			// Created empty by the broker so the pod can mount it: add the
			// worktree into it below.
		default:
			return worktreeOutcome{}, fmt.Errorf("ProvisionShared: %s exists but is not a git worktree of this checkout; refusing to reuse or remove it", mountedDisplayPath(in, worktreePath))
		}
	}

	// Verify the shared checkout exists (.git dir present).
	gitDir := filepath.Join(base, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return worktreeOutcome{}, fmt.Errorf("ProvisionShared: shared checkout .git not found at %s — "+
			"cannot create worktree without a cloned repository", gitDir)
	}

	// A branch checked out in the shared checkout itself cannot also be
	// checked out in a worktree.
	if current := currentBranch(ctx, base); current != "" && current == branchName {
		if in.MountedWorktree {
			return worktreeOutcome{}, fmt.Errorf("ProvisionShared: branch %q is checked out in the project's shared checkout (%s), so it cannot also be checked out "+
				"in this agent's worktree. Switch the shared checkout to another branch or detach it (git switch --detach, run in the shared checkout), "+
				"or start the agent with a different branch", branchName, mountedWorkspaceName(in))
		}
		return worktreeOutcome{}, fmt.Errorf("ProvisionShared: branch %q is checked out in the project's shared checkout (%s), so it cannot also be checked out "+
			"in this agent's worktree. Switch the shared checkout to another branch or detach it (git -C %s switch --detach), "+
			"or start the agent with a different branch", branchName, base, base)
	}

	// --- JOIN check: does a worktree for this branch already exist? ---

	// 1. Check the sharer registry. The registry is written by every
	// worktree-mode agent's own container (it lives under the shared,
	// read-write-mounted .git); only join the path it names when that path
	// is both a real worktree of this same base and a direct child of its
	// "worktrees" directory.
	sharers, existingWtPath, err := ListSharers(base, branchName)
	if err != nil {
		return worktreeOutcome{}, fmt.Errorf("ProvisionShared: list sharers for branch %q: %w", branchName, err)
	}
	if len(sharers) > 0 && existingWtPath != "" {
		if _, statErr := os.Lstat(existingWtPath); statErr == nil {
			if !IsRealWorktreeDir(existingWtPath, base) || !isDirectChildOfWorktreesDir(base, existingWtPath) {
				return worktreeOutcome{}, fmt.Errorf("ProvisionShared: the sharer registry for branch %q names %s, which is not a direct worktree of this checkout; refusing to join it", branchName, existingWtPath)
			}
			switch {
			case in.MountedWorktree && currentBranch(ctx, existingWtPath) != branchName:
				// The agent there has switched to another branch since it
				// was registered: the git worktree list below decides.
				slog.Info("ProvisionShared: the registered worktree for this branch is on another branch now",
					"agent_id", in.AgentID, "branch", branchName, "path", existingWtPath)
			case in.MountedWorktree:
				return worktreeOutcome{}, branchInOtherWorktreeError(in, branchName, existingWtPath)
			default:
				slog.Info("ProvisionShared: joining existing worktree (registry)",
					"agent_id", in.AgentID, "branch", branchName, "path", existingWtPath,
					"existing_sharers", sharers)
				return registered(existingWtPath)
			}
		} else {
			slog.Warn("ProvisionShared: registry points to missing path, will create new worktree",
				"agent_id", in.AgentID, "branch", branchName, "stale_path", existingWtPath)
		}
	}

	// 2. Check git worktree list for a prior-run worktree without a registry entry.
	if existingPath, findErr := findWorktreeForBranch(ctx, base, branchName); findErr == nil && existingPath != "" && IsRealWorktreeDir(existingPath, base) && isDirectChildOfWorktreesDir(base, existingPath) {
		if in.MountedWorktree {
			return worktreeOutcome{}, branchInOtherWorktreeError(in, branchName, existingPath)
		}
		slog.Info("ProvisionShared: joining pre-existing worktree (git)",
			"agent_id", in.AgentID, "branch", branchName, "path", existingPath)
		return registered(existingPath)
	}

	// --- CREATE: no existing worktree for this branch ---

	worktreesDir := filepath.Join(base, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0770); err != nil {
		return worktreeOutcome{}, fmt.Errorf("ProvisionShared: mkdir worktrees dir: %w", err)
	}

	if in.MountedWorktree && !gitSupportsRelativeWorktrees() {
		// The worktree's paths must resolve in the agent container's own
		// layout, which needs relative worktree paths. Leave the agent's
		// directory empty instead: the agent container clones its own
		// checkout into it.
		slog.Warn("ProvisionShared: git is too old for relative worktree paths (needs 2.48 or later); "+
			"the agent container will clone its own checkout instead of using a worktree",
			"agent_id", in.AgentID, "path", worktreePath)
		if err := os.MkdirAll(worktreePath, 0770); err != nil {
			return worktreeOutcome{}, err
		}
		return worktreeOutcome{emptyDir: true}, nil
	}

	addedWorktree := func() (worktreeOutcome, error) {
		if in.MountedWorktree {
			if _, err := recordStartBranch(base, worktreePath, branchName); err != nil {
				return worktreeOutcome{added: true}, fmt.Errorf("ProvisionShared: %w", err)
			}
		}
		o, err := registered(worktreePath)
		o.added = true
		return o, err
	}

	slog.Info("ProvisionShared: creating worktree",
		"agent_id", in.AgentID, "branch", branchName, "path", worktreePath)

	// git worktree add --relative-paths -b <branch> <path>
	// --relative-paths is mandatory for container path-identity (design §6).
	cmd := exec.CommandContext(ctx, "git", "worktree", "add", "--relative-paths", "-b", branchName, worktreePath)
	cmd.Dir = base
	output, err := cmd.CombinedOutput()
	if err != nil {
		outputStr := strings.TrimSpace(string(output))

		// Branch collision: the proactive JOIN checks above should catch this,
		// but handle defensively in case of a concurrent change or stale state. git itself
		// permits a nested worktree (e.g. worktrees/agent-a/sub), so the path
		// findWorktreeForBranch returns here needs the same validation as the
		// proactive JOIN checks above — it is not guaranteed safe just because
		// git reported it.
		if strings.Contains(outputStr, "already checked out") || strings.Contains(outputStr, "already used by worktree") {
			if attachPath, findErr := findWorktreeForBranch(ctx, base, branchName); findErr == nil && attachPath != "" {
				if !IsRealWorktreeDir(attachPath, base) || !isDirectChildOfWorktreesDir(base, attachPath) {
					return worktreeOutcome{}, fmt.Errorf("git worktree add: branch %q already checked out, but %s is not a direct worktree of this checkout; refusing to join it",
						branchName, attachPath)
				}
				if in.MountedWorktree {
					return worktreeOutcome{}, branchInOtherWorktreeError(in, branchName, attachPath)
				}
				slog.Info("ProvisionShared: attaching to existing worktree (git fallback)",
					"agent_id", in.AgentID, "branch", branchName, "path", attachPath)
				return registered(attachPath)
			}
			return worktreeOutcome{}, fmt.Errorf("git worktree add: branch %q already checked out but cannot find existing worktree: %s",
				branchName, outputStr)
		}

		// If branch already exists (but not checked out), try without -b.
		if strings.Contains(outputStr, "already exists") {
			cmd = exec.CommandContext(ctx, "git", "worktree", "add", "--relative-paths", worktreePath, branchName)
			cmd.Dir = base
			output, err = cmd.CombinedOutput()
			if err != nil {
				reuse := strings.TrimSpace(string(output))
				if strings.Contains(reuse, "already checked out") || strings.Contains(reuse, "already used by worktree") {
					if attachPath, findErr := findWorktreeForBranch(ctx, base, branchName); findErr == nil && attachPath != "" {
						if !IsRealWorktreeDir(attachPath, base) || !isDirectChildOfWorktreesDir(base, attachPath) {
							return worktreeOutcome{}, fmt.Errorf("git worktree add: branch %q already checked out, but %s is not a direct worktree of this checkout; refusing to join it",
								branchName, attachPath)
						}
						if in.MountedWorktree {
							return worktreeOutcome{}, branchInOtherWorktreeError(in, branchName, attachPath)
						}
						slog.Info("ProvisionShared: attaching to existing worktree (reuse fallback)",
							"agent_id", in.AgentID, "branch", branchName, "path", attachPath)
						return registered(attachPath)
					}
					return worktreeOutcome{}, fmt.Errorf("git worktree add: branch %q already checked out: %s", branchName, reuse)
				}
				return worktreeOutcome{}, fmt.Errorf("git worktree add (reuse branch): %s", reuse)
			}
			return addedWorktree()
		}

		return worktreeOutcome{}, fmt.Errorf("git worktree add: %s", outputStr)
	}

	return addedWorktree()
}

// mountedDirKind classifies an existing entry at an agent's worktree path
// that is not a worktree of the shared checkout (see mountedDirState).
type mountedDirKind int

const (
	mountedDirOther      mountedDirKind = iota // anything else: never reused
	mountedDirEmpty                            // a real, empty directory
	mountedDirStandalone                       // a real directory with its own .git directory
)

// mountedDirState reports what kind of entry is at path, without following
// symlinks.
func mountedDirState(path string) mountedDirKind {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return mountedDirOther
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return mountedDirOther
	}
	if len(entries) == 0 {
		return mountedDirEmpty
	}
	gfi, err := os.Lstat(filepath.Join(path, ".git"))
	if err == nil && gfi.IsDir() && gfi.Mode()&os.ModeSymlink == 0 {
		return mountedDirStandalone
	}
	return mountedDirOther
}

// branchInOtherWorktreeError is returned for MountedWorktree when the
// agent's branch is already checked out in another worktree directory: the
// agent's pod mounts only its own directory, so it cannot share that one.
// The paths are shown relative to the shared checkout, since the init
// container's mount paths exist nowhere else.
func branchInOtherWorktreeError(in ProvisionInput, branch, path string) error {
	rel := sharedCheckoutRel(in, path)
	return fmt.Errorf("ProvisionShared: branch %q is already checked out in %s. "+
		"Start the agent with a different branch, or delete the agent that uses that worktree together with its files, "+
		"on a broker that mounts the export (which removes the worktree). "+
		"If no agent uses it any more, remove it (git worktree remove %s, run in the shared checkout) and start the agent again",
		branch, mountedDisplayPath(in, path), rel)
}

// sharedCheckoutRel returns path relative to the shared checkout, or path
// itself when it is not below it.
func sharedCheckoutRel(in ProvisionInput, path string) string {
	rel, err := filepath.Rel(in.Resolved.HostPath, path)
	if err != nil || !filepath.IsLocal(rel) {
		return path
	}
	return filepath.ToSlash(rel)
}

// mountedWorkspaceName names the project's shared checkout in messages
// from the provisioning init container.
func mountedWorkspaceName(in ProvisionInput) string {
	if in.ProjectID == "" {
		return "the project's shared checkout on the NFS export"
	}
	return fmt.Sprintf("the shared checkout of project %s (<subpath_root>/%s/workspace on the NFS export)", in.ProjectID, in.ProjectID)
}

// mountedDisplayPath names path in messages from the provisioning init
// container: relative to the shared checkout, followed by where that is.
func mountedDisplayPath(in ProvisionInput, path string) string {
	return fmt.Sprintf("%s of %s", sharedCheckoutRel(in, path), mountedWorkspaceName(in))
}

// checkBranchName checks that name is a valid branch name, as git itself
// would accept it for a new branch, and returns an error naming it if not.
// It runs in the shared checkout at base. A name git expands to another
// one there (such as @{-1}) is refused as well. Only git exiting with an
// error status means the name is invalid; git failing to start or to be
// waited on is returned as an error running git.
func checkBranchName(ctx context.Context, base, name string) error {
	out, err := exec.CommandContext(ctx, "git", "-C", base, "check-ref-format", "--branch", name).Output()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		return fmt.Errorf("ProvisionShared: %q is not a valid branch name", name)
	case err != nil:
		return fmt.Errorf("ProvisionShared: checking branch name %q: running git check-ref-format: %w", name, err)
	case strings.TrimSpace(string(out)) != name:
		return fmt.Errorf("ProvisionShared: %q is not a valid branch name", name)
	}
	return nil
}

// linkedWorktreeKind classifies the git admin entry of a worktree directory
// (see linkedWorktreeState).
type linkedWorktreeKind int

const (
	linkedWorktreeOK       linkedWorktreeKind = iota // the admin entry exists and points back to the directory
	linkedWorktreeNoAdmin                            // the shared .git has no admin entry for the directory
	linkedWorktreeMismatch                           // the admin entry exists but points somewhere else
)

// worktreeAdminDir returns the admin directory in the shared .git
// (.git/worktrees/<id>) named by the .git file of the worktree at path. ok
// is false unless that directory is a real directory directly inside
// <base>/.git/worktrees.
func worktreeAdminDir(path, base string) (dir string, ok bool) {
	data, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		return "", false
	}
	target, found := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !found {
		return "", false
	}
	target = strings.TrimSpace(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(path, target)
	}
	target = filepath.Clean(target)
	if filepath.Dir(target) != filepath.Join(base, ".git", "worktrees") {
		return "", false
	}
	fi, err := os.Lstat(target)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	return target, true
}

// linkedWorktreeState reports whether the shared .git still knows the
// worktree at path: its admin directory exists and that directory's gitdir
// file resolves back to <path>/.git.
func linkedWorktreeState(base, path string) linkedWorktreeKind {
	admin, ok := worktreeAdminDir(path, base)
	if !ok {
		return linkedWorktreeNoAdmin
	}
	data, err := os.ReadFile(filepath.Join(admin, "gitdir"))
	if err != nil {
		return linkedWorktreeMismatch
	}
	back := strings.TrimSpace(string(data))
	if !filepath.IsAbs(back) {
		back = filepath.Join(admin, back)
	}
	if filepath.Clean(back) != filepath.Join(path, ".git") {
		return linkedWorktreeMismatch
	}
	return linkedWorktreeOK
}

// reuseMountedWorktree reuses the agent's existing worktree for
// MountedWorktree. The worktree must be on the requested branch, or the
// requested branch must be the one the worktree was started with (the
// agent switched branches in its own worktree and is restarting). The
// start branch is recorded in the worktree's admin directory, so it does
// not depend on the sharer registry. A worktree on another branch is never
// registered under the requested one.
func reuseMountedWorktree(ctx context.Context, in ProvisionInput, path, branch string) (worktreeOutcome, error) {
	base := in.Resolved.HostPath
	current := currentBranch(ctx, path)
	if current == branch {
		slog.Debug("ProvisionShared: worktree already exists", "agent_id", in.AgentID, "path", path)
		record, err := recordStartBranch(base, path, branch)
		if err != nil {
			return worktreeOutcome{}, fmt.Errorf("ProvisionShared: %w", err)
		}
		o := worktreeOutcome{branch: branch}
		rewritten, err := UnregisterSharerElsewhere(base, in.AgentID, branch)
		o.rewritten = append(rewritten, record...)
		if err != nil {
			return o, fmt.Errorf("ProvisionShared: update the sharer registry: %w", err)
		}
		return o, RegisterSharer(base, branch, path, in.AgentID)
	}
	if startBranch(base, path) == branch {
		slog.Info("ProvisionShared: reusing the agent's worktree, which is now on another branch",
			"agent_id", in.AgentID, "path", path, "branch", current, "started_with", branch)
		// The agent stays listed under its start branch. The registry
		// check reads the worktree's current branch, so that does not hold
		// the start branch for another agent.
		return worktreeOutcome{}, nil
	}
	shown := current
	if shown == "" {
		shown = "a detached HEAD"
	} else {
		shown = fmt.Sprintf("branch %q", shown)
	}
	return worktreeOutcome{}, fmt.Errorf("ProvisionShared: the agent's existing worktree %s is on %s, not on the requested branch %q. "+
		"Start the agent with branch %q, or delete the agent together with its files (on a broker that mounts the export) "+
		"and create it again with the new branch", mountedDisplayPath(in, path), shown, branch, current)
}

// startBranchFile is the file in a worktree's admin directory that records
// the branch the agent's worktree was started with. git worktree prune
// deletes it together with the admin directory.
const startBranchFile = "scion-start-branch"

// startBranch returns the branch recorded for the worktree at path, or ""
// when there is none. Only a regular file is read.
func startBranch(base, path string) string {
	admin, ok := worktreeAdminDir(path, base)
	if !ok {
		return ""
	}
	file := filepath.Join(admin, startBranchFile)
	if fi, err := os.Lstat(file); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// recordStartBranch records branch as the start branch of the worktree at
// path. It returns the file when it was written, and nothing when it
// already held branch. The record is written to a new file in the admin
// directory and renamed over the old one, so it is replaced as a whole
// and whatever was at that name is replaced rather than written through.
func recordStartBranch(base, path, branch string) ([]string, error) {
	admin, ok := worktreeAdminDir(path, base)
	if !ok {
		return nil, fmt.Errorf("record the start branch of %s: git has no entry for it", path)
	}
	if startBranch(base, path) == branch {
		return nil, nil
	}
	file := filepath.Join(admin, startBranchFile)
	if err := writeFileAtomic(file, []byte(branch+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("record the start branch of %s: %w", path, err)
	}
	return []string{file}, nil
}

// writeFileAtomic writes data to a new temporary file next to path and
// renames it over path.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// currentBranch returns the branch checked out in the repository at
// repoDir, or "" when HEAD is detached or cannot be read.
func currentBranch(ctx context.Context, repoDir string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitSupportsRelativeWorktrees reports whether the git binary supports
// "git worktree add --relative-paths" well enough for a checkout shared
// with other containers (git 2.48 or later). A variable so tests can stand
// in for an older git.
var gitSupportsRelativeWorktrees = func() bool {
	version, _, err := util.GetGitVersion()
	if err != nil {
		return false
	}
	return util.CompareGitVersion(version, 2, 48) == nil
}

// findWorktreeForBranch parses 'git worktree list --porcelain' output to find
// the worktree path for a given branch. Returns "" if no worktree has that
// branch checked out.
func findWorktreeForBranch(ctx context.Context, repoDir, branch string) (string, error) {
	// Prune first so a worktree dir removed on disk (but not unregistered in git)
	// isn't returned as a stale join target pointing at a non-existent path.
	_ = exec.CommandContext(ctx, "git", "-C", repoDir, "worktree", "prune").Run()
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "worktree", "list", "--porcelain")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git worktree list: %w", err)
	}
	var currentPath string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			currentPath = strings.TrimPrefix(line, "worktree ")
		}
		if strings.HasPrefix(line, "branch refs/heads/") {
			b := strings.TrimPrefix(line, "branch refs/heads/")
			if b == branch {
				return currentPath, nil
			}
		}
	}
	return "", nil
}

// prepareBaseForWorktrees configures a freshly cloned base checkout for
// worktree-per-agent use: detaches HEAD (so no branch is "owned" by the base),
// disables auto-gc, and excludes worktrees/ from untracked file lists.
func prepareBaseForWorktrees(ctx context.Context, hostPath string) error {
	if err := gitDetach(ctx, hostPath); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "git", "-C", hostPath, "config", "gc.auto", "0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git config gc.auto 0: %s", strings.TrimSpace(string(output)))
	}

	return appendGitExclude(hostPath, "worktrees/")
}

// prepareExistingBaseForWorktrees adds what worktrees need to a shared
// checkout that was already provisioned, possibly in another mode: auto-gc
// off and worktrees/ excluded from untracked files. Unlike
// prepareBaseForWorktrees it leaves HEAD alone, since agents may be using
// the checkout as it is. Both steps are idempotent.
func prepareExistingBaseForWorktrees(ctx context.Context, hostPath string) error {
	if _, err := os.Stat(filepath.Join(hostPath, ".git")); err != nil {
		// No checkout: ensureWorktree reports this.
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", hostPath, "config", "gc.auto", "0")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config gc.auto 0: %s", strings.TrimSpace(string(output)))
	}
	return appendGitExclude(hostPath, "worktrees/")
}

// gitDetach detaches HEAD in the repo at hostPath so the base checkout owns
// no branch. Tries 'git switch --detach' first, falls back to 'git checkout
// --detach' for older git versions.
func gitDetach(ctx context.Context, hostPath string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", hostPath, "switch", "--detach")
	if _, err := cmd.CombinedOutput(); err == nil {
		return nil
	}
	cmd = exec.CommandContext(ctx, "git", "-C", hostPath, "checkout", "--detach")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git detach: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

// appendGitExclude appends a pattern to .git/info/exclude if not already present.
//
// It never follows a symlink below hostPath: .git, .git/info and
// .git/info/exclude are each opened relative to the previous directory with
// O_NOFOLLOW (see openGitExclude), so a symlink at any of them (for example
// one committed to or planted in a shared checkout that the root init
// container writes to) makes it fail instead of writing elsewhere. A missing
// .git/info is created (0755) and a missing exclude file (0644), as before;
// anything other than a regular file at exclude is refused.
func appendGitExclude(hostPath, pattern string) error {
	f, err := openGitExclude(hostPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.NewSectionReader(f, 0, 1<<62))
	if err != nil {
		return fmt.Errorf("read %s: %w", f.Name(), err)
	}
	// Exact line match — strings.Contains would false-positive on e.g.
	// "my-worktrees/" or "worktrees/agent-1" and skip appending the pattern.
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == strings.TrimSpace(pattern) {
			return nil
		}
	}
	// The file is opened with O_APPEND: every write lands at the end.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err = f.WriteString(pattern + "\n")
	return err
}

// openGitExclude opens hostPath/.git/info/exclude for reading and appending
// without following a symlink at .git, info or exclude: each component is
// opened with openat relative to its parent's descriptor and O_NOFOLLOW, so
// none of them can be redirected by a symlink, or swapped for one between
// a check and the open. info is created if missing; exclude is created if
// missing and must be a regular file (O_NONBLOCK keeps a FIFO planted there
// from blocking the open; it is then refused).
func openGitExclude(hostPath string) (*os.File, error) {
	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	rootFd, err := unix.Open(hostPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", hostPath, err)
	}
	defer func() { _ = unix.Close(rootFd) }()
	gitDir := filepath.Join(hostPath, ".git")
	gitFd, err := unix.Openat(rootFd, ".git", dirFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s without following symlinks: %w", gitDir, err)
	}
	defer func() { _ = unix.Close(gitFd) }()

	infoDir := filepath.Join(gitDir, "info")
	infoFd, err := unix.Openat(gitFd, "info", dirFlags, 0)
	if errors.Is(err, unix.ENOENT) {
		if mkErr := unix.Mkdirat(gitFd, "info", 0o755); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
			return nil, fmt.Errorf("mkdir %s: %w", infoDir, mkErr)
		}
		infoFd, err = unix.Openat(gitFd, "info", dirFlags, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s without following symlinks: %w", infoDir, err)
	}
	defer func() { _ = unix.Close(infoFd) }()

	excludePath := filepath.Join(infoDir, "exclude")
	fd, err := unix.Openat(infoFd, "exclude", unix.O_RDWR|unix.O_APPEND|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s without following symlinks: %w", excludePath, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("stat %s: %w", excludePath, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%s is not a regular file", excludePath)
	}
	return os.NewFile(uintptr(fd), excludePath), nil
}

// sanitizeBranchName produces a git-safe branch name from an agent name.
func sanitizeBranchName(name string) string {
	// Replace characters invalid in git branch names.
	replacer := strings.NewReplacer(
		" ", "-", "/", "-", "\\", "-", "..", "-",
		"~", "-", "^", "-", ":", "-", "?", "-",
		"*", "-", "[", "-", "]", "-",
	)
	result := replacer.Replace(name)
	// Trim leading/trailing dashes and dots.
	result = strings.Trim(result, "-.")
	if result == "" {
		return "agent"
	}
	return result
}

// chownTarget returns the directory to recursively chown for a freshly
// provisioned workspace.
//
// Broker-side, the project root is the parent of the workspace dir (it also
// holds the shared-dirs siblings), so we chown the parent. But inside a k8s
// init container only the workspace dir itself is mounted (subPath), so its
// parent resolves to the filesystem root "/". Chowning "/" recursively is
// wrong — and a latent security hazard if the pod's security context is ever
// relaxed — so fall back to chowning the workspace dir itself in that case.
func chownTarget(hostPath string) string {
	parent := filepath.Dir(hostPath)
	if parent == "/" || parent == "." {
		return hostPath
	}
	return parent
}

// chownProjectTree sets ownership of the project root and its contents to the
// given UID/GID. This is a ONE-TIME operation done under the advisory lock
// during first provisioning (design §9.1). Per-start chown is NOT done for
// NFS (slow, and unsafe to run concurrently with other starts, over the network).
//
// fsutil.CheckRoot runs first and refuses outright if projectRoot is a known
// critical system path or looks like a filesystem root by content — an
// additional guard against a resolution bug elsewhere computing an
// unintended chown root, independent of chownTarget's own narrower "/"
// handling, and standing in front of the shared-dir chown calls too, which
// do not route through chownTarget.
//
// Walks the tree itself (filepath.WalkDir + os.Lchown) rather than shelling
// out to the chown binary, for two reasons:
//
//   - No following of symlinks (F-111 review, tf-lead): a symlink inside a
//     cloned (possibly untrusted) repo pointing outside the chowned tree
//     (elsewhere in the init container's own filesystem view, or another
//     mounted shared dir) must have its REFERENT left alone — only the link
//     itself is re-owned. os.Lchown is exactly that operation, with no
//     dependency on a particular chown binary's own flag support.
//   - lockDir, when non-empty, is the SAME directory the file-lock fallback
//     above publishes its lock into (see resolveSentinelDir and the k8s
//     init container's SentinelDir, which has no other directory available
//     to it in that mount) — so when projectRoot IS that directory, a plain
//     recursive chown walking the whole tree would also walk the lock's own
//     live directory, its staging and evicted-generation entries, and its
//     clock-probe files — every one of them transient bookkeeping a
//     CONCURRENT waiter is free to create, rename, or remove at any moment
//     while this holder is still walking underneath them (see
//     acquireFileLock's correctness argument). A waiter's own probe file
//     disappearing mid-walk, or an evicted entry being swept by garbage
//     collection mid-walk, is completely unrelated to the actual
//     provisioning work and must never be able to fail it. isLockArtifactPath
//     identifies that entire prefix by name alone (never stat'd), so none
//     of it is ever visited, stat'd, or chowned — excluded rather than
//     interleaved with.
//
// The exclusion applies ONLY to DIRECT children of lockDir itself, never at
// any other depth and never when lockDir is empty: the lock only ever lives
// directly inside lockDir (see acquireFileLock's doc — it is never nested),
// so a project file or directory anywhere else that merely happens to share
// the lock's name prefix (in a cloned repo, or inside a shared dir, which
// never holds the lock at all) is ordinary content and must still be
// chowned like everything else, not silently skipped and left under the
// init container's root ownership.
//
// Never calls os.Chown or os.Chmod on a walked path — only lchownFile
// (os.Lchown in production), which operates on the path by name and never
// dereferences a symlink regardless of what currently sits at that name, so
// a symlink swapped in between WalkDir observing it and this call still has
// only its own link ownership changed, never a followed target's. WalkDir
// itself never descends into a symlinked directory (a symlink's DirEntry
// carries the link's own mode bits from its parent's directory read, not
// the target's, so d.IsDir() is false for it) — nothing here calls
// os.ReadDir or otherwise manually recurses, so that default holds
// unmodified. The walk starts at, and never climbs above, projectRoot:
// WalkDir only ever descends into children it discovers itself.
//
// Continues past a per-entry failure — a failed Lchown, or a path that
// disappeared between being listed and being visited — rather than
// aborting the whole walk on the first one, the same as the recursive
// chown binary this replaces: every reachable path still gets a real
// attempt, and the first failure (if any) is what gets returned, not
// necessarily the only one. An EPERM on an entry already owned by
// uid:gid is not a failure (see checkChownEPERM); such entries are counted
// and summarized in one Info line per walk. A ctx cancellation is the one
// thing that stops the walk outright, matching the previous
// exec.CommandContext-based implementation being killed on cancellation.
//
// TestChownProjectTree_DanglingSymlink_DoesNotFail is the regression guard
// on no-dereference: a symlink to a path that exists nowhere, so re-owning
// the link itself (Lchown) succeeds while resolving it (Chown) would fail
// with ENOENT. It needs no timing assumptions and fails deterministically if
// the walk ever starts dereferencing.
func chownProjectTree(ctx context.Context, projectRoot, lockDir string, uid, gid int) error {
	return chownProjectTreeExcluding(ctx, projectRoot, []string{lockDir}, uid, gid)
}

// chownProjectTreeExcluding is chownProjectTree with several lock
// directories (empty entries are ignored): the lock artifacts directly in
// any of them are skipped. ProvisionShared passes both the sentinel
// directory and the legacy lock directory (ProvisionInput.LegacyDir).
func chownProjectTreeExcluding(ctx context.Context, projectRoot string, lockDirs []string, uid, gid int) error {
	if err := fsutil.CheckRoot(projectRoot); err != nil {
		return fmt.Errorf("recursive chown %s to %d:%d: %w", projectRoot, uid, gid, err)
	}
	excluded := make(map[string]bool, len(lockDirs))
	for _, dir := range lockDirs {
		if dir != "" {
			excluded[filepath.Clean(dir)] = true // guard against a trailing slash silently disabling the exclusion below
		}
	}
	var firstErr error
	tolerated := 0
	walkErr := filepath.WalkDir(projectRoot, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if excluded[filepath.Dir(path)] && isLockArtifactPath(path) {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return nil
		}
		if lerr := lchownFile(path, uid, gid); lerr != nil {
			cerr := checkChownEPERM(path, uid, gid, lerr)
			switch {
			case cerr == nil:
				tolerated++
			case firstErr == nil:
				firstErr = cerr
			}
		}
		return nil
	})
	if tolerated > 0 {
		slog.Info("chown not permitted on some entries; ownership already matched",
			"root", projectRoot, "count", tolerated, "uid", uid, "gid", gid)
	}
	if walkErr != nil {
		return walkErr
	}
	return firstErr
}

// checkChownEPERM decides whether a failed lchown of path is fatal. It
// returns nil only when lerr is EPERM and path (Lstat'd, so a symlink is
// never followed, matching lchownFile) is already owned by uid:gid. That is
// the normal outcome on an NFS export with all_squash, or root_squash with
// anonuid/anongid set to the workspace owner, where root cannot chown but
// the server already maps every file to the desired identity. Any other error, or EPERM with a different owner, is returned
// as an error naming the path, the desired owner and the actual owner.
func checkChownEPERM(path string, uid, gid int, lerr error) error {
	if !errors.Is(lerr, syscall.EPERM) {
		return fmt.Errorf("lchown %d:%d %s: %w", uid, gid, path, lerr)
	}
	info, serr := os.Lstat(path)
	if serr != nil {
		return fmt.Errorf("lchown %d:%d %s: %w (stat for owner check failed: %v)", uid, gid, path, lerr, serr)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("lchown %d:%d %s: %w (owner unknown)", uid, gid, path, lerr)
	}
	// -1 means "leave unchanged" to lchown, so it matches any on-disk ID.
	if (uid == -1 || int(st.Uid) == uid) && (gid == -1 || int(st.Gid) == gid) {
		slog.Debug("chown not permitted but ownership already matches, continuing",
			"path", path, "uid", uid, "gid", gid)
		return nil
	}
	return fmt.Errorf("lchown %d:%d %s: %w (owner is %d:%d, want %d:%d)",
		uid, gid, path, lerr, st.Uid, st.Gid, uid, gid)
}

// isLockArtifactPath reports whether path's base name is the file lock's own
// live directory (provisionFileLockName), or one of its staging, evicted-
// generation, or clock-probe variants — every name chownProjectTree
// excludes from its walk. All of them share the same name prefix by
// construction: provisionLockStagingPattern and evictPathFor both build on
// provisionFileLockName, and provisionClockProbeFile is defined as
// provisionFileLockName plus a suffix specifically so this one prefix check
// catches all of them.
func isLockArtifactPath(path string) bool {
	base := filepath.Base(path)
	return base == provisionFileLockName || strings.HasPrefix(base, provisionFileLockName+".")
}

// resolveUID returns the NFS UID to use for chown, defaulting to 1000.
func resolveUID(in ProvisionInput) int {
	if in.NFSUID != 0 {
		return in.NFSUID
	}
	return 1000
}

// resolveGID returns the NFS GID to use for chown, defaulting to 1000.
func resolveGID(in ProvisionInput) int {
	if in.NFSGID != 0 {
		return in.NFSGID
	}
	return 1000
}

// writeSentinel writes the provisioning sentinel file atomically using
// write-to-temp + rename. The sentinel's existence is the fast-path check
// that short-circuits re-provisioning.
func writeSentinel(path string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".scion-provisioned-*")
	if err != nil {
		return fmt.Errorf("create temp sentinel: %w", err)
	}
	tmpName := tmp.Name()

	// Write a timestamp for debugging.
	_, _ = fmt.Fprintf(tmp, "provisioned_at=%s\n", time.Now().UTC().Format(time.RFC3339))
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp sentinel: %w", err)
	}

	// Atomic rename.
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename sentinel: %w", err)
	}
	return nil
}
