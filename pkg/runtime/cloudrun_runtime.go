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
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

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

const cloudRunInstanceIDMaxLength = 63

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
	return &CloudRunRuntime{
		config: &config.CloudRunConfig{
			ProjectID: cfg.ProjectID,
			Location:  cfg.Region,
		},
	}, nil
}

func (r *CloudRunRuntime) Name() string { return "cloudrun" }

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

func (r *CloudRunRuntime) Run(ctx context.Context, cfg RunConfig) (string, error) {
	parent := fmt.Sprintf("projects/%s/locations/%s", r.config.ProjectID, r.config.Location)
	agentID := cfg.Labels["agent_id"]
	if agentID == "" {
		return "", fmt.Errorf("agent_id label is required")
	}
	instanceID := cloudRunInstanceID(agentID)

	uid := 1000
	gid := 1000
	if cfg.WorkspaceBackendName != "nfs" {
		uid = os.Getuid()
		gid = os.Getgid()
	} else if cfg.NFSUID != 0 {
		uid = cfg.NFSUID
		gid = cfg.NFSGID
	}

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
	_, err = c.GetInstance(ctx, getReq, defaultCallOpts...)
	if err == nil {
		// Instance exists. Try to start it if it's stopped.
		startReq := &runpb.StartInstanceRequest{
			Name: getReq.Name,
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
		labels[k] = v
	}

	inst := &runpb.Instance{
		LaunchStage: googleapi.LaunchStage_ALPHA,
		Containers: []*runpb.Container{
			{
				Name:         "scion-agent",
				Image:        cfg.Image,
				Command:      cfg.CommandArgs,
				Env:          envVars,
				VolumeMounts: volumeMounts,
			},
		},
		Volumes: volumes,
		Labels:  labels,
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

func (r *CloudRunRuntime) provisionCloudRunNFS(ctx context.Context, cfg RunConfig, agentID string, uid, gid int) (*cloudRunNFSProvisionPaths, error) {
	if cfg.WorkspaceBackendName != "nfs" {
		return nil, nil
	}
	if r.config.NFSServer == "" {
		return nil, fmt.Errorf("cloudrun: nfs_server must be non-empty when workspace backend is NFS")
	}
	paths, err := cloudRunNFSExportPaths(r.config.NFSExport, cfg.ProjectID, agentID)
	if err != nil {
		return nil, err
	}
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("cloudrun: cannot provision NFS workspace because RunConfig.Workspace is empty; "+
			"mount the Filestore export into the Hub/Broker and pass the resolved host path for %s, "+
			"or run an external provisioner before creating Cloud Run instances", paths.workspaceExportPath)
	}
	hostPaths, err := cloudRunNFSHostPaths(cfg.Workspace, cfg.ProjectID, agentID)
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

	if gid == 0 {
		gid = 1000
	}
	if uid == 0 {
		uid = 1000
	}
	resolved := ResolvedWorkspace{
		HostPath:           hostPaths.workspaceHostPath,
		ServerRelativePath: path.Join("projects", cfg.ProjectID, "workspace"),
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

func cloudRunNFSExportPaths(nfsExport, projectID, agentID string) (*cloudRunNFSProvisionPaths, error) {
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

	paths := &cloudRunNFSProvisionPaths{
		workspaceExportPath: path.Join(exportRoot, "projects", projectID, "workspace"),
		homeExportPath:      path.Join(exportRoot, "projects", projectID, "agents", agentID, "home"),
		secretsExportPath:   path.Join(exportRoot, "projects", projectID, "agents", agentID, "secrets"),
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
	hostBase          string
	workspaceHostPath string
	homeHostPath      string
	secretsHostPath   string
}

func cloudRunNFSHostPaths(workspaceHostPath, projectID, agentID string) (*cloudRunNFSHostProvisionPaths, error) {
	if err := validateCloudRunNFSElement("project_id", projectID); err != nil {
		return nil, err
	}
	if err := validateCloudRunNFSElement("agent_id", agentID); err != nil {
		return nil, err
	}

	workspaceHostPath = filepath.Clean(workspaceHostPath)
	if !filepath.IsAbs(workspaceHostPath) {
		return nil, fmt.Errorf("cloudrun: NFS workspace host path must be absolute, got %q", workspaceHostPath)
	}
	expectedSuffix := filepath.Join("projects", projectID, "workspace")
	hostSlash := filepath.ToSlash(workspaceHostPath)
	suffixSlash := filepath.ToSlash(expectedSuffix)
	if hostSlash != suffixSlash && !strings.HasSuffix(hostSlash, "/"+suffixSlash) {
		return nil, fmt.Errorf("cloudrun: NFS workspace host path %q must end with %q so it maps to <export>/projects/<project-id>/workspace",
			workspaceHostPath, expectedSuffix)
	}

	projectRoot := filepath.Dir(workspaceHostPath)
	hostBase := filepath.Dir(filepath.Dir(projectRoot))
	if err := ValidateNotExportRoot(workspaceHostPath, hostBase); err != nil {
		return nil, fmt.Errorf("cloudrun: invalid NFS workspace host path: %w", err)
	}
	return &cloudRunNFSHostProvisionPaths{
		hostBase:          hostBase,
		workspaceHostPath: workspaceHostPath,
		homeHostPath:      filepath.Join(projectRoot, "agents", agentID, "home"),
		secretsHostPath:   filepath.Join(projectRoot, "agents", agentID, "secrets"),
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

func (r *CloudRunRuntime) Stop(ctx context.Context, id string) error {
	c, err := r.client(ctx)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}
	defer func() { _ = c.Close() }()

	name := r.instanceResourceName(id)
	req := &runpb.StopInstanceRequest{
		Name: name,
	}

	op, err := c.StopInstance(ctx, req, defaultCallOpts...)
	if err != nil {
		return fmt.Errorf("failed to stop instance: %w", err)
	}

	if _, err := op.Wait(ctx); err != nil {
		return fmt.Errorf("wait for stop operation failed: %w", err)
	}

	return nil
}

func (r *CloudRunRuntime) Delete(ctx context.Context, id string) error {
	c, err := r.client(ctx)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}
	defer func() { _ = c.Close() }()

	name := r.instanceResourceName(id)
	req := &runpb.DeleteInstanceRequest{
		Name: name,
	}

	op, err := c.DeleteInstance(ctx, req, defaultCallOpts...)
	if err != nil {
		return fmt.Errorf("failed to delete instance: %w", err)
	}

	if _, err := op.Wait(ctx); err != nil {
		return fmt.Errorf("wait for delete operation failed: %w", err)
	}

	return nil
}

func (r *CloudRunRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
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
			if inst.Labels[k] != v {
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
			Name:            inst.Name,
			ContainerStatus: status,
			Labels:          inst.Labels,
		})
	}

	return agents, nil
}

func (r *CloudRunRuntime) GetLogs(ctx context.Context, id string) (string, error) {
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
	out, err := r.exec.Exec(ctx, r.config.ProjectID, r.config.Location, id, cmd)
	return string(out), err
}

func (r *CloudRunRuntime) Attach(ctx context.Context, id string) error {
	return r.exec.Connect(ctx, r.config.ProjectID, r.config.Location, id)
}

func (r *CloudRunRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	return "", fmt.Errorf("cloudrun: host workspace paths are not available for Cloud Run instances; use the Hub workspace API")
}

// StreamLogs tails log output in real time (for scion look / scion logs -f).
func (r *CloudRunRuntime) StreamLogs(ctx context.Context, instanceName string, opts cloudrun.LogOptions) (<-chan cloudrun.LogEntry, error) {
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
