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

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// nfsAgentDirSelection decides whether an agent on the NFS workspace backend
// gets its own directory (agents/<agent name> next to the project's
// workspace path), with its own workspace the agent container clones into,
// instead of mounting the project's shared checkout.
//
// It returns the directory name and the agent's branch when the dispatch
// carries the canonical workspace mode clone-per-agent
// (SCION_WORKSPACE_MODE) and the project is git-backed (gitClone has a
// URL), and two empty strings otherwise. In that mode the agent name must
// be an agent slug: any other name is an error, so the agent never falls
// back to the shared checkout.
//
// The branch is SCION_AGENT_BRANCH when set, otherwise scion/<agent name>,
// the same choice the agent container's clone step makes.
func nfsAgentDirSelection(env map[string]string, gitClone *api.GitCloneConfig, agentName string) (name, branch string, err error) {
	if env == nil || gitClone == nil || gitClone.URL == "" {
		return "", "", nil
	}
	if store.ResolveWorkspaceSharingMode(env["SCION_WORKSPACE_MODE"]) != store.SharingModeClonePerAgent {
		return "", "", nil
	}
	if !isNFSWorktreeName(agentName) {
		return "", "", fmt.Errorf("workspace_storage nfs: clone-per-agent needs the agent name to be an agent slug (lower-case letters, digits and dashes), got %q", agentName)
	}
	branch = env["SCION_AGENT_BRANCH"]
	if branch == "" {
		branch = "scion/" + agentName
	}
	return agentName, branch, nil
}

// nfsOwnAgentDirSelection decides whether the agent gets its own directory
// on the NFS workspace export (<subpath_root>/<projectID>/agents/<agent
// name>), and returns its name in emptyDirName for an empty-per-agent agent
// (nfsEmptyAgentDirSelection) or in cloneDirName, with the agent's branch,
// for a clone-per-agent agent on Kubernetes, or on a local container
// runtime for a project on a shared Layout (agentDirOnShare for runtimeName
// and projectDir; nfsAgentDirSelection). All three are empty when the agent
// gets none. Start uses it to lay out the workspace and
// CheckMoveWorkspaceAbsent to find what a start would use, so both always
// agree.
func nfsOwnAgentDirSelection(runtimeName, projectDir string, ws *config.V1WorkspaceStorageConfig, emptyPerAgent bool, env map[string]string, gitClone *api.GitCloneConfig, agentName string) (emptyDirName, cloneDirName, cloneBranch string, err error) {
	if emptyPerAgent {
		emptyDirName, err = nfsEmptyAgentDirSelection(runtimeName, projectDir, ws, agentName)
		return emptyDirName, "", "", err
	}
	if !agentDirOnShare(runtimeName, projectDir) || ws == nil || ws.Backend != "nfs" {
		return "", "", "", nil
	}
	cloneDirName, cloneBranch, err = nfsAgentDirSelection(env, gitClone, agentName)
	return "", cloneDirName, cloneBranch, err
}

// nfsAgentDirLockWait bounds how long RemoveNFSAgentFiles waits for the
// agent directory's lock. A variable so tests can shorten it.
var nfsAgentDirLockWait = 30 * time.Second

// storageAuditLoggerKey carries the logger storage deletions are audited
// to (WithStorageAuditLogger).
type storageAuditLoggerKey struct{}

// WithStorageAuditLogger returns ctx with logger as the log the agent's
// storage removals started under ctx are audited to, including those that
// finish in the background (the purge of a moved-aside workspace). The
// broker passes its agent lifecycle log.
func WithStorageAuditLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, storageAuditLoggerKey{}, logger)
}

