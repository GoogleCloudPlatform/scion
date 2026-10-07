/*
Package runtime implements the Cloud Run Instances runtime for Scion.

Service Account and IAM Requirements:
The Runtime Broker executing this code must have a Service Account with the following IAM roles:
- roles/run.admin (to manage Cloud Run Instances)
- roles/iam.serviceAccountUser (to attach the runtime service account to instances)
- roles/logging.viewer (to stream and retrieve logs)
- roles/iap.tunnelResourceAccessor (to exec into instances via IAP)

Authentication Methods:

 1. GKE Workload Identity (Recommended for GKE-hosted brokers):
    Bind a Kubernetes Service Account (KSA) to the Google Service Account (GSA) using:
    `gcloud iam service-accounts add-iam-policy-binding <GSA_EMAIL> \
    --role roles/iam.workloadIdentityUser \
    --member "serviceAccount:<PROJECT_ID>.svc.id.goog[<NAMESPACE>/<KSA_NAME>]"`
    Annotate the KSA: `kubectl annotate sa <KSA_NAME> iam.gke.io/gcp-service-account=<GSA_EMAIL>`

 2. Key File (For VM-hosted brokers or local testing):
    Set the GOOGLE_APPLICATION_CREDENTIALS environment variable to point to a valid JSON key file
    downloaded from the GCP console for the target Service Account.
*/
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/iterator"
	googleapi "google.golang.org/genproto/googleapis/api"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const cloudRunInstanceIDMaxLength = 49

var defaultCallOpts = []gax.CallOption{
	gax.WithRetry(func() gax.Retryer {
		return gax.OnCodes([]codes.Code{
			codes.Unavailable,       // 503
			codes.ResourceExhausted, // 429
		}, gax.Backoff{
			Initial:    100 * time.Millisecond,
			Max:        10 * time.Second,
			Multiplier: 1.3,
		})
	}),
}

type CloudRunRuntime struct {
	config *config.CloudRunConfig
	exec   cloudrun.ExecConnector
	// newClient overrides how the Instances API client is constructed. Nil in
	// production (the real Cloud Run Admin API); tests inject a fake so the
	// lifecycle can be exercised without GCP credentials.
	newClient func(ctx context.Context) (cloudrun.InstancesAPI, error)

	// resolveMu protects the resolution of GCP metadata. When ProjectID
	// and Location are both empty (auto-detected Cloud Run environment),
	// resolveConfig populates them from the GCE metadata server on first
	// use. A mutex+bool is used instead of sync.Once so transient errors
	// can be retried on subsequent calls.
	resolveMu sync.Mutex
	resolved  bool
}

func NewCloudRunRuntime(cfg *config.CloudRunConfig) (*CloudRunRuntime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("CloudRunConfig cannot be nil")
	}
	// When both ProjectID and Location are empty, this is an auto-detected
	// Cloud Run environment (K_SERVICE set, no explicit settings). The
	// runtime is valid — project/region will be discovered from GCP metadata
	// when API calls are made. Validate only when fields are provided.
	if cfg.ProjectID != "" || cfg.Location != "" {
		if cfg.ProjectID == "" {
			return nil, fmt.Errorf("cloudrun: ProjectID must be non-empty when Location is set")
		}
		if cfg.Location == "" || len(strings.Split(cfg.Location, "-")) < 2 {
			return nil, fmt.Errorf("cloudrun: Location must be a valid GCP region format (e.g., 'us-central1'), got %q", cfg.Location)
		}
	}

	execConn := cloudrun.NewIAPExecConnector("") // IapTunnelUrlOverride can be handled later if added to config
	return &CloudRunRuntime{config: cfg, exec: execConn}, nil
}

// NewCloudRunRuntimeFromInstances returns a new CloudRunRuntime from the
// Cloud Run Instances configuration. The instances variant uses ProjectID
// and Region from V1CloudRunInstancesConfig, mapping Region to Location.
func NewCloudRunRuntimeFromInstances(cfg *config.V1CloudRunInstancesConfig) (*CloudRunRuntime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cloudrun-instances: config cannot be nil")
	}
	if cfg.ProjectID == "" {
		return nil, fmt.Errorf("cloudrun-instances: ProjectID must be non-empty")
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("cloudrun-instances: Region must be non-empty")
	}
	execConn := cloudrun.NewIAPExecConnector("") // IapTunnelUrlOverride can be handled later if added to config
	return &CloudRunRuntime{
		config: &config.CloudRunConfig{
			ProjectID: cfg.ProjectID,
			Location:  cfg.Region,
		},
		exec: execConn,
	}, nil
}

