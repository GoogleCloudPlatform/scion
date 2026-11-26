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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/homeprep"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/bundle"
	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

// moveBundleTargetLchown is the injectable chown seam used when provisioning
// K8s NFS target home files so unit tests can assert ownership without root.
var moveBundleTargetLchown = os.Lchown

// MoveBundleRuntimeInput holds the broker-supplied inputs needed to evaluate
// move-bundle durability and resolve host-side workspace and $HOME directories
// for an agent on a specific runtime.
type MoveBundleRuntimeInput struct {
	RuntimeName string
	AgentSlug   string
	// AgentID is the hub agent UUID, required by runtimes whose NFS home leaf
	// is keyed by agent ID (e.g. Kubernetes NFS home, Cloud Run).
	AgentID     string
	ProjectPath string
	ProjectID   string
	BrokerMode  bool
}

// MoveBundleDurability reports whether an agent's workspace and $HOME on a
// source broker survive Stop and are reachable on the broker host for bundling.
type MoveBundleDurability struct {
	Durable bool
	// Reason is a class-only user-facing explanation when Durable is false
	// (never contains host paths or secrets).
	Reason string
}

// MoveBundleRollbackPaths holds the host-side paths for a runtime's
// move-bundle target home leaf (e.g. an NFS home share leaf) used by
// CheckMoveMarkers and RemoveTargetLeaves during rollback deletes.
type MoveBundleRollbackPaths struct {
	ResolvedHostBase string
	ExportAgentDir   string
	HomeLeafPath     string
	OK               bool
}

// MoveBundleRuntime defines the per-runtime hooks for portable move-bundle
// transfers: capability configuration, durability check, host-side directory
// resolution, target provisioning, and marker-guarded rollback cleanup.
//
// Each supported runtime registers an implementation in
// lookupMoveBundleRuntime. Provision, CheckMoveMarkers, RemoveNFSAgentFiles,
// CheckMoveWorkspaceAbsent, capability advertisement, and precheck/export/import
// all dispatch through this single registry keyed by runtime name.
type MoveBundleRuntime interface {
	// Configured reports whether this broker's global settings have the
	// runtime configured for move-bundle transfers (used for capability
	// advertisement).
	Configured() bool

	// UsesNFSWorkspaceTarget reports whether this runtime provisions a fresh
	// per-agent workspace on an NFS export for WorkspaceSource="move-bundle"
	// (used by CheckMoveWorkspaceAbsent).
	UsesNFSWorkspaceTarget() bool

	// CheckSourceDurability reports whether the agent's workspace and $HOME
	// survive Stop and can be read on this broker. Called by precheck and
	// export. Returns fs.ErrNotExist when the runtime is configured for
	// durable storage but the agent directory does not exist.
	CheckSourceDurability(ctx context.Context, in MoveBundleRuntimeInput) (MoveBundleDurability, error)

	// ResolveDirs resolves the host-side agent state dir, workspace dir, and
	// home dir for precheck, export, import, and commit.
	ResolveDirs(ctx context.Context, in MoveBundleRuntimeInput) (MoveBundleAgentDirs, error)

	// ProvisionTarget prepares runtime-specific storage (e.g. NFS workspace
	// dir, move marker, home-storage record, NFS home leaf, sentinel) when
	// provisioning a target agent with WorkspaceSource="move-bundle".
	ProvisionTarget(ctx context.Context, opts api.StartOptions, projectDir, agentDir, agentHome string) error

	// RollbackPaths resolves runtime-specific home leaf paths for move
	// rollback marker checks.
	RollbackPaths(ctx context.Context, projectPath, projectID, agentName, hubAgentID string) (MoveBundleRollbackPaths, error)

	// RemoveTargetLeaves removes runtime-specific target leaves (e.g. NFS
	// home leaf and staging siblings) during a marker-guarded move rollback
	// delete (moveID != "").
	RemoveTargetLeaves(ctx context.Context, projectPath, projectID, agentName, hubAgentID, moveID string) (d projectfs.Deletion, homeParent string, ok bool)
}

// MoveBundleRuntimeSupported reports whether runtimeName has a registered
// MoveBundleRuntime implementation.
func MoveBundleRuntimeSupported(runtimeName string) bool {
	_, ok := lookupMoveBundleRuntime(runtimeName)
	return ok
}

