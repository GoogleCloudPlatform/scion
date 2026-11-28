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

import "github.com/GoogleCloudPlatform/scion/pkg/config"

// sandboxDockerDir is where an inner Docker engine keeps its state.
const sandboxDockerDir = "/var/lib/docker"

// sandboxIsGVisor reports whether the run uses a gVisor sandbox runtime.
func sandboxIsGVisor(cfg RunConfig) bool {
	return cfg.Sandbox != nil && cfg.Sandbox.Kind == config.SandboxKindGVisor
}

// appendSandboxRuntimeArgs adds the operator-declared sandbox runtime and the
// flags its kind needs for an inner container engine. The flags are added
// only together with --runtime and only for the matching kind: under the
// default runtime they would give the agent host-level privileges.
//
// gVisor: every capability and an unconfined engine-level seccomp profile
// (both confined to gVisor's user-space kernel, which installs its own
// seccomp filter on the host), plus an executable tmpfs for /var/lib/docker
// (Docker's default --tmpfs is noexec, which fails every inner container).
// Sysbox needs nothing beyond --runtime.
func appendSandboxRuntimeArgs(args []string, cfg RunConfig) []string {
	s := cfg.Sandbox
	if s == nil || s.Name == "" {
		return args
	}
	args = append(args, "--runtime", s.Name)
	if s.Kind == config.SandboxKindGVisor {
		args = append(args,
			"--cap-add", "ALL",
			"--security-opt", "seccomp=unconfined",
			"--tmpfs", sandboxDockerDir+":rw,exec,size="+s.StorageSize(),
		)
	}
	return args
}
