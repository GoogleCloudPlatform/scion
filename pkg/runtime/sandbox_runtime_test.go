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
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func TestAppendSandboxRuntimeArgs(t *testing.T) {
	base := []string{"run", "-t"}
	tests := []struct {
		name    string
		sandbox *config.V1SandboxRuntime
		want    []string
	}{
		{"none", nil, []string{"run", "-t"}},
		{"empty name", &config.V1SandboxRuntime{Kind: config.SandboxKindGVisor}, []string{"run", "-t"}},
		{"gvisor default size", &config.V1SandboxRuntime{Name: "runsc-dind", Kind: config.SandboxKindGVisor},
			[]string{"run", "-t", "--runtime", "runsc-dind", "--cap-add", "ALL", "--security-opt", "seccomp=unconfined", "--tmpfs", "/var/lib/docker:rw,exec,size=6g"}},
		{"gvisor size", &config.V1SandboxRuntime{Name: "runsc", Kind: config.SandboxKindGVisor, DockerStorageSize: "2g"},
			[]string{"run", "-t", "--runtime", "runsc", "--cap-add", "ALL", "--security-opt", "seccomp=unconfined", "--tmpfs", "/var/lib/docker:rw,exec,size=2g"}},
		{"sysbox runtime only", &config.V1SandboxRuntime{Name: "sysbox-runc", Kind: config.SandboxKindSysbox},
			[]string{"run", "-t", "--runtime", "sysbox-runc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendSandboxRuntimeArgs(append([]string(nil), base...), RunConfig{Sandbox: tt.sandbox})
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("appendSandboxRuntimeArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildCommonRunArgs_NetRawDropKeptExceptUnderGVisor(t *testing.T) {
	hasNetRawDrop := func(args []string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--cap-drop" && args[i+1] == "NET_RAW" {
				return true
			}
		}
		return false
	}
	cfg := RunConfig{Name: "a", Image: "img", RuntimeName: "docker", UnixUsername: "scion", Harness: &harness.Generic{}}
	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !hasNetRawDrop(args) {
		t.Fatalf("default runtime: want --cap-drop NET_RAW in %q", args)
	}
	cfg.Sandbox = &config.V1SandboxRuntime{Name: "runsc", Kind: config.SandboxKindGVisor}
	if args, err = buildCommonRunArgs(cfg); err != nil {
		t.Fatal(err)
	}
	if hasNetRawDrop(args) {
		t.Fatalf("gVisor sandbox: want no --cap-drop NET_RAW in %q", args)
	}
	cfg.Sandbox = &config.V1SandboxRuntime{Name: "sysbox-runc", Kind: config.SandboxKindSysbox}
	if args, err = buildCommonRunArgs(cfg); err != nil {
		t.Fatal(err)
	}
	if !hasNetRawDrop(args) {
		t.Fatalf("sysbox sandbox: want --cap-drop NET_RAW in %q", args)
	}
}