// resolveConfig ensures that ProjectID and Location are populated. When both
// are empty (auto-detected Cloud Run environment with no explicit settings),
// this method discovers them from the GCE metadata server. The resolution is
// idempotent and retryable — on success the result is cached; on failure
// subsequent calls will retry, allowing recovery from transient errors.
//
// This fills the gap described in NewCloudRunRuntime: "project/region will be
// discovered from GCP metadata when API calls are made." Without this, Run()
// formats an empty parent ("projects//locations/") causing
// RESOURCE_PROJECT_INVALID from the Cloud Run Instances API.
func (r *CloudRunRuntime) resolveConfig(ctx context.Context) error {
	r.resolveMu.Lock()
	defer r.resolveMu.Unlock()

	if r.resolved {
		return nil
	}

	if r.config.ProjectID != "" && r.config.Location != "" {
		r.resolved = true
		return nil
	}

	projectID := r.config.ProjectID
	if projectID == "" {
		var err error
		projectID, err = metadata.ProjectIDWithContext(ctx)
		if err != nil {
			return fmt.Errorf("cloudrun: auto-detecting ProjectID from GCE metadata: %w", err)
		}
	}

	location := r.config.Location
	if location == "" {
		// On Cloud Run, metadata.Zone() returns a full zone like
		// "projects/NUM/zones/us-central1-1". We need the region
		// portion (e.g. "us-central1"), which is the zone minus
		// the trailing "-<letter/number>" suffix.
		zone, err := metadata.ZoneWithContext(ctx)
		if err != nil {
			return fmt.Errorf("cloudrun: auto-detecting Location from GCE metadata: %w", err)
		}
		// metadata.Zone() returns the bare zone name (e.g. "us-central1-1").
		// Strip the last hyphen-delimited segment to derive the region.
		if idx := strings.LastIndex(zone, "-"); idx > 0 {
			location = zone[:idx]
		} else {
			location = zone
		}
	}

	// Commit both values atomically only after both resolve successfully.
	r.config.ProjectID = projectID
	r.config.Location = location
	r.resolved = true
	return nil
}

func (r *CloudRunRuntime) Name() string { return "cloudrun" }

// SupportsEmptyPerAgentWorkspace reports false: Run rejects empty-per-agent
// workspaces (rejectEmptyPerAgentOnCloudRun), so a broker whose default
// runtime is Cloud Run must not advertise the capability.
func (r *CloudRunRuntime) SupportsEmptyPerAgentWorkspace() bool { return false }

func (r *CloudRunRuntime) ExecUser() string {
	return "scion"
}

// client creates a new Cloud Run Instances client. Caller is responsible for closing.
func (r *CloudRunRuntime) client(ctx context.Context) (cloudrun.InstancesAPI, error) {
	if r.newClient != nil {
		return r.newClient(ctx)
	}
	return cloudrun.NewInstancesClient(ctx)
}

// cloudRunMaxEnvValueBytes is Cloud Run's size cap for a single environment
// variable value (32 KiB). The name has its own cap, so only the value counts.
const cloudRunMaxEnvValueBytes = 32 * 1024

// cloudRunEnvLimit applies cloudRunMaxEnvValueBytes to values only.
var cloudRunEnvLimit = envSizeLimit{maxBytes: cloudRunMaxEnvValueBytes}

// cloudRunRuntimeEnvKeys are set by buildCloudRunInstance after cfg.Env, so
// an env-type secret must not also supply them (no duplicate EnvVar names).
var cloudRunRuntimeEnvKeys = []string{"SCION_HOST_UID", "SCION_HOST_GID"}

// cloudRunOwnerIDs returns the uid and gid the Cloud Run instance runs
// as and owns its NFS workspace with: the broker's own ids for a
// non-NFS backend, otherwise the configured NFS ids with 0 (unset)
// meaning 1000, as in buildCommonRunArgs.
func cloudRunOwnerIDs(cfg RunConfig) (uid, gid int) {
	if cfg.WorkspaceBackendName != "nfs" {
		return os.Getuid(), os.Getgid()
	}
	return nfsOwnerIDs(cfg.NFSUID, cfg.NFSGID)
}

