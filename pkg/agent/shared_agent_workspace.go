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
	"slices"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// agentRuntimeKey carries the agent manager's runtime name
// (contextWithAgentRuntime).
type agentRuntimeKey struct{}

// contextWithAgentRuntime records the runtime the agent manager starts the
// agent on, for decisions made below Start, Provision and Reprovision
// (sharedLayoutAgentWorkspace).
func contextWithAgentRuntime(ctx context.Context, runtimeName string) context.Context {
	return context.WithValue(ctx, agentRuntimeKey{}, runtimeName)
}

// agentRuntimeFromContext is the runtime name contextWithAgentRuntime
// recorded, or "" when none was.
func agentRuntimeFromContext(ctx context.Context) string {
	v, _ := ctx.Value(agentRuntimeKey{}).(string)
	return v
}

// runtimeName is the manager's runtime name, or "" without a runtime.
func (m *AgentManager) runtimeName() string {
	if m == nil || m.Runtime == nil {
		return ""
	}
	return m.Runtime.Name()
}

// clonePerAgentWorkspaceKey marks a provisioning or start context whose
// agent uses the clone-per-agent sharing mode (contextWithClonePerAgentWorkspace).
type clonePerAgentWorkspaceKey struct{}

// isClonePerAgentStart reports whether opts asks for a clone-per-agent
// workspace: a git clone, with the clone-per-agent sharing mode in the
// request env (SCION_WORKSPACE_MODE). A git clone in any other mode (for
// example the worktree-per-agent dispatch on Kubernetes, which also
// carries the clone) is not.
func isClonePerAgentStart(opts api.StartOptions) bool {
	return opts.GitClone != nil &&
		store.ResolveWorkspaceSharingMode(opts.Env["SCION_WORKSPACE_MODE"]) == store.SharingModeClonePerAgent
}

// cloudRunClonePerAgentStart reports whether a start is a Cloud Run
// clone-per-agent start whose workspace the broker chose (no explicit
// workspace). The cloudrun runtime mounts such an agent's own workspace on
// the share (agents/<slug>/workspace, created by provisionCloudRunNFS),
// which the agent clones into, so the broker-local <agentDir>/workspace is
// never mounted.
func cloudRunClonePerAgentStart(runtimeName string, opts api.StartOptions, explicitWorkspace bool) bool {
	return isCloudRunRuntime(runtimeName) && !explicitWorkspace && isClonePerAgentStart(opts)
}

// contextWithClonePerAgentWorkspace marks ctx when opts asks for a
// clone-per-agent workspace (isClonePerAgentStart).
func contextWithClonePerAgentWorkspace(ctx context.Context, opts api.StartOptions) context.Context {
	if !isClonePerAgentStart(opts) {
		return ctx
	}
	return context.WithValue(ctx, clonePerAgentWorkspaceKey{}, true)
}

// isClonePerAgentWorkspace reports whether ctx was marked by
// contextWithClonePerAgentWorkspace.
func isClonePerAgentWorkspace(ctx context.Context) bool {
	v, _ := ctx.Value(clonePerAgentWorkspaceKey{}).(bool)
	return v
}

// agentScopedWorkspace reports whether ctx's agent uses an agent-scoped
// sharing mode, empty-per-agent or clone-per-agent: the modes whose
// workspace is the agent's own directory (sharedLayoutAgentWorkspace).
func agentScopedWorkspace(ctx context.Context) bool {
	return api.IsEmptyPerAgentWorkspaceFromContext(ctx) || isClonePerAgentWorkspace(ctx)
}

// localRuntimeAgentDirOnShare reports whether a start on runtimeName, a
// local container runtime (docker, podman, container), places an
// agent-scoped workspace (clone-per-agent or empty-per-agent) of the
// project in projectDir in the agent's own directory on the share: the
// project's configuration is on a shared Layout
// (config.IsSharedProjectConfigDir). The runtime bind-mounts that
// directory from the broker's own mount of the share; Start's NFS leaf
// helpers create it first, as they do for Kubernetes.
func localRuntimeAgentDirOnShare(runtimeName, projectDir string) bool {
	return isLocalContainerRuntime(runtimeName) && config.IsSharedProjectConfigDir(projectDir)
}