// storageAuditLogger returns ctx's storage audit logger, or slog.Default.
func storageAuditLogger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(storageAuditLoggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// purgeRemovedAgentWorkspaces deletes the workspaces RemoveNFSAgentFiles
// moved aside in an agent directory, in the background and without a
// deadline, and audits the result to log, for the workspace's path (its
// removal from that path was audited when it was moved aside). A variable
// so tests can run it inline.
var purgeRemovedAgentWorkspaces = func(agentDir, projectID, agentName string, log *slog.Logger) {
	go func() {
		d := projectfs.Deletion{Tree: projectfs.TreeAgentWorkspace, Path: filepath.Join(agentDir, provision.AgentWorkspaceDir),
			Outcome: projectfs.OutcomeDeleted, Reason: "background purge of the agent's moved-aside workspace"}
		if err := provision.PurgeRemovedAgentWorkspaces(agentDir); err != nil {
			d.Outcome, d.Err = projectfs.OutcomeFailed, err
		}
		projectfs.Audit(context.Background(), log, projectfs.DeleterBroker, projectID, agentName, d)
	}()
}

// deleteHubAgentIDKey, deleteMoveReincarnationIDKey, and
// deleteHomeSubPathRootKey carry optional rollback-delete metadata for
// CheckMoveMarkers and RemoveNFSAgentFiles without changing their signatures.
type (
	deleteHubAgentIDKey          struct{}
	deleteMoveReincarnationIDKey struct{}
	deleteHomeSubPathRootKey     struct{}
)

// WithDeleteHubAgentID attaches hubAgentID to ctx for CheckMoveMarkers and
// RemoveNFSAgentFiles.
func WithDeleteHubAgentID(ctx context.Context, hubAgentID string) context.Context {
	if hubAgentID == "" {
		return ctx
	}
	return context.WithValue(ctx, deleteHubAgentIDKey{}, hubAgentID)
}

func deleteHubAgentIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(deleteHubAgentIDKey{}).(string)
	return v
}

// WithDeleteMoveReincarnationID attaches moveReincarnationID to ctx so
// RemoveNFSAgentFiles scopes move-bundle staging purge, K8s NFS home-leaf
// removal, and empty-agent-dir cleanup strictly to move rollback deletes (R4).
func WithDeleteMoveReincarnationID(ctx context.Context, moveReincarnationID string) context.Context {
	if moveReincarnationID == "" {
		return ctx
	}
	return context.WithValue(ctx, deleteMoveReincarnationIDKey{}, moveReincarnationID)
}

func deleteMoveReincarnationIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(deleteMoveReincarnationIDKey{}).(string)
	return v
}

// WithDeleteHomeSubPathRoot attaches the SubPathRoot recorded in the agent's
// home-storage.json (read before DeleteTarget removes agentDir) so
// removeK8sNFSAgentHome targets the recorded subpath root (N4).
func WithDeleteHomeSubPathRoot(ctx context.Context, subPathRoot string) context.Context {
	if subPathRoot == "" {
		return ctx
	}
	return context.WithValue(ctx, deleteHomeSubPathRootKey{}, subPathRoot)
}

func deleteHomeSubPathRootFrom(ctx context.Context) string {
	v, _ := ctx.Value(deleteHomeSubPathRootKey{}).(string)
	return v
}

