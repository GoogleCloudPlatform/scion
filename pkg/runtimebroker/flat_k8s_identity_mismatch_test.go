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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestFlatKubernetesIdentity_EarlyLateInstanceMismatchRefused: the start and
// restart consistency checks compare the flat instance selection the GCP
// identity was resolved for (early) with the late selection. A late
// selection of ANOTHER flat instance is refused with 409 for both assign and
// block; the same instance passes. On a running instance the runtime and
// selection are fixed, so a differing late selection is unreachable live:
// this is controlled unit evidence on the production checks and the
// production selection resolver, with no production mutation hook
// (ptone/scion#3274 amendment, start/restart consistency).
func TestFlatKubernetesIdentity_EarlyLateInstanceMismatchRefused(t *testing.T) {
	a := newFlatKubernetesStartServer(t, config.V1RuntimeTargetConfig{})
	b := newFlatKubernetesStartServer(t, config.V1RuntimeTargetConfig{})
	b.config.FlatInstance.Identity.InstanceKey = "k8s-b"
	b.config.FlatInstance.Instance.Key = "k8s-b"
	b.config.FlatInstance.Identity.RuntimeTarget.ID = "flat-k8s-target-b"

	early := a.resolveDispatchProfileSelection(api.StartOptions{})
	same := func() dispatchProfileSelection { return a.resolveDispatchProfileSelection(api.StartOptions{}) }
	other := func() dispatchProfileSelection { return b.resolveDispatchProfileSelection(api.StartOptions{}) }
	if early != wantFlatSelection() {
		t.Fatalf("early selection = %+v, want %+v", early, wantFlatSelection())
	}
	if got := other(); got == early || got.InstanceKey != "k8s-b" {
		t.Fatalf("the other instance's selection = %+v, want instance k8s-b", got)
	}

	assign := api.StartOptions{ResolvedKubernetesServiceAccountName: "instance-ksa"}
	block := api.StartOptions{KubernetesBlockIdentity: &api.KubernetesBlockIdentity{ServiceAccountName: "block-ksa"}}
	checks := map[string]func(late func() dispatchProfileSelection) *startContextError{
		"assign": func(late func() dispatchProfileSelection) *startContextError {
			return rejectKubernetesAssignRuntimeChange(assign, &early, "kubernetes", late)
		},
		"block": func(late func() dispatchProfileSelection) *startContextError {
			return rejectKubernetesBlockRuntimeChange(block, &early, "kubernetes", late)
		},
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			if sce := check(same); sce != nil {
				t.Errorf("the same instance was refused: %v", sce.Message)
			}
			sce := check(other)
			if sce == nil || sce.Status != http.StatusConflict {
				t.Fatalf("another instance's late selection: got %v, want a 409 refusal", sce)
			}
		})
	}
}