func (r *CloudRunRuntime) Run(ctx context.Context, cfg RunConfig) (string, error) {
	// Checked before anything is resolved or provisioned: this runtime
	// always mounts the project's shared NFS workspace (see
	// provisionCloudRunNFS), which would break empty-per-agent isolation.
	if err := rejectEmptyPerAgentOnCloudRun(cfg); err != nil {
		return "", err
	}
	// Deliver resolved secrets through the instance env: env-type secrets as
	// plain variables, file/variable secrets as the staged blob that
	// sciontool init writes out. Done before any provisioning so an
	// oversized secret fails fast.
	if _, err := applyResolvedSecretsToEnv(&cfg, cloudRunEnvLimit, cloudRunRuntimeEnvKeys...); err != nil {
		return "", fmt.Errorf("cloudrun: %w", err)
	}
	if err := r.resolveConfig(ctx); err != nil {
		return "", fmt.Errorf("failed to resolve Cloud Run config: %w", err)
	}
	parent := fmt.Sprintf("projects/%s/locations/%s", r.config.ProjectID, r.config.Location)
	agentID := cfg.Labels["agent_id"]
	if agentID == "" {
		return "", fmt.Errorf("agent_id label is required")
	}
	instanceID := cloudRunInstanceID(agentID)

	uid, gid := cloudRunOwnerIDs(cfg)

	nfsPaths, err := r.provisionCloudRunNFS(ctx, cfg, agentID, uid, gid)
	if err != nil {
		return "", err
	}

	c, err := r.client(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to create client: %w", err)
	}
	defer func() { _ = c.Close() }()

	// Check if the instance already exists
	getReq := &runpb.GetInstanceRequest{
		Name: fmt.Sprintf("%s/instances/%s", parent, instanceID),
	}
	existing, err := c.GetInstance(ctx, getReq, defaultCallOpts...)
	if err == nil {
		// Instance exists. The instance ID is deterministic per agent, so
		// it may belong to another run (Start's pre-clean normally deletes
		// it first; this is reached after a failed listing or a race). The
		// Instances API (cloud.google.com/go/run v1.21.0) has no
		// UpdateInstance, so an instance's labels change only by recreating
		// it, and reusing another run's instance would serve this run under
		// that run's label: refuse it with ErrRunConflict (ptone/scion#2550).
		// An instance of this run is started. So is a legacy instance with
		// no run label, left unstamped: it keeps reporting no run ID, and
		// the legacy rule lets this run's stop or delete still target it.
		// Either start carries the read's etag, so an instance replaced
		// after the read is not started.
		runKey := sanitizeGCPLabelKey(api.LabelRunID)
		if want := sanitizeGCPLabelValue(cfg.Labels[api.LabelRunID]); want != "" {
			if run := existing.GetLabels()[runKey]; run != "" && run != want {
				runtimeLog.Info("Cloud Run instance of another run holds the agent's instance ID; not reusing it",
					"instance", instanceID, "run_id", want, "instance_run_id", run)
				return "", fmt.Errorf("instance %s belongs to run %q, not %q: %w", instanceID, run, want, ErrRunConflict)
			}
		}
		startReq := &runpb.StartInstanceRequest{
			Name: getReq.Name,
			Etag: existing.GetEtag(),
		}
		op, err := c.StartInstance(ctx, startReq, defaultCallOpts...)
		if err != nil {
			return "", fmt.Errorf("failed to start existing instance: %w", err)
		}
		if _, err := op.Wait(ctx); err != nil {
			return "", fmt.Errorf("wait for start operation failed: %w", err)
		}
		return instanceID, nil
	}
	if status.Code(err) != codes.NotFound {
		return "", fmt.Errorf("failed to get instance %s: %w", instanceID, err)
	}

	// Instance doesn't exist, proceed to create.
	inst := r.buildCloudRunInstance(cfg, uid, gid, nfsPaths)

	req := &runpb.CreateInstanceRequest{
		Parent:     parent,
		InstanceId: instanceID,
		Instance:   inst,
	}

	op, err := c.CreateInstance(ctx, req, defaultCallOpts...)
	if err != nil {
		return "", fmt.Errorf("failed to create instance: %w", err)
	}

	if _, err := op.Wait(ctx); err != nil {
		return "", fmt.Errorf("wait for create operation failed: %w", err)
	}

	return instanceID, nil
}

func (r *CloudRunRuntime) buildCloudRunInstance(cfg RunConfig, uid, gid int, nfsPaths *cloudRunNFSProvisionPaths) *runpb.Instance {
	var envVars []*runpb.EnvVar
	for _, e := range cfg.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envVars = append(envVars, &runpb.EnvVar{
				Name:   parts[0],
				Values: &runpb.EnvVar_Value{Value: parts[1]},
			})
		}
	}

	envVars = append(envVars, &runpb.EnvVar{
		Name:   "SCION_HOST_UID",
		Values: &runpb.EnvVar_Value{Value: fmt.Sprintf("%d", uid)},
	})
	envVars = append(envVars, &runpb.EnvVar{
		Name:   "SCION_HOST_GID",
		Values: &runpb.EnvVar_Value{Value: fmt.Sprintf("%d", gid)},
	})

	var volumes []*runpb.Volume
	var volumeMounts []*runpb.VolumeMount

	if nfsPaths != nil {
		workspaceMount := cfg.ContainerWorkspace
		if workspaceMount == "" {
			workspaceMount = "/workspace"
		}

		volumes = append(volumes, &runpb.Volume{
			Name: "workspace",
			VolumeType: &runpb.Volume_Nfs{
				Nfs: &runpb.NFSVolumeSource{
					Server:   r.config.NFSServer,
					Path:     nfsPaths.workspaceExportPath,
					ReadOnly: false,
				},
			},
		})
		volumeMounts = append(volumeMounts, &runpb.VolumeMount{
			Name:      "workspace",
			MountPath: workspaceMount,
		})

		volumes = append(volumes, &runpb.Volume{
			Name: "home",
			VolumeType: &runpb.Volume_Nfs{
				Nfs: &runpb.NFSVolumeSource{
					Server:   r.config.NFSServer,
					Path:     nfsPaths.homeExportPath,
					ReadOnly: false,
				},
			},
		})
		volumeMounts = append(volumeMounts, &runpb.VolumeMount{
			Name:      "home",
			MountPath: "/home/" + r.ExecUser(),
		})

		volumes = append(volumes, &runpb.Volume{
			Name: "secrets",
			VolumeType: &runpb.Volume_Nfs{
				Nfs: &runpb.NFSVolumeSource{
					Server:   r.config.NFSServer,
					Path:     nfsPaths.secretsExportPath,
					ReadOnly: true,
				},
			},
		})
		volumeMounts = append(volumeMounts, &runpb.VolumeMount{
			Name:      "secrets",
			MountPath: "/home/" + r.ExecUser() + "/.scion/secrets",
		})
	}

	labels := make(map[string]string)
	for k, v := range cfg.Labels {
		labels[sanitizeGCPLabelKey(k)] = sanitizeGCPLabelValue(v)
	}

	// Build the container command. Cloud Run Instances have no TTY, so the
	// harness is wrapped in tmux (which allocates a PTY) following the same
	// pattern as cloudrun-sandbox (buildEntrypoint in
	// cloudrun_sandbox_runtime.go).
	//
	// The image ENTRYPOINT is "sciontool init --" (from scion-base), so we
	// set Args (CMD override) to the tmux-wrapped command. sciontool init
	// receives it as the child process to supervise.
	//
	// Previously, cfg.CommandArgs was passed as Container.Command (which
	// overrides ENTRYPOINT), bypassing both GetCommand() and tmux. The CLI
	// then ran without a PTY and crashed: "bubbletea: could not open TTY".
	container := &runpb.Container{
		Name:         "scion-agent",
		Image:        cfg.Image,
		Env:          envVars,
		VolumeMounts: volumeMounts,
	}

	if cfg.NoAuth || cfg.Harness != nil {
		// harnessCmdLine covers both branches (NoAuth checked first,
		// matching the priority order above); ok is always true here.
		cmdLine, _ := harnessCmdLine(cfg)
		agentWindowCmd := tmuxAgentWindowCmd("/bin/sh", cmdLine)
		// Use poll loop instead of attach-session: CRI has no TTY for PID 1,
		// so tmux attach-session would fail with "not a terminal". The poll
		// loop tracks the tmux session's lifetime without needing a terminal,
		// matching the cloudrun-sandbox pattern.
		tmuxCmd := buildTmuxStartCmd(agentWindowCmd, tmuxPollSession)
		container.Args = []string{"/bin/sh", "-c", tmuxCmd}
	} else if len(cfg.CommandArgs) > 0 {
		// Fallback: no harness, pass raw command args as CMD override.
		container.Args = cfg.CommandArgs
	}
	// If none of the above, image defaults (ENTRYPOINT + CMD) are used.

	inst := &runpb.Instance{
		LaunchStage: googleapi.LaunchStage_ALPHA,
		Annotations: map[string]string{
			// Cloud Run Instances default to OnFailure, which kills the instance
			// permanently on clean exit (code 0). Agents that complete a task and
			// exit 0 must be restarted so they stay available for new messages.
			"run.googleapis.com/restart-policy": "Always",
		},
		Containers: []*runpb.Container{container},
		Volumes:    volumes,
		Labels:     labels,
	}

	if r.config != nil {
		inst.ServiceAccount = r.config.ServiceAccount
		if r.config.Network != "" || r.config.Subnetwork != "" {
			inst.VpcAccess = &runpb.VpcAccess{
				NetworkInterfaces: []*runpb.VpcAccess_NetworkInterface{
					{
						Network:    r.config.Network,
						Subnetwork: r.config.Subnetwork,
					},
				},
			}
		}
	}
	return inst
}

