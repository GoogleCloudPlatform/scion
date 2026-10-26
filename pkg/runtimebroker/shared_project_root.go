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

package runtimebroker

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Hub-managed projects on a shared storage backend (ptone/scion#5823).
//
// A hub-managed project dispatched by slug runs from the broker's Layout's
// project root, <host base>/<subpath root>/<project UUID>
// (projectfs.Layout.ProjectRoot), not from a broker-local
// ~/.scion/projects/<slug>: every broker of the project reads the same
// project configuration, <ProjectRoot>/.scion, which the hub seeds with
// settings.yaml, project-id and project-slug. The broker never writes that
// directory (it records its state for the workspace configuration check,
// keyed by the project root). Broker-private state stays broker-local:
// agent state dirs live in the external agents root
// ~/.scion/project-configs/<slug>__<id8>/.scion/agents for every sharing
// mode (config.AgentsRootForProject), and the ownership store and launch
// journal are keyed by project ID and agent, never by the project path.

// Refusal texts for a shared-backend dispatch (ptone/scion#5823). They name no host path except the share's mount point, which is
// the operator's own configuration.
const (
	sharedProjectNoIDText     = "a hub-managed project on shared project storage needs the project ID; upgrade the Hub"
	sharedProjectConfigText   = "project configuration missing or does not match; run project-storage-migrate"
	sharedProjectNoSlugText   = "project configuration has no project-slug; run project-storage-migrate"
	sharedProjectUnmountedFmt = "project storage share is not mounted on this broker (%s; mount it or set nfs.auto_mount)"
)

// sharedHubProjectPath returns the project path of a hub-managed project
// dispatched by slug when the broker's Layout is shared: the Layout's
// project root for projectID, after checking that the share is mounted and
// that <ProjectRoot>/.scion is the hub-seeded configuration of this project
// with a project-slug. ok is false (and the caller keeps today's
// ~/.scion/projects/<slug>) when the Layout is not shared. A failed check is
// a *startContextError: 503 for an unmounted share, 409 otherwise. Nothing
// is created or repaired.
func (s *Server) sharedHubProjectPath(ctx context.Context, projectID string) (path string, ok bool, sce *startContextError) {
	layout := s.config.StorageLayout
	if !layout.Backend.Shared() {
		return "", false, nil
	}
	if projectID == "" {
		return "", true, &startContextError{Status: http.StatusConflict, Message: sharedProjectNoIDText}
	}
	root, err := layout.ProjectRoot(projectID)
	if err != nil {
		return "", true, &startContextError{Status: http.StatusConflict, Message: sharedProjectConfigText, OriginalErr: err}
	}
	if err := s.ensureLayoutShareMounted(ctx, layout); err != nil {
		s.agentLifecycleLog.Error("Project storage share is not mounted on this broker", "project_id", projectID,
			"host_base", layout.HostBase, "error", err)
		return "", true, &startContextError{Status: http.StatusServiceUnavailable, Code: ErrCodeProjectStorageUnavailable,
			Message: fmt.Sprintf(sharedProjectUnmountedFmt, layout.HostBase), OriginalErr: err}
	}
	if sce := checkSharedProjectConfig(root.HostPath, projectID); sce != nil {
		s.agentLifecycleLog.Error("Shared project configuration refused", "project_id", projectID,
			"path", root.HostPath, "reason", sce.Message)
		return "", true, sce
	}
	return root.HostPath, true, nil
}

// checkSharedProjectConfig checks that projectRoot is a directory (not a
// symlink) holding the hub-seeded configuration of projectID:
// <projectRoot>/.scion is a directory whose project-id names projectID
// (config.IsSharedProjectConfigDir) and that has a valid project-slug.
func checkSharedProjectConfig(projectRoot, projectID string) *startContextError {
	if info, err := os.Lstat(projectRoot); err != nil || !info.IsDir() {
		return &startContextError{Status: http.StatusConflict, Message: sharedProjectConfigText, OriginalErr: err}
	}
	scionDir := filepath.Join(projectRoot, config.DotScion)
	if filepath.Base(projectRoot) != projectID || !config.IsSharedProjectConfigDir(scionDir) {
		return &startContextError{Status: http.StatusConflict, Message: sharedProjectConfigText}
	}
	if _, err := config.ReadProjectSlug(scionDir); err != nil {
		return &startContextError{Status: http.StatusConflict, Message: sharedProjectNoSlugText, OriginalErr: err}
	}
	return nil
}

// ensureLayoutShareMounted checks that the Layout's share is mounted at its
// host base: for nfs through the broker's NFS mount reconciler when it has
// one (EnsureShareMounted, which mounts only with auto_mount on), otherwise
// by the host base being a directory. A broker without the mount refuses
// shared-backend dispatches (ptone/scion#5823).
func (s *Server) ensureLayoutShareMounted(ctx context.Context, layout projectfs.Layout) error {
	if layout.Backend == projectfs.BackendNFS && s.nfsMountReconciler != nil {
		if err := s.nfsMountReconciler.EnsureShareMounted(ctx, layout.Share.ID); err != nil {
			return err
		}
	}
	info, err := os.Stat(layout.HostBase)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", layout.HostBase)
	}
	return nil
}

