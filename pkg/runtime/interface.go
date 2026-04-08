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
	Debug                bool
	MetadataInterception bool     // Add NET_ADMIN cap for iptables-based metadata server interception
	ExtraHosts           []string // Extra /etc/hosts entries (e.g. "host.docker.internal:host-gateway")
	NetworkMode          string   // Container network mode (e.g. "host" for --network=host)
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
	PullImage(ctx context.Context, image string) error
	Sync(ctx context.Context, id string, direction SyncDirection) error
	Exec(ctx context.Context, id string, cmd []string) (string, error)
	// GetWorkspacePath returns the host path to the container's /workspace mount.
	// This is used for workspace sync operations.
	GetWorkspacePath(ctx context.Context, id string) (string, error)
	// ExecUser returns the container user for exec/attach commands. Most
	// runtimes return "scion". Rootless Podman returns "root" because the
	// child process runs as UID 0 (which maps to the unprivileged host user).
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
