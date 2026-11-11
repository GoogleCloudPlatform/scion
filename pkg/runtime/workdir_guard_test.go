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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// TestBuildCommonRunArgs_NoWorkspaceCwdFlag guards the boundary between this
// package and the substrate harness working directory (see
// cmd/sciontool/commands/substrate_serve.go's resolveSubstrateHarnessCwd,
// which sets exec.Cmd.Dir on the process substrate-serve's own supervisor
// spawns, entirely inside pkg/sciontool/substrate and
// cmd/sciontool/commands — never touching this package). Docker/Podman/
// Apple Container already get the correct cwd for free from the image's
// WORKDIR (which they honour, unlike Substrate's ateom), so
// buildCommonRunArgs's tmux invocation must keep relying on that and never
// grow a `-c <dir>`/cwd flag of its own. This pins the current output so a
// future change to either code path is caught if it ever starts threading a
// workspace directory through here.
func TestBuildCommonRunArgs_NoWorkspaceCwdFlag(t *testing.T) {
	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Task:         "hello",
	}
	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs() error = %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "tmux new-session -d -s scion -n agent") {
		t.Fatalf("buildCommonRunArgs() output changed shape, want the tmux new-session invocation unchanged; got: %s", joined)
	}
	if strings.Contains(joined, "new-session -d -s scion -n agent -c ") {
		t.Errorf("buildCommonRunArgs() unexpectedly added a `-c <dir>` flag to tmux new-session; Docker/Podman/Apple Container rely on the image WORKDIR, not a Go-side cwd flag: %s", joined)
	}
}

// TestKubernetesRuntime_BuildPod_NoWorkspaceCwdFlag is
// TestBuildCommonRunArgs_NoWorkspaceCwdFlag's Kubernetes counterpart:
// SCION_START_CMD (the tmux invocation buildPod sets as an env var) must
// stay exactly as it is today — Kubernetes, like Docker, already gets the
// correct cwd from the image's WORKDIR, so nothing here should start
// injecting a workspace directory into the tmux command.
func TestKubernetesRuntime_BuildPod_NoWorkspaceCwdFlag(t *testing.T) {
	clientset := k8sfake.NewClientset()
	scheme := k8sruntime.NewScheme()
	fc := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(fc, clientset)
	r := NewKubernetesRuntime(client)

	config := RunConfig{
		Name:    "test-agent",
		Image:   "test-image",
		Harness: &MockHarness{},
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod() error = %v", err)
	}

	var startCmd string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "SCION_START_CMD" {
			startCmd = e.Value
		}
	}
	if !strings.Contains(startCmd, "tmux new-session -d -s scion -n agent") {
		t.Fatalf("SCION_START_CMD changed shape, want the tmux new-session invocation unchanged; got: %s", startCmd)
	}
	if strings.Contains(startCmd, "new-session -d -s scion -n agent -c ") {
		t.Errorf("SCION_START_CMD unexpectedly contains a `-c <dir>` flag on tmux new-session; Kubernetes relies on the image WORKDIR, not a Go-side cwd flag: %s", startCmd)
	}
}