// cloudRunShortInstanceID returns the bare instance ID for either a short ID
// or a full "projects/<p>/locations/<l>/instances/<id>" resource name. Run
// returns short IDs while the Instances API reports full resource names, so
// either form can reach Stop, Delete or GetLogs.
func cloudRunShortInstanceID(id string) string {
	if idx := strings.LastIndex(id, "/"); idx != -1 {
		return id[idx+1:]
	}
	return id
}

// instanceResourceName builds the fully qualified instance resource name,
// accepting either a short ID or an already-qualified name so that qualifying
// an ID twice is impossible.
func (r *CloudRunRuntime) instanceResourceName(id string) string {
	return fmt.Sprintf("projects/%s/locations/%s/instances/%s",
		r.config.ProjectID, r.config.Location, cloudRunShortInstanceID(id))
}

func cloudRunInstanceID(agentID string) string {
	slug := api.Slugify(agentID)
	sum := sha256.Sum256([]byte(agentID))
	suffix := hex.EncodeToString(sum[:])[:10]
	if slug == "" {
		slug = "agent"
	}

	prefix := "agent-"
	maxSlugLen := cloudRunInstanceIDMaxLength - len(prefix) - 1 - len(suffix)
	if len(slug) > maxSlugLen {
		slug = strings.TrimRight(slug[:maxSlugLen], "-")
	}
	if slug == "" {
		slug = "agent"
	}
	return fmt.Sprintf("%s%s-%s", prefix, slug, suffix)
}

type cloudRunNFSProvisionPaths struct {
	workspaceExportPath string
	homeExportPath      string
	secretsExportPath   string
	hostBase            string
	workspaceHostPath   string
	homeHostPath        string
	secretsHostPath     string
}

