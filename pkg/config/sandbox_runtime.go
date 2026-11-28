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

package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Sandbox kinds a profile's sandbox_runtime may declare. The kind, not the
// runtime name, decides which extra container flags Scion adds: those flags
// are only safe under the matching sandbox.
const (
	// SandboxKindGVisor is gVisor (runsc). The agent gets every capability,
	// an unconfined seccomp profile at the engine level and an executable
	// tmpfs for /var/lib/docker; all of that exists only inside gVisor's
	// user-space kernel, which applies its own seccomp filter on the host.
	SandboxKindGVisor = "gvisor"
	// SandboxKindSysbox is Sysbox (sysbox-runc). It needs no extra flags.
	SandboxKindSysbox = "sysbox"
)

// V1SandboxRuntime is profiles.<name>.sandbox_runtime.
type V1SandboxRuntime struct {
	// Name is the runtime name registered with the engine (daemon.json
	// "runtimes" key, e.g. "runsc-dind"); passed as --runtime.
	Name string `json:"name" yaml:"name" koanf:"name"`
	// Kind is the sandbox technology: "gvisor" or "sysbox".
	Kind string `json:"kind" yaml:"kind" koanf:"kind"`
	// DockerStorageSize is the size of the executable tmpfs mounted at
	// /var/lib/docker for gVisor (default "6g"). gVisor needs tmpfs as the
	// upper layer for an inner overlay filesystem. The tmpfs uses host memory
	// and counts against the agent's memory limit.
	DockerStorageSize string `json:"docker_storage_size,omitempty" yaml:"docker_storage_size,omitempty" koanf:"docker_storage_size"`
}

// DefaultSandboxDockerStorageSize is the gVisor /var/lib/docker tmpfs size
// when docker_storage_size is unset.
const DefaultSandboxDockerStorageSize = "6g"

var (
	sandboxRuntimeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	sandboxTmpfsSizeRE   = regexp.MustCompile(`^[1-9][0-9]{0,5}[kmg]$`)
)

// Validate checks a sandbox_runtime block. It refuses the default runtime
// names, because the gVisor flags are dangerous under runc.
func (s *V1SandboxRuntime) Validate() error {
	if s == nil {
		return nil
	}
	if !sandboxRuntimeNameRE.MatchString(s.Name) {
		return fmt.Errorf("sandbox_runtime.name %q must be a registered runtime name (letters, digits, '.', '_', '-')", s.Name)
	}
	switch strings.ToLower(s.Name) {
	case "runc", "crun", "io.containerd.runc.v2", "io.containerd.runtime.v1.linux":
		return fmt.Errorf("sandbox_runtime.name %q is not a sandbox runtime", s.Name)
	}
	switch s.Kind {
	case SandboxKindGVisor:
		if s.DockerStorageSize != "" && !sandboxTmpfsSizeRE.MatchString(s.DockerStorageSize) {
			return fmt.Errorf("sandbox_runtime.docker_storage_size %q must be a size such as 6g or 4096m", s.DockerStorageSize)
		}
	case SandboxKindSysbox:
		if s.DockerStorageSize != "" {
			return fmt.Errorf("sandbox_runtime.docker_storage_size applies to kind %q only", SandboxKindGVisor)
		}
	default:
		return fmt.Errorf("sandbox_runtime.kind %q must be %q or %q", s.Kind, SandboxKindGVisor, SandboxKindSysbox)
	}
	return nil
}

// StorageSize returns the gVisor tmpfs size, defaulted.
func (s *V1SandboxRuntime) StorageSize() string {
	if s == nil || s.DockerStorageSize == "" {
		return DefaultSandboxDockerStorageSize
	}
	return s.DockerStorageSize
}

// LoadProfileSandboxRuntime returns the sandbox_runtime of the named profile
// from the global settings file only (no project settings, Hub overlay or
// SCION_* environment), so nothing a project or requester controls can set
// it. It returns nil when the profile has none. An invalid block is an error,
// and the caller refuses the start.
func LoadProfileSandboxRuntime(profile string) (*V1SandboxRuntime, error) {
	if profile == "" {
		return nil, nil
	}
	vs, _, err := LoadGlobalSettings()
	if err != nil || vs == nil {
		if GlobalSettingsMentions("sandbox_runtime") {
			return nil, fmt.Errorf("the global settings file sets sandbox_runtime but cannot be loaded: %v", err)
		}
		return nil, nil
	}
	p, ok := vs.Profiles[profile]
	if !ok || p.SandboxRuntime == nil {
		return nil, nil
	}
	if err := p.SandboxRuntime.Validate(); err != nil {
		return nil, fmt.Errorf("profiles.%s.%w", profile, err)
	}
	s := *p.SandboxRuntime
	return &s, nil
}