// MoveBundleRuntimeConfigured reports whether runtimeName has a registered
// MoveBundleRuntime whose broker-wide prerequisites are configured.
func MoveBundleRuntimeConfigured(runtimeName string) bool {
	rt, ok := lookupMoveBundleRuntime(runtimeName)
	if !ok {
		return false
	}
	return rt.Configured()
}

// k8sMoveBundleConfigured reports whether this broker's global settings have
// Kubernetes NFS workspace storage, a usable claim-backed NFS home share, and
// the K8s NFS home prerequisites configured (N1).
//
// Capability advertisement is a broker-wide approximation (reading global
// settings with context.Background() without per-project overrides); precheck
// and provision make the authoritative per-project decision.
func k8sMoveBundleConfigured() bool {
	gs, _, err := config.LoadGlobalSettingsWithOverlayFor(context.Background())
	if err != nil || gs == nil || gs.Server == nil {
		return false
	}
	ws := gs.Server.EffectiveWorkspaceStorage()
	if ws == nil || ws.Backend != config.WorkspaceStorageBackendNFS {
		return false
	}
	if gs.ResolveHomeStorage().Backend != config.HomeStorageBackendNFS {
		return false
	}
	hs := homeStorageGlobal(gs)
	if hs == nil || !hs.AllowIncompletePhases {
		return false
	}
	hl, err := homeShareLayout(gs, "")
	if err != nil || hl.Share.PVName == "" {
		return false
	}
	return true
}

// ReadHomeStorageSubPathRoot returns the recorded SubPathRoot from the agent's
// home-storage.json when Backend == "nfs", or "" if unavailable. Callers on
// the delete path invoke this before DeleteTarget removes agentDir so
// RemoveNFSAgentFiles can target the recorded subpath root (N4).
func ReadHomeStorageSubPathRoot(ctx context.Context, projectPath, hubProjectID, agentName string) string {
	agentDir, err := provisionAgentStateDir(ctx, api.StartOptions{
		Name:         agentName,
		ProjectPath:  projectPath,
		HubProjectID: hubProjectID,
		BrokerMode:   true,
	})
	if err != nil || agentDir == "" {
		return ""
	}
	rec, err := readHomeStorageRecord(agentDir)
	if err != nil || rec == nil || rec.Backend != config.HomeStorageBackendNFS {
		return ""
	}
	return rec.SubPathRoot
}

// isCloudRunMoveBundleRuntime reports whether name is a Cloud Run Instances
// runtime spelling ("cloudrun" or "cloudrun-instances", never "cloudrun-sandbox").
func isCloudRunMoveBundleRuntime(name string) bool {
	return isCloudRunRuntime(name) || name == "cloudrun-instances"
}

// cloudRunMoveBundleConfigured reports whether this broker's global settings
// have NFS project storage with a usable share configured for Cloud Run
// Instances move-bundle transfers. Unlike Kubernetes, Cloud Run always places
// agent homes on the workspace NFS share and mounts by server/export rather
// than a PV claim, so project_storage.homes and pv_name are not required.
func cloudRunMoveBundleConfigured() bool {
	gs, _, err := config.LoadGlobalSettingsWithOverlayFor(context.Background())
	if err != nil || gs == nil || gs.Server == nil {
		return false
	}
	ws := gs.Server.EffectiveWorkspaceStorage()
	if ws == nil || ws.Backend != config.WorkspaceStorageBackendNFS || ws.NFS == nil {
		return false
	}
	if ws.NFS.MountRoot == "" || len(ws.NFS.Shares) == 0 {
		return false
	}
	sh := ws.NFS.Shares[0]
	return sh.ID != "" && sh.Server != "" && sh.Export != ""
}

// lookupMoveBundleRuntime returns the per-runtime move-bundle strategy for
// runtimeName, or (nil, false) for runtimes not supported (such as
// cloudrun-sandbox and substrate).
func lookupMoveBundleRuntime(runtimeName string) (MoveBundleRuntime, bool) {
	switch {
	case api.IsLocalDiskRuntime(runtimeName):
		return localDiskMoveBundleRuntime{}, true
	case isKubernetesRuntime(runtimeName):
		return kubernetesMoveBundleRuntime{}, true
	case isCloudRunMoveBundleRuntime(runtimeName):
		return cloudRunMoveBundleRuntime{}, true
	default:
		return nil, false
	}
}

