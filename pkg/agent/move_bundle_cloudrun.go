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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// --- Cloud Run (NFS) runtime ---

type cloudRunMoveBundleRuntime struct{}

const (
	cloudRunNonDurableReason      = "Cloud Run requires NFS project storage for bundle moves"
	cloudRunMissingIdentityReason = "agent ID and project ID are required for Cloud Run NFS bundle moves"
	cloudRunSharedCheckoutReason  = "shared layout workspace is not supported for bundle moves"
)

func (cloudRunMoveBundleRuntime) Configured() bool {
	return cloudRunMoveBundleConfigured()
}

func (cloudRunMoveBundleRuntime) UsesNFSWorkspaceTarget() bool {
	return true
}

// CheckSourceDurability evaluates whether a Cloud Run Instances agent has both
// its per-agent workspace (<subpath_root>/<projectID>/agents/<slug>/workspace)
// and home leaf (<subpath_root>/<projectID>/agents/<slug>/home-<agentID>) on
// the broker-mounted NFS share.
//
// On Cloud Run, Stop calls StopInstance (cloudrun_runtime.go:1940), which
// leaves the Filestore/NFS share untouched. Both workspace and home live on
// the same workspace NFS share (cloudrun_home.go:35-36; cloudrun_runtime.go:
// 1634-1696, 1806-1851); project_storage.homes does not apply.
func (cloudRunMoveBundleRuntime) CheckSourceDurability(ctx context.Context, in MoveBundleRuntimeInput) (MoveBundleDurability, error) {
	wsBackend, resolvedWSBase, rel, _, err := k8sResolveNFSWorkspaceBackend(ctx, in.ProjectPath, in.ProjectID, in.AgentSlug)
	if err != nil {
		if !k8sProjectUsesNFSWorkspace(ctx, in.ProjectPath) {
			return MoveBundleDurability{Durable: false, Reason: cloudRunNonDurableReason}, nil
		}
		if !shareddirs.ValidProjectID(in.ProjectID) || !validHomeAgentID(in.AgentID, in.AgentSlug) {
			return MoveBundleDurability{Durable: false, Reason: cloudRunMissingIdentityReason}, nil
		}
		return MoveBundleDurability{Durable: false, Reason: k8sNFSNotMountedReason}, nil
	}
	if wsBackend != "nfs" {
		return MoveBundleDurability{Durable: false, Reason: cloudRunNonDurableReason}, nil
	}
	if resolvedWSBase == "" {
		return MoveBundleDurability{Durable: false, Reason: k8sNFSNotMountedReason}, nil
	}
	if !shareddirs.ValidProjectID(in.ProjectID) || !validHomeAgentID(in.AgentID, in.AgentSlug) {
		return MoveBundleDurability{Durable: false, Reason: cloudRunMissingIdentityReason}, nil
	}

	// Verify broker-local agentDir exists (holds .scion-move-bundle-home.json,
	// cloudrun-agent-workspace.json, .scion-move).
	agentDir, err := resolveK8sBrokerAgentDir(ctx, in)
	if err != nil {
		return MoveBundleDurability{}, err
	}
	if _, statErr := os.Stat(agentDir); errors.Is(statErr, fs.ErrNotExist) {
		return MoveBundleDurability{}, fs.ErrNotExist
	}

	// Legacy Cloud Run agents created before per-agent workspaces lack
	// cloudrun-agent-workspace.json and agents/<slug>/workspace and still work
	// in the project's shared checkout (cloudRunKeepSharedCheckout). Refuse
	// bundling a shared checkout.
	agentRel, err := runtime.NFSAgentDirSubPath(rel, in.AgentSlug)
	if err != nil {
		return MoveBundleDurability{Durable: false, Reason: cloudRunMissingIdentityReason}, nil
	}
	wsDir := filepath.Join(resolvedWSBase, agentRel, provision.AgentWorkspaceDir)
	if _, recErr := os.Stat(filepath.Join(agentDir, cloudRunAgentWorkspaceRecordFile)); errors.Is(recErr, fs.ErrNotExist) {
		if _, wsErr := os.Lstat(wsDir); errors.Is(wsErr, fs.ErrNotExist) {
			return MoveBundleDurability{Durable: false, Reason: cloudRunSharedCheckoutReason}, nil
		}
	}

	// Verify the home leaf path can be resolved via the canonical layout helper.
	if _, _, err := cloudRunResolveNFSHomeDir(ctx, in.ProjectPath, resolvedWSBase, in.ProjectID, in.AgentSlug, in.AgentID); err != nil {
		return MoveBundleDurability{Durable: false, Reason: cloudRunMissingIdentityReason}, nil
	}

	return MoveBundleDurability{Durable: true}, nil
}

