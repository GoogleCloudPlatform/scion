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
	"strings"
	"testing"
)

func TestSandboxRuntimeValidate(t *testing.T) {
	tests := []struct {
		name    string
		in      V1SandboxRuntime
		wantErr string
	}{
		{"gvisor ok", V1SandboxRuntime{Name: "runsc-dind", Kind: "gvisor"}, ""},
		{"gvisor size ok", V1SandboxRuntime{Name: "runsc", Kind: "gvisor", DockerStorageSize: "4096m"}, ""},
		{"sysbox ok", V1SandboxRuntime{Name: "sysbox-runc", Kind: "sysbox"}, ""},
		{"empty name", V1SandboxRuntime{Kind: "gvisor"}, "sandbox_runtime.name"},
		{"name with space", V1SandboxRuntime{Name: "runsc dind", Kind: "gvisor"}, "sandbox_runtime.name"},
		{"runc refused", V1SandboxRuntime{Name: "runc", Kind: "gvisor"}, "not a sandbox runtime"},
		{"RUNC refused", V1SandboxRuntime{Name: "RUNC", Kind: "gvisor"}, "not a sandbox runtime"},
		{"containerd runc refused", V1SandboxRuntime{Name: "io.containerd.runc.v2", Kind: "gvisor"}, "not a sandbox runtime"},
		{"unknown kind", V1SandboxRuntime{Name: "kata", Kind: "kata"}, "sandbox_runtime.kind"},
		{"empty kind", V1SandboxRuntime{Name: "runsc"}, "sandbox_runtime.kind"},
		{"bad size", V1SandboxRuntime{Name: "runsc", Kind: "gvisor", DockerStorageSize: "6GB"}, "docker_storage_size"},
		{"zero size", V1SandboxRuntime{Name: "runsc", Kind: "gvisor", DockerStorageSize: "0g"}, "docker_storage_size"},
		{"size on sysbox", V1SandboxRuntime{Name: "sysbox-runc", Kind: "sysbox", DockerStorageSize: "6g"}, "applies to kind"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.in.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestSandboxRuntimeStorageSizeDefault(t *testing.T) {
	if got := (&V1SandboxRuntime{Name: "runsc", Kind: "gvisor"}).StorageSize(); got != DefaultSandboxDockerStorageSize {
		t.Fatalf("StorageSize() = %q, want %q", got, DefaultSandboxDockerStorageSize)
	}
	if got := (&V1SandboxRuntime{Name: "runsc", Kind: "gvisor", DockerStorageSize: "2g"}).StorageSize(); got != "2g" {
		t.Fatalf("StorageSize() = %q, want 2g", got)
	}
}

const sandboxRuntimeDoc = `schema_version: "1"
profiles:
  local:
    runtime: docker
  build:
    runtime: docker
    sandbox_runtime:
      name: runsc-dind
      kind: gvisor
      docker_storage_size: 4g
`

func TestLoadProfileSandboxRuntime(t *testing.T) {
	writeAgentPolicyGlobalSettings(t, sandboxRuntimeDoc)

	got, err := LoadProfileSandboxRuntime("build")
	if err != nil {
		t.Fatalf("LoadProfileSandboxRuntime(build) error = %v", err)
	}
	if got == nil || got.Name != "runsc-dind" || got.Kind != SandboxKindGVisor || got.DockerStorageSize != "4g" {
		t.Fatalf("LoadProfileSandboxRuntime(build) = %+v", got)
	}
	for _, p := range []string{"local", "missing", ""} {
		got, err := LoadProfileSandboxRuntime(p)
		if err != nil || got != nil {
			t.Fatalf("LoadProfileSandboxRuntime(%q) = %+v, %v; want nil, nil", p, got, err)
		}
	}
}

func TestLoadProfileSandboxRuntimeInvalidRefuses(t *testing.T) {
	writeAgentPolicyGlobalSettings(t, `schema_version: "1"
profiles:
  build:
    runtime: docker
    sandbox_runtime:
      name: runc
      kind: gvisor
`)
	got, err := LoadProfileSandboxRuntime("build")
	if err == nil || got != nil {
		t.Fatalf("LoadProfileSandboxRuntime(build) = %+v, %v; want refusal", got, err)
	}
	if !strings.Contains(err.Error(), "profiles.build.sandbox_runtime.name") {
		t.Fatalf("error %q does not name the setting", err)
	}
}

func TestLoadProfileSandboxRuntimeIgnoresProjectSettings(t *testing.T) {
	// Only the global file counts: with no global sandbox_runtime the
	// profile has none, whatever else is on disk.
	writeAgentPolicyGlobalSettings(t, `schema_version: "1"
profiles:
  build:
    runtime: docker
`)
	got, err := LoadProfileSandboxRuntime("build")
	if err != nil || got != nil {
		t.Fatalf("LoadProfileSandboxRuntime(build) = %+v, %v; want nil, nil", got, err)
	}
}