const cloudRunSandboxNonDurableReason = "Cloud Run Sandbox workspace and home are not durable for bundle moves"

// CheckMoveBundleSourceDurability evaluates whether an agent on in.RuntimeName
// is durable for move-bundle export. Cloud Run Sandbox is refused with a
// class-only non-durable reason; other unsupported runtimes return
// Durable: false, Reason: "not yet supported".
func CheckMoveBundleSourceDurability(ctx context.Context, in MoveBundleRuntimeInput) (MoveBundleDurability, error) {
	if in.RuntimeName == "cloudrun-sandbox" {
		return MoveBundleDurability{
			Durable: false,
			Reason:  cloudRunSandboxNonDurableReason,
		}, nil
	}
	rt, ok := lookupMoveBundleRuntime(in.RuntimeName)
	if !ok {
		return MoveBundleDurability{
			Durable: false,
			Reason:  "not yet supported",
		}, nil
	}
	return rt.CheckSourceDurability(ctx, in)
}

// ResolveMoveBundleDirsForRuntime resolves the host-side agent state dir,
// workspace dir, and $HOME dir for in.RuntimeName. Fails closed with an error
// when no MoveBundleRuntime is registered for in.RuntimeName so an unsupported
// runtime never silently falls back to broker-local directories.
func ResolveMoveBundleDirsForRuntime(ctx context.Context, in MoveBundleRuntimeInput) (MoveBundleAgentDirs, error) {
	rt, ok := lookupMoveBundleRuntime(in.RuntimeName)
	if !ok {
		return MoveBundleAgentDirs{}, fmt.Errorf("move-bundle runtime %q is not supported", in.RuntimeName)
	}
	return rt.ResolveDirs(ctx, in)
}

// --- Local-disk runtimes (Docker, Podman, Apple Container) ---

type localDiskMoveBundleRuntime struct{}

func (localDiskMoveBundleRuntime) Configured() bool {
	return true
}

func (localDiskMoveBundleRuntime) UsesNFSWorkspaceTarget() bool {
	return false
}

func (localDiskMoveBundleRuntime) CheckSourceDurability(_ context.Context, _ MoveBundleRuntimeInput) (MoveBundleDurability, error) {
	return MoveBundleDurability{Durable: true}, nil
}

func (localDiskMoveBundleRuntime) ResolveDirs(ctx context.Context, in MoveBundleRuntimeInput) (MoveBundleAgentDirs, error) {
	return ResolveMoveBundleAgentDirs(ctx, in.RuntimeName, api.StartOptions{
		Name:         in.AgentSlug,
		ProjectPath:  in.ProjectPath,
		HubProjectID: in.ProjectID,
		BrokerMode:   in.BrokerMode,
	})
}

func (localDiskMoveBundleRuntime) ProvisionTarget(_ context.Context, _ api.StartOptions, _, _, _ string) error {
	return nil
}

func (localDiskMoveBundleRuntime) RollbackPaths(_ context.Context, _, _, _, _ string) (MoveBundleRollbackPaths, error) {
	return MoveBundleRollbackPaths{}, nil
}

func (localDiskMoveBundleRuntime) RemoveTargetLeaves(_ context.Context, _, _, _, _, _ string) (projectfs.Deletion, string, bool) {
	return projectfs.Deletion{}, "", false
}

// --- Kubernetes runtime (NFS workspace + NFS home) ---

type kubernetesMoveBundleRuntime struct{}

func (kubernetesMoveBundleRuntime) Configured() bool {
	return k8sMoveBundleConfigured()
}

func (kubernetesMoveBundleRuntime) UsesNFSWorkspaceTarget() bool {
	return true
}

func (kubernetesMoveBundleRuntime) RollbackPaths(ctx context.Context, _, projectID, agentName, hubAgentID string) (MoveBundleRollbackPaths, error) {
	resolvedHostBase, exportAgentDir, homeLeafPath, ok, err := resolveK8sNFSHomeRollbackPaths(ctx, projectID, agentName, hubAgentID)
	return MoveBundleRollbackPaths{
		ResolvedHostBase: resolvedHostBase,
		ExportAgentDir:   exportAgentDir,
		HomeLeafPath:     homeLeafPath,
		OK:               ok,
	}, err
}