// ResolveDirs resolves broker-local AgentDir, NFS per-agent WorkspaceDir,
// NFS HomeDir (<exportAgentDir>/home-<agentID>), ExportAgentDir, and
// Cloud Run container ownership for precheck, export, import, and commit.
//
// Cloud Run instances run as uid:gid = DefaultOwnerID(nfs.uid) :
// DefaultOwnerID(nfs.gid) (cloudRunOwnerIDs in pkg/runtime/cloudrun_runtime.go:
// 522-527, AdvertisedHostOwnerIDs in pkg/runtime/common.go:3046-3057), and
// provisionCloudRunNFS / provisionCloudRunHome chown both workspace and home
// leaves to that same uid:gid pair.
func (cloudRunMoveBundleRuntime) ResolveDirs(ctx context.Context, in MoveBundleRuntimeInput) (MoveBundleAgentDirs, error) {
	agentDir, err := resolveK8sBrokerAgentDir(ctx, in)
	if err != nil {
		return MoveBundleAgentDirs{}, err
	}

	wsBackend, resolvedWSBase, rel, wsOwner, err := k8sResolveNFSWorkspaceBackend(ctx, in.ProjectPath, in.ProjectID, in.AgentSlug)
	if err != nil {
		return MoveBundleAgentDirs{}, err
	}
	if wsBackend != "nfs" || resolvedWSBase == "" {
		return MoveBundleAgentDirs{}, fmt.Errorf("cloudrun move-bundle requires a broker-mounted NFS workspace export")
	}
	agentRel, err := runtime.NFSAgentDirSubPath(rel, in.AgentSlug)
	if err != nil {
		return MoveBundleAgentDirs{}, fmt.Errorf("workspace_storage nfs: %w", err)
	}
	exportAgentDir := filepath.Join(resolvedWSBase, agentRel)
	wsDir := filepath.Join(exportAgentDir, provision.AgentWorkspaceDir)

	_, homeDir, err := cloudRunResolveNFSHomeDir(ctx, in.ProjectPath, resolvedWSBase, in.ProjectID, in.AgentSlug, in.AgentID)
	if err != nil {
		return MoveBundleAgentDirs{}, err
	}

	// Match cloudRunOwnerIDs (pkg/runtime/cloudrun_runtime.go:522-527):
	// Cloud Run containers run as nfsOwnerIDs(nfs.uid, nfs.gid) for both
	// workspace and home leaves.
	owner := shareddirs.Owner{
		UID: provision.DefaultOwnerID(wsOwner.UID),
		GID: provision.DefaultOwnerID(wsOwner.GID),
	}

	return MoveBundleAgentDirs{
		AgentDir:       agentDir,
		WorkspaceDir:   wsDir,
		HomeDir:        homeDir,
		ExportAgentDir: exportAgentDir,
		WorkspaceOwner: owner,
		HomeOwner:      owner,
	}, nil
}

