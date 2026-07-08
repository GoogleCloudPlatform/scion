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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type RunConfig struct {
	Name                 string
	Template             string
	UnixUsername         string
	Image                string
	HomeDir              string
	Workspace            string
	RepoRoot             string
	ContainerWorkspace   string // The container-side workspace path (e.g., /workspace or /repo-root/.scion/agents/foo/workspace)
	Env                  []string
	ResolvedSecrets      []api.ResolvedSecret
	Volumes              []api.VolumeMount
	Labels               map[string]string
	Annotations          map[string]string
	ResolvedAuth         *api.ResolvedAuth
	Harness              api.Harness
	Task                 string
	CommandArgs          []string
	Resume               bool
	TelemetryEnabled     bool
	Resources            *api.ResourceSpec
	Kubernetes           *api.KubernetesConfig
	GitClone             *api.GitCloneConfig
	SharedDirs           []api.SharedDir
	BrokerMode           bool
	NoAuth               bool
	NoAuthMessage        string
	NoAuthCommand        string
	Debug                bool
	MetadataInterception bool     // Add NET_ADMIN cap for iptables-based metadata server interception
	ExtraHosts           []string // Extra /etc/hosts entries (e.g. "host.docker.internal:host-gateway")
	NetworkMode          string   // Container network mode (e.g. "host" for --network=host)
	Project              string   // Project name (e.g., "global" or "my-project")
	ProjectID            string   // Project ID (e.g., "550e8400-e29b-41d4-a716-446655440000")

	// WorkspaceBackendName is "local" or "nfs", set by the workspace backend
	// selector. Used to branch UID/GID injection and skip per-start chown
	// when NFS (N1-5).
	WorkspaceBackendName string
	// NFSUID and NFSGID are the stable, node-independent UID/GID for NFS-backed
	// workspaces. Advertised as SCION_HOST_UID/GID when WorkspaceBackendName is "nfs"
	// instead of os.Getuid()/os.Getgid(). Default 1000:1000 (design §9.1).
	NFSUID int
	NFSGID int

	// NFSPVClaimName is the K8s PVC name for the NFS-backed workspace volume.
	// Set when WorkspaceBackendName is "nfs". The PVC references a static RWX PV
	// bound to the Filestore/NFS export. Empty for local backend.
	NFSPVClaimName string
	// NFSSubPath is the subPath within the NFS PVC that isolates this project's
	// workspace (e.g. "projects/<pid>/workspace"). Used by K8s buildPod to scope
	// the volume mount — pod sees only its project subtree (design §9.4).
	NFSSubPath string
	// NFSStorageClass is the K8s StorageClass for NFS-backed PVCs.
	// Used when creating shared-dir PVCs on NFS. Empty uses cluster default.
	NFSStorageClass string

	// GitCloneForInit holds git clone configuration for NFS init-container
	// workspace provisioning (N2-2). When set, buildPod adds an init container
	// that clones/provisions the workspace before the main container starts.
	GitCloneForInit *api.GitCloneConfig

	// Locker provides the per-project advisory lock for NFS workspace
	// provisioning (N2-2b, design §7, risk RN1). When set and backend=nfs,
	// the K8s runtime acquires the lock before building the pod to determine
	// whether this pod should clone (lock winner) or wait for the sentinel
	// (lock loser). This prevents concurrent first-clone corruption when
	// two pods for the same project are scheduled on different nodes.
	//
	// May be nil — when absent, all pods get the cloning init container
	// (sentinel-only guard, correct for single-node but unsafe for
	// multi-node). On Postgres-backed deployments this is wired from
	// the store's AdvisoryLocker capability.
	Locker store.AdvisoryLocker

	// nfsProvisionLockLost is set internally by Run() after a failed
	// advisory lock acquisition attempt. When true, buildPod injects a
	// wait-for-sentinel init container instead of the cloning one.
	// Callers should not set this field.
	nfsProvisionLockLost bool
}

type Runtime interface {
	Name() string
	Run(ctx context.Context, config RunConfig) (string, error)
	Stop(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error)
	GetLogs(ctx context.Context, id string) (string, error)
	Attach(ctx context.Context, id string) error
	ImageExists(ctx context.Context, image string) (bool, error)
	ImageID(ctx context.Context, image string) (string, error)
	RemoveImage(ctx context.Context, image string) error
	PullImage(ctx context.Context, image string) error
	Sync(ctx context.Context, id string, direction SyncDirection) error
	Exec(ctx context.Context, id string, cmd []string) (string, error)
	// GetWorkspacePath returns the host path to the container's /workspace mount.
	// This is used for workspace sync operations.
	GetWorkspacePath(ctx context.Context, id string) (string, error)
	// ExecUser returns the container user for exec/attach commands.
	// All runtimes return "scion" — the tmux session runs under the scion
	// user after sciontool init sets up the environment.
	ExecUser() string
}

type SyncDirection string

const (
	SyncTo          SyncDirection = "to"
	SyncFrom        SyncDirection = "from"
	SyncUnspecified SyncDirection = ""

	// LegacyAgentPhaseEnded is the historical terminal phase returned by some
	// runtime list implementations before Scion standardized on stopped/error.
	LegacyAgentPhaseEnded = "ended"
)
