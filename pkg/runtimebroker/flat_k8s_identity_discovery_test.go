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

package runtimebroker

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// A flat Kubernetes instance's assign identity comes from its own mapping
// only (ptone/scion#3274 amendment r5): ServiceAccount annotation discovery,
// which a legacy broker uses for an unmapped GSA, is never consulted. The
// legacy discovery behaviour is covered by the TestKSADiscovery_* tests.

// flatDiscoveryProbe installs a discovery client that counts its uses and
// whose namespace holds one KSA annotated for the GSA.
func flatDiscoveryProbe(srv *Server) *int {
	calls := 0
	client := fake.NewClientset(annotatedKSA("agents", "annotated-ksa", flatTestGSA))
	srv.assignKSAClientset = func(agent.Manager) (kubernetes.Interface, error) {
		calls++
		return client, nil
	}
	return &calls
}

// TestFlatKubernetesIdentity_UnmappedIgnoresAnnotatedKSA: with no instance
// mapping for the GSA, the assign dispatch is refused with 400
// identity_not_mapped although the instance's namespace holds exactly one
// KSA annotated for it, and the discovery client is never used.
func TestFlatKubernetesIdentity_UnmappedIgnoresAnnotatedKSA(t *testing.T) {
	srv := newFlatKubernetesStartServer(t, config.V1RuntimeTargetConfig{})
	calls := flatDiscoveryProbe(srv)
	_, err := srv.buildStartContext(context.Background(), assignInputs("flat-assign-annotated", nil))
	var sce *startContextError
	if !errors.As(err, &sce) || sce.Status != http.StatusBadRequest || sce.Code != ErrCodeIdentityNotMapped {
		t.Fatalf("want 400 %s, got %v", ErrCodeIdentityNotMapped, err)
	}
	if *calls != 0 {
		t.Errorf("the discovery client was used %d time(s), want none", *calls)
	}
}

// TestFlatKubernetesIdentity_MappingWinsOverAnnotations: a mapped GSA runs
// as the instance's mapped KSA even when another KSA in the namespace is
// annotated for it, and the discovery client is never used.
func TestFlatKubernetesIdentity_MappingWinsOverAnnotations(t *testing.T) {
	srv := newFlatKubernetesStartServer(t, config.V1RuntimeTargetConfig{
		KubernetesServiceAccountMappings: map[string]string{flatTestGSA: "instance-ksa"}})
	calls := flatDiscoveryProbe(srv)
	sc, err := srv.buildStartContext(context.Background(), assignInputs("flat-assign-mapped", nil))
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	if got := sc.Opts.ResolvedKubernetesServiceAccountName; got != "instance-ksa" {
		t.Errorf("assign ServiceAccount = %q, want instance-ksa", got)
	}
	if *calls != 0 {
		t.Errorf("the discovery client was used %d time(s), want none", *calls)
	}
}
