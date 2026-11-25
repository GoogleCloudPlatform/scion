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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/homeprep"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

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

// MoveBundleRuntime defines the per-runtime hooks for portable move-bundle
// transfers (durability check and host-side directory resolution for source
// and target).
//
// Each supported runtime registers an implementation in
// lookupMoveBundleRuntime. Adding a new durable runtime (such as Cloud Run or
// Cloud Run Sandbox in Phase 2a part 2) only requires implementing this
// interface, registering it in lookupMoveBundleRuntime, and adding the
// concrete runtime type to moveBundleSupported in
// pkg/runtimebroker/static_capabilities.go.
type MoveBundleRuntime interface {
	// CheckSourceDurability reports whether the agent's workspace and $HOME
	// survive Stop and can be read on this broker. Called by precheck and
	// export. Returns fs.ErrNotExist when the runtime is configured for
	// durable storage but the agent directory does not exist.
	CheckSourceDurability(ctx context.Context, in MoveBundleRuntimeInput) (MoveBundleDurability, error)

	// ResolveDirs resolves the host-side agent state dir, workspace dir, and
	// home dir for precheck, export, import, and commit.
	ResolveDirs(ctx context.Context, in MoveBundleRuntimeInput, forTarget bool) (MoveBundleAgentDirs, error)
}

// MoveBundleRuntimeSupported reports whether runtimeName has a registered
// MoveBundleRuntime implementation.
func MoveBundleRuntimeSupported(runtimeName string) bool {
	_, ok := lookupMoveBundleRuntime(runtimeName)
	return ok
}

// lookupMoveBundleRuntime returns the per-runtime move-bundle strategy for
// runtimeName, or (nil, false) for runtimes not yet supported.
func lookupMoveBundleRuntime(runtimeName string) (MoveBundleRuntime, bool) {
	switch {
	case api.IsLocalDiskRuntime(runtimeName):
		return localDiskMoveBundleRuntime{}, true
	case isKubernetesRuntime(runtimeName):
		return kubernetesMoveBundleRuntime{}, true
	default:
		return nil, false
	}
}