// ProvisionTarget prepares the NFS export agent dir, empty workspace dir,
// move marker, and empty NFS home leaf for a Cloud Run target provisioned with
// WorkspaceSource="move-bundle".
//
// Unlike Kubernetes (which uses home-storage.json and .scion-home-seed.json
// for the home-prepare init container), Cloud Run always places homes on the
// workspace NFS share and seeds the broker-rendered home onto the leaf at
// Start via provisionCloudRunHome (pkg/runtime/cloudrun_home.go:180-257),
// which overwrites rendered files while preserving imported source files.
func (cloudRunMoveBundleRuntime) ProvisionTarget(ctx context.Context, opts api.StartOptions, _, agentDir, _ string) (retErr error) {
	if opts.WorkspaceSource != api.WorkspaceSourceMoveBundle {
		return nil
	}

	projectID := opts.HubProjectID
	if projectID == "" {
		projectID = opts.Env["SCION_PROJECT_ID"]
	}
	agentID := opts.Env["SCION_AGENT_ID"]
	slug := opts.Name

	if !shareddirs.ValidProjectID(projectID) || !validHomeAgentID(agentID, slug) {
		return fmt.Errorf("move-bundle target provision: %s", cloudRunMissingIdentityReason)
	}

	wsBackend, resolvedWSBase, rel, wsOwner, err := k8sResolveNFSWorkspaceBackend(ctx, opts.ProjectPath, projectID, slug)
	if err != nil {
		return fmt.Errorf("move-bundle target provision: %w", err)
	}
	if wsBackend != "nfs" || resolvedWSBase == "" {
		return fmt.Errorf("move-bundle target provision: cloudrun target requires a broker-mounted NFS workspace export")
	}
	agentRel, err := runtime.NFSAgentDirSubPath(rel, slug)
	if err != nil {
		return fmt.Errorf("move-bundle target provision: workspace_storage nfs: %w", err)
	}
	exportAgentDir := filepath.Join(resolvedWSBase, agentRel)

	_, createdExportAgentDir, err := ensureNFSLeafClaim(resolvedWSBase, agentRel, wsOwner)
	if err != nil {
		return fmt.Errorf("move-bundle target provision: create export agent dir: %w", err)
	}
	if opts.MoveReincarnationID != "" && !createdExportAgentDir {
		return fmt.Errorf("%w: the agent directory on the NFS export already exists", ErrMoveWorkspacePresent)
	}

	var createdHomeLeaf string
	defer func() {
		if retErr == nil {
			return
		}
		if createdHomeLeaf != "" {
			_ = os.RemoveAll(createdHomeLeaf)
		}
		if createdExportAgentDir {
			_ = os.RemoveAll(exportAgentDir)
		}
	}()

	// Record that this Cloud Run agent owns agents/<slug>/workspace so Start
	// never mistakes it for a legacy shared-checkout agent.
	recordCloudRunAgentWorkspace(agentDir, slug)

	if opts.MoveReincarnationID != "" {
		if err := markMoveExportAgentDir(agentDir, exportAgentDir); err != nil {
			return fmt.Errorf("move-bundle target provision: %w", err)
		}
	}

	owner := shareddirs.Owner{
		UID: provision.DefaultOwnerID(wsOwner.UID),
		GID: provision.DefaultOwnerID(wsOwner.GID),
	}

	wsRel := filepath.Join(agentRel, provision.AgentWorkspaceDir)
	if _, _, err := ensureNFSLeafClaim(resolvedWSBase, wsRel, wsOwner); err != nil {
		return fmt.Errorf("move-bundle target provision: create empty NFS workspace: %w", err)
	}
	lchownBestEffort(filepath.Join(exportAgentDir, provision.AgentWorkspaceDir), owner.UID, owner.GID)

	homeAgentDirRel, homeRel, err := cloudRunNFSHomeSubPaths(ctx, opts.ProjectPath, projectID, slug, agentID)
	if err != nil {
		return fmt.Errorf("move-bundle target provision: %w", err)
	}
	nfsHomeDir := filepath.Join(resolvedWSBase, homeRel)
	if opts.MoveReincarnationID != "" {
		if err := checkMovePathAbsent(nfsHomeDir); err != nil {
			return fmt.Errorf("move-bundle target provision: %w", err)
		}
	}
	_, homeStatErr := os.Lstat(nfsHomeDir)
	homeExisted := homeStatErr == nil
	if err := homeLeafOps.EnsureHome(resolvedWSBase, homeAgentDirRel, filepath.Base(homeRel), owner.GID); err != nil {
		return fmt.Errorf("move-bundle target provision: create NFS home leaf: %w", err)
	}
	lchownBestEffort(nfsHomeDir, owner.UID, owner.GID)
	if !homeExisted {
		createdHomeLeaf = nfsHomeDir
	}

	// Always mark the home leaf this call created so rollback delete can verify
	// leaf ownership independently of the parent directory.
	if opts.MoveReincarnationID != "" {
		if err := markMoveExportAgentDir(agentDir, nfsHomeDir); err != nil {
			return fmt.Errorf("move-bundle target provision: %w", err)
		}
	}

	return nil
}

func (cloudRunMoveBundleRuntime) RollbackPaths(ctx context.Context, projectPath, projectID, agentName, hubAgentID string) (MoveBundleRollbackPaths, error) {
	resolvedHostBase, exportAgentDir, homeLeafPath, ok, err := resolveCloudRunNFSHomeRollbackPaths(ctx, projectPath, projectID, agentName, hubAgentID)
	return MoveBundleRollbackPaths{
		ResolvedHostBase: resolvedHostBase,
		ExportAgentDir:   exportAgentDir,
		HomeLeafPath:     homeLeafPath,
		OK:               ok,
	}, err
}

