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
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os/exec"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	"golang.org/x/sync/singleflight"
)

type DockerRuntime struct {
	Command string
	Host    string

	// listGroup collapses concurrent List calls into a single `docker ps`
	// exec. Every caller (PTY lookup, message delivery, heartbeat) issues
	// the exact same command regardless of labelFilter — filtering happens
	// afterward in Go — so there is never a reason to run it twice at once.
	// Its zero value is ready to use.
	listGroup singleflight.Group
}

func NewDockerRuntime() *DockerRuntime {
	return &DockerRuntime{
		Command: "docker",
	}
}

func (r *DockerRuntime) Name() string {
	return "docker"
}

func (r *DockerRuntime) ExecUser() string {
	return "scion"
}

func (r *DockerRuntime) Run(ctx context.Context, config RunConfig) (string, error) {
	if err := prepareContainerSecretEnv(&config); err != nil {
		return "", err
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		return "", err
	}

	// sciontool already handles PID 1 responsibilities (zombie reaping, signal forwarding),
	// so we don't use --init to avoid competing init processes.
	newArgs := []string{"run", "-t"}

	// Apply resource constraints from config.
	//
	// TODO(cgroup-limits): rootless Podman on cgroup v1 cannot set resource
	// limits at all — passing --cpus (or --memory) makes the container fail to
	// start, and the error is returned from here, so the agent start fails hard
	// rather than degrading to an unlimited container. Docker itself is not
	// affected; the exposure is in the sibling adapter in podman.go, which has
	// an identical block and carries the same TODO.
	//
	// This matters now that config.BuiltinDefaultResources() supplies a default
	// limits.cpu of "2" for every agent, so --cpus is emitted on hosts that
	// previously never saw it. The planned fix is to probe the host once and
	// skip the resource flags with a warning when limits are unsupported;
	// podman.go already detects rootless mode (detectRootlessMode), so only the
	// cgroup-version half of the probe is missing. That probe is deliberately
	// not implemented here. Until it lands, affected deployments must opt out
	// via `runtime.enforce_resource_defaults: false`.
	newArgs, err = appendContainerResourceArgs(newArgs, config.Resources)
	if err != nil {
		return "", err
	}

	newArgs = append(newArgs, args[1:]...)

	WriteRuntimeDebugFile(config, r.Command, newArgs)

	out, err := runSimpleCommand(ctx, r.Command, newArgs...)
	if err != nil {
		if ctx.Err() != nil {
			// The caller gave up while the daemon may still have been
			// creating/starting the container. Clean up any partial result
			// instead of leaking it. See ptone/scion#1886.
			rollbackCancelledCreate(r.Command, config.Name)
			return "", ctx.Err()
		}
		return "", fmt.Errorf("container run failed: %w (output: %s)", err, out)
	}

	return strings.TrimSpace(out), nil
}