func (kubernetesMoveBundleRuntime) RemoveTargetLeaves(ctx context.Context, _, projectID, agentName, hubAgentID, moveID string) (projectfs.Deletion, string, bool) {
	return removeK8sNFSAgentHome(ctx, projectID, agentName, hubAgentID, moveID)
}

const (
	k8sNonDurablePodReason       = "workspace and home live only in the pod and are deleted on stop"
	k8sNonDurableHomeOnlyReason  = "agent home lives only in the pod and is deleted on stop"
	k8sNFSNotMountedReason       = "NFS workspace or home export is not mounted on this broker"
	k8sMissingIdentityReason     = "agent ID and project ID are required for Kubernetes NFS bundle moves"
	k8sHomeShareMismatchReason   = "recorded NFS home share is not configured on this broker"
	k8sHomeRecordCorruptedReason = "agent home storage record is unreadable"
)

func (kubernetesMoveBundleRuntime) CheckSourceDurability(ctx context.Context, in MoveBundleRuntimeInput) (MoveBundleDurability, error) {
	// 1. Check whether the project's workspace storage uses the NFS backend
	//    and the export is mounted on this broker.
	wsBackend, resolvedWSBase, _, _, err := k8sResolveNFSWorkspaceBackend(ctx, in.ProjectPath, in.ProjectID, in.AgentSlug)
	if err != nil {
		if !k8sProjectUsesNFSWorkspace(ctx, in.ProjectPath) {
			return MoveBundleDurability{Durable: false, Reason: k8sNonDurablePodReason}, nil
		}
		if !shareddirs.ValidProjectID(in.ProjectID) || !validHomeAgentID(in.AgentID, in.AgentSlug) {
			return MoveBundleDurability{Durable: false, Reason: k8sMissingIdentityReason}, nil
		}
		return MoveBundleDurability{Durable: false, Reason: k8sNFSNotMountedReason}, nil
	}
	if wsBackend != "nfs" {
		return MoveBundleDurability{Durable: false, Reason: k8sNonDurablePodReason}, nil
	}
	if resolvedWSBase == "" {
		return MoveBundleDurability{Durable: false, Reason: k8sNFSNotMountedReason}, nil
	}

	// 2. Resolve broker-local agentDir (where home-storage.json lives).
	agentDir, err := resolveK8sBrokerAgentDir(ctx, in)
	if err != nil {
		return MoveBundleDurability{}, err
	}
	if _, statErr := os.Stat(agentDir); errors.Is(statErr, fs.ErrNotExist) {
		return MoveBundleDurability{}, fs.ErrNotExist
	}

	// 3. Check home-storage.json: must be recorded as HomeStorageBackendNFS.
	rec, err := readHomeStorageRecord(agentDir)
	if err != nil {
		return MoveBundleDurability{Durable: false, Reason: k8sHomeRecordCorruptedReason}, nil
	}
	if rec == nil || rec.Backend != config.HomeStorageBackendNFS {
		return MoveBundleDurability{Durable: false, Reason: k8sNonDurableHomeOnlyReason}, nil
	}

	// 4. Verify agentID/projectID and that the recorded NFS home share is
	//    configured and mounted on this broker. Readability of individual
	//    files is checked during the bounded, excluder-filtered Estimate walk
	//    so excluded caches are skipped and entry/byte limits are enforced.
	if !shareddirs.ValidProjectID(in.ProjectID) || !validHomeAgentID(in.AgentID, in.AgentSlug) {
		return MoveBundleDurability{Durable: false, Reason: k8sMissingIdentityReason}, nil
	}
	if _, _, _, err := resolveK8sNFSHomeDir(ctx, rec, in.ProjectID, in.AgentSlug, in.AgentID); err != nil {
		if errors.Is(err, errK8sNFSHomeNotMounted) {
			return MoveBundleDurability{Durable: false, Reason: k8sNFSNotMountedReason}, nil
		}
		return MoveBundleDurability{Durable: false, Reason: k8sHomeShareMismatchReason}, nil
	}

	return MoveBundleDurability{Durable: true}, nil
}