// provisionCloudRunNFS prepares the agent's NFS directories. uid and gid
// must already be defaulted by the caller (cloudRunOwnerIDs); they are
// used as given.
func (r *CloudRunRuntime) provisionCloudRunNFS(ctx context.Context, cfg RunConfig, agentID string, uid, gid int) (*cloudRunNFSProvisionPaths, error) {
	if cfg.WorkspaceBackendName != "nfs" {
		return nil, nil
	}
	if r.config.NFSServer == "" {
		return nil, fmt.Errorf("cloudrun: nfs_server must be non-empty when workspace backend is NFS")
	}
	paths, err := cloudRunNFSExportPaths(r.config.NFSExport, cfg.NFSSubPathRoot, cfg.ProjectID, agentID)
	if err != nil {
		return nil, err
	}
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("cloudrun: cannot provision NFS workspace because RunConfig.Workspace is empty; "+
			"mount the Filestore export into the Hub/Broker and pass the resolved host path for %s, "+
			"or run an external provisioner before creating Cloud Run instances", paths.workspaceExportPath)
	}
	hostPaths, err := cloudRunNFSHostPaths(cfg.Workspace, cfg.NFSSubPathRoot, cfg.ProjectID, agentID)
	if err != nil {
		return nil, err
	}
	paths.hostBase = hostPaths.hostBase
	paths.workspaceHostPath = hostPaths.workspaceHostPath
	paths.homeHostPath = hostPaths.homeHostPath
	paths.secretsHostPath = hostPaths.secretsHostPath

	if err := requireNFSFilesystem(hostPaths.hostBase); err != nil {
		return nil, err
	}

	resolved := ResolvedWorkspace{
		HostPath:           hostPaths.workspaceHostPath,
		ServerRelativePath: hostPaths.serverRelativePath,
		HostBase:           hostPaths.hostBase,
		Backend:            "nfs",
		SharedDirs:         map[string]ResolvedSharedDir{},
	}
	if err := ProvisionShared(ProvisionInput{
		Ctx:       ctx,
		Resolved:  resolved,
		ProjectID: cfg.ProjectID,
		AgentID:   agentID,
		Mode:      store.SharingModeSharedPlain,
		GitClone:  cfg.GitClone,
		Locker:    cfg.Locker,
		NFSUID:    uid,
		NFSGID:    gid,
	}); err != nil {
		return nil, fmt.Errorf("cloudrun: provision NFS workspace %s via Hub-mounted path %s: %w; "+
			"verify the Hub Cloud Run service mounts the Filestore export read/write or run an external provisioner",
			paths.workspaceExportPath, hostPaths.workspaceHostPath, err)
	}
	if err := mkdirNFSAgentDir(hostPaths.homeHostPath, uid, gid); err != nil {
		return nil, fmt.Errorf("cloudrun: provision NFS agent home %s via Hub-mounted path %s: %w",
			paths.homeExportPath, hostPaths.homeHostPath, err)
	}
	if err := mkdirNFSAgentDir(hostPaths.secretsHostPath, uid, gid); err != nil {
		return nil, fmt.Errorf("cloudrun: provision NFS agent secrets %s via Hub-mounted path %s: %w",
			paths.secretsExportPath, hostPaths.secretsHostPath, err)
	}

	return paths, nil
}

// cloudRunNFSExportPaths builds the server-side export paths of an agent's
// workspace, home and secrets under <nfs_export>/<subpath_root>/<project>.
// subPathRoot is workspace_storage.nfs.subpath_root as configured (empty
// means the default); it is validated before any path is built from it.
func cloudRunNFSExportPaths(nfsExport, subPathRoot, projectID, agentID string) (*cloudRunNFSProvisionPaths, error) {
	if nfsExport == "" {
		return nil, fmt.Errorf("cloudrun: nfs_export must be non-empty when workspace backend is NFS")
	}
	exportRoot := path.Clean(nfsExport)
	if !path.IsAbs(exportRoot) {
		return nil, fmt.Errorf("cloudrun: nfs_export must be an absolute server path, got %q", nfsExport)
	}
	if err := validateCloudRunNFSElement("project_id", projectID); err != nil {
		return nil, err
	}
	if err := validateCloudRunNFSElement("agent_id", agentID); err != nil {
		return nil, err
	}
	root, err := config.ResolveSubPathRoot(subPathRoot)
	if err != nil {
		return nil, fmt.Errorf("cloudrun: invalid NFS %w", err)
	}
	// Export paths are always slash-separated server paths, hence path.Join.
	root = filepath.ToSlash(root)

	paths := &cloudRunNFSProvisionPaths{
		workspaceExportPath: path.Join(exportRoot, root, projectID, "workspace"),
		homeExportPath:      path.Join(exportRoot, root, projectID, "agents", agentID, "home"),
		secretsExportPath:   path.Join(exportRoot, root, projectID, "agents", agentID, "secrets"),
	}
	for name, p := range map[string]string{
		"workspace": paths.workspaceExportPath,
		"home":      paths.homeExportPath,
		"secrets":   paths.secretsExportPath,
	} {
		if err := validateUnixPathBelowRoot(p, exportRoot); err != nil {
			return nil, fmt.Errorf("cloudrun: invalid NFS %s path: %w", name, err)
		}
	}
	return paths, nil
}

type cloudRunNFSHostProvisionPaths struct {
	// serverRelativePath is <subpath_root>/<project>/workspace, relative to
	// hostBase (and to the export root).
	serverRelativePath string
	hostBase           string
	workspaceHostPath  string
	homeHostPath       string
	secretsHostPath    string
}

