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
	"io"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type RunConfig struct {
	Name               string
	Template           string
	UnixUsername       string
	Image              string
	HomeDir            string
	Workspace          string
	RepoRoot           string
	ContainerWorkspace string // The container-side workspace path (e.g., /workspace or /repo-root/.scion/agents/foo/workspace)
	Env                []string
	ResolvedSecrets    []api.ResolvedSecret
	Volumes            []api.VolumeMount
	Labels             map[string]string
	Annotations        map[string]string
	ResolvedAuth       *api.ResolvedAuth
	Harness            api.Harness
	Task               string
	CommandArgs        []string
	Resume             bool
	TelemetryEnabled   bool
	Resources          *api.ResourceSpec
	Kubernetes         *api.KubernetesConfig
	GitClone           *api.GitCloneConfig
	// TrustedHubEndpoint is the hub endpoint Substrate's egress allowlist
	// trusts. In broker mode, this field arrives already resolved by the
	// runtime broker from operator-controlled tiers only — the request's
	// HubEndpoint field, the hub connection endpoint, or the broker's own
	// configured HubEndpoint (see api.StartOptions.TrustedHubEndpoint and
	// runtimebroker's resolveEffectiveHubEndpoint) — with project settings
	// and the resolved env excluded from this egress-trust path, even
	// though either may still supply the agent's own delivered hub
	// endpoint. Outside broker mode, it is instead the caller-provided
	// opts.Env's own SCION_HUB_ENDPOINT, captured at the top of Start
	// before anything can override it, falling back to the project
	// settings file when that is empty — a legitimate trusted source on
	// this path, since there is no broker-side operator resolution to
	// defer to. Neither the agent-level Hub config nor an agent/template
	// config's own SCION_HUB_ENDPOINT env entry ever feeds this field
	// (pkg/agent/run.go): both are creator-controlled and are applied only
	// to the final agent env, after this value is captured or resolved.
	// Every runtime except Substrate ignores this field; base
	// env-resolution behaviour for every other runtime is unchanged.
	// Substrate uses it as the one egress-allowlisted hub host, independent
	// of whatever SCION_HUB_ENDPOINT/SCION_HUB_URL end up in the final
	// agent env (see substrateEgressHostnames) — an agent/template env
	// override can point the *agent's own* hub calls at a different value,
	// but must never widen the egress allowlist to match it.
	TrustedHubEndpoint string
	SharedDirs         []api.SharedDir
	// SharedDirStorage holds the resolved shared-dir storage plan when
	// server.shared_dir_storage.backend is "nfs" (design
	// deploy-config-explore §3.2.3/§3.2.4). It is independent of
	// WorkspaceBackendName/NFS* above, which describe workspace storage
	// only. Nil means shared dirs use the default local layout (or, on K8s,
	// fall back to the existing workspace_storage:nfs subPath branch or
	// per-dir dynamic PVCs).
	SharedDirStorage     *SharedDirRealization
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

	// WorkspaceBackendName is the name of the backend chosen by the workspace
	// backend selector: "local", "nfs", "cloudrun-volume" or
	// "gke-shared-volume". Used to branch UID/GID injection and skip per-start
	// chown when NFS (N1-5); the branches below key on "nfs" only.
	WorkspaceBackendName string
	// HomeStorageBackend selects where the agent home lives on the
	// Kubernetes runtime. Empty (or "local") keeps the home in the pod and
	// the pod spec unchanged. HomeStorageNFS builds an NFS-home pod (see
	// k8s_nfs_home.go). Nothing sets HomeStorageNFS yet.
	HomeStorageBackend string
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
	// NFSWorkspacePreCreated is true when, before the pod was built, the
	// broker either created the NFSSubPath directory (and the directory of
	// each shared dir served from the same claim) on its own mount of the
	// export, or found it there with setgid and group write. Only then does
	// the provisioning init container treat a failed chown as a warning
	// (SCION_PROVISION_CHOWN_BEST_EFFORT), since access to those directories
	// comes from their setgid group.
	NFSWorkspacePreCreated bool
	// NFSWorktreeName is set to the agent's slug for worktree-per-agent git
	// projects on the NFS backend. NFSSubPath then names the project's
	// shared checkout, and the agent gets its own worktree at
	// NFSSubPath/worktrees/<agent name>: the provisioning init container
	// adds it, and the agent container mounts it with the shared .git at
	// /repo-root (NFSWorktreeContainerPath). Empty keeps every agent on the
	// shared checkout at /workspace.
	NFSWorktreeName string
	// NFSWorktreeBranch is the branch the agent's worktree is created on.
	// Only used with NFSWorktreeName.
	NFSWorktreeBranch string
	// NFSAgentDirName is set to the agent's slug for clone-per-agent git
	// projects on the NFS backend. NFSSubPath still names the project's
	// workspace path, and the agent gets its own directory next to it at
	// <project>/agents/<agent name> (NFSAgentDirSubPath): the provisioning
	// init container mounts that directory and prepares its workspace/
	// directory without cloning, and the agent container mounts
	// <project>/agents/<agent name>/workspace at /workspace and clones the
	// repository into it, as on the local runtimes.
	NFSAgentDirName string
	// NFSAgentBranch is the branch the agent's workspace is created for,
	// recorded by the init container. Only used with NFSAgentDirName.
	NFSAgentBranch string
	// NFSAgentDirEmpty marks an NFSAgentDirName agent of an empty-per-agent
	// project: the mounts are the same, but the init container prepares an
	// empty workspace with no branch record (SCION_WORKSPACE_MODE
	// empty-per-agent) and nothing clones into it. NFSAgentBranch is unused.
	NFSAgentDirEmpty bool
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

	// Checkpoint and OnResourceCreated are an async launch's runtime hooks
	// (design t1-async-create-v11.md §3.8.3, §3.8.4), copied from
	// api.StartOptions. Runtimes call them through launchHooks, whose
	// checkpoint and created methods are no-ops when the hook is nil (the
	// synchronous path).
	//
	// OnResourceCreated also selects who cleans up a start that fails or is
	// cancelled. When it is set, the runtime skips its own start cleanup and
	// leaves every created resource to the caller, which deletes the
	// reported handles (the async launch's CleanupLaunch). When it is nil,
	// the runtime cleans up as on the synchronous path, whether or not
	// Checkpoint is set. Callers set both hooks together.
	Checkpoint        func(ctx context.Context, step string) error
	OnResourceCreated func(api.ResourceHandle)
}