func (cloudRunMoveBundleRuntime) RemoveTargetLeaves(ctx context.Context, projectPath, projectID, agentName, hubAgentID, moveID string) (projectfs.Deletion, string, bool) {
	if moveID == "" {
		return projectfs.Deletion{}, "", false
	}
	resolvedHostBase, exportAgentDir, homeLeafPath, resolved, err := resolveCloudRunNFSHomeRollbackPaths(ctx, projectPath, projectID, agentName, hubAgentID)
	if err != nil {
		return projectfs.Deletion{
			Tree:    projectfs.TreeAgentHome,
			Path:    homeLeafPath,
			Outcome: projectfs.OutcomeFailed,
			Err:     fmt.Errorf("home storage nfs: %w", err),
		}, "", true
	}
	if !resolved {
		return projectfs.Deletion{}, "", false
	}
	return removeNFSHomeLeaf(resolvedHostBase, exportAgentDir, homeLeafPath, agentName, moveID)
}

// cloudRunNFSHomeSubPaths returns the export-relative agent dir and home leaf
// paths for Cloud Run using the workspace NFS config's SubPathRoot and
// runtime.NFSHomeSubPaths (matching cloudRunAgentHomeLeaf in
// pkg/runtime/cloudrun_runtime.go:1893-1900).
func cloudRunNFSHomeSubPaths(ctx context.Context, projectPath, projectID, agentSlug, agentID string) (agentDirRel, homeRel string, err error) {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve project dir for Cloud Run NFS home: %w", err)
	}
	settings, _, err := config.LoadEffectiveSettingsFor(ctx, projectDir)
	if err != nil {
		return "", "", fmt.Errorf("load settings for Cloud Run NFS home: %w", err)
	}
	var rawSubPathRoot string
	if settings != nil && settings.Server != nil {
		if ws := settings.Server.EffectiveWorkspaceStorage(); ws != nil && ws.NFS != nil {
			rawSubPathRoot = ws.NFS.SubPathRoot
		}
	}
	subPathRoot, err := config.ResolveSubPathRoot(rawSubPathRoot)
	if err != nil {
		return "", "", fmt.Errorf("cloudrun: invalid NFS %w", err)
	}
	return runtime.NFSHomeSubPaths(subPathRoot, projectID, agentSlug, agentID)
}

func cloudRunResolveNFSHomeDir(ctx context.Context, projectPath, resolvedWSBase, projectID, agentSlug, agentID string) (homeExportAgentDir, homeDir string, err error) {
	agentDirRel, homeRel, err := cloudRunNFSHomeSubPaths(ctx, projectPath, projectID, agentSlug, agentID)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(resolvedWSBase, agentDirRel), filepath.Join(resolvedWSBase, homeRel), nil
}

// resolveCloudRunNFSHomeRollbackPaths resolves the workspace NFS share's host
// base, export agent dir, and home-<agentID> leaf for Cloud Run move rollback
// marker checks and leaf removal.
func resolveCloudRunNFSHomeRollbackPaths(ctx context.Context, projectPath, projectID, agentName, hubAgentID string) (resolvedHostBase, exportAgentDir, homeLeafPath string, ok bool, err error) {
	if !shareddirs.ValidProjectID(projectID) || !validHomeAgentID(hubAgentID, agentName) {
		return "", "", "", false, nil
	}
	wsBackend, resolvedWSBase, _, _, err := k8sResolveNFSWorkspaceBackend(ctx, projectPath, projectID, agentName)
	if err != nil {
		if !k8sProjectUsesNFSWorkspace(ctx, projectPath) {
			return "", "", "", false, nil
		}
		slog.Warn("cloudrun home storage nfs rollback: could not resolve NFS share",
			"project_id", projectID, "agent", agentName, "error", err)
		return "", "", "", false, err
	}
	if wsBackend != "nfs" || resolvedWSBase == "" {
		return "", "", "", false, nil
	}
	homeExportAgentDir, homeDir, err := cloudRunResolveNFSHomeDir(ctx, projectPath, resolvedWSBase, projectID, agentName, hubAgentID)
	if err != nil {
		return "", "", "", false, err
	}
	return resolvedWSBase, homeExportAgentDir, homeDir, true, nil
}