// cloudRunNFSHostPaths derives the Hub-mounted host paths of an agent from
// the resolved workspace host path, which must end with
// <subpath_root>/<project>/workspace. subPathRoot is
// workspace_storage.nfs.subpath_root as configured (empty means the
// default); it is validated before any path is built from it.
func cloudRunNFSHostPaths(workspaceHostPath, subPathRoot, projectID, agentID string) (*cloudRunNFSHostProvisionPaths, error) {
	if err := validateCloudRunNFSElement("project_id", projectID); err != nil {
		return nil, err
	}
	if err := validateCloudRunNFSElement("agent_id", agentID); err != nil {
		return nil, err
	}

	root, err := config.ResolveSubPathRoot(subPathRoot)
	if err != nil {
		return nil, fmt.Errorf("cloudrun: invalid NFS %w", err)
	}

	workspaceHostPath = filepath.Clean(workspaceHostPath)
	if !filepath.IsAbs(workspaceHostPath) {
		return nil, fmt.Errorf("cloudrun: NFS workspace host path must be absolute, got %q", workspaceHostPath)
	}
	expectedSuffix := filepath.Join(root, projectID, "workspace")
	hostSlash := filepath.ToSlash(workspaceHostPath)
	suffixSlash := filepath.ToSlash(expectedSuffix)
	if !strings.HasSuffix(hostSlash, "/"+suffixSlash) {
		return nil, fmt.Errorf("cloudrun: NFS workspace host path %q must end with %q so it maps to <export>/%s/<project-id>/workspace",
			workspaceHostPath, expectedSuffix, filepath.ToSlash(root))
	}

	projectRoot := filepath.Dir(workspaceHostPath)
	// The host base is what remains once the <subpath_root>/<project>/workspace
	// suffix is removed; subpath_root may span several path segments.
	hostBase := filepath.Clean(filepath.FromSlash(strings.TrimSuffix(hostSlash, "/"+suffixSlash)))
	if hostBase == "" || hostBase == "." {
		hostBase = string(filepath.Separator)
	}
	if err := ValidateNotExportRoot(workspaceHostPath, hostBase); err != nil {
		return nil, fmt.Errorf("cloudrun: invalid NFS workspace host path: %w", err)
	}
	return &cloudRunNFSHostProvisionPaths{
		serverRelativePath: path.Join(filepath.ToSlash(root), projectID, "workspace"),
		hostBase:           hostBase,
		workspaceHostPath:  workspaceHostPath,
		homeHostPath:       filepath.Join(projectRoot, "agents", agentID, "home"),
		secretsHostPath:    filepath.Join(projectRoot, "agents", agentID, "secrets"),
	}, nil
}

func validateCloudRunNFSElement(name, value string) error {
	if value == "" || value == "." || value == ".." ||
		strings.Contains(value, "/") || strings.Contains(value, "\\") {
		return fmt.Errorf("cloudrun: %s %q is not a safe NFS path element", name, value)
	}
	return nil
}

func validateUnixPathBelowRoot(child, root string) error {
	child = path.Clean(child)
	root = path.Clean(root)
	if child == root {
		return fmt.Errorf("path %q equals export root %q; Cloud Run agents must mount project subtrees, never the export root", child, root)
	}
	if root == "/" {
		if !path.IsAbs(child) || child == "/" {
			return fmt.Errorf("path %q is not below export root %q", child, root)
		}
		return nil
	}
	if !strings.HasPrefix(child, root+"/") {
		return fmt.Errorf("path %q is not below export root %q", child, root)
	}
	return nil
}

func mkdirNFSAgentDir(dir string, uid, gid int) error {
	if err := os.MkdirAll(dir, 0770); err != nil {
		return err
	}
	_ = os.Chown(dir, uid, gid)
	return nil
}

// Stop stops the Cloud Run instance ref.ID. The instance ID is
// deterministic per agent, so another run can hold it; when ref.RunID is set
// the stop is run-checked (see runScopedInstanceOp) and an instance of
// another run is left running with ErrRunMismatch (ptone/scion#2550).
func (r *CloudRunRuntime) Stop(ctx context.Context, ref RunRef) error {
	return r.instanceOp(ctx, ref, "stop", func(c cloudrun.InstancesAPI, name, etag string) (cloudrun.InstanceOperation, error) {
		return c.StopInstance(ctx, &runpb.StopInstanceRequest{Name: name, Etag: etag}, defaultCallOpts...)
	})
}

// Delete removes the Cloud Run instance ref.ID. As for Stop, when ref.RunID
// is set the delete is run-checked and an instance of another run is left
// untouched with ErrRunMismatch.
func (r *CloudRunRuntime) Delete(ctx context.Context, ref RunRef) error {
	return r.instanceOp(ctx, ref, "delete", func(c cloudrun.InstancesAPI, name, etag string) (cloudrun.InstanceOperation, error) {
		return c.DeleteInstance(ctx, &runpb.DeleteInstanceRequest{Name: name, Etag: etag}, defaultCallOpts...)
	})
}

// cloudRunRunCheckAttempts bounds how often a run-checked stop or delete
// re-reads an instance whose etag changed between the read and the call.
const cloudRunRunCheckAttempts = 3

// instanceCall issues a stop or delete for the instance resource name,
// with etag as its precondition ("" for none), and returns the operation.
type instanceCall func(c cloudrun.InstancesAPI, name, etag string) (cloudrun.InstanceOperation, error)