func (kubernetesMoveBundleRuntime) ResolveDirs(ctx context.Context, in MoveBundleRuntimeInput) (MoveBundleAgentDirs, error) {
	agentDir, err := resolveK8sBrokerAgentDir(ctx, in)
	if err != nil {
		return MoveBundleAgentDirs{}, err
	}

	// Resolve NFS workspace dir using the same helper Start and RemoveNFSAgentFiles use.
	wsBackend, resolvedWSBase, rel, wsOwner, err := k8sResolveNFSWorkspaceBackend(ctx, in.ProjectPath, in.ProjectID, in.AgentSlug)
	if err != nil {
		return MoveBundleAgentDirs{}, err
	}
	if wsBackend != "nfs" || resolvedWSBase == "" {
		return MoveBundleAgentDirs{}, fmt.Errorf("kubernetes move-bundle requires a broker-mounted NFS workspace export")
	}
	agentRel, err := runtime.NFSAgentDirSubPath(rel, in.AgentSlug)
	if err != nil {
		return MoveBundleAgentDirs{}, fmt.Errorf("workspace_storage nfs: %w", err)
	}
	exportAgentDir := filepath.Join(resolvedWSBase, agentRel)
	wsDir := filepath.Join(exportAgentDir, provision.AgentWorkspaceDir)

	// Resolve NFS home leaf from home-storage.json using the same helpers
	// resolveHomeStorage and prepareHomeStorage use.
	rec, err := readHomeStorageRecord(agentDir)
	if err != nil {
		return MoveBundleAgentDirs{}, err
	}
	if rec == nil || rec.Backend != config.HomeStorageBackendNFS {
		return MoveBundleAgentDirs{}, fmt.Errorf("kubernetes move-bundle requires HomeStorageBackend=%q", config.HomeStorageBackendNFS)
	}
	_, homeDir, _, err := resolveK8sNFSHomeDir(ctx, rec, in.ProjectID, in.AgentSlug, in.AgentID)
	if err != nil {
		return MoveBundleAgentDirs{}, err
	}

	// Match Kubernetes pod securityContext / nfsProvisionCommand
	// (pkg/runtime/k8s_runtime.go): K8s pods always run as K8sContainerUID
	// (1000) regardless of nfs.uid, with fsGroup DefaultOwnerID(nfs.gid).
	wsChownOwner := shareddirs.Owner{
		UID: int(runtime.K8sContainerUID),
		GID: provision.DefaultOwnerID(wsOwner.GID),
	}

	return MoveBundleAgentDirs{
		AgentDir:       agentDir,
		WorkspaceDir:   wsDir,
		HomeDir:        homeDir,
		ExportAgentDir: exportAgentDir,
		WorkspaceOwner: wsChownOwner,
		HomeOwner:      shareddirs.Owner{UID: int(runtime.K8sContainerUID), GID: homeStorageGID},
	}, nil
}

var errK8sNFSHomeNotMounted = errors.New("NFS home export is not mounted on this broker")

func resolveK8sBrokerAgentDir(ctx context.Context, in MoveBundleRuntimeInput) (string, error) {
	projectDir, err := config.GetResolvedProjectDir(in.ProjectPath)
	if err != nil {
		return "", fmt.Errorf("resolve project dir: %w", err)
	}
	if _, err := checkAgentDirContained(projectDir, in.AgentSlug, false); err != nil {
		return "", fmt.Errorf("agent dir containment: %w", err)
	}
	ctx = contextWithAgentRuntime(ctx, in.RuntimeName)
	agentDir, err := provisionAgentStateDir(ctx, api.StartOptions{
		Name:         in.AgentSlug,
		ProjectPath:  in.ProjectPath,
		HubProjectID: in.ProjectID,
		BrokerMode:   in.BrokerMode,
	})
	if err != nil {
		return "", fmt.Errorf("resolve agent state dir: %w", err)
	}
	return agentDir, nil
}

func k8sProjectUsesNFSWorkspace(ctx context.Context, projectPath string) bool {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return false
	}
	settings, _, err := config.LoadEffectiveSettingsFor(ctx, projectDir)
	if err != nil || settings == nil || settings.Server == nil || settings.Server.EffectiveWorkspaceStorage() == nil {
		return false
	}
	return runtime.SelectWorkspaceBackend(settings.Server.EffectiveWorkspaceStorage(), store.SharingModeEmptyPerAgent).Name() == "nfs"
}

