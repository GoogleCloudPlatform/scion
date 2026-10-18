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
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/startuplistener"
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
	// provisionNFS overrides how Run prepares the agent's NFS directories.
	// Nil in production (provisionCloudRunNFS); tests inject a stub so Run
	// can reach the Instances API without a mounted NFS export.
	provisionNFS func(ctx context.Context, cfg RunConfig, agentID string, uid, gid int) (*cloudRunNFSProvisionPaths, error)

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

// NewCloudRunRuntimeForTest builds a CloudRunRuntime for cfg whose Instances
// API client comes from newClient. Exported so tests in other packages (e.g.
// pkg/agent, pkg/runtimebroker) can drive the real runtime through a fake
// Instances client, as NewCloudRunSandboxRuntimeForTest does for the
// sandbox runtime.
func NewCloudRunRuntimeForTest(cfg *config.CloudRunConfig, newClient func(ctx context.Context) (cloudrun.InstancesAPI, error)) (*CloudRunRuntime, error) {
	rt, err := NewCloudRunRuntime(cfg)
	if err != nil {
		return nil, err
	}
	rt.newClient = newClient
	return rt, nil
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

// SupportsAttach implements AttachCapableRuntime and reports false: the
// agent terminal path cannot serve interactive attach for Cloud Run
// instances, so the broker refuses attach up front (and reports it as
// unsupported to the Hub) instead of failing after the stream opens. A
// later terminal over conduit may add attach (ptone/scion#2782). The
// cloudrun-sandbox runtime is separate and keeps attach support.
func (r *CloudRunRuntime) SupportsAttach() bool { return false }

var _ AttachCapableRuntime = (*CloudRunRuntime)(nil)

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
var cloudRunRuntimeEnvKeys = []string{"SCION_HOST_UID", "SCION_HOST_GID", startuplistener.EnvPort}

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
	// Also checked up front: buildCloudRunInstance mounts the workspace,
	// agent home and staged files only from the NFS share, so an instance
	// on any other backend has none of them and can never run.
	if err := rejectNonNFSOnCloudRun(cfg); err != nil {
		return "", err
	}
	if err := r.checkOwnerLabel(cfg); err != nil {
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

	// Async-launch gate before the NFS workspace and agent directories
	// are provisioned (ptone/scion#4761); a no-op on the
	// synchronous path. Nothing has been written yet.
	hooks := cfg.launchHooks()
	if err := hooks.checkpoint(ctx, CheckpointStepWorkspaceProvision); err != nil {
		return "", err
	}

	provision := r.provisionCloudRunNFS
	if r.provisionNFS != nil {
		provision = r.provisionNFS
	}
	nfsPaths, err := provision(ctx, cfg, agentID, uid, gid)
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
		if existing == nil {
			// Not an unlabelled legacy instance with no etag: refuse
			// rather than reuse it unchecked.
			return "", fmt.Errorf("failed to get instance %s: GetInstance returned no instance", instanceID)
		}
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
		// Either start carries the read's etag, meant to keep an instance
		// replaced after the read from being started. Whether the API
		// enforces it on a start is unverified; it does not on a delete
		// (ptone/scion#4760).
		runKey := sanitizeGCPLabelKey(api.LabelRunID)
		want := sanitizeGCPLabelValue(cfg.Labels[api.LabelRunID])
		run := existing.GetLabels()[runKey]
		if want != "" && run != "" && run != want {
			runtimeLog().Info("Cloud Run instance of another run holds the agent's instance ID; not reusing it",
				"instance", instanceID, "run_id", want, "instance_run_id", run)
			return "", fmt.Errorf("instance %s belongs to run %q, not %q: %w", instanceID, run, want, ErrRunConflict)
		}
		if err := hooks.checkpoint(ctx, CheckpointStepInstanceStart); err != nil {
			return "", err
		}
		// Only this run's own instance is reported: a legacy instance with
		// no run label is not this launch's to clean up (DeleteResource
		// would skip it anyway).
		if want != "" && run == want {
			hooks.created(api.ResourceHandle{Kind: api.ResourceKindCloudRunInstance, Namespace: parent, Name: instanceID, UID: cfg.Labels[api.LabelRunID]})
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

	// Async-launch gate immediately before CreateInstance
	// (ptone/scion#4761); a no-op on the synchronous path.
	if err := hooks.checkpoint(ctx, CheckpointStepInstanceCreate); err != nil {
		return "", err
	}
	// The handle is reported before the create is issued, so a create cut
	// off mid-request still leaves the launch's cleanup something to
	// delete. With the hooks active the create starts in doubt: until it
	// is observed, DeleteResource re-polls a NotFound instead of treating
	// it as gone (cloudRunCreatesInDoubt).
	runID := cfg.Labels[api.LabelRunID]
	if hooks.active() {
		markCloudRunCreateInDoubt(ctx, getReq.Name, runID)
	}
	hooks.created(api.ResourceHandle{Kind: api.ResourceKindCloudRunInstance, Namespace: parent, Name: instanceID, UID: runID})

	op, err := c.CreateInstance(ctx, req, defaultCallOpts...)
	if err != nil {
		if cloudRunServerRejected(ctx, err) {
			clearCloudRunCreateInDoubt(getReq.Name, runID)
		}
		return "", fmt.Errorf("failed to create instance: %w", err)
	}

	if _, err := op.Wait(ctx); err != nil {
		return "", fmt.Errorf("wait for create operation failed: %w", err)
	}
	// The create has finished, so it can no longer appear later. That is
	// not proof the instance exists: the operation succeeds even when the
	// instance was deleted while it ran (ptone/scion#4760).
	clearCloudRunCreateInDoubt(getReq.Name, runID)

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
	// sciontool init opens the startup listener on this port, which the
	// container's startup probe below checks.
	envVars = append(envVars, &runpb.EnvVar{
		Name:   startuplistener.EnvPort,
		Values: &runpb.EnvVar_Value{Value: strconv.Itoa(startuplistener.Port)},
	})

	// The template env fallback (ptone/scion#4524) is the LAST env step:
	// only names no source above set (env-type secrets are already folded
	// into cfg.Env), never a suppressed key.
	emitted := make(map[string]bool, len(envVars))
	for _, ev := range envVars {
		emitted[ev.GetName()] = true
	}
	for _, e := range FallbackEnvEntries(cfg.FallbackEnv, cfg.SuppressedFallbackKeys, emitted) {
		k, v, _ := strings.Cut(e, "=")
		envVars = append(envVars, &runpb.EnvVar{Name: k, Values: &runpb.EnvVar_Value{Value: v}})
	}

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
					Server:   nfsPaths.server,
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
					Server:   nfsPaths.server,
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
					Server:   nfsPaths.server,
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
		// The agent serves nothing, so without an explicit probe the
		// platform's default TCP startup probe on 8080 never succeeds and
		// the instance is restarted about every 4 minutes. Probe sciontool's
		// startup listener instead: every 10s, up to 24 failures (a 240s
		// budget, the same as the default probe's).
		Ports: []*runpb.ContainerPort{{ContainerPort: startuplistener.Port}},
		StartupProbe: &runpb.Probe{
			PeriodSeconds:    10,
			FailureThreshold: 24,
			ProbeType: &runpb.Probe_TcpSocket{
				TcpSocket: &runpb.TCPSocketAction{Port: startuplistener.Port},
			},
		},
	}

	if cfg.NoAuth || cfg.Harness != nil {
		// harnessCmdLine covers both branches (NoAuth checked first,
		// matching the priority order above); ok is always true here.
		cmdLine, _ := harnessCmdLine(cfg, harnessCmdLineOptions{})
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
		// No run.googleapis.com/ annotations: the Instances API rejects
		// system annotations with INVALID_ARGUMENT. The instance therefore
		// gets the platform default restart behaviour, under which an agent
		// that exits 0 may not be restarted. Restoring ALWAYS needs the
		// native Instance.restartPolicy field, which runpb does not expose
		// yet; see ptone/scion#5141.
		LaunchStage: googleapi.LaunchStage_ALPHA,
		Containers:  []*runpb.Container{container},
		Volumes:     volumes,
		Labels:      labels,
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
	// server is the NFS server the instance mounts; see
	// resolveCloudRunNFSTarget.
	server              string
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
	server, export, err := resolveCloudRunNFSTarget(r.config.NFSServer, r.config.NFSExport, cfg.NFSShareServer, cfg.NFSShareExport)
	if err != nil {
		return nil, err
	}
	paths, err := cloudRunNFSExportPaths(export, cfg.NFSSubPathRoot, cfg.ProjectID, agentID)
	if err != nil {
		return nil, err
	}
	paths.server = server
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

// resolveCloudRunNFSTarget returns the NFS server and export the Cloud Run
// instance mounts. The broker provisions the workspace on
// workspace_storage.nfs.shares[0], so the instance must mount that same
// share. An empty cloudrun.nfs_server or cloudrun.nfs_export defaults to the
// share's value; a set value that differs from the share is a configuration
// error, since the broker would write to one export and the instance would
// mount another. Exports are compared after path.Clean.
func resolveCloudRunNFSTarget(runtimeServer, runtimeExport, shareServer, shareExport string) (server, export string, err error) {
	server, export = runtimeServer, runtimeExport
	if server == "" {
		server = shareServer
	} else if shareServer != "" && server != shareServer {
		return "", "", fmt.Errorf("cloudrun: runtime setting cloudrun.nfs_server %q does not match "+
			"server.workspace_storage.nfs.shares[0].server %q; the broker provisions workspaces on the share, "+
			"so set cloudrun.nfs_server to the same value or leave it empty to use the share's", runtimeServer, shareServer)
	}
	if export == "" {
		export = shareExport
	} else if shareExport != "" && path.Clean(export) != path.Clean(shareExport) {
		return "", "", fmt.Errorf("cloudrun: runtime setting cloudrun.nfs_export %q does not match "+
			"server.workspace_storage.nfs.shares[0].export %q; the broker provisions workspaces on the share, "+
			"so set cloudrun.nfs_export to the same value or leave it empty to use the share's", runtimeExport, shareExport)
	}
	if server == "" {
		return "", "", fmt.Errorf("cloudrun: nfs_server must be non-empty when workspace backend is NFS " +
			"(set cloudrun.nfs_server or server.workspace_storage.nfs.shares[0].server)")
	}
	if export == "" {
		return "", "", fmt.Errorf("cloudrun: nfs_export must be non-empty when workspace backend is NFS " +
			"(set cloudrun.nfs_export or server.workspace_storage.nfs.shares[0].export)")
	}
	return server, export, nil
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
	return r.instanceOp(ctx, ref, "stop", false, func(c cloudrun.InstancesAPI, name, etag string) (cloudrun.InstanceOperation, error) {
		return c.StopInstance(ctx, &runpb.StopInstanceRequest{Name: name, Etag: etag}, defaultCallOpts...)
	})
}

// Delete removes the Cloud Run instance ref.ID. As for Stop, when ref.RunID
// is set the delete is run-checked and an instance of another run is left
// untouched with ErrRunMismatch. The check is on the read only: the API
// does not enforce the etag a delete carries (a made-up etag was
// accepted, ptone/scion#4760).
func (r *CloudRunRuntime) Delete(ctx context.Context, ref RunRef) error {
	return r.instanceOp(ctx, ref, "delete", false, func(c cloudrun.InstancesAPI, name, etag string) (cloudrun.InstanceOperation, error) {
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
//   - the call carries the read's etag. The API does not enforce it on a
//     delete (a made-up etag was accepted, ptone/scion#4760), so
//     for a delete the run label check on the read is the only guard, and
//     an instance replaced in the short time between the read and the
//     delete is not detected; the API has no other delete precondition.
//     Whether a stop enforces it is unverified. When the API does refuse
//     the call (ABORTED or FAILED_PRECONDITION), the instance is read and
//     checked again, at most cloudRunRunCheckAttempts times in all. If the
//     re-read shows the etag the call carried, nothing changed and that
//     refusal is returned;
//   - an instance that is gone at the read gives the same NotFound error the
//     call itself gives.
//
// strict (the launch cleanup, DeleteResource) also leaves a legacy
// instance with no run label untouched and returns errCloudRunUnlabelled;
// user stop and delete pass false and keep the legacy rule.
func (r *CloudRunRuntime) instanceOp(ctx context.Context, ref RunRef, verb string, strict bool, call instanceCall) error {
	if err := r.resolveConfig(ctx); err != nil {
		return fmt.Errorf("failed to resolve Cloud Run config: %w", err)
	}
	c, err := r.client(ctx)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}
	defer func() { _ = c.Close() }()
	return r.instanceOpWith(ctx, c, ref, verb, strict, call)
}

// instanceOpWith is instanceOp on the open client c, which the caller
// owns and closes; the caller has resolved the config. DeleteResource uses
// it to share one client across its retries.
func (r *CloudRunRuntime) instanceOpWith(ctx context.Context, c cloudrun.InstancesAPI, ref RunRef, verb string, strict bool, call instanceCall) error {
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
		if inst == nil {
			// Not an unlabelled legacy instance with no etag: refuse
			// rather than call without the run check and precondition.
			return fmt.Errorf("failed to %s instance: GetInstance returned no instance", verb)
		}
		if lastErr != nil && inst.GetEtag() == sentEtag {
			// The refusal was not a change: the instance still has the
			// etag the call carried, so the API refused it for another
			// reason (for example a state precondition). Report that
			// refusal rather than retrying it.
			return fmt.Errorf("failed to %s instance: %w", verb, lastErr)
		}
		if strict && inst.GetLabels()[runKey] == "" {
			return fmt.Errorf("instance %s: %w", cloudRunShortInstanceID(name), errCloudRunUnlabelled)
		}
		if run := inst.GetLabels()[runKey]; run != "" && run != want {
			runtimeLog().Info("Left a Cloud Run instance of another run untouched",
				"instance", name, "verb", verb, "run_id", ref.RunID, "instance_run_id", run)
			return fmt.Errorf("instance %s belongs to run %q, not %q: %w", cloudRunShortInstanceID(name), run, want, ErrRunMismatch)
		}
		sentEtag = inst.GetEtag()
		op, err := call(c, name, sentEtag)
		if err != nil {
			if code := status.Code(err); code == codes.Aborted || code == codes.FailedPrecondition {
				// The instance may have changed after the read: re-read
				// and re-check.
				runtimeLog().Info("Cloud Run refused a run-checked call; re-checking",
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
	return fmt.Errorf("failed to %s instance: refused in each of %d run-checked attempts: %w", verb, cloudRunRunCheckAttempts, lastErr)
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

		phase, status := cloudRunInstancePhase(inst)

		agents = append(agents, api.AgentInfo{
			ID:              inst.Labels["agent_id"],
			ContainerID:     cloudRunShortInstanceID(inst.Name),
			RunID:           inst.Labels[sanitizeGCPLabelKey(api.LabelRunID)], // Run stored scion.run_id under its GCP-sanitized key
			Name:            cloudRunAgentName(inst),
			ProjectID:       inst.Labels[sanitizeGCPLabelKey(projectkeys.LabelProjectID)],
			ContainerStatus: status,
			Phase:           phase,
			Labels:          inst.Labels,
		})
	}

	return agents, nil
}

// cloudRunInstancePhase maps inst to the agent phase and container status
// List reports (ptone/scion#5270). In order:
//   - delete_time set: "stopped" (the instance is soft-deleted);
//   - reconciling: "provisioning" (the system is still acting on it);
//   - Ready condition succeeded: "provisioning" (see below);
//   - Ready condition failed: "error";
//   - otherwise (no condition, unspecified, pending, reconciling state):
//     "provisioning".
//
// A succeeded Ready condition is ambiguous: it is reported for a serving
// instance, and may also be what a stopped instance reports, since the API
// has no run-state field and the shape after StopInstance is unverified
// (ptone/scion#5295). Reporting it as "running" would make Start's
// pre-clean keep a stopped instance instead of recreating it, so it is
// reported as "provisioning", a non-terminal phase that is not running,
// until that shape is known.
//
// The container status is the raw condition state ("unknown" without a
// condition), with ",reconciling" appended while reconciling.
func cloudRunInstancePhase(inst *runpb.Instance) (phase, containerStatus string) {
	containerStatus = "unknown"
	condState := runpb.Condition_STATE_UNSPECIFIED
	if tc := inst.GetTerminalCondition(); tc != nil {
		condState = tc.GetState()
		containerStatus = condState.String()
	}
	if inst.GetReconciling() {
		containerStatus += ",reconciling"
	}

	switch {
	case inst.GetDeleteTime() != nil:
		phase = string(state.PhaseStopped)
	case inst.GetReconciling():
		phase = string(state.PhaseProvisioning)
	case condState == runpb.Condition_CONDITION_SUCCEEDED:
		phase = string(state.PhaseProvisioning)
	case condState == runpb.Condition_CONDITION_FAILED:
		phase = string(state.PhaseError)
	default:
		phase = string(state.PhaseProvisioning)
	}
	return phase, containerStatus
}

// cloudRunAgentName is the agent name List reports for inst: the agent slug
// from the scion.name label (stored under its GCP-sanitized key), as
// DockerRuntime.List reports it, not the instance resource name. The
// broker's delete and stop resolution and AgentManager match the hub-side
// agent name against Name, so a resource-name Name never matches and the
// instance is left behind (ptone/scion#5268). An instance without the label
// falls back to its resource name.
func cloudRunAgentName(inst *runpb.Instance) string {
	if name := inst.GetLabels()[sanitizeGCPLabelKey("scion.name")]; name != "" {
		return name
	}
	return inst.GetName()
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

// StreamLogs tails log output in real time.
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

// errCloudRunNeedsNFSWorkspace is returned, wrapped with the backend name,
// by CloudRunRuntime.Run for a workspace backend other than "nfs"
// (ptone/scion#5264).
var errCloudRunNeedsNFSWorkspace = errors.New("cloudrun: the Cloud Run runtime needs an NFS (Filestore) workspace share: " +
	"it mounts the agent's workspace and home only from that share. Set server.workspace_storage.backend to \"nfs\" " +
	"with the Filestore share in server.workspace_storage.nfs.shares[0] (server and export), mounted on the broker " +
	"under server.workspace_storage.nfs.mount_root, or use a Docker, Podman, Apple or Kubernetes broker for this project")

// rejectNonNFSOnCloudRun fails when cfg's workspace backend is not "nfs".
func rejectNonNFSOnCloudRun(cfg RunConfig) error {
	if cfg.WorkspaceBackendName == "nfs" {
		return nil
	}
	backend := cfg.WorkspaceBackendName
	if backend == "" {
		backend = "none"
	}
	return fmt.Errorf("%w (workspace backend: %s)", errCloudRunNeedsNFSWorkspace, backend)
}

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
