package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ptone/scion-agent/pkg/api"
)

type AppleContainerRuntime struct {
	Command string
}

func NewAppleContainerRuntime() *AppleContainerRuntime {
	return &AppleContainerRuntime{
		Command: "container",
	}
}

func (r *AppleContainerRuntime) Name() string {
	return "container"
}

func (r *AppleContainerRuntime) Run(ctx context.Context, config RunConfig) (string, error) {
	args, err := buildCommonRunArgs(config)
	if err != nil {
		return "", err
	}

	// For Apple Container, we want to ensure -d and -t are present for 'run'
	// matching the working manual command.
	newArgs := []string{"run", "-d", "-t"}
	// Skip the original 'run', '-d', and '-i' from buildCommonRunArgs (indices 0, 1, 2)
	newArgs = append(newArgs, args[3:]...)

	out, err := runSimpleCommand(ctx, r.Command, newArgs...)
	if err != nil {
		return "", fmt.Errorf("container run failed: %w (output: %s)", err, out)
	}

	// The output of 'container run -d' is the container ID
	return strings.TrimSpace(out), nil
}

func (r *AppleContainerRuntime) Stop(ctx context.Context, id string) error {
	_, err := runSimpleCommand(ctx, r.Command, "stop", id)
	return err
}

func (r *AppleContainerRuntime) Delete(ctx context.Context, id string) error {
	// For container, we might need to stop it first if we want to delete it,
	// but 'rm' usually works if it's stopped.
	_, err := runSimpleCommand(ctx, r.Command, "rm", id)
	return err
}

type containerListOutput struct {
	Status        string `json:"status"`
	Configuration struct {
		ID     string            `json:"id"`
		Labels map[string]string `json:"labels"`
		Image  struct {
			Reference string `json:"reference"`
		} `json:"image"`
	} `json:"configuration"`
}

func (r *AppleContainerRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	args := []string{"list", "-a", "--format", "json"}

	cmd := exec.CommandContext(ctx, r.Command, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("container list failed: %w (output: %s)", err, string(out))
	}

	var raw []containerListOutput
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse container list output: %w (output: %s)", err, string(out))
	}

	var agents []api.AgentInfo
	for _, c := range raw {
		// Filter by labels if requested
		if len(labelFilter) > 0 {
			match := true
			for k, v := range labelFilter {
				if lv, ok := c.Configuration.Labels[k]; !ok || lv != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}

		agents = append(agents, api.AgentInfo{
			ID:              c.Configuration.ID,
			Name:            c.Configuration.Labels["scion.name"],
			Template:        c.Configuration.Labels["scion.template"],
			Grove:           c.Configuration.Labels["scion.grove"],
			GrovePath:       c.Configuration.Labels["scion.grove_path"],
			Labels:          c.Configuration.Labels,
			Annotations:     c.Configuration.Labels,
			ContainerStatus: c.Status,
			Status:          "created", // Default status, updated by AgentManager logic
			Image:           c.Configuration.Image.Reference,
			Runtime:         r.Name(),
		})
	}

	return agents, nil
}


func (r *AppleContainerRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	return runSimpleCommand(ctx, r.Command, "logs", id)
}

func (r *AppleContainerRuntime) Attach(ctx context.Context, id string) error {
	// 1. Find container to check for tmux label
	agents, err := r.List(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	var a *api.AgentInfo
	for _, agent := range agents {
		// Match by full ID, or name
		if agent.ID == id || agent.Name == id || strings.TrimPrefix(agent.Name, "/") == id {
			a = &agent
			break
		}
	}

	if a == nil {
		return fmt.Errorf("agent '%s' container not found. It may have exited and been removed.", id)
	}

	// Check if running
	status := strings.ToLower(a.ContainerStatus)
	if !strings.HasPrefix(status, "up") && status != "running" {
		return fmt.Errorf("agent '%s' is not running (status: %s). Use 'scion start %s' to resume it.", id, a.ContainerStatus, id)
	}

	if a.Labels["scion.tmux"] == "true" {
		// container exec -it <id> tmux attach -t scion
		return runInteractiveCommand(r.Command, "exec", "-it", a.ID, "tmux", "attach", "-t", "scion")
	}

	return fmt.Errorf("apple container runtime does not support 'attach' without tmux")
}

func (r *AppleContainerRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	_, err := runSimpleCommand(ctx, r.Command, "image", "inspect", image)
	return err == nil, nil
}

func (r *AppleContainerRuntime) PullImage(ctx context.Context, image string) error {
	return runInteractiveCommand(r.Command, "image", "pull", image)
}

func (r *AppleContainerRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {

	// Apple container runtime uses bind mounts (if configured), so sync is likely automatic/noop

	return nil

}