// agentDirOnShare reports whether a start on runtimeName may place an
// agent-scoped workspace of the project in projectDir in the agent's own
// directory on the share (<subpath_root>/<projectID>/agents/<agent>):
// always on Kubernetes, and on a local container runtime for a project on a
// shared Layout (localRuntimeAgentDirOnShare). Every decision about that
// directory (placement, selection, creation, removal) uses this predicate,
// so they agree.
func agentDirOnShare(runtimeName, projectDir string) bool {
	return isKubernetesRuntime(runtimeName) || localRuntimeAgentDirOnShare(runtimeName, projectDir)
}

// sharedLayoutAgentWorkspace returns the workspace directory of an
// agent-scoped workspace (clone-per-agent or empty-per-agent) of a project
// whose configuration is on a shared Layout: <AgentRoot>/workspace, where
// AgentRoot is projectfs.Layout.AgentRoot(projectID, agentName)
// (<HostBase>/<subpath_root>/<projectID>/agents/<agent>), the directory
// runtime.NFSAgentDirSubPath and the NFS workspace leaf helpers address.
//
// The agent's broker-private state directory (agentDir) is broker-local for
// such a project (config.AgentsRootForProject), so the workspace is never
// derived from it: <agentDir>/workspace would put the agent's workspace off
// the share, outside the project's tree.
//
// It returns "" (and no error) when the rule does not apply, and the caller
// keeps <agentDir>/workspace:
//   - the agent's runtime (agentRuntimeFromContext) does not place
//     agent-scoped workspaces on the share (agentDirOnShare): only
//     Kubernetes and the local container runtimes do, and Start's NFS leaf
//     helpers create the agent's directory for both. Every other runtime,
//     including a caller that recorded none, keeps the broker-local
//     workspace it had before;
//   - projectDir is not a shared project configuration directory
//     (config.IsSharedProjectConfigDir);
//   - agentDir is the agent's legacy in-project directory
//     (config.LegacySharedProjectAgentDir), whose workspace stays where it
//     is until the agent is recreated (legacyAgentWorkspaceRoot);
//   - the project storage Layout is not a shared one.
//
// The Layout is built from settings (config.ResolveProjectStorage and
// config.ProjectStorageLayout), or loaded for projectDir when settings is
// nil. A Layout that cannot be built, or whose project root is not the
// directory holding projectDir, is an error: the start is refused rather
// than placing the workspace somewhere else.
func sharedLayoutAgentWorkspace(ctx context.Context, settings *config.VersionedSettings, projectDir, agentName, agentDir string) (string, error) {
	if !agentDirOnShare(agentRuntimeFromContext(ctx), projectDir) {
		return "", nil
	}
	if !config.IsSharedProjectConfigDir(projectDir) {
		return "", nil
	}
	if legacy := config.LegacySharedProjectAgentDir(projectDir, agentName); legacy != "" && samePath(legacy, agentDir) {
		return "", nil
	}
	if settings == nil {
		vs, _, err := config.LoadEffectiveSettingsFor(ctx, projectDir)
		if err != nil {
			return "", fmt.Errorf("agent workspace on the shared project tree: load settings: %w", err)
		}
		settings = vs
	}
	var srv *config.V1ServerConfig
	if settings != nil {
		srv = settings.Server
	}
	ps, _, err := config.ResolveProjectStorage(srv)
	if err != nil {
		return "", fmt.Errorf("agent workspace on the shared project tree: %w", err)
	}
	layout, err := config.ProjectStorageLayout(ps, config.DefaultVolumeMountBase)
	if err != nil {
		return "", fmt.Errorf("agent workspace on the shared project tree: %w", err)
	}
	if !layout.Backend.Shared() {
		return "", nil
	}
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return "", fmt.Errorf("agent workspace on the shared project tree: %w", err)
	}
	// IsSharedProjectConfigDir checked that the parent's name is the
	// project ID recorded in the configuration.
	projectID := filepath.Base(filepath.Dir(abs))
	projectRoot, err := layout.ProjectRoot(projectID)
	if err != nil {
		return "", fmt.Errorf("agent workspace on the shared project tree: %w", err)
	}
	if !samePath(projectRoot.HostPath, filepath.Dir(abs)) {
		return "", fmt.Errorf("agent workspace on the shared project tree: the project storage layout places project %s at %s, not at %s; refusing to start", projectID, projectRoot.HostPath, filepath.Dir(abs))
	}
	agentRoot, err := layout.AgentRoot(projectID, agentName)
	if err != nil {
		return "", fmt.Errorf("agent workspace on the shared project tree: %w", err)
	}
	return filepath.Join(agentRoot.HostPath, "workspace"), nil
}