// CheckMoveBundleSourceDurability evaluates whether an agent on in.RuntimeName
// is durable for move-bundle export. Unsupported runtimes return
// Durable: false, Reason: "not yet supported".
func CheckMoveBundleSourceDurability(ctx context.Context, in MoveBundleRuntimeInput) (MoveBundleDurability, error) {
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
// workspace dir, and $HOME dir for in.RuntimeName.
func ResolveMoveBundleDirsForRuntime(ctx context.Context, in MoveBundleRuntimeInput, forTarget bool) (MoveBundleAgentDirs, error) {
	rt, ok := lookupMoveBundleRuntime(in.RuntimeName)
	if !ok {
		// Fall back to broker-local resolution for unknown runtimes (preserving
		// Phase 1 behavior for tests/callers that resolve dirs without durability).
		return ResolveMoveBundleAgentDirs(ctx, in.RuntimeName, api.StartOptions{
			Name:         in.AgentSlug,
			ProjectPath:  in.ProjectPath,
			HubProjectID: in.ProjectID,
			BrokerMode:   in.BrokerMode,
		})
	}
	return rt.ResolveDirs(ctx, in, forTarget)
}

// --- Local-disk runtimes (Docker, Podman, Apple Container) ---

type localDiskMoveBundleRuntime struct{}

func (localDiskMoveBundleRuntime) CheckSourceDurability(_ context.Context, _ MoveBundleRuntimeInput) (MoveBundleDurability, error) {
	return MoveBundleDurability{Durable: true}, nil
}

func (localDiskMoveBundleRuntime) ResolveDirs(ctx context.Context, in MoveBundleRuntimeInput, _ bool) (MoveBundleAgentDirs, error) {
	return ResolveMoveBundleAgentDirs(ctx, in.RuntimeName, api.StartOptions{
		Name:         in.AgentSlug,
		ProjectPath:  in.ProjectPath,
		HubProjectID: in.ProjectID,
		BrokerMode:   in.BrokerMode,
	})
}

// --- Kubernetes runtime (NFS workspace + NFS home) ---

type kubernetesMoveBundleRuntime struct{}

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
	wsBackend, resolvedWSBase, _, err := k8sResolveNFSWorkspaceBackend(ctx, in.ProjectPath, in.ProjectID, in.AgentSlug)
	if err != nil {
		// Invalid project/agent IDs or settings load errors: if projectID is
		// empty/invalid on a plain K8s setup without NFS workspace, check
		// settings first so plain K8s gets the pod-ephemeral reason.
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
	//    configured and mounted on this broker.
	if !shareddirs.ValidProjectID(in.ProjectID) || !validHomeAgentID(in.AgentID, in.AgentSlug) {
		return MoveBundleDurability{Durable: false, Reason: k8sMissingIdentityReason}, nil
	}
	if _, _, err := resolveK8sNFSHomeDir(ctx, rec, in.ProjectID, in.AgentSlug, in.AgentID); err != nil {
		if errors.Is(err, errK8sNFSHomeNotMounted) {
			return MoveBundleDurability{Durable: false, Reason: k8sNFSNotMountedReason}, nil
		}
		return MoveBundleDurability{Durable: false, Reason: k8sHomeShareMismatchReason}, nil
	}

	return MoveBundleDurability{Durable: true}, nil
}

func (kubernetesMoveBundleRuntime) ResolveDirs(ctx context.Context, in MoveBundleRuntimeInput, _ bool) (MoveBundleAgentDirs, error) {
	agentDir, err := resolveK8sBrokerAgentDir(ctx, in)
	if err != nil {
		return MoveBundleAgentDirs{}, err
	}

	// Resolve NFS workspace dir using the same helper Start and RemoveNFSAgentFiles use.
	wsBackend, resolvedWSBase, rel, err := k8sResolveNFSWorkspaceBackend(ctx, in.ProjectPath, in.ProjectID, in.AgentSlug)
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
	homeDir, _, err := resolveK8sNFSHomeDir(ctx, rec, in.ProjectID, in.AgentSlug, in.AgentID)
	if err != nil {
		return MoveBundleAgentDirs{}, err
	}

	return MoveBundleAgentDirs{
		AgentDir:       agentDir,
		WorkspaceDir:   wsDir,
		HomeDir:        homeDir,
		ExportAgentDir: exportAgentDir,
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

// k8sResolveNFSWorkspaceBackend resolves the workspace backend name and
// broker-mounted NFS hostBase + project workspace relative path using
// nfsExportWorkspaceOnAnyRuntimeCtx.
func k8sResolveNFSWorkspaceBackend(ctx context.Context, projectPath, projectID, agentSlug string) (backendName, resolvedHostBase, rel string, err error) {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return "", "", "", fmt.Errorf("workspace_storage nfs: resolve project directory: %w", err)
	}
	settings, _, err := config.LoadEffectiveSettingsFor(ctx, projectDir)
	if err != nil {
		return "", "", "", fmt.Errorf("workspace_storage nfs: load settings: %w", err)
	}
	if settings == nil || settings.Server == nil || settings.Server.EffectiveWorkspaceStorage() == nil {
		return "local", "", "", nil
	}
	backend := runtime.SelectWorkspaceBackend(settings.Server.EffectiveWorkspaceStorage(), store.SharingModeEmptyPerAgent)
	if backend.Name() != "nfs" {
		return backend.Name(), "", "", nil
	}
	resolvedHostBase, rel, _, err = nfsExportWorkspaceOnAnyRuntimeCtx(ctx, projectPath, projectID, agentSlug)
	if err != nil {
		return "nfs", "", "", err
	}
	return "nfs", resolvedHostBase, rel, nil
}

// resolveK8sNFSHomeDir resolves the host-side NFS home leaf directory
// (<homeHostBase>/<subPathRoot>/<projectID>/agents/<slug>/home-<agentID>) and
// its parent export agent dir using the exact same homeShareLayout and
// runtime.NFSHomeSubPaths helpers that resolveHomeStorage and
// prepareHomeStorage use.
func resolveK8sNFSHomeDir(ctx context.Context, rec *homeStorageRecord, projectID, agentSlug, agentID string) (homeDir, homeExportAgentDir string, err error) {
	gs, err := loadHomeStorageSettings(ctx)
	if err != nil {
		return "", "", fmt.Errorf("load global settings for NFS home: %w", err)
	}
	hl, err := homeShareLayout(gs, "")
	if err != nil {
		return "", "", err
	}
	if hl.Share.ID != rec.ShareID || hl.Share.PVName != rec.PVClaimName {
		return "", "", fmt.Errorf("recorded home share %s (%s) does not match configured share %s (%s)",
			rec.ShareID, rec.PVClaimName, hl.Share.ID, hl.Share.PVName)
	}
	info, err := os.Stat(hl.HostBase)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", fmt.Errorf("%w: %s", errK8sNFSHomeNotMounted, hl.HostBase)
		}
		return "", "", fmt.Errorf("stat NFS home export %s: %w", hl.HostBase, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("%w: %s is not a directory", errK8sNFSHomeNotMounted, hl.HostBase)
	}
	resolvedHomeBase, err := filepath.EvalSymlinks(hl.HostBase)
	if err != nil {
		return "", "", fmt.Errorf("resolve NFS home export %s: %w", hl.HostBase, err)
	}
	agentDirRel, homeRel, err := runtime.NFSHomeSubPaths(rec.SubPathRoot, projectID, agentSlug, agentID)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(resolvedHomeBase, homeRel), filepath.Join(resolvedHomeBase, agentDirRel), nil
}

// provisionK8sMoveBundleTarget prepares the NFS workspace agent dir, empty
// workspace, move marker, home-storage.json, and rendered NFS home leaf for a
// Kubernetes target provisioned with WorkspaceSource="move-bundle" (design
// §3.5 step 7, Phase 2a).
func (m *AgentManager) provisionK8sMoveBundleTarget(ctx context.Context, opts api.StartOptions, _, agentDir, agentHome string) (retErr error) {
	if !isKubernetesRuntime(m.runtimeName()) || opts.WorkspaceSource != api.WorkspaceSourceMoveBundle {
		return nil
	}

	projectID := opts.HubProjectID
	if projectID == "" {
		projectID = opts.Env["SCION_PROJECT_ID"]
	}
	agentID := opts.Env["SCION_AGENT_ID"]
	slug := opts.Name

	// 1. Resolve NFS workspace export and create the fresh agent dir + empty workspace.
	wsBackend, resolvedWSBase, rel, err := k8sResolveNFSWorkspaceBackend(ctx, opts.ProjectPath, projectID, slug)
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
	exportWS := filepath.Join(exportAgentDir, provision.AgentWorkspaceDir)

	if err := os.MkdirAll(filepath.Dir(exportAgentDir), 0o755); err != nil {
		return fmt.Errorf("move-bundle target provision: create export agents parent: %w", err)
	}
	createdExportAgentDir, err := claimDir(exportAgentDir)
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
	if err := os.Mkdir(exportWS, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("move-bundle target provision: create empty NFS workspace: %w", err)
	}

	// 2. Resolve and record NFS home storage using the exact same
	//    resolveHomeStorage helper Start uses.
	homePlan, err := resolveHomeStorage(homeStorageInput{
		AgentDir:     agentDir,
		AgentName:    opts.Name,
		Slug:         slug,
		RuntimeName:  m.runtimeName(),
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
	nfsHomeDir, nfsHomeParent, err := resolveK8sNFSHomeDir(ctx, rec, projectID, slug, agentID)
	if err != nil {
		return fmt.Errorf("move-bundle target provision: %w", err)
	}
	if nfsHomeParent != exportAgentDir {
		if err := os.MkdirAll(filepath.Dir(nfsHomeParent), 0o755); err != nil {
			return fmt.Errorf("move-bundle target provision: create NFS home parent: %w", err)
		}
		if created, err := claimDir(nfsHomeParent); err != nil {
			return fmt.Errorf("move-bundle target provision: create NFS home agent dir: %w", err)
		} else if created {
			createdHomeParent = nfsHomeParent
			if opts.MoveReincarnationID != "" {
				if err := markMoveExportAgentDir(agentDir, nfsHomeParent); err != nil {
					return fmt.Errorf("move-bundle target provision: %w", err)
				}
			}
		}
	}
	createdHome, err := claimDir(nfsHomeDir)
	if err != nil {
		return fmt.Errorf("move-bundle target provision: create NFS home leaf: %w", err)
	}
	if opts.MoveReincarnationID != "" && !createdHome {
		return fmt.Errorf("%w: the agent home leaf on the NFS export already exists", ErrMoveWorkspacePresent)
	}
	createdHomeLeaf = nfsHomeDir

	// 3. Copy the provision-rendered home files into the NFS home leaf so
	//    move-bundle/import merges with provision-rendered files winning.
	if err := copyProvisionedHomeToNFSLeaf(agentHome, nfsHomeDir); err != nil {
		return fmt.Errorf("move-bundle target provision: render NFS home leaf: %w", err)
	}

	// 4. Write the homeprep seeded sentinel so the target pod's home-prepare
	//    init container chooses "seed-over" and keeps the imported home files.
	if err := writeMoveBundleNFSHomeSentinel(nfsHomeDir, agentID); err != nil {
		return fmt.Errorf("move-bundle target provision: write NFS home sentinel: %w", err)
	}

	return nil
}

// copyProvisionedHomeToNFSLeaf copies regular files, directories, and internal
// symlinks rendered by ProvisionAgent in srcHome into dstHome using os.Root so
// writes stay strictly inside dstHome.
func copyProvisionedHomeToNFSLeaf(srcHome, dstHome string) error {
	srcRoot, err := os.OpenRoot(srcHome)
	if err != nil {
		return fmt.Errorf("open provisioned home: %w", err)
	}
	defer func() { _ = srcRoot.Close() }()

	dstRoot, err := os.OpenRoot(dstHome)
	if err != nil {
		return fmt.Errorf("open NFS home leaf: %w", err)
	}
	defer func() { _ = dstRoot.Close() }()

	return fs.WalkDir(srcRoot.FS(), ".", func(rel string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			perm := mode.Perm() & 0o777
			if perm == 0 {
				perm = 0o755
			}
			if err := dstRoot.Mkdir(rel, perm); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		case mode&fs.ModeSymlink != 0:
			target, err := srcRoot.Readlink(rel)
			if err != nil {
				return err
			}
			if err := dstRoot.Symlink(target, rel); err != nil {
				return err
			}
		case mode.IsRegular():
			in, _, err := rootopen.OpenRegularForRead(srcRoot, rel, info)
			if err != nil {
				return err
			}
			defer func() { _ = in.Close() }()
			perm := mode.Perm() & 0o777
			if perm == 0 {
				perm = 0o644
			}
			out, err := rootopen.OpenFileForWrite(dstRoot, rel, uint32(perm), false)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, in)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		return nil
	})
}

// writeMoveBundleNFSHomeSentinel writes .scion-home-seed.json (StateSeeded)
// into the freshly provisioned K8s NFS home leaf so home-prepare on target
// Start runs in seed-over mode rather than refusing a non-empty home without a
// sentinel.
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
	return cErr
}

// isMoveBundleStagingEntry reports whether name is a move-bundle sibling
// staging directory ".<base>.bundle-<32hex>".
func isMoveBundleStagingEntry(name string) bool {
	if !strings.HasPrefix(name, ".") {
		return false
	}
	idx := strings.LastIndex(name, ".bundle-")
	if idx <= 1 {
		return false
	}
	suffix := name[idx+len(".bundle-"):]
	if len(suffix) != 32 {
		return false
	}
	for _, c := range suffix {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
		if isMoveBundleStagingEntry(e.Name()) {
			if rmErr := root.RemoveAll(e.Name()); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				errs = append(errs, rmErr)
			}
		}
	}
	return errors.Join(errs...)
}