// RemoveNFSAgentFiles removes what the agent named agentName has on the NFS
// workspace export, through the broker's own mount of the export. The
// broker calls it when it deletes an agent with its files. It removes,
// independently of each other:
//   - the agent's worktree under the project's shared checkout
//     (RemoveNFSWorktree);
//   - the agent's own workspace in its agent directory
//     (<subpath_root>/<projectID>/agents/<agentName>/workspace), with the
//     branch record next to it;
//   - on move rollback deletes only (WithDeleteMoveReincarnationID set):
//     any move-bundle .bundle-* staging siblings, the marker-guarded K8s NFS
//     home leaf (home-<agentID>, when WithDeleteHubAgentID is set), and the
//     empty marked export agent dir. Normal deletes leave home-<agentID> to
//     Hub-owned projectfs.Layout.RemoveAgentStorage (R4).
//
// The project's workspace mode is not consulted: it can change after an
// agent was created, so each removal acts on what is on the export.
// Nothing outside worktrees/<agentName> and agents/<agentName> is touched.
// It returns the paths it looked at and any failures, joined; each failure
// leaves that path in place, and an agent created again with the same name
// reuses it. The worktree is removed only on Kubernetes and the agent's
// workspace on Kubernetes or Cloud Run (whose clone-per-agent agents mount
// agents/<agentName>/workspace); either needs the project's
// workspace_storage backend to be nfs and the export mounted on the
// broker.
func (m *AgentManager) RemoveNFSAgentFiles(ctx context.Context, projectPath, projectID, agentName string) (ds []projectfs.Deletion, err error) {
	var errs []error
	var rollbackDirs []string
	if d, ok := m.removeNFSWorktreeOutcome(ctx, projectPath, projectID, agentName); ok {
		ds = append(ds, d)
		if d.Err != nil {
			errs = append(errs, d.Err)
		}
	}
	if d, wsDir, ok := m.removeNFSAgentWorkspace(ctx, projectPath, projectID, agentName); ok {
		ds = append(ds, d)
		if d.Err != nil {
			errs = append(errs, d.Err)
		}
		if wsDir != "" {
			rollbackDirs = append(rollbackDirs, wsDir)
		}
	}
	if moveID := deleteMoveReincarnationIDFrom(ctx); moveID != "" {
		if d, homeParent, ok := m.removeK8sNFSAgentHome(ctx, projectID, agentName, deleteHubAgentIDFrom(ctx), moveID); ok {
			ds = append(ds, d)
			if d.Err != nil {
				errs = append(errs, d.Err)
			}
			if homeParent != "" {
				rollbackDirs = append(rollbackDirs, homeParent)
			}
		}
		// Single final cleanup pass (N3): remove .scion-move and empty export
		// agent dirs after both workspace and home removals finish, regardless
		// of whether the home share equals the workspace share.
		seen := make(map[string]struct{}, len(rollbackDirs))
		for _, dir := range rollbackDirs {
			if _, dup := seen[dir]; dup {
				continue
			}
			seen[dir] = struct{}{}
			cleanupEmptyMarkedExportAgentDir(dir)
		}
	}
	return ds, errors.Join(errs...)
}

// removeNFSAgentWorkspace removes the workspace of the clone-per-agent agent
// named agentName from its agent directory on the export: under the agent
// directory's lock, the workspace is moved aside and the branch record
// dropped (provision.RemoveAgentWorkspaceOutcome), then the files are
// deleted in the background. It reports what happened to the workspace
// (<agent directory>/workspace): deleted when it was moved aside, absent
// when there was none, failed with the error; ok is false when the
// project's workspace is not on an NFS export this broker has mounted.
// Every component from the export down to the agent directory must be a
// real directory, not a symlink.
func (m *AgentManager) removeNFSAgentWorkspace(ctx context.Context, projectPath, projectID, agentName string) (d projectfs.Deletion, exportAgentDir string, ok bool) {
	d = projectfs.Deletion{Tree: projectfs.TreeAgentWorkspace}
	fail := func(err error) (projectfs.Deletion, string, bool) {
		d.Outcome, d.Err = projectfs.OutcomeFailed, err
		return d, "", true
	}
	resolvedHostBase, rel, err := m.nfsAgentDirExportWorkspace(projectPath, projectID, agentName)
	if err != nil {
		return fail(err)
	}
	if resolvedHostBase == "" {
		return d, "", false
	}
	agentRel, err := runtime.NFSAgentDirSubPath(rel, agentName)
	if err != nil {
		return fail(fmt.Errorf("workspace_storage nfs: %w", err))
	}
	agentDir := filepath.Join(resolvedHostBase, agentRel)
	d.Path = filepath.Join(agentDir, provision.AgentWorkspaceDir)
	if _, err := os.Lstat(agentDir); errors.Is(err, fs.ErrNotExist) {
		d.Outcome = projectfs.OutcomeAbsent
		return d, "", true
	}
	if real, err := filepath.EvalSymlinks(agentDir); err != nil || real != agentDir {
		return fail(fmt.Errorf("workspace_storage nfs: %s is not a plain directory on the export; left in place", agentDir))
	}
	// The agent directory is the agent's; the project's shared checkout
	// next to it is never removed (agentFilesGuard). The removal acts on
	// the path the guard returns.
	checkedDir, err := newAgentFilesGuard(agentName, []string{resolvedHostBase}, []string{agentDir}, []string{filepath.Join(resolvedHostBase, rel)}).check(agentDir)
	if err != nil {
		return fail(fmt.Errorf("workspace_storage nfs: %w", err))
	}
	removed, err := provision.RemoveAgentWorkspaceOutcome(ctx, checkedDir, nfsAgentDirLockWait)
	if err != nil {
		return fail(err)
	}
	isRollback := deleteMoveReincarnationIDFrom(ctx) != ""
	if isRollback {
		if stageErr := purgeMoveBundleStagingEntries(checkedDir); stageErr != nil {
			return fail(fmt.Errorf("purge move-bundle staging in %s: %w", checkedDir, stageErr))
		}
		// Purge moved-aside workspaces synchronously during move rollback so
		// the single final cleanupEmptyMarkedExportAgentDir pass can remove
		// agents/<slug> deterministically for a clean retry.
		if purgeErr := provision.PurgeRemovedAgentWorkspaces(checkedDir); purgeErr != nil {
			return fail(fmt.Errorf("purge removed workspace in %s: %w", checkedDir, purgeErr))
		}
	}
	if !removed {
		d.Outcome = projectfs.OutcomeAbsent
		return d, checkedDir, true
	}
	if !isRollback {
		purgeRemovedAgentWorkspaces(checkedDir, projectID, agentName, storageAuditLogger(ctx))
	}
	slog.Info("workspace_storage nfs: removed the agent's workspace", "path", d.Path)
	d.Outcome = projectfs.OutcomeDeleted
	return d, checkedDir, true
}