// ErrAgentWorkspaceOffShare refuses a local container runtime start of an
// agent-scoped workspace (clone-per-agent or empty-per-agent) on a shared
// Layout whose workspace is not in the agent's own directory on the share:
// an agent created before its project storage moved to the shared Layout
// (checkLocalRuntimeAgentWorkspace). It is an agent-state conflict
// (config.IsAgentStateConflict), so brokers answer it with 409 and its
// message, which names no path; the broker log has the paths.
var ErrAgentWorkspaceOffShare error = agentWorkspaceOffShareError{}

type agentWorkspaceOffShareError struct{}

func (agentWorkspaceOffShareError) Error() string {
	return "this agent's workspace is not in its directory on the shared project storage, so the agent was not started and the workspace was left as it is. " +
		"The Runtime Broker log names the workspace; an operator can move its work out. Deleting the agent removes that workspace; " +
		"recreate the agent to start it on the shared project storage"
}

func (agentWorkspaceOffShareError) Unwrap() error { return config.ErrAgentStateConflict }

// provisioningOnlyEntries are the entries provisioning may leave in an
// otherwise empty agent workspace (the same set isWorkspaceEmptyDir skips).
var provisioningOnlyEntries = []string{".scion", ".scion-volumes"}

// workspaceHoldsOnlyProvisioningEntries reports whether dir, a directory,
// holds nothing but provisioningOnlyEntries. Unlike isWorkspaceEmptyDir it
// fails closed: any read error is returned, never read as "empty".
func workspaceHoldsOnlyProvisioningEntries(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !slices.Contains(provisioningOnlyEntries, e.Name()) {
			return false, nil
		}
	}
	return true, nil
}

