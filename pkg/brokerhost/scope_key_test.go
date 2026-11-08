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

package brokerhost

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
)

func crScope(number, region string) brokeridentity.ExecutionScope {
	return brokeridentity.ExecutionScope{Type: brokeridentity.TargetTypeCloudRun,
		CloudRun: &brokeridentity.CloudRunScope{ProjectNumber: number, Region: region, ProjectID: "example-project"}}
}

// TestScopeKey_CloudRunDistinctPerProjectRegion: two cloudrun instances
// share a scope key only when both the project number and the region
// match, so one unresolved instance never refuses instances of an
// unrelated project or region.
func TestScopeKey_CloudRunDistinctPerProjectRegion(t *testing.T) {
	base := ScopeKey(crScope("111111111111", "us-central1"))
	if base != "cloudrun:111111111111/us-central1" {
		t.Fatalf("ScopeKey = %q", base)
	}
	if got := ScopeKey(crScope("222222222222", "us-central1")); got == base {
		t.Fatalf("two projects share the key %q", got)
	}
	if got := ScopeKey(crScope("111111111111", "europe-west4")); got == base {
		t.Fatalf("two regions share the key %q", got)
	}
	same := crScope("111111111111", "us-central1")
	same.CloudRun.ProjectID = "an-alias"
	if got := ScopeKey(same); got != base {
		t.Fatalf("the informational project ID changed the key: %q vs %q", got, base)
	}
}

// TestScopeKey_DelegatesToScopeIdentity: for every target type the key is
// brokeridentity.ScopeIdentity, and the "<type>:" fallback applies only to
// a scope ScopeIdentity cannot identify.
func TestScopeKey_DelegatesToScopeIdentity(t *testing.T) {
	identified := map[string]brokeridentity.ExecutionScope{
		"docker": {Type: brokeridentity.TargetTypeDocker, Docker: &brokeridentity.DockerScope{DaemonID: "D1", Endpoint: "unix:///var/run/docker.sock"}},
		"kubernetes": {Type: brokeridentity.TargetTypeKubernetes,
			Kubernetes: &brokeridentity.KubernetesScope{ClusterUID: "uid-1", Namespace: "agents", APIServer: "https://10.0.0.1:443"}},
		"cloudrun": crScope("111111111111", "us-central1"),
	}
	for name, s := range identified {
		t.Run(name, func(t *testing.T) {
			want, err := brokeridentity.ScopeIdentity(s)
			if err != nil {
				t.Fatalf("ScopeIdentity: %v", err)
			}
			if got := ScopeKey(s); got != want {
				t.Fatalf("ScopeKey = %q, ScopeIdentity = %q", got, want)
			}
		})
	}
	unidentified := map[string]brokeridentity.ExecutionScope{
		"docker":     {Type: brokeridentity.TargetTypeDocker, Docker: &brokeridentity.DockerScope{}},
		"kubernetes": {Type: brokeridentity.TargetTypeKubernetes, Kubernetes: &brokeridentity.KubernetesScope{ClusterUID: "uid-1"}},
		"cloudrun":   crScope("", "us-central1"),
		"unknown":    {Type: "podman"},
	}
	for name, s := range unidentified {
		t.Run("fallback "+name, func(t *testing.T) {
			if _, err := brokeridentity.ScopeIdentity(s); err == nil {
				t.Fatal("expected ScopeIdentity to fail")
			}
			if got := ScopeKey(s); got != s.Type+":" {
				t.Fatalf("ScopeKey = %q, want %q", got, s.Type+":")
			}
		})
	}
}
