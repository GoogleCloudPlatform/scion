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
	"strings"
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

// TestFlatKubernetesIdentity_MismatchRefusalNamesInstances: the start and
// restart refusals name a flat selection by its instance key and runtime
// target (a flat selection has no profile or runtime entry), and keep the
// profile and runtime entry wording for a legacy selection.
func TestFlatKubernetesIdentity_MismatchRefusalNamesInstances(t *testing.T) {
	early := dispatchProfileSelection{InstanceKey: "k8s-a", RuntimeTargetID: "flat-k8s-target"}
	other := dispatchProfileSelection{InstanceKey: "k8s-b", RuntimeTargetID: "flat-k8s-target-b"}
	late := func() dispatchProfileSelection { return other }
	assign := api.StartOptions{ResolvedKubernetesServiceAccountName: "instance-ksa"}
	block := api.StartOptions{KubernetesBlockIdentity: &api.KubernetesBlockIdentity{ServiceAccountName: "block-ksa"}}

	for _, tc := range []struct {
		name string
		sce  *startContextError
		want string
	}{
		{
			name: "assign, another instance",
			sce:  rejectKubernetesAssignRuntimeChange(assign, &early, "kubernetes", late),
			want: `GCP identity mode "assign" was resolved for Runtime Broker instance "k8s-a" and runtime target "flat-k8s-target", but this dispatch now resolves to Runtime Broker instance "k8s-b" and runtime target "flat-k8s-target-b"; `,
		},
		{
			name: "block, another instance",
			sce:  rejectKubernetesBlockRuntimeChange(block, &early, "kubernetes", late),
			want: `GCP identity mode "block" was resolved for Runtime Broker instance "k8s-a" and runtime target "flat-k8s-target", but this dispatch now resolves to Runtime Broker instance "k8s-b" and runtime target "flat-k8s-target-b"; `,
		},
		{
			name: "assign, now not Kubernetes",
			sce:  rejectKubernetesAssignRuntimeChange(assign, &early, "docker", late),
			want: `GCP identity mode "assign" was resolved for the Kubernetes runtime (Runtime Broker instance "k8s-a", runtime target "flat-k8s-target"), but this dispatch now resolves to runtime "docker"; `,
		},
		{
			name: "block, now not Kubernetes",
			sce:  rejectKubernetesBlockRuntimeChange(block, &early, "docker", late),
			want: `GCP identity mode "block" was resolved for the Kubernetes runtime (Runtime Broker instance "k8s-a", runtime target "flat-k8s-target"), but this dispatch now resolves to runtime "docker"; `,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.sce == nil || tc.sce.Status != http.StatusConflict {
				t.Fatalf("got %v, want a 409 refusal", tc.sce)
			}
			if !strings.HasPrefix(tc.sce.Message, tc.want) {
				t.Errorf("message = %q, want prefix %q", tc.sce.Message, tc.want)
			}
			if strings.Contains(tc.sce.Message, `profile ""`) || strings.Contains(tc.sce.Message, `runtime entry ""`) {
				t.Errorf("message names an empty profile or runtime entry: %q", tc.sce.Message)
			}
		})
	}

	t.Run("legacy wording unchanged", func(t *testing.T) {
		was := dispatchProfileSelection{ProfileName: "prod", RuntimeEntryName: "k8s"}
		now := dispatchProfileSelection{ProfileName: "team", RuntimeEntryName: "k8s-team"}
		sce := rejectKubernetesAssignRuntimeChange(assign, &was, "kubernetes", func() dispatchProfileSelection { return now })
		want := `GCP identity mode "assign" was resolved for profile "prod" and runtime entry "k8s", but this dispatch now resolves to profile "team" and runtime entry "k8s-team"; `
		if sce == nil || !strings.HasPrefix(sce.Message, want) {
			t.Fatalf("got %v, want message prefix %q", sce, want)
		}
		sce = rejectKubernetesBlockRuntimeChange(block, &was, "docker", func() dispatchProfileSelection { return now })
		want = `GCP identity mode "block" was resolved for the Kubernetes runtime (profile "prod", runtime entry "k8s"), but this dispatch now resolves to runtime "docker"; `
		if sce == nil || !strings.HasPrefix(sce.Message, want) {
			t.Fatalf("got %v, want message prefix %q", sce, want)
		}
	})
}
