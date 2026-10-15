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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// U10 (agent runtime host): an empty-namespace entry takes its namespace
// from defaultKubernetesNamespace(), here POD_NAMESPACE, and matches a
// runtime in that namespace. Not parallel: it sets environment variables.
func TestMatchKubernetesEntry_U10_PodNamespaceDefault(t *testing.T) {
	t.Setenv("SCION_K8S_NAMESPACE", "")
	t.Setenv("POD_NAMESPACE", "agents-ns")
	defaultNS := defaultKubernetesNamespace()
	if defaultNS != "agents-ns" {
		t.Fatalf("defaultKubernetesNamespace() = %q, want agents-ns", defaultNS)
	}
	got, cause := config.MatchKubernetesEntry(config.V1RuntimeConfig{Type: "kubernetes"}, "", "agents-ns", true,
		config.KubeConnInCluster, config.RuntimeBuildRecord{Present: true}, defaultNS, nil)
	if got != config.Match {
		t.Fatalf("MatchKubernetesEntry = %v (cause %q), want match", got, cause)
	}
}