// isSharedHubProjectRoot reports whether projectPath is a hub-managed
// project root on a shared backend: its .scion is a shared project
// configuration (config.IsSharedProjectConfigDir: the UUID-keyed tree only
// the hub seeds).
func isSharedHubProjectRoot(projectPath string) bool {
	if projectPath == "" || filepath.Base(projectPath) == config.DotScion {
		return false
	}
	return config.IsSharedProjectConfigDir(filepath.Join(projectPath, config.DotScion))
}

// findAgentInSharedProject resolves, on the broker's shared Layout, the
// configuration dir (<ProjectRoot>/.scion) of hub-managed project projectID
// when agent agentName has state of it on this broker: in the broker-local
// external agents root, or in its retired pre-move legacy dir (found so a
// delete removes it and a start is refused cleanly, ptone/scion#5835;
// config.LegacySharedProjectAgentDir). handled is true for
// every request on a shared Layout with a project ID; the caller then uses
// dir and err as the answer and never falls back to the broker-local
// project scan. It is fail-closed like a dispatch: the share is checked
// mounted (EnsureShareMounted) and the configuration checked as the
// project's, with a project-slug (checkSharedProjectConfig); any failure is
// errDeleteTargetUnknown, so a delete is not acknowledged as done. dir is ""
// (with no error) only when the checks pass and the agent has no state of
// the project here.
//
// probeMount runs the share mount check (EnsureShareMounted, which may
// reconcile or mount); without it (the own-runtime look-up on every
// existing-agent request) only the host base is checked to be a directory.
func (s *Server) findAgentInSharedProject(ctx context.Context, agentName, projectID string, probeMount bool) (dir string, handled bool, err error) {
	layout := s.config.StorageLayout
	if !layout.Backend.Shared() || projectID == "" {
		return "", false, nil
	}
	if !isSingleCleanPathElement(agentName) {
		return "", true, nil
	}
	root, err := layout.ProjectRoot(projectID)
	if err != nil {
		return "", true, fmt.Errorf("%w: resolve project root: %v", errDeleteTargetUnknown, err)
	}
	var mountErr error
	if probeMount {
		mountCtx, cancel := context.WithTimeout(ctx, sharedLookupMountTimeout)
		mountErr = s.ensureLayoutShareMounted(mountCtx, layout)
		cancel()
	} else if info, statErr := os.Stat(layout.HostBase); statErr != nil {
		mountErr = statErr
	} else if !info.IsDir() {
		mountErr = fmt.Errorf("%s is not a directory", layout.HostBase)
	}
	if err := mountErr; err != nil {
		s.agentLifecycleLog.Error("Agent look-up: project storage share is not mounted on this broker", "agent", agentName,
			"project_id", projectID, "host_base", layout.HostBase, "error", err)
		return "", true, fmt.Errorf("%w: project storage share is not mounted", errDeleteTargetUnknown)
	}
	if sce := checkSharedProjectConfig(root.HostPath, projectID); sce != nil {
		s.agentLifecycleLog.Error("Agent look-up: shared project configuration refused", "agent", agentName,
			"project_id", projectID, "reason", sce.Message)
		return "", true, fmt.Errorf("%w: %s", errDeleteTargetUnknown, sce.Message)
	}
	scionDir := filepath.Join(root.HostPath, config.DotScion)
	agents, err := config.AgentsRootForProject(scionDir, false, projectID)
	if err != nil {
		return "", true, fmt.Errorf("%w: %v", errDeleteTargetUnknown, err)
	}
	if _, statErr := os.Lstat(filepath.Join(agents, agentName)); statErr == nil {
		return scionDir, true, nil
	} else if !os.IsNotExist(statErr) {
		return "", true, fmt.Errorf("%w: check agent state: %v", errDeleteTargetUnknown, statErr)
	}
	if config.LegacySharedProjectAgentDir(scionDir, agentName) != "" {
		return scionDir, true, nil
	}
	return "", true, nil
}

