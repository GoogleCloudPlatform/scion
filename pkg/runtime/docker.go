package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ptone/scion-agent/pkg/api"
)

type DockerRuntime struct {
	Command string
	Host    string
}

func NewDockerRuntime() *DockerRuntime {
	return &DockerRuntime{
		Command: "docker",
	}
}

func (r *DockerRuntime) Name() string {
	return "docker"
}

func (r *DockerRuntime) Run(ctx context.Context, config RunConfig) (string, error) {
	args, err := buildCommonRunArgs(config)
	if err != nil {
		return "", err
	}

	// Docker supports --init, which we want to use if possible.
	// We insert it after 'run'
	newArgs := []string{"run", "--init", "-t"}
	newArgs = append(newArgs, args[1:]...)

	out, err := runSimpleCommand(ctx, r.Command, newArgs...)
	if err != nil {
		return "", fmt.Errorf("container run failed: %w (output: %s)", err, out)
	}

	return strings.TrimSpace(out), nil
}

func (r *DockerRuntime) Stop(ctx context.Context, id string) error {
	_, err := runSimpleCommand(ctx, r.Command, "stop", id)
	return err
}

func (r *DockerRuntime) Delete(ctx context.Context, id string) error {
	_, err := runSimpleCommand(ctx, r.Command, "rm", "-f", id)
	return err
}

type dockerListOutput struct {
	ID     string `json:"ID"`
	Names  string `json:"Names"`
	Status string `json:"Status"`
	Image  string `json:"Image"`
	Labels string `json:"Labels"`
}

func (r *DockerRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	args := []string{"ps", "-a", "--no-trunc", "--format", "{{json .}}"}
	cmd := exec.CommandContext(ctx, r.Command, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker ps failed: %w", err)
	}

	var agents []api.AgentInfo
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		var d dockerListOutput
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			continue
		}

		labels := make(map[string]string)
		for _, pair := range strings.Split(d.Labels, ",") {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 {
				labels[parts[0]] = parts[1]
			}
		}

		// Filter by labels if requested
		match := true
		for k, v := range labelFilter {
			if labels[k] != v {
				match = false
				break
			}
		}

		if match {
			agents = append(agents, api.AgentInfo{
				ID:              d.ID,
				Name:            d.Names,
				ContainerStatus: d.Status,
				Status:          "created", // Default status, updated by AgentManager logic
				Image:           d.Image,
				Labels:          labels,
				Annotations:     labels,
				Template:    labels["scion.template"],
				Grove:       labels["scion.grove"],
				GrovePath:   labels["scion.grove_path"],
				Runtime:     r.Name(),
			})
		}
	}

	return agents, nil
}

func (r *DockerRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	return runSimpleCommand(ctx, r.Command, "logs", id)
}

func (r *DockerRuntime) Attach(ctx context.Context, id string) error {
	// We need to find the container first to handle names properly
	agents, err := r.List(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		// Match by full ID, short ID (12 chars), or name (with or without leading slash)
		if a.ID == id || (len(id) >= 12 && strings.HasPrefix(a.ID, id)) || (len(a.ID) >= 12 && strings.HasPrefix(id, a.ID)) ||
			a.Name == id || a.Name == "/"+id || strings.TrimPrefix(a.Name, "/") == id {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' container not found. It may have exited and been removed.", id)
	}

	// Check if running
	status := strings.ToLower(agent.ContainerStatus)
	if !strings.HasPrefix(status, "up") && status != "running" {
		return fmt.Errorf("agent '%s' is not running (status: %s). Use 'scion start %s' to resume it.", id, agent.ContainerStatus, id)
	}

	if agent.Labels["scion.tmux"] == "true" {
		return runInteractiveCommand(r.Command, "exec", "-it", agent.ID, "tmux", "attach", "-t", "scion")
	}

	return runInteractiveCommand(r.Command, "attach", agent.ID)
}

func (r *DockerRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	_, err := runSimpleCommand(ctx, r.Command, "image", "inspect", image)
	return err == nil, nil
}

func (r *DockerRuntime) PullImage(ctx context.Context, image string) error {
	return runInteractiveCommand(r.Command, "pull", image)
}

func (r *DockerRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {

	// Docker runtime uses bind mounts, so sync is automatic/noop

	return nil

}
