package runtime

import (
	"context"
	"fmt"
	"strings"

	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"google.golang.org/api/iterator"
)

type CloudRunRuntime struct {
	config *config.CloudRunInstancesConfig
}

func NewCloudRunRuntime(cfg *config.CloudRunInstancesConfig) (*CloudRunRuntime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("CloudRunInstancesConfig cannot be nil")
	}
	return &CloudRunRuntime{config: cfg}, nil
}

// NewCloudRunRuntimeFromInstances returns a new CloudRunRuntime from the
// Cloud Run Instances configuration. The instances variant uses ProjectID
// rather than Project, but the underlying runtime is the same.
func NewCloudRunRuntimeFromInstances(cfg *config.V1CloudRunInstancesConfig) *CloudRunRuntime {
	rt := &CloudRunRuntime{}
	if cfg != nil {
		rt.Project = cfg.ProjectID
		rt.Region = cfg.Region
	}
	return rt
}

func (r *CloudRunRuntime) Name() string { return "cloudrun" }

func (r *CloudRunRuntime) ExecUser() string {
	return "scion"
}

// client creates a new Cloud Run Instances client. Caller is responsible for closing.
func (r *CloudRunRuntime) client(ctx context.Context) (*run.InstancesClient, error) {
	return run.NewInstancesClient(ctx)
}

func (r *CloudRunRuntime) Run(ctx context.Context, cfg RunConfig) (string, error) {
	c, err := r.client(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to create client: %w", err)
	}
	defer c.Close()

	parent := fmt.Sprintf("projects/%s/locations/%s", r.config.ProjectID, r.config.Location)
	agentID := cfg.Labels["agent_id"]
	if agentID == "" {
		return "", fmt.Errorf("agent_id label is required")
	}
	instanceID := "agent-" + agentID

	// Check if the instance already exists
	getReq := &runpb.GetInstanceRequest{
		Name: fmt.Sprintf("%s/instances/%s", parent, instanceID),
	}
	_, err = c.GetInstance(ctx, getReq)
	if err == nil {
		// Instance exists. Try to start it if it's stopped.
		startReq := &runpb.StartInstanceRequest{
			Name: getReq.Name,
		}
		op, err := c.StartInstance(ctx, startReq)
		if err != nil {
			return "", fmt.Errorf("failed to start existing instance: %w", err)
		}
		if _, err := op.Wait(ctx); err != nil {
			return "", fmt.Errorf("wait for start operation failed: %w", err)
		}
		return instanceID, nil
	}

	// Instance doesn't exist or other error, proceed to create
	var envVars []*runpb.EnvVar
	for _, e := range cfg.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envVars = append(envVars, &runpb.EnvVar{
				Name: parts[0],
				Values: &runpb.EnvVar_Value{Value: parts[1]},
			})
		}
	}

	labels := make(map[string]string)
	for k, v := range cfg.Labels {
		labels[k] = v
	}

	inst := &runpb.Instance{
		LaunchStage: 1, // ALPHA
		Containers: []*runpb.Container{
			{
				Name:    "scion-agent",
				Image:   cfg.Image,
				Command: cfg.CommandArgs,
				Env:     envVars,
			},
		},
		Labels: labels,
	}

	req := &runpb.CreateInstanceRequest{
		Parent:     parent,
		InstanceId: instanceID,
		Instance:   inst,
	}

	op, err := c.CreateInstance(ctx, req)
	if err != nil {
		return "", fmt.Errorf("failed to create instance: %w", err)
	}

	if _, err := op.Wait(ctx); err != nil {
		return "", fmt.Errorf("wait for create operation failed: %w", err)
	}

	return instanceID, nil
}

func (r *CloudRunRuntime) Stop(ctx context.Context, id string) error {
	c, err := r.client(ctx)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}
	defer c.Close()

	name := fmt.Sprintf("projects/%s/locations/%s/instances/%s", r.config.ProjectID, r.config.Location, id)
	req := &runpb.StopInstanceRequest{
		Name: name,
	}

	op, err := c.StopInstance(ctx, req)
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
	defer c.Close()

	name := fmt.Sprintf("projects/%s/locations/%s/instances/%s", r.config.ProjectID, r.config.Location, id)
	req := &runpb.DeleteInstanceRequest{
		Name: name,
	}

	op, err := c.DeleteInstance(ctx, req)
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
	defer c.Close()

	parent := fmt.Sprintf("projects/%s/locations/%s", r.config.ProjectID, r.config.Location)
	req := &runpb.ListInstancesRequest{
		Parent: parent,
	}

	it := c.ListInstances(ctx, req)
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
			ContainerID:     inst.Name,
			Name:            inst.Name,
			ContainerStatus: status,
			Labels:          inst.Labels,
		})
	}

	return agents, nil
}

func (r *CloudRunRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	return "", fmt.Errorf("cloudrun: GetLogs not yet implemented in Phase 1")
}

func (r *CloudRunRuntime) Attach(ctx context.Context, id string) error {
	return fmt.Errorf("cloudrun: Attach not yet implemented in Phase 1")
}

func (r *CloudRunRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	return false, fmt.Errorf("cloudrun: ImageExists not yet implemented in Phase 1")
}

func (r *CloudRunRuntime) ImageID(ctx context.Context, image string) (string, error) {
	return "", fmt.Errorf("cloudrun: ImageID not yet implemented")
}

func (r *CloudRunRuntime) RemoveImage(ctx context.Context, image string) error {
	return fmt.Errorf("cloudrun: RemoveImage not yet implemented")
}

func (r *CloudRunRuntime) PullImage(ctx context.Context, image string) error {
	return fmt.Errorf("cloudrun: PullImage not yet implemented in Phase 1")
}

func (r *CloudRunRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {
	return fmt.Errorf("cloudrun: Sync not yet implemented in Phase 1")
}

func (r *CloudRunRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	return "", fmt.Errorf("cloudrun: Exec not yet implemented in Phase 1")
}

func (r *CloudRunRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	return "", fmt.Errorf("cloudrun: GetWorkspacePath not yet implemented in Phase 1")
}