// Checkpoint step names a runtime passes to RunConfig.Checkpoint (design
// §3.9's step names; container runtimes keep the generic "launching" step).
const (
	CheckpointStepSecrets   = "secrets"
	CheckpointStepPodCreate = "pod_create"
	CheckpointStepLaunching = "launching"
	// CheckpointStepPreClean precedes a name-based delete of a stale
	// resource left by an earlier agent of the same name.
	CheckpointStepPreClean = "pre_clean"
)

// launchHooks carries RunConfig's async-launch hooks into the helpers that
// make the resource-creating calls. Its zero value (both hooks nil, the
// synchronous path) makes every method a no-op.
type launchHooks struct {
	checkpointFn func(ctx context.Context, step string) error
	createdFn    func(api.ResourceHandle)
}

// launchHooks returns config's async-launch hooks.
func (config *RunConfig) launchHooks() launchHooks {
	return launchHooks{checkpointFn: config.Checkpoint, createdFn: config.OnResourceCreated}
}

// checkpoint is called immediately before a resource-creating call. A
// non-nil error means the launch is over and the resource must not be
// created.
func (h launchHooks) checkpoint(ctx context.Context, step string) error {
	if h.checkpointFn == nil {
		return nil
	}
	return h.checkpointFn(ctx, step)
}

// active reports whether these are an async launch's hooks (a resource
// handle is being recorded), as opposed to the synchronous path. It is keyed
// on OnResourceCreated alone: it selects the cleanup owner (see
// RunConfig.OnResourceCreated), and only a caller that records handles can
// clean up what the runtime leaves.
func (h launchHooks) active() bool {
	return h.createdFn != nil
}

// created is called after a true create of a launch-owned resource.
func (h launchHooks) created(handle api.ResourceHandle) {
	if h.createdFn == nil {
		return
	}
	h.createdFn(handle)
}

// SharedDirRealization holds the plan for realizing a project's shared
// directories when server.shared_dir_storage.backend is "nfs" (design
// deploy-config-explore §3.2.3/§3.2.4). It is computed once (in
// pkg/agent.resolveSharedDirs) and consumed by the K8s runtime's buildPod,
// which mounts PVClaimName by subPath instead of creating per-dir dynamic
// PVCs. Docker/Podman/Apple consume the equivalent bind-mount VolumeMounts
// directly (from runtime.NFSSharedDirsToVolumeMounts) rather than this
// struct.
type SharedDirRealization struct {
	// Backend is "nfs" — the only realized backend today.
	Backend string
	// PVClaimName is the K8s PVC claim name holding the shared NFS export
	// root. Empty means shared_dir_storage nfs is misconfigured (missing
	// pv_name); buildPod must fail closed rather than fall back to EmptyDir
	// (design G5).
	PVClaimName string
	// SubPaths maps each shared dir name to its subPath within PVClaimName,
	// e.g. "projects/<pid>/shared-dirs/<name>".
	SubPaths map[string]string
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
	// ExecWithStdin runs cmd with stdin piped from the given reader, instead
	// of embedding data in the command's argv. Callers delivering secrets
	// (e.g. a token) into a container MUST use this instead of interpolating
	// the secret into cmd: argv (including heredoc bodies passed via `sh -c`)
	// becomes part of the outer host process's command line and is readable
	// via /proc/<pid>/cmdline for the lifetime of the exec, even though a
	// heredoc keeps the secret out of the *inner* command's argv. See #1355.
	ExecWithStdin(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error)
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
