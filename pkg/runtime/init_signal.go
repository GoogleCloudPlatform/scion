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
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// InitSignaler is implemented by runtimes that can deliver the credential
// reload signal to an agent container's main process (sciontool init, PID 1)
// from the runtime side, rather than by running a command inside the
// container. The runtime broker's reset-auth prefers it over an in-container
// `kill -USR2 1`; runtimes that do not implement it keep that exec path.
type InitSignaler interface {
	// SignalInitReload sends the credential reload signal to the main
	// process of the container identified by id.
	SignalInitReload(ctx context.Context, id string) error
}

// initReloadSignal is the signal sciontool init handles as "re-read the
// agent credential file and restart the refresh loop". It is passed by name
// (SIG-prefixed), which the docker, podman and Apple container CLIs all
// accept for `kill --signal`.
const initReloadSignal = "SIGUSR2"

// initReloadKillArgs returns the CLI arguments that send initReloadSignal to
// the main process of container id. id must be a container reference: an
// empty id or one starting with '-' is refused so it can never be read as a
// flag.
func initReloadKillArgs(id string) ([]string, error) {
	if id == "" || strings.HasPrefix(id, "-") {
		return nil, fmt.Errorf("invalid container reference for reload signal")
	}
	return []string{"kill", "--signal", initReloadSignal, id}, nil
}

// signalInitReloadWithCLI sends the reload signal to container id (already
// resolved against the runtime's listed agents) with `<command> kill`.
func signalInitReloadWithCLI(ctx context.Context, command, id string) error {
	args, err := initReloadKillArgs(id)
	if err != nil {
		return err
	}
	_, err = runSimpleCommand(ctx, command, args...)
	return err
}

// SignalInitReload implements InitSignaler with `docker kill --signal`.
func (r *DockerRuntime) SignalInitReload(ctx context.Context, id string) error {
	return signalInitReloadWithCLI(ctx, r.Command, resolveListedID(ctx, r.List, id))
}

// SignalInitReload implements InitSignaler with `podman kill --signal`.
func (r *PodmanRuntime) SignalInitReload(ctx context.Context, id string) error {
	return signalInitReloadWithCLI(ctx, r.Command, resolveListedID(ctx, r.List, id))
}

// SignalInitReload implements InitSignaler with `container kill --signal`.
func (r *AppleContainerRuntime) SignalInitReload(ctx context.Context, id string) error {
	return signalInitReloadWithCLI(ctx, r.Command, resolveListedID(ctx, r.List, id))
}

// resolveListedID maps a slug or name to the listed container ID, falling
// back to id when the list fails or has no match (the same resolution Exec
// uses).
func resolveListedID(ctx context.Context, list func(context.Context, map[string]string) ([]api.AgentInfo, error), id string) string {
	if agents, err := list(ctx, nil); err == nil {
		return resolveContainerID(agents, id)
	}
	return id
}

var (
	_ InitSignaler = (*DockerRuntime)(nil)
	_ InitSignaler = (*PodmanRuntime)(nil)
	_ InitSignaler = (*AppleContainerRuntime)(nil)
)