func (r *DockerRuntime) Stop(ctx context.Context, id string) error {
	out, err := runSimpleCommand(ctx, r.Command, "stop", id)
	if err != nil && out != "" {
		// Include runtime's stderr output in the error so callers can match
		// on messages like "not running" or "No such container".
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
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

// dockerListFormat renders exactly the fields in dockerListOutput as JSON.
//
// Do not replace this with "{{json .}}": that template references every
// container field, including .Size, which makes the Docker CLI request
// size=1 from the daemon. Computing sizes walks every container's overlay
// filesystem (snapshotter.Usage) and fails the whole `docker ps` call when a
// file disappears mid-walk, which is routine for agent containers with busy
// /tmp directories. See ptone/scion#1867.
const dockerListFormat = `{"ID":{{json .ID}},"Names":{{json .Names}},"Status":{{json .Status}},"Image":{{json .Image}},"Labels":{{json .Labels}}}`

// Bounded retry/backoff for `docker ps`. Even with the size-free format
// (dockerListFormat, see #1867/#1888) `docker ps` can still fail
// intermittently — a busy daemon, a momentarily locked overlay snapshot,
// etc. These failures are typically transient, and List has ~20 callers
// (PTY attach, message delivery, heartbeat) that previously hard-failed on
// the first error. See #1864.
const (
	dockerListMaxAttempts    = 3
	dockerListInitialBackoff = 150 * time.Millisecond
	dockerListBackoffMult    = 3.0
	// dockerListJitter is the +/- fraction applied to each backoff so that
	// concurrent callers retrying after the same failure don't all land on
	// the daemon at once.
	dockerListJitter = 0.2
	// dockerListGroupTimeout bounds the detached context the singleflight
	// group's shared exec runs under (see List below). It comfortably covers
	// dockerListMaxAttempts worth of retries and backoff plus exec time, so a
	// slow-but-successful `docker ps` isn't cut off before every joined
	// caller would have given up on their own.
	dockerListGroupTimeout = 10 * time.Second
)

func (r *DockerRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	// Use DoChan rather than Do: Do would run the shared exec under
	// whichever caller's ctx happens to start the call, so that caller
	// cancelling would abort docker ps for every other caller collapsed into
	// it. Instead the shared exec runs on its own detached-but-bounded
	// context, and each caller selects between the shared result and its own
	// ctx.Done().
	resultCh := r.listGroup.DoChan("ps", func() (any, error) {
		groupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dockerListGroupTimeout)
		defer cancel()
		return r.execListWithRetry(groupCtx)
	})

	var out []byte
	select {
	case res := <-resultCh:
		if res.Err != nil {
			return nil, res.Err
		}
		out = res.Val.([]byte)
	case <-ctx.Done():
		return nil, fmt.Errorf("docker ps failed: %w", ctx.Err())
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
			actual := labels[k]
			// Fallback for project labels
			if actual == "" {
				switch k {
				case projectcompat.LabelProject:
					actual = projectcompat.ProjectNameFromLabels(labels)
				case projectcompat.LabelProjectID:
					actual = projectcompat.ProjectIDFromLabels(labels)
				case projectcompat.LabelProjectPath:
					actual = projectcompat.ProjectPathFromLabels(labels)
				}
			}

			if actual != v {
				match = false
				break
			}
		}

		if match {
			// Prefer the scion.name label (slugified) over Docker container name
			agentName := labels["scion.name"]
			if agentName == "" {
				agentName = d.Names
			}
			info := api.AgentInfo{
				ContainerID:     d.ID,
				Name:            agentName,
				ContainerStatus: d.Status,
				Phase:           phaseFromContainerStatus(d.Status),
				Image:           d.Image,
				Labels:          labels,
				Annotations:     labels,
				Template:        labels["scion.template"],
				HarnessConfig:   labels["scion.harness_config"],
				HarnessAuth:     labels["scion.harness_auth"],
				Project:         projectcompat.ProjectNameFromLabels(labels),
				ProjectID:       projectcompat.ProjectIDFromLabels(labels),
				ProjectPath:     projectcompat.ProjectPathFromLabels(labels),
				Runtime:         r.Name(),
			}
			if code, ok := ExitCodeFromContainerStatus(d.Status); ok {
				c := code
				info.ExitCode = &c
				if code != 0 {
					info.ExitReason = string(state.ExitReasonCrashed)
				}
			}
			agents = append(agents, info)
		}
	}

	return agents, nil
}

// execListWithRetry runs `docker ps` for List, retrying transient failures
// with jittered backoff. It gives up immediately once ctx is done (including
// the last attempt reaching the deadline), since a retry cannot outlive its
// caller's context anyway.
func (r *DockerRuntime) execListWithRetry(ctx context.Context) ([]byte, error) {
	args := []string{"ps", "-a", "--no-trunc", "--format", dockerListFormat}

	var lastErr error
	backoff := dockerListInitialBackoff
	for attempt := 1; attempt <= dockerListMaxAttempts; attempt++ {
		cmd := exec.CommandContext(ctx, r.Command, args...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return out, nil
		}
		lastErr = fmt.Errorf("docker ps failed: %w", err)

		if attempt == dockerListMaxAttempts || ctx.Err() != nil {
			break
		}

		jitter := 1 + dockerListJitter*(2*rand.Float64()-1)
		sleep := time.Duration(float64(backoff) * jitter)
		runtimeLog.Debug("docker ps failed, retrying", "attempt", attempt, "max_attempts", dockerListMaxAttempts, "backoff", sleep, "error", err)

		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("docker ps failed: %w", ctx.Err())
		case <-timer.C:
		}

		backoff = time.Duration(float64(backoff) * dockerListBackoffMult)
	}

	return nil, lastErr
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

	agent := findContainerAgent(agents, id)
	if agent == nil {
		return fmt.Errorf("agent '%s' container not found, it may have exited and been removed", id)
	}

	// Check if running
	if agent.Phase != string(state.PhaseRunning) {
		return fmt.Errorf("agent '%s' is not running (status: %s), use 'scion start %s' to resume it", id, agent.ContainerStatus, id)
	}

	// Ensure tmux uses the latest client's terminal size so the session
	// redraws correctly on attach (handles containers started before the
	// window-size option was added to session creation).
	_, _ = runSimpleCommand(ctx, r.Command, "exec", "--user", "scion",
		agent.ContainerID, "tmux", "set-option", "-g", "window-size", "latest")

	return runInteractiveCommand(r.Command, "exec", "-it", "--user", "scion", agent.ContainerID, "tmux", "attach", "-t", "scion")
}