// checkLocalRuntimeAgentWorkspace is Start's check, for a local container
// runtime, before the NFS leaf helpers create the agent's own directory
// dirName on the share (<project>/agents/<dirName>, nfsOwnAgentDirRel of
// resolved). It admits the start only when:
//   - workspace, the workspace Start validated, is that directory's
//     workspace, and
//   - that workspace exists as a real directory, or it does not exist and
//     the agent's broker-local <agentDir>/workspace either does not exist or
//     is a real directory that reads without error and holds nothing but
//     the entries provisioning may leave there (.scion, .scion-volumes;
//     workspaceHoldsOnlyProvisioningEntries). Such a workspace never held
//     the agent's work: a clone-per-agent agent whose start was refused
//     before its workspace moved to the share never cloned into it. It is
//     left as it is.
//
// Otherwise it returns ErrAgentWorkspaceOffShare, or the error of a check of
// the share's workspace that fails for a reason other than its absence. It
// fails closed: a broker-local workspace that cannot be read, or is not a
// real directory, refuses. The cases it refuses:
//   - an agent provisioned before its project storage moved to the shared
//     Layout, in its legacy in-project directory: its workspace is not the
//     share's. Local container runtimes on the nfs backend refuse such an
//     agent although the workspace source guard admits its legacy root
//     (legacyAgentWorkspaceRoot). Before, such a start mounted the project's
//     shared checkout (the nfs backend's path) rather than that workspace,
//     so its work may be in either;
//   - an agent provisioned before the move in the external agents root,
//     whose work may still be in <agentDir>/workspace.
//
// Nothing is created on the share and no local workspace is changed; the
// workspace source guard and its roots are unchanged. The paths go to the
// broker log.
func checkLocalRuntimeAgentWorkspace(agentName, workspace, agentDir string, resolved runtime.ResolvedWorkspace, dirName string) error {
	agentRel, err := nfsOwnAgentDirRel(resolved, dirName)
	if err != nil {
		return err
	}
	// The workspace Start validated is symlink-free; so is the share's
	// path once its mount point is resolved (nothing below it may be a
	// symlink: the leaf helpers refuse one).
	hostBase := resolved.HostBase
	if resolvedBase, err := filepath.EvalSymlinks(hostBase); err == nil {
		hostBase = resolvedBase
	}
	shareWS := filepath.Join(hostBase, agentRel, provision.AgentWorkspaceDir)
	refuse := func(old, why string) error {
		slog.Warn("start refused: the agent's workspace is not in its directory on the shared project storage; it is left as it is",
			"agent", agentName, "workspace", old, "share_workspace", shareWS, "reason", why)
		return ErrAgentWorkspaceOffShare
	}
	if filepath.Clean(workspace) != shareWS && !samePath(workspace, shareWS) {
		return refuse(workspace, "the validated workspace is not the agent's directory on the share")
	}
	shareInfo, err := os.Lstat(shareWS)
	switch {
	case err == nil && shareInfo.IsDir():
		warnLocalWorkOffShare(agentName, agentDir, shareWS)
		return nil
	case err == nil:
		return refuse(shareWS, "the agent's workspace on the share is not a directory")
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("check the agent's workspace on the shared project storage: %w", err)
	}
	if agentDir == "" {
		return nil
	}
	local := filepath.Join(agentDir, provision.AgentWorkspaceDir)
	if samePath(local, shareWS) {
		return nil
	}
	info, err := os.Lstat(local)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return refuse(local, "the broker-local workspace cannot be checked: "+err.Error())
	case !info.IsDir():
		return refuse(local, "the broker-local workspace is not a directory")
	}
	if only, err := workspaceHoldsOnlyProvisioningEntries(local); err != nil {
		return refuse(local, "the broker-local workspace cannot be read: "+err.Error())
	} else if !only {
		return refuse(local, "the broker-local workspace holds work")
	}
	return nil
}

// warnLocalWorkOffShare logs, for an agent admitted on its existing
// workspace on the share (checkLocalRuntimeAgentWorkspace), when its
// broker-local <agentDir>/workspace still holds more than provisioning
// entries: that work is not mounted, and deleting the agent removes it. A
// pre-switch agent whose share leaf already existed for another reason (an
// operator's mkdir, an earlier agent of the same name) is then not
// unnoticed. It never blocks the start: a check that fails is logged too.
// The paths go to the broker log only.
func warnLocalWorkOffShare(agentName, agentDir, shareWS string) {
	if agentDir == "" {
		return
	}
	local := filepath.Join(agentDir, provision.AgentWorkspaceDir)
	if samePath(local, shareWS) {
		return
	}
	info, err := os.Lstat(local)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return
	case err != nil:
		slog.Warn("could not check the agent's broker-local workspace; the agent starts on its directory on the shared project storage",
			"agent", agentName, "workspace", local, "error", err)
		return
	case !info.IsDir():
		slog.Warn("the agent's broker-local workspace is not a directory; the agent starts on its directory on the shared project storage",
			"agent", agentName, "workspace", local)
		return
	}
	only, err := workspaceHoldsOnlyProvisioningEntries(local)
	switch {
	case err != nil:
		slog.Warn("could not read the agent's broker-local workspace; the agent starts on its directory on the shared project storage",
			"agent", agentName, "workspace", local, "error", err)
	case !only:
		slog.Warn("the agent's broker-local workspace still holds files while it runs on its directory on the shared project storage; they are not mounted, and deleting the agent removes them",
			"agent", agentName, "workspace", local, "share_workspace", shareWS)
	}
}