// sharedLookupMountTimeout bounds the share mount check of an agent look-up
// on a request path.
const sharedLookupMountTimeout = 30 * time.Second

// sharedProjectsWithLocalAgents returns the configuration dirs
// (<ProjectRoot>/.scion) of the projects on the broker's shared Layout for
// which this broker keeps agent state (a non-empty external agents root),
// so start-up discovery reads their settings as it reads the broker-local
// hub-managed projects'. It lists the Layout's project tree once; nil when
// the Layout is not shared or the tree cannot be read.
func (s *Server) sharedProjectsWithLocalAgents() []string {
	layout := s.config.StorageLayout
	if !layout.Backend.Shared() {
		return nil
	}
	// The project tree is the parent of any project root; the Layout
	// validates and defaults the subpath root.
	probe, err := layout.ProjectRoot("00000000-0000-0000-0000-000000000000")
	if err != nil {
		return nil
	}
	tree := filepath.Dir(probe.HostPath)
	entries, err := os.ReadDir(tree)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		scionDir := filepath.Join(tree, e.Name(), config.DotScion)
		if !config.IsSharedProjectConfigDir(scionDir) {
			continue
		}
		root, err := config.AgentsRootForProject(scionDir, false, e.Name())
		if err != nil {
			continue
		}
		if agents, err := os.ReadDir(root); err == nil && len(agents) > 0 {
			out = append(out, scionDir)
		}
	}
	return out
}

// sharedBootstrapWorkspace returns the target of a workspace bootstrap for
// a hub-managed project on the broker's shared Layout (req.ProjectPath is
// the project's root, set by createAgent): the project's workspace Mount as
// the agent manager's backend resolves it, Layout.Resolve(workspace) =
// <ProjectRoot>/workspace, with its path relative to the Layout's host
// base. ok is false for any other request, which keeps the broker-local
// target (ptone/scion#5823).
func (s *Server) sharedBootstrapWorkspace(req CreateAgentRequest) (ws, rel string, ok bool, err error) {
	layout := s.config.StorageLayout
	if req.ProjectSlug == "" || !layout.Backend.Shared() || !isSharedHubProjectRoot(req.ProjectPath) {
		return "", "", false, nil
	}
	pr, err := layout.ProjectRoot(req.ProjectID)
	if err != nil {
		return "", "", true, err
	}
	if filepath.Clean(pr.HostPath) != filepath.Clean(req.ProjectPath) {
		return "", "", true, fmt.Errorf("project path is not the root of project %s on the shared layout", req.ProjectID)
	}
	r, err := layout.Resolve(projectfs.Project{ID: req.ProjectID}, "", projectfs.WorkspaceMount(projectfs.ModeSharedPlain, ""))
	if err != nil {
		return "", "", true, err
	}
	return r.HostPath, r.RelPath, true, nil
}

// sharedWorkspaceProvisioned reports whether the shared workspace at ws is
// in use, so a workspace bootstrap (a sync that makes the target match the
// upload) must never be applied to it: it carries the provisioning sentinel
// (in the project state directory, or at the workspace root where an older
// layout kept it), or it holds any entry other than provisioning
// bookkeeping (provision.IsProvisioningArtifact; content a migration, the hub or an agent placed there without a
// sentinel). An unreadable workspace counts as in use.
func sharedWorkspaceProvisioned(ws string) bool {
	if provision.SentinelPresent(provision.ProjectStateDir(ws), ws) {
		return true
	}
	entries, err := os.ReadDir(ws)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	for _, e := range entries {
		if !provision.IsProvisioningArtifact(e.Name()) {
			return true
		}
	}
	return false
}

// applySharedBootstrap applies a workspace bootstrap to the shared
// workspace at ws (rel below the Layout's host base) under the project's
// provisioning locks (provision.WithWorkspaceProvisionLocks, the locks every
// provisioner of the workspace takes): when the workspace is already in use
// (sharedWorkspaceProvisioned: sentinel, or any content) it is left alone
// (skipped); otherwise apply
// places the upload and the sentinel is written, so later provisioners
// reuse it. The workspace and its state directory are created first with
// the broker's leaf contract (shareddirs.EnsureLeaf).
func (s *Server) applySharedBootstrap(ctx context.Context, ws, rel string, apply func(ctx context.Context) error) (skipped bool, err error) {
	base, err := filepath.EvalSymlinks(s.config.StorageLayout.HostBase)
	if err != nil {
		return false, fmt.Errorf("resolve host base: %w", err)
	}
	stateRel := filepath.Join(filepath.Dir(rel), provision.ProvisionStateDirName)
	for _, leaf := range []string{rel, stateRel} {
		fd, _, err := shareddirs.EnsureLeaf(base, leaf)
		if err != nil {
			return false, fmt.Errorf("create %s: %w", filepath.Base(leaf), err)
		}
		if err := shareddirs.CloseFd(fd); err != nil {
			return false, err
		}
	}
	wsPath := filepath.Join(base, rel)
	err = provision.WithWorkspaceProvisionLocks(ctx, wsPath, func(lockCtx context.Context) error {
		if sharedWorkspaceProvisioned(wsPath) {
			skipped = true
			return nil
		}
		if err := apply(lockCtx); err != nil {
			return err
		}
		// The sentinel is irreversible: never write it under a lost lock.
		if err := lockCtx.Err(); err != nil {
			return fmt.Errorf("provisioning lock lost: %w", err)
		}
		return provision.MarkProvisioned(provision.ProjectStateDir(wsPath))
	})
	return skipped, err
}