func (r *DockerRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	out, err := runSimpleCommand(ctx, r.Command, "image", "inspect", image)
	if err == nil {
		return true, nil
	}
	// Exit-code errors could mean "image not found" OR a daemon-level failure
	// (e.g. daemon unreachable). Both produce exec.ExitError with a non-zero
	// exit code, so we inspect the command output to distinguish the two.
	// Docker prints "No such image" when the image genuinely does not exist.
	if isExitError(err) {
		if isImageNotFoundOutput(out) {
			return false, nil
		}
		return false, err
	}
	return false, err
}

func (r *DockerRuntime) ImageID(ctx context.Context, image string) (string, error) {
	out, err := runSimpleCommand(ctx, r.Command, "image", "inspect", "--format", "{{.ID}}", image)
	if err != nil {
		return "", fmt.Errorf("image inspect failed: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (r *DockerRuntime) RemoveImage(ctx context.Context, image string) error {
	_, err := runSimpleCommand(ctx, r.Command, "rmi", image)
	return err
}

func (r *DockerRuntime) PullImage(ctx context.Context, image string) error {
	out, err := runSimpleCommand(ctx, r.Command, "pull", image)
	if err != nil {
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			return fmt.Errorf("pull %q: %w\n%s", image, err, trimmed)
		}
		return fmt.Errorf("pull %q: %w", image, err)
	}
	return nil
}

func (r *DockerRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {
	agents, err := r.List(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	agent := findContainerAgent(agents, id)
	if agent == nil {
		return fmt.Errorf("agent '%s' container not found", id)
	}

	// Check for GCS volumes
	if encoded := agent.Labels["scion.gcs_volumes"]; encoded != "" {
		return syncGCSVolumes(ctx, encoded, direction)
	}

	// Docker runtime uses bind mounts for normal volumes, so sync is automatic/noop
	return nil
}

func (r *DockerRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	// Resolve slug/name to actual container ID (container names include the
	// project prefix, e.g. "myproject--agent", so the bare slug won't match).
	if agents, err := r.List(ctx, nil); err == nil {
		id = resolveContainerID(agents, id)
	}
	args := append([]string{"exec", "--user", "scion", id}, cmd...)
	return runSimpleCommand(ctx, r.Command, args...)
}

// ExecWithStdin runs cmd inside the container with stdin piped from the
// given reader. The -i flag is required for `docker exec` to attach stdin;
// without it, data written to stdin never reaches the container even though
// os/exec has a Stdin set on the outer `docker` process. See #1355.
func (r *DockerRuntime) ExecWithStdin(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
	if agents, err := r.List(ctx, nil); err == nil {
		id = resolveContainerID(agents, id)
	}
	args := append([]string{"exec", "-i", "--user", "scion", id}, cmd...)
	return runSimpleCommandWithStdin(ctx, stdin, r.Command, args...)
}

// GetWorkspacePath returns the host path to the container's /workspace mount.
func (r *DockerRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	// Use docker inspect to get mount information
	out, err := runSimpleCommand(ctx, r.Command, "inspect", "--format", "{{json .Mounts}}", id)
	if err != nil {
		return "", fmt.Errorf("failed to inspect container: %w", err)
	}

	type mountInfo struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Type        string `json:"Type"`
	}

	var mounts []mountInfo
	if err := json.Unmarshal([]byte(out), &mounts); err != nil {
		return "", fmt.Errorf("failed to parse mounts: %w", err)
	}

	// Look for /workspace mount
	for _, m := range mounts {
		if m.Destination == "/workspace" {
			return m.Source, nil
		}
	}

	return "", fmt.Errorf("no /workspace mount found for container %s", id)
}