// resolveK8sNFSHomeRollbackPaths resolves the home share's host base, agent
// directory, and home-<agentID> leaf path for K8s rollback marker checks and
// removal, preferring the SubPathRoot recorded in home-storage.json when
// available (N4).
func resolveK8sNFSHomeRollbackPaths(ctx context.Context, projectID, agentName, hubAgentID string) (resolvedHostBase, exportAgentDir, homeLeafPath string, ok bool, err error) {
	if !shareddirs.ValidProjectID(projectID) || !validHomeAgentID(hubAgentID, agentName) {
		return "", "", "", false, nil
	}
	gs, err := loadHomeStorageSettings(ctx)
	if err != nil {
		slog.Warn("home storage nfs rollback: could not load global settings; skipping NFS home rollback check",
			"project_id", projectID, "agent", agentName, "error", err)
		return "", "", "", false, nil
	}
	hl, err := homeShareLayout(gs, "")
	if err != nil {
		slog.Warn("home storage nfs rollback: could not resolve home share layout; skipping NFS home rollback check",
			"project_id", projectID, "agent", agentName, "error", err)
		return "", "", "", false, nil
	}
	info, err := os.Stat(hl.HostBase)
	if err != nil || !info.IsDir() {
		slog.Warn("home storage nfs rollback: home share mount root unavailable; skipping NFS home rollback check",
			"project_id", projectID, "agent", agentName, "host_base", hl.HostBase, "error", err)
		return "", "", "", false, nil
	}
	resolvedHostBase, err = filepath.EvalSymlinks(hl.HostBase)
	if err != nil {
		return "", "", "", false, err
	}
	subPathRoot := deleteHomeSubPathRootFrom(ctx)
	if subPathRoot == "" {
		subPathRoot = hl.SubPathRoot
	}
	agentDirRel, homeRel, err := runtime.NFSHomeSubPaths(subPathRoot, projectID, agentName, hubAgentID)
	if err != nil {
		return "", "", "", false, err
	}
	return resolvedHostBase, filepath.Join(resolvedHostBase, agentDirRel), filepath.Join(resolvedHostBase, homeRel), true, nil
}