// k8sResolveNFSWorkspaceBackend resolves the workspace backend name,
// broker-mounted NFS hostBase, project workspace relative path, and configured
// leaf owner using nfsExportWorkspaceOnAnyRuntimeCtx.
func k8sResolveNFSWorkspaceBackend(ctx context.Context, projectPath, projectID, agentSlug string) (backendName, resolvedHostBase, rel string, owner shareddirs.Owner, err error) {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return "", "", "", shareddirs.Owner{}, fmt.Errorf("workspace_storage nfs: resolve project directory: %w", err)
	}
	settings, _, err := config.LoadEffectiveSettingsFor(ctx, projectDir)
	if err != nil {
		return "", "", "", shareddirs.Owner{}, fmt.Errorf("workspace_storage nfs: load settings: %w", err)
	}
	if settings == nil || settings.Server == nil || settings.Server.EffectiveWorkspaceStorage() == nil {
		return "local", "", "", shareddirs.Owner{}, nil
	}
	backend := runtime.SelectWorkspaceBackend(settings.Server.EffectiveWorkspaceStorage(), store.SharingModeEmptyPerAgent)
	if backend.Name() != "nfs" {
		return backend.Name(), "", "", shareddirs.Owner{}, nil
	}
	resolvedHostBase, rel, owner, err = nfsExportWorkspaceOnAnyRuntimeCtx(ctx, projectPath, projectID, agentSlug)
	if err != nil {
		return "nfs", "", "", shareddirs.Owner{}, err
	}
	return "nfs", resolvedHostBase, rel, owner, nil
}

// resolveK8sNFSHomeDir resolves the host-side NFS home export base, the NFS
// home leaf directory (<resolvedHomeBase>/<subPathRoot>/<projectID>/agents/<slug>/home-<agentID>),
// and its parent export agent dir using the exact same homeShareLayout and
// runtime.NFSHomeSubPaths helpers that resolveHomeStorage and
// prepareHomeStorage use.
func resolveK8sNFSHomeDir(ctx context.Context, rec *homeStorageRecord, projectID, agentSlug, agentID string) (resolvedHomeBase, homeDir, homeExportAgentDir string, err error) {
	gs, err := loadHomeStorageSettings(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("load global settings for NFS home: %w", err)
	}
	hl, err := homeShareLayout(gs, "")
	if err != nil {
		return "", "", "", err
	}
	if hl.Share.ID != rec.ShareID || hl.Share.PVName != rec.PVClaimName {
		return "", "", "", fmt.Errorf("recorded home share %s (%s) does not match configured share %s (%s)",
			rec.ShareID, rec.PVClaimName, hl.Share.ID, hl.Share.PVName)
	}
	info, err := os.Stat(hl.HostBase)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", "", fmt.Errorf("%w: %s", errK8sNFSHomeNotMounted, hl.HostBase)
		}
		return "", "", "", fmt.Errorf("stat NFS home export %s: %w", hl.HostBase, err)
	}
	if !info.IsDir() {
		return "", "", "", fmt.Errorf("%w: %s is not a directory", errK8sNFSHomeNotMounted, hl.HostBase)
	}
	resolvedHomeBase, err = filepath.EvalSymlinks(hl.HostBase)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve NFS home export %s: %w", hl.HostBase, err)
	}
	agentDirRel, homeRel, err := runtime.NFSHomeSubPaths(rec.SubPathRoot, projectID, agentSlug, agentID)
	if err != nil {
		return "", "", "", err
	}
	return resolvedHomeBase, filepath.Join(resolvedHomeBase, homeRel), filepath.Join(resolvedHomeBase, agentDirRel), nil
}

// provisionMoveBundleTarget dispatches move-bundle target provisioning through
// the registered MoveBundleRuntime for m.runtimeName().
func (m *AgentManager) provisionMoveBundleTarget(ctx context.Context, opts api.StartOptions, projectDir, agentDir, agentHome string) error {
	if opts.WorkspaceSource != api.WorkspaceSourceMoveBundle {
		return nil
	}
	rt, ok := lookupMoveBundleRuntime(m.runtimeName())
	if !ok {
		return fmt.Errorf("move-bundle target provision: runtime %q is not supported", m.runtimeName())
	}
	return rt.ProvisionTarget(ctx, opts, projectDir, agentDir, agentHome)
}