// startWorkspaceSharingMode is the sharing mode Start selects the workspace
// backend with, before the Kubernetes worktree adjustment: shared-plain for
// a shared workspace or a git clone, empty-per-agent for an empty-per-agent
// agent (it has no shared checkout: without NFS storage it is node-local, or
// pod-local EmptyDir on Kubernetes; with NFS storage it gets its own agent
// directory on the export, design #2703 P3, which only the Kubernetes
// runtime can mount, see nfsEmptyAgentDirSelection), and worktree-per-agent
// otherwise.
func startWorkspaceSharingMode(opts api.StartOptions, emptyPerAgent bool) store.WorkspaceSharingMode {
	switch {
	case emptyPerAgent:
		return store.SharingModeEmptyPerAgent
	case opts.SharedWorkspace || opts.GitClone != nil:
		return store.SharingModeSharedPlain
	default:
		return store.SharingModeWorktreePerAgent
	}
}

// startUsesNFSWorkspaceBackend reports whether Start lays out the workspace
// through the nfs workspace backend (runtime.SelectWorkspaceBackend). The
// Kubernetes worktree adjustment Start applies later (shared-plain to
// worktree-per-agent) does not change the answer: the nfs backend serves
// both modes. Today the mode argument cannot change the nfs answer for any
// mode Start passes (shared-plain, worktree-per-agent, empty-per-agent all
// select nfs when the backend is nfs); it is passed so the answer stays
// Start's own if SelectWorkspaceBackend changes.
func startUsesNFSWorkspaceBackend(settings *config.VersionedSettings, opts api.StartOptions, emptyPerAgent bool) bool {
	if settings == nil || settings.Server == nil {
		return false
	}
	ws := settings.Server.EffectiveWorkspaceStorage()
	if ws == nil {
		return false
	}
	return runtime.SelectWorkspaceBackend(ws, startWorkspaceSharingMode(opts, emptyPerAgent)).Name() == "nfs"
}

// checkStartWorkspaceExists is Start's safety net before the workspace is
// mounted: GetAgent should have created or recreated workspace, but if it is
// still missing the start fails with a clear error rather than a cryptic
// mount error from the container runtime.
//
// A Kubernetes start through the nfs workspace backend
// (startUsesNFSWorkspaceBackend) skips it. There the workspace is created
// later in Start, by the NFS leaf helpers (which create the leaf, with its
// atomic claim, or return an error), by the Kubernetes workspace init
// container, or the start is refused (errEmptyPerAgentNFSNoClaim). An
// agent-scoped workspace on a shared Layout does not exist yet on a fresh
// agent, because provisioning leaves it to those helpers.
//
// A local container runtime start of an agent-scoped workspace of a project
// on a shared Layout (localRuntimeAgentDirOnShare) through the nfs backend
// skips it for the same reason: the leaf helpers create the agent's
// directory on the share before the bind mount, or the start is refused
// (checkLocalRuntimeAgentWorkspace, the leaf helpers' own errors). Every
// other start keeps the check, including any other nfs start on a local
// container runtime: nothing creates the workspace there, and a bind mount
// of a missing path could create it with the wrong owner.
func checkStartWorkspaceExists(runtimeName, projectDir, workspace string, settings *config.VersionedSettings, opts api.StartOptions, emptyPerAgent bool) error {
	if workspace == "" || (isKubernetesRuntime(runtimeName) && startUsesNFSWorkspaceBackend(settings, opts, emptyPerAgent)) {
		return nil
	}
	if (emptyPerAgent || isClonePerAgentStart(opts)) && localRuntimeAgentDirOnShare(runtimeName, projectDir) &&
		startUsesNFSWorkspaceBackend(settings, opts, emptyPerAgent) {
		return nil
	}
	if _, err := os.Stat(workspace); os.IsNotExist(err) {
		return fmt.Errorf("workspace directory does not exist: %s (try deleting and recreating the agent)", workspace)
	}
	return nil
}