// removeK8sNFSAgentHome removes the Kubernetes NFS home leaf (home-<agentID>)
// and any move-bundle staging siblings from the home share's agent directory
// during a move rollback delete (moveID != ""). Refuses to remove home-<agentID>
// unless either the leaf itself or its parent carries MoveMarkerFile == moveID
// (R3). Normal deletes never call this function (R4).
func (m *AgentManager) removeK8sNFSAgentHome(ctx context.Context, projectID, agentName, hubAgentID, moveID string) (d projectfs.Deletion, homeParent string, ok bool) {
	if moveID == "" || m.Runtime == nil || !isKubernetesRuntime(m.Runtime.Name()) {
		return d, "", false
	}
	resolvedHostBase, exportAgentDir, homeLeafPath, resolved, err := resolveK8sNFSHomeRollbackPaths(ctx, projectID, agentName, hubAgentID)
	if err != nil || !resolved {
		return d, "", false
	}
	d = projectfs.Deletion{Tree: projectfs.TreeAgentHome, Path: homeLeafPath}
	fail := func(err error) (projectfs.Deletion, string, bool) {
		d.Outcome, d.Err = projectfs.OutcomeFailed, err
		return d, "", true
	}
	if _, err := os.Lstat(exportAgentDir); errors.Is(err, fs.ErrNotExist) {
		d.Outcome = projectfs.OutcomeAbsent
		return d, "", true
	}
	if real, err := filepath.EvalSymlinks(exportAgentDir); err != nil || real != exportAgentDir {
		return fail(fmt.Errorf("home storage nfs: %s is not a plain directory on the export; left in place", exportAgentDir))
	}
	checkedDir, err := newAgentFilesGuard(agentName, []string{resolvedHostBase}, []string{exportAgentDir}, nil).check(exportAgentDir)
	if err != nil {
		return fail(fmt.Errorf("home storage nfs: %w", err))
	}
	if stageErr := purgeMoveBundleStagingEntries(checkedDir); stageErr != nil {
		return fail(fmt.Errorf("home storage nfs: purge move-bundle staging: %w", stageErr))
	}
	root, err := os.OpenRoot(checkedDir)
	if err != nil {
		return fail(fmt.Errorf("home storage nfs: open export agent dir: %w", err))
	}
	defer func() { _ = root.Close() }()
	homeBase := filepath.Base(homeLeafPath)
	if _, err := root.Lstat(homeBase); errors.Is(err, fs.ErrNotExist) {
		d.Outcome = projectfs.OutcomeAbsent
		return d, checkedDir, true
	} else if err != nil {
		return fail(fmt.Errorf("home storage nfs: stat %s: %w", homeBase, err))
	}
	// Marker guard (R3): never delete home-<agentID> unless either the leaf
	// itself or its parent carries this move's reincarnation ID.
	if checkMoveMarker(homeLeafPath, moveID) != nil && checkMoveMarker(checkedDir, moveID) != nil {
		return fail(fmt.Errorf("home storage nfs: %w: %s has no matching move marker", ErrMoveMarkerMismatch, homeLeafPath))
	}
	if err := root.RemoveAll(homeBase); err != nil {
		return fail(fmt.Errorf("home storage nfs: remove %s: %w", homeBase, err))
	}
	slog.Info("home storage nfs: removed the agent's home leaf", "path", d.Path)
	d.Outcome = projectfs.OutcomeDeleted
	return d, checkedDir, true
}

// cleanupEmptyMarkedExportAgentDir removes MoveMarkerFile, lock-release
// eviction litter (.scion-provision.lock.evict-*), and the export agent
// directory itself when nothing else remains inside dir, so a rollback delete
// leaves agents/<slug> absent for a clean retry.
func cleanupEmptyMarkedExportAgentDir(dir string) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		_ = root.Close()
		return
	}
	for _, e := range entries {
		name := e.Name()
		if name != MoveMarkerFile && !strings.HasPrefix(name, ".scion-provision.lock.evict-") {
			_ = root.Close()
			return
		}
	}
	for _, e := range entries {
		_ = root.RemoveAll(e.Name())
	}
	_ = root.Close()
	_ = os.Remove(dir)
}

// nfsAgentDirRecordFile is the per-agent file, in the agent's broker
// directory next to home-storage.json, that records that the agent uses its
// own NFS agent directory (agents/<agent name>) whenever its mode selects
// one. Every create writes it (Provision and Start with FreshProvision,
// whatever the mode or runtime), so only an agent created before
// ptone/scion#3998 can lack it; a start that uses agents/<agent name> also
// writes it. It is removed with that directory when the agent is deleted.
const nfsAgentDirRecordFile = "nfs-agent-dir.json"