// ProvisionTarget prepares the NFS workspace agent dir, empty workspace, move
// marker, home-storage.json, and rendered NFS home leaf for a Kubernetes target
// provisioned with WorkspaceSource="move-bundle" (design §3.5 step 7, Phase 2a).
//
// Uses the canonical NFS leaf helpers (ensureNFSLeafClaim for agent dir and
// workspace; homeLeafOps.EnsureHome for home-<agentID>) so directory modes,
// setgid bits, ACLs, and group ownership match Start.
func (kubernetesMoveBundleRuntime) ProvisionTarget(ctx context.Context, opts api.StartOptions, _, agentDir, _ string) (retErr error) {
	if opts.WorkspaceSource != api.WorkspaceSourceMoveBundle {
		return nil
	}

	projectID := opts.HubProjectID
	if projectID == "" {
		projectID = opts.Env["SCION_PROJECT_ID"]
	}
	agentID := opts.Env["SCION_AGENT_ID"]
	slug := opts.Name

	// 1. Resolve NFS workspace export and create the fresh agent dir + empty
	//    workspace using ensureNFSLeafClaim (canonical modes 2775 + default ACL).
	wsBackend, resolvedWSBase, rel, wsOwner, err := k8sResolveNFSWorkspaceBackend(ctx, opts.ProjectPath, projectID, slug)
	if err != nil {
		return fmt.Errorf("move-bundle target provision: %w", err)
	}
	if wsBackend != "nfs" || resolvedWSBase == "" {
		return fmt.Errorf("move-bundle target provision: kubernetes target requires a broker-mounted NFS workspace export")
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
	var createdHomeParent string
	defer func() {
		if retErr == nil {
			return
		}
		if createdHomeLeaf != "" {
			_ = os.RemoveAll(createdHomeLeaf)
		}
		if createdHomeParent != "" && createdHomeParent != exportAgentDir {
			_ = os.Remove(filepath.Join(createdHomeParent, MoveMarkerFile))
			_ = os.Remove(createdHomeParent)
		}
		if createdExportAgentDir {
			_ = os.RemoveAll(exportAgentDir)
		}
	}()

	if opts.MoveReincarnationID != "" {
		if err := markMoveExportAgentDir(agentDir, exportAgentDir); err != nil {
			return fmt.Errorf("move-bundle target provision: %w", err)
		}
	}
	wsRel := filepath.Join(agentRel, provision.AgentWorkspaceDir)
	if _, _, err := ensureNFSLeafClaim(resolvedWSBase, wsRel, wsOwner); err != nil {
		return fmt.Errorf("move-bundle target provision: create empty NFS workspace: %w", err)
	}

	// 2. Resolve and record NFS home storage using the exact same
	//    resolveHomeStorage helper Start uses.
	runtimeName := agentRuntimeFromContext(ctx)
	if runtimeName == "" {
		runtimeName = "kubernetes"
	}
	homePlan, err := resolveHomeStorage(homeStorageInput{
		AgentDir:     agentDir,
		AgentName:    opts.Name,
		Slug:         slug,
		RuntimeName:  runtimeName,
		Profile:      opts.Profile,
		AgentID:      agentID,
		ProjectID:    projectID,
		ExperimentOn: api.HubAgentDefaultsFromContext(ctx).ExperimentEnabled(experiments.K8sNFSHome),
		LoadSettings: func() (*config.VersionedSettings, error) {
			return loadHomeStorageSettings(ctx)
		},
	})
	if err != nil {
		return fmt.Errorf("move-bundle target provision: %w", err)
	}
	if homePlan == nil || homePlan.Backend != config.HomeStorageBackendNFS {
		return fmt.Errorf("move-bundle target provision: kubernetes target requires HomeStorageBackend=%q", config.HomeStorageBackendNFS)
	}

	rec, err := readHomeStorageRecord(agentDir)
	if err != nil || rec == nil {
		return fmt.Errorf("move-bundle target provision: read recorded home storage: %w", err)
	}
	resolvedHomeBase, nfsHomeDir, nfsHomeParent, err := resolveK8sNFSHomeDir(ctx, rec, projectID, slug, agentID)
	if err != nil {
		return fmt.Errorf("move-bundle target provision: %w", err)
	}

	// Refuse to adopt a pre-existing home-<agentID> leaf (R3).
	if opts.MoveReincarnationID != "" {
		if err := checkMovePathAbsent(nfsHomeDir); err != nil {
			return fmt.Errorf("move-bundle target provision: %w", err)
		}
	}

	homeParentExisted := true
	if nfsHomeParent != exportAgentDir {
		if _, statErr := os.Lstat(nfsHomeParent); errors.Is(statErr, fs.ErrNotExist) {
			homeParentExisted = false
		}
	}

	// Create the home leaf using canonical homeLeafOps.EnsureHome (2771, ACLs
	// cleared, export gid set), reusing resolvedHomeBase directly from
	// resolveK8sNFSHomeDir (C2).
	homeAgentDirRel, homeRel, err := runtime.NFSHomeSubPaths(rec.SubPathRoot, projectID, slug, agentID)
	if err != nil {
		return fmt.Errorf("move-bundle target provision: %w", err)
	}
	if err := homeLeafOps.EnsureHome(resolvedHomeBase, homeAgentDirRel, filepath.Base(homeRel), homeStorageGID); err != nil {
		return fmt.Errorf("move-bundle target provision: create NFS home leaf: %w", err)
	}
	lchownBestEffort(nfsHomeDir, int(runtime.K8sContainerUID), homeStorageGID)
	createdHomeLeaf = nfsHomeDir
	if !homeParentExisted && nfsHomeParent != exportAgentDir {
		createdHomeParent = nfsHomeParent
		if opts.MoveReincarnationID != "" {
			if err := markMoveExportAgentDir(agentDir, nfsHomeParent); err != nil {
				return fmt.Errorf("move-bundle target provision: %w", err)
			}
		}
	}
	// Always mark the home leaf this call created so rollback delete can verify
	// ownership even when nfsHomeParent already existed (R3).
	if opts.MoveReincarnationID != "" {
		if err := markMoveExportAgentDir(agentDir, nfsHomeDir); err != nil {
			return fmt.Errorf("move-bundle target provision: %w", err)
		}
	}

	// 3. Write the homeprep seeded sentinel so the target pod's home-prepare
	//    init container chooses "seed-over" and transfers the broker's
	//    rendered home over the imported source files at Start (single
	//    canonical home-seeding path, C1).
	if err := writeMoveBundleNFSHomeSentinel(nfsHomeDir, agentID); err != nil {
		return fmt.Errorf("move-bundle target provision: write NFS home sentinel: %w", err)
	}

	return nil
}

func lchownBestEffort(path string, uid, gid int) {
	if err := moveBundleTargetLchown(path, uid, gid); err != nil {
		slog.Debug("move-bundle target provision: chown skipped", "path", path, "uid", uid, "gid", gid, "error", err)
	}
}

// writeMoveBundleNFSHomeSentinel writes .scion-home-seed.json (StateSeeded)
// into the freshly provisioned K8s NFS home leaf so home-prepare on target
// Start runs in seed-over mode rather than refusing a non-empty home without a
// sentinel (pkg/sciontool/homeprep/sentinel.go:180).
//
// Setting UID to K8sContainerUID is required by homeprep.Decide (which refuses
// any sentinel whose UID differs from the pod's uid 1000) and matches the
// K8sContainerUID:homeStorageGID ownership applied to the home leaf and its
// imported entries. SkeletonNone intentionally skips copying the target image's
// skeleton so the imported source home skeleton is preserved.
func writeMoveBundleNFSHomeSentinel(nfsHomeDir, agentID string) error {
	s := homeprep.Sentinel{
		Version:  1,
		AgentID:  agentID,
		State:    homeprep.StateSeeded,
		UID:      int(runtime.K8sContainerUID),
		GID:      homeStorageGID,
		Skeleton: homeprep.SkeletonNone,
	}
	data, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	root, err := os.OpenRoot(nfsHomeDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	f, err := rootopen.OpenFileForWrite(root, homeprep.SentinelName, 0o644, false)
	if err != nil {
		return err
	}
	_, wErr := f.Write(data)
	cErr := f.Close()
	if wErr != nil {
		return wErr
	}
	if cErr != nil {
		return cErr
	}
	lchownBestEffort(filepath.Join(nfsHomeDir, homeprep.SentinelName), int(runtime.K8sContainerUID), homeStorageGID)
	return nil
}

// purgeMoveBundleStagingEntries removes any move-bundle sibling staging
// directories (.<base>.bundle-<32hex>) inside dir using os.Root.
func purgeMoveBundleStagingEntries(dir string) error {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if bundle.IsAnyStageName(e.Name()) {
			if rmErr := root.RemoveAll(e.Name()); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				errs = append(errs, rmErr)
			}
		}
	}
	return errors.Join(errs...)
}