// instanceOp runs a stop or delete (verb) of the instance ref.ID and waits
// for it.
//
// Without ref.RunID the call is made by name with no precondition, as
// before run IDs existed. With it, the call is run-scoped:
//   - the instance is read; one labelled with another run is left untouched
//     and ErrRunMismatch is returned. An instance of ref.RunID, or a legacy
//     one with no run label (the rule k8s and the broker apply), passes;
//   - the call carries the read's etag, so an instance replaced (or changed)
//     after the read is refused by the API (ABORTED or FAILED_PRECONDITION).
//     The instance is then read and checked again, at most
//     cloudRunRunCheckAttempts times in all. If the re-read shows the etag
//     the call carried, nothing changed and that refusal is returned;
//   - an instance that is gone at the read gives the same NotFound error the
//     call itself gives.
func (r *CloudRunRuntime) instanceOp(ctx context.Context, ref RunRef, verb string, call instanceCall) error {
	if err := r.resolveConfig(ctx); err != nil {
		return fmt.Errorf("failed to resolve Cloud Run config: %w", err)
	}
	c, err := r.client(ctx)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}
	defer func() { _ = c.Close() }()

	name := r.instanceResourceName(ref.ID)
	if ref.RunID == "" {
		op, err := call(c, name, "")
		if err != nil {
			return fmt.Errorf("failed to %s instance: %w", verb, err)
		}
		if _, err := op.Wait(ctx); err != nil {
			return fmt.Errorf("wait for %s operation failed: %w", verb, err)
		}
		return nil
	}

	want := sanitizeGCPLabelValue(ref.RunID)
	runKey := sanitizeGCPLabelKey(api.LabelRunID)
	var lastErr error
	var sentEtag string
	for attempt := 0; attempt < cloudRunRunCheckAttempts; attempt++ {
		inst, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name}, defaultCallOpts...)
		if err != nil {
			return fmt.Errorf("failed to %s instance: %w", verb, err)
		}
		if lastErr != nil && inst.GetEtag() == sentEtag {
			// The refusal was not a change: the instance still has the
			// etag the call carried, so the API refused it for another
			// reason (for example a state precondition). Report that
			// refusal rather than retrying it.
			return fmt.Errorf("failed to %s instance: %w", verb, lastErr)
		}
		if run := inst.GetLabels()[runKey]; run != "" && run != want {
			runtimeLog.Info("Left a Cloud Run instance of another run untouched",
				"instance", name, "verb", verb, "run_id", ref.RunID, "instance_run_id", run)
			return fmt.Errorf("instance %s belongs to run %q, not %q: %w", cloudRunShortInstanceID(name), run, want, ErrRunMismatch)
		}
		sentEtag = inst.GetEtag()
		op, err := call(c, name, sentEtag)
		if err != nil {
			if code := status.Code(err); code == codes.Aborted || code == codes.FailedPrecondition {
				// The instance may have changed after the read: re-read
				// and re-check.
				runtimeLog.Info("Cloud Run instance changed before a run-checked call; re-checking",
					"instance", name, "verb", verb, "run_id", ref.RunID, "attempt", attempt+1, "error", err)
				lastErr = err
				continue
			}
			return fmt.Errorf("failed to %s instance: %w", verb, err)
		}
		if _, err := op.Wait(ctx); err != nil {
			return fmt.Errorf("wait for %s operation failed: %w", verb, err)
		}
		return nil
	}
	return fmt.Errorf("failed to %s instance: it kept changing during %d run-checked attempts: %w", verb, cloudRunRunCheckAttempts, lastErr)
}

func (r *CloudRunRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	if err := r.resolveConfig(ctx); err != nil {
		return nil, fmt.Errorf("failed to resolve Cloud Run config: %w", err)
	}
	c, err := r.client(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}
	defer func() { _ = c.Close() }()

	parent := fmt.Sprintf("projects/%s/locations/%s", r.config.ProjectID, r.config.Location)
	req := &runpb.ListInstancesRequest{
		Parent: parent,
	}

	it := c.ListInstances(ctx, req, defaultCallOpts...)
	var agents []api.AgentInfo
	for {
		inst, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error listing instances: %w", err)
		}

		match := true
		for k, v := range labelFilter {
			if inst.Labels[sanitizeGCPLabelKey(k)] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}

		status := "unknown"
		// map state to status
		if inst.TerminalCondition != nil {
			status = inst.TerminalCondition.State.String()
		}

		agents = append(agents, api.AgentInfo{
			ID:              inst.Labels["agent_id"],
			ContainerID:     cloudRunShortInstanceID(inst.Name),
			RunID:           inst.Labels[sanitizeGCPLabelKey(api.LabelRunID)], // Run stored scion.run_id under its GCP-sanitized key
			Name:            inst.Name,
			ContainerStatus: status,
			Labels:          inst.Labels,
		})
	}

	return agents, nil
}

func (r *CloudRunRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	if err := r.resolveConfig(ctx); err != nil {
		return "", fmt.Errorf("failed to resolve Cloud Run config: %w", err)
	}
	logClient, err := cloudrun.NewLogClient(ctx, r.config.ProjectID)
	if err != nil {
		return "", fmt.Errorf("initializing log client: %w", err)
	}
	defer func() { _ = logClient.Close() }()

	entries, err := logClient.GetLogs(ctx, cloudRunShortInstanceID(id), cloudrun.LogOptions{Lines: 100}) // default lines
	if err != nil {
		return "", fmt.Errorf("fetching logs: %w", err)
	}

	var sb strings.Builder
	for _, entry := range entries {
		sb.WriteString(entry.Message)
		if !strings.HasSuffix(entry.Message, "\n") {
			sb.WriteString("\n")
		}
	}
	return sb.String(), nil
}

func (r *CloudRunRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	if strings.TrimSpace(image) == "" {
		return false, fmt.Errorf("cloudrun: image must be non-empty")
	}
	return true, nil
}

func (r *CloudRunRuntime) ImageID(ctx context.Context, image string) (string, error) {
	return "", fmt.Errorf("cloudrun: ImageID not yet implemented")
}

func (r *CloudRunRuntime) RemoveImage(ctx context.Context, image string) error {
	return fmt.Errorf("cloudrun: RemoveImage not yet implemented")
}

func (r *CloudRunRuntime) PullImage(ctx context.Context, image string) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("cloudrun: image must be non-empty")
	}
	return nil
}