// localContainerRuntime reports whether runtimeName runs agents as local
// containers on the broker host (Docker, Podman, Apple container), whose
// workspace bind mount the broker itself prepares.
func localContainerRuntime(runtimeName string) bool {
	switch runtimeName {
	case "docker", "podman", "container":
		return true
	}
	return false
}

// provisionSharedPlainWorkspace is the shared-plain provisioning backstop
// for a git project on the broker's shared Layout dispatched to a local
// container runtime (ptone/scion#5823): before the agent starts, the broker
// runs provision.ProvisionShared for the project's workspace on the share,
// exactly as the Kubernetes provisioning init container lays it out (the
// sentinel and lock in the project state directory, the workspace root as
// the legacy sentinel location, ownership from the Layout, a failed chown
// not fatal). An already provisioned workspace (sentinel present, as after
// the hub's provisioning at project create) takes the reuse path; a lost
// sentinel or a project from before that provisioning gets the clone, under
// the provisioning lock. The clone gets the agent's git token only for the
// clone URL's own host. It reports whether it provisioned (the caller then
// suppresses the in-container clone); false with no error when the request
// is not such a dispatch.
func (s *Server) provisionSharedPlainWorkspace(ctx context.Context, in startContextInputs, env map[string]string) (bool, error) {
	layout := s.config.StorageLayout
	if !layout.Backend.Shared() || !isSharedHubProjectRoot(in.ProjectPath) || in.Config == nil || in.Config.GitClone == nil {
		return false, nil
	}
	gc := *in.Config.GitClone
	if err := provision.CheckHostCloneURL(gc.URL); err != nil {
		return false, err
	}
	r, err := layout.Resolve(projectfs.Project{ID: in.ProjectID}, "", projectfs.WorkspaceMount(projectfs.ModeSharedPlain, ""))
	if err != nil {
		return false, err
	}
	base, err := filepath.EvalSymlinks(layout.HostBase)
	if err != nil {
		return false, fmt.Errorf("resolve host base: %w", err)
	}
	stateRel := filepath.Join(filepath.Dir(r.RelPath), provision.ProvisionStateDirName)
	for _, leaf := range []string{r.RelPath, stateRel} {
		fd, _, err := shareddirs.EnsureLeaf(base, leaf)
		if err != nil {
			return false, fmt.Errorf("create %s: %w", filepath.Base(leaf), err)
		}
		if err := shareddirs.CloseFd(fd); err != nil {
			return false, err
		}
	}
	ws := filepath.Join(base, r.RelPath)
	stateDir := provision.ProjectStateDir(ws)
	uid, gid := provision.DefaultOwnerID(layout.UID), provision.DefaultOwnerID(layout.GID)
	if err := provision.PrepareStateDir(stateDir, uid, gid, false); err != nil {
		return false, err
	}
	err = provisionSharedFn(provision.ProvisionInput{
		Ctx:                 ctx,
		Resolved:            provision.ResolvedWorkspace{HostPath: ws, ServerRelativePath: r.RelPath, HostBase: base, Backend: string(layout.Backend)},
		ProjectID:           in.ProjectID,
		AgentID:             in.AgentID,
		Mode:                store.SharingModeSharedPlain,
		GitClone:            &gc,
		CloneToken:          env[provision.GitTokenEnv],
		CloneTokenRemote:    gc.URL,
		HostGitHardened:     true,
		NFSUID:              uid,
		NFSGID:              gid,
		SentinelDir:         stateDir,
		LegacyDir:           ws,
		RequireChownSuccess: false,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// provisionSharedFn is provision.ProvisionShared; a variable so tests can
// observe the input without cloning.
var provisionSharedFn = provision.ProvisionShared
