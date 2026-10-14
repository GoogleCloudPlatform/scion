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

//go:build !no_sqlite

package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// ptone/scion#5088: the flat refusal tests must not reach an ambient
// Kubernetes cluster, and the test Docker probe must not panic on another
// runtime.

// The test Docker probe returns an error for a runtime that is not Docker,
// and still probes a Docker runtime.
func TestDockerScopeProbe_NonDockerRuntimeIsAnError(t *testing.T) {
	probe := dockerScopeProbe(fakeDockerProbe("daemon-1", nil))
	inst := flatTestInstance()

	scope, err := probe(context.Background(), inst, runtime.NewDockerRuntime())
	if err != nil || scope.Docker == nil || scope.Docker.DaemonID != "daemon-1" {
		t.Fatalf("docker runtime: scope %+v, err %v; want daemon-1", scope, err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("docker scope probe panicked on a non-Docker runtime: %v", r)
		}
	}()
	_, err = probe(context.Background(), inst, &runtime.KubernetesRuntime{})
	if err == nil || !strings.Contains(err.Error(), "not *runtime.DockerRuntime") {
		t.Fatalf("non-Docker runtime: err = %v, want the runtime type error", err)
	}
}

// A non-Docker target is refused through its own configuration error even
// when the environment points at a reachable cluster: here an ambient
// KUBECONFIG naming a fake API server, standing in for the in-cluster
// config a pod provides. isolateKubernetesEnv (as the refusal test uses it)
// keeps the instance from building a runtime from that cluster.
func TestPrepareFlatInstance_NonDockerTarget_AmbientClusterIgnored(t *testing.T) {
	cluster := newFakeKubeCluster(t, "cluster-uid-1")
	t.Setenv("KUBECONFIG", writeTestKubeconfigFile(t, t.TempDir(), "ambient", cluster.URL))
	t.Setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "6443")
	isolateKubernetesEnv(t)

	s := newTestStore(t)
	srv := flatTestHub(t, s, true)
	inst := flatTestInstance()
	inst.RuntimeTarget.Type = "kubernetes"
	opts := hub.EmbeddedFlatRegistrationOptions{Endpoint: "http://localhost:9800"}
	_, err := prepareFlatInstance(context.Background(), srv, runtime.NewDockerRuntime(), inst, nil, t.TempDir(), opts, fakeDockerProbe("daemon-1", nil))
	if err == nil {
		t.Fatal("non-Docker target with no kubeconfig was activated")
	}
	if strings.Contains(err.Error(), "test docker probe") {
		t.Fatalf("the instance built a runtime from the ambient cluster: %v", err)
	}
	if !strings.Contains(err.Error(), "building its runtime") {
		t.Fatalf("refusal = %v, want the runtime build error", err)
	}
}