func (r *CloudRunRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {
	return fmt.Errorf("cloudrun: agent workspace sync is not supported by the Cloud Run runtime; use the Hub workspace API for hosted agents")
}

func (r *CloudRunRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	if r.exec == nil {
		return "", fmt.Errorf("cloudrun: exec connector not configured")
	}
	if err := r.resolveConfig(ctx); err != nil {
		return "", fmt.Errorf("failed to resolve Cloud Run config: %w", err)
	}
	out, err := r.exec.Exec(ctx, r.config.ProjectID, r.config.Location, id, cmd)
	return string(out), err
}

// ExecWithStdin runs cmd on the instance with stdin piped from the given
// reader, instead of embedding data in cmd's argv. See #1355.
func (r *CloudRunRuntime) ExecWithStdin(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
	if r.exec == nil {
		return "", fmt.Errorf("cloudrun: exec connector not configured")
	}
	if err := r.resolveConfig(ctx); err != nil {
		return "", fmt.Errorf("failed to resolve Cloud Run config: %w", err)
	}
	out, err := r.exec.ExecWithStdin(ctx, r.config.ProjectID, r.config.Location, id, cmd, stdin)
	return string(out), err
}

func (r *CloudRunRuntime) Attach(ctx context.Context, id string) error {
	if r.exec == nil {
		return fmt.Errorf("cloudrun: exec connector not configured")
	}
	if err := r.resolveConfig(ctx); err != nil {
		return fmt.Errorf("failed to resolve Cloud Run config: %w", err)
	}
	return r.exec.Connect(ctx, r.config.ProjectID, r.config.Location, id)
}

func (r *CloudRunRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	return "", fmt.Errorf("cloudrun: host workspace paths are not available for Cloud Run instances; use the Hub workspace API")
}

// StreamLogs tails log output in real time (for scion look / scion logs -f).
func (r *CloudRunRuntime) StreamLogs(ctx context.Context, instanceName string, opts cloudrun.LogOptions) (<-chan cloudrun.LogEntry, error) {
	if err := r.resolveConfig(ctx); err != nil {
		return nil, fmt.Errorf("failed to resolve Cloud Run config: %w", err)
	}
	logClient, err := cloudrun.NewLogClient(ctx, r.config.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("initializing log client: %w", err)
	}
	// Note: We don't defer logClient.Close() here because it's streaming.
	// The client might need to be closed later, or we can rely on GC/context cancellation.

	ch, err := logClient.StreamLogs(ctx, instanceName, opts)
	if err != nil {
		_ = logClient.Close()
		return nil, fmt.Errorf("streaming logs: %w", err)
	}

	// Create a wrapper channel to close the client when context is done or channel is closed
	outCh := make(chan cloudrun.LogEntry)
	go func() {
		defer func() { _ = logClient.Close() }()
		defer close(outCh)
		for entry := range ch {
			select {
			case outCh <- entry:
			case <-ctx.Done():
				return
			}
		}
	}()
	return outCh, nil
}

// sanitizeGCPLabelKey converts a label key to comply with GCP naming constraints.
// GCP label keys must: start with a lowercase letter, contain only lowercase letters,
// digits, underscores, and dashes, and be at most 63 characters long.
// Dots are replaced with underscores.
func sanitizeGCPLabelKey(key string) string {
	key = strings.Map(func(r rune) rune {
		if r == '.' || r == '/' {
			return '_'
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32 // lowercase
		}
		return '_'
	}, key)
	// GCP label keys must start with a lowercase letter. Prepend "k_" if
	// the sanitized key begins with a digit, underscore, or dash.
	if len(key) > 0 && (key[0] < 'a' || key[0] > 'z') {
		key = "k_" + key
	}
	if len(key) > 63 {
		key = key[:63]
	}
	return key
}

// sanitizeGCPLabelValue ensures a label value complies with GCP constraints.
// Values must be at most 63 characters, contain only lowercase letters, digits,
// underscores, and dashes, and must start and end with alphanumeric characters.
// Empty values are allowed.
func sanitizeGCPLabelValue(value string) string {
	if value == "" {
		return value
	}
	value = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32 // lowercase
		}
		return '_'
	}, value)
	// GCP label values must start and end with an alphanumeric character.
	value = strings.TrimFunc(value, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	if len(value) > 63 {
		value = value[:63]
	}
	return value
}

// errEmptyPerAgentCloudRun is returned by CloudRunRuntime.Run for an
// empty-per-agent agent (design #2703).
var errEmptyPerAgentCloudRun = errors.New("cloudrun: \"Empty directory per agent\" (empty-per-agent) workspaces are not supported on the Cloud Run runtime, " +
	"which always mounts the project's shared workspace; use a Docker, Podman, Apple or Kubernetes broker for this project")

// isEmptyPerAgentRun reports whether cfg starts an empty-per-agent agent,
// identified by SCION_WORKSPACE_MODE in its env. Runtimes that cannot
// provide the private workspace use it to refuse the mode at Run, which
// backs up their SupportsEmptyPerAgentWorkspace=false opt-out for requests
// that reach the broker before its first heartbeat corrects the static
// registration capabilities.
func isEmptyPerAgentRun(cfg RunConfig) bool {
	for _, kv := range cfg.Env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == "SCION_WORKSPACE_MODE" && store.ResolveWorkspaceSharingMode(v) == store.SharingModeEmptyPerAgent {
			return true
		}
	}
	return false
}

// rejectEmptyPerAgentOnCloudRun fails when cfg starts an empty-per-agent
// agent.
func rejectEmptyPerAgentOnCloudRun(cfg RunConfig) error {
	if isEmptyPerAgentRun(cfg) {
		return errEmptyPerAgentCloudRun
	}
	return nil
}