// nfsKeepSharedCheckout reports whether an agent that nfsAgentDirSelection
// put in its own agent directory keeps mounting the project's shared
// checkout instead, and why. That holds only for an agent created before
// unlabelled git projects were dispatched as clone-per-agent
// (ptone/scion#3998), which worked in the shared checkout: a start that is
// not a create, with no record in agentDir (every create since writes one,
// including a provision-only create), on a broker with the export
// mounted, where the agent's directory does not exist. A create, a record,
// an existing directory, an unmounted export or any other stat error all
// keep the agent directory. The record is broker-local, so on another
// broker the stat alone decides. The check only reads.
func nfsKeepSharedCheckout(fresh bool, agentDir string, resolved runtime.ResolvedWorkspace, agentName string) (bool, string) {
	if fresh {
		return false, ""
	}
	if _, err := os.Stat(filepath.Join(agentDir, nfsAgentDirRecordFile)); err == nil {
		return false, ""
	}
	sub, err := runtime.NFSAgentDirSubPath(resolved.ServerRelativePath, agentName)
	if err != nil || resolved.HostBase == "" {
		return false, ""
	}
	if _, err := os.Stat(resolved.HostBase); err != nil {
		return false, ""
	}
	if _, err := os.Lstat(filepath.Join(resolved.HostBase, sub)); !errors.Is(err, fs.ErrNotExist) {
		return false, ""
	}
	return true, "existing agent without its own NFS agent directory keeps the shared checkout"
}

// cloudRunAgentWorkspaceRecordFile is the per-agent file, in the agent's
// broker directory, that records that a Cloud Run clone-per-agent agent
// uses its own workspace on the share (agents/<agent name>/workspace).
// Every create writes it, like nfsAgentDirRecordFile, so only an agent
// created before Cloud Run clone-per-agent agents got their own workspace
// can lack it. nfsAgentDirRecordFile cannot serve: creates have written it
// on every runtime since ptone/scion#3998, while Cloud Run agents kept
// working in the shared checkout.
const cloudRunAgentWorkspaceRecordFile = "cloudrun-agent-workspace.json"

// cloudRunKeepSharedCheckout reports whether a Cloud Run clone-per-agent
// agent keeps mounting the project's shared checkout instead of its own
// workspace, and why: only an existing agent that worked there before, i.e.
// a start that is not a create, with no cloudRunAgentWorkspaceRecordFile in
// agentDir, on a broker with the export mounted, where the agent's own
// workspace does not exist. As in nfsKeepSharedCheckout, a create, a
// record, an existing workspace, an unmounted export or any other stat
// error keep the agent's own workspace. The check only reads.
func cloudRunKeepSharedCheckout(fresh bool, agentDir string, resolved runtime.ResolvedWorkspace, agentName string) (bool, string) {
	if fresh {
		return false, ""
	}
	if _, err := os.Stat(filepath.Join(agentDir, cloudRunAgentWorkspaceRecordFile)); err == nil {
		return false, ""
	}
	sub, err := runtime.NFSAgentDirSubPath(resolved.ServerRelativePath, agentName)
	if err != nil || resolved.HostBase == "" {
		return false, ""
	}
	if _, err := os.Stat(resolved.HostBase); err != nil {
		return false, ""
	}
	if _, err := os.Lstat(filepath.Join(resolved.HostBase, sub, provision.AgentWorkspaceDir)); !errors.Is(err, fs.ErrNotExist) {
		return false, ""
	}
	return true, "existing Cloud Run agent without its own workspace keeps the shared checkout"
}

// recordCloudRunAgentWorkspace writes cloudRunAgentWorkspaceRecordFile. A
// failure is logged and does not fail the start: the workspace stat still
// decides next time.
func recordCloudRunAgentWorkspace(agentDir, agentName string) {
	if err := os.WriteFile(filepath.Join(agentDir, cloudRunAgentWorkspaceRecordFile), []byte("{\"layout\":\"agent-workspace\"}\n"), 0o600); err != nil {
		slog.Warn("cloudrun: could not record the agent's own workspace", "agent", agentName, "error", err)
	}
}

// recordNFSAgentDir writes nfsAgentDirRecordFile. A failure is logged and
// does not fail the start: the directory stat still decides next time.
func recordNFSAgentDir(agentDir, agentName string) {
	if err := os.WriteFile(filepath.Join(agentDir, nfsAgentDirRecordFile), []byte("{\"layout\":\"agent-dir\"}\n"), 0o600); err != nil {
		slog.Warn("workspace_storage nfs: could not record the agent's NFS agent directory", "agent", agentName, "error", err)
	}
}

// withEnvValue returns env with key set to value, replacing any entry for key.
func withEnvValue(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}
