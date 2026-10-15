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
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// MT1: MergeNodeSelector per-key upper wins; MergeTolerations union, upper
// first, de-duplicated with "" as Equal, distinct values kept;
// runtime_class_name upper if set; nil/empty in and out.
func TestPlacementMerge_MT1(t *testing.T) {
	t.Run("node selector upper wins per key", func(t *testing.T) {
		got := MergeNodeSelector(
			map[string]string{"pool": "lower", "zone": "a"},
			map[string]string{"pool": "upper", "cloud.google.com/gke-nodepool": "agents"},
		)
		want := map[string]string{"pool": "upper", "zone": "a", "cloud.google.com/gke-nodepool": "agents"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("MergeNodeSelector = %v, want %v", got, want)
		}
	})
	t.Run("node selector nil and empty", func(t *testing.T) {
		if got := MergeNodeSelector(nil, nil); got != nil {
			t.Fatalf("MergeNodeSelector(nil, nil) = %v, want nil", got)
		}
		if got := MergeNodeSelector(map[string]string{}, map[string]string{}); got != nil {
			t.Fatalf("MergeNodeSelector({}, {}) = %v, want nil", got)
		}
		if got := MergeNodeSelector(nil, map[string]string{"a": "1"}); !reflect.DeepEqual(got, map[string]string{"a": "1"}) {
			t.Fatalf("MergeNodeSelector(nil, upper) = %v", got)
		}
		if got := MergeNodeSelector(map[string]string{"a": "1"}, nil); !reflect.DeepEqual(got, map[string]string{"a": "1"}) {
			t.Fatalf("MergeNodeSelector(lower, nil) = %v", got)
		}
	})
	t.Run("tolerations union upper first with dedupe", func(t *testing.T) {
		lower := []api.K8sToleration{
			{Key: "dedicated", Operator: "", Value: "agents", Effect: "NoSchedule"}, // duplicate of upper[0] ("" == Equal)
			{Key: "dedicated", Operator: "Equal", Value: "other", Effect: "NoSchedule"},
			{Key: "gpu", Operator: "Exists", Effect: "NoSchedule"},
			{Key: "gpu", Operator: "Exists", Effect: "NoSchedule"}, // duplicate within lower
		}
		upper := []api.K8sToleration{
			{Key: "dedicated", Operator: "Equal", Value: "agents", Effect: "NoSchedule"},
			{Key: "gpu", Operator: "Exists", Effect: "NoExecute"},
		}
		got := MergeTolerations(lower, upper)
		want := []api.K8sToleration{
			{Key: "dedicated", Operator: "Equal", Value: "agents", Effect: "NoSchedule"},
			{Key: "gpu", Operator: "Exists", Effect: "NoExecute"},
			{Key: "dedicated", Operator: "Equal", Value: "other", Effect: "NoSchedule"},
			{Key: "gpu", Operator: "Exists", Effect: "NoSchedule"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("MergeTolerations =\n%v\nwant\n%v", got, want)
		}
	})
	t.Run("tolerations nil and empty", func(t *testing.T) {
		if got := MergeTolerations(nil, nil); got != nil {
			t.Fatalf("MergeTolerations(nil, nil) = %v, want nil", got)
		}
		if got := MergeTolerations([]api.K8sToleration{}, []api.K8sToleration{}); got != nil {
			t.Fatalf("MergeTolerations([], []) = %v, want nil", got)
		}
		one := []api.K8sToleration{{Key: "a", Operator: "Exists"}}
		if got := MergeTolerations(nil, one); !reflect.DeepEqual(got, one) {
			t.Fatalf("MergeTolerations(nil, upper) = %v", got)
		}
		if got := MergeTolerations(one, nil); !reflect.DeepEqual(got, one) {
			t.Fatalf("MergeTolerations(lower, nil) = %v", got)
		}
	})
	t.Run("runtime class upper if set", func(t *testing.T) {
		cases := []struct{ lower, upper, want string }{
			{"", "", ""},
			{"lower", "", "lower"},
			{"", "upper", "upper"},
			{"lower", "upper", "upper"},
		}
		for _, tc := range cases {
			if got := mergeRuntimeClassName(tc.lower, tc.upper); got != tc.want {
				t.Errorf("mergeRuntimeClassName(%q, %q) = %q, want %q", tc.lower, tc.upper, got, tc.want)
			}
		}
	})
}

func m1Base() *api.KubernetesConfig {
	return &api.KubernetesConfig{
		Namespace:        "ns",
		RuntimeClassName: "gvisor",
		NodeSelector:     map[string]string{"pool": "agent", "extra": "x"},
		Tolerations:      []api.K8sToleration{{Key: "gpu", Operator: "Exists", Effect: "NoSchedule"}},
		Resources:        &api.K8sResources{Requests: map[string]string{"cpu": "1"}, Limits: map[string]string{"cpu": "2"}},
		SafeToEvict:      boolPtr(false),
	}
}

func m1Placement() *api.ResolvedPlacement {
	return &api.ResolvedPlacement{
		NodeSelector:     map[string]string{"pool": "default"},
		Tolerations:      []api.K8sToleration{{Key: "dedicated", Operator: "Equal", Value: "agents", Effect: "NoSchedule"}},
		RuntimeClassName: "gvisor",
		Sources:          map[string][]string{"node_selector": {"runtimes.k8s"}},
	}
}

// mutateKubernetesConfig changes every map, slice and pointer reachable
// from k.
func mutateKubernetesConfig(k *api.KubernetesConfig) {
	if k == nil {
		return
	}
	if k.NodeSelector != nil {
		k.NodeSelector["pool"] = "mutated"
		k.NodeSelector["new"] = "mutated"
	}
	for i := range k.Tolerations {
		k.Tolerations[i].Key = "mutated"
	}
	if k.Resources != nil {
		k.Resources.Requests["cpu"] = "mutated"
		k.Resources.Limits["cpu"] = "mutated"
	}
	if k.SafeToEvict != nil {
		*k.SafeToEvict = true
	}
}

// M1: ApplyPlacement and the merge helpers never mutate inputs; outputs do
// not alias input maps or slices.
func TestPlacementNoMutation_M1(t *testing.T) {
	t.Run("merge helpers", func(t *testing.T) {
		lower := map[string]string{"a": "1"}
		upper := map[string]string{"b": "2"}
		got := MergeNodeSelector(lower, upper)
		got["a"], got["b"] = "x", "y"
		if !reflect.DeepEqual(lower, map[string]string{"a": "1"}) || !reflect.DeepEqual(upper, map[string]string{"b": "2"}) {
			t.Fatalf("MergeNodeSelector output aliases an input: lower=%v upper=%v", lower, upper)
		}
		// Upper alone and lower alone must still be copied.
		onlyUpper := MergeNodeSelector(nil, upper)
		onlyUpper["b"] = "z"
		onlyLower := MergeNodeSelector(lower, nil)
		onlyLower["a"] = "z"
		if upper["b"] != "2" || lower["a"] != "1" {
			t.Fatalf("MergeNodeSelector single-input output aliases its input")
		}

		lt := []api.K8sToleration{{Key: "a", Operator: "Exists"}}
		ut := []api.K8sToleration{{Key: "b", Operator: "Exists"}}
		gt := MergeTolerations(lt, ut)
		for i := range gt {
			gt[i].Key = "x"
		}
		if lt[0].Key != "a" || ut[0].Key != "b" {
			t.Fatalf("MergeTolerations output aliases an input: lower=%v upper=%v", lt, ut)
		}
		gu := MergeTolerations(nil, ut)
		gu[0].Key = "y"
		gl := MergeTolerations(lt, nil)
		gl[0].Key = "y"
		if lt[0].Key != "a" || ut[0].Key != "b" {
			t.Fatalf("MergeTolerations single-input output aliases its input")
		}
	})

	for _, tc := range []struct {
		name string
		d    *api.ResolvedPlacement
	}{
		{"in effect", m1Placement()},
		{"not in effect", &api.ResolvedPlacement{}},
		{"nil placement", nil},
	} {
		t.Run("ApplyPlacement "+tc.name, func(t *testing.T) {
			base := m1Base()
			wantBase := m1Base()
			var wantD *api.ResolvedPlacement
			if tc.d != nil {
				c := *tc.d
				wantD = &c
				if tc.d.InEffect() {
					wantD = m1Placement()
				}
			}
			out, _, err := ApplyPlacement(base, tc.d)
			if err != nil {
				t.Fatalf("ApplyPlacement: %v", err)
			}
			if out == base {
				t.Fatal("ApplyPlacement returned its input pointer")
			}
			if !reflect.DeepEqual(base, wantBase) {
				t.Fatalf("ApplyPlacement mutated base: %+v", base)
			}
			if !reflect.DeepEqual(tc.d, wantD) {
				t.Fatalf("ApplyPlacement mutated placement: %+v", tc.d)
			}
			mutateKubernetesConfig(out)
			if !reflect.DeepEqual(base, wantBase) {
				t.Fatalf("ApplyPlacement output aliases base: %+v", base)
			}
			if !reflect.DeepEqual(tc.d, wantD) {
				t.Fatalf("ApplyPlacement output aliases placement: %+v", tc.d)
			}
		})
	}

	t.Run("ApplyPlacement nil base in effect", func(t *testing.T) {
		d := m1Placement()
		out, _, err := ApplyPlacement(nil, d)
		if err != nil {
			t.Fatalf("ApplyPlacement: %v", err)
		}
		mutateKubernetesConfig(out)
		if !reflect.DeepEqual(d, m1Placement()) {
			t.Fatalf("ApplyPlacement output aliases placement: %+v", d)
		}
	})
}

// V1: validators: label key/value syntax; toleration operator, effect,
// empty-key and Exists rules; DNS-1123 runtime class.
func TestPlacementValidators_V1(t *testing.T) {
	t.Run("node selector", func(t *testing.T) {
		valid := map[string]string{
			"pool":                          "agents",
			"cloud.google.com/gke-nodepool": "scion-agents",
			"example.com/empty":             "",
			"a_b.c-d":                       "A-1_b.2",
		}
		if err := ValidateNodeSelector(valid); err != nil {
			t.Fatalf("ValidateNodeSelector(valid) = %v", err)
		}
		if err := ValidateNodeSelector(nil); err != nil {
			t.Fatalf("ValidateNodeSelector(nil) = %v", err)
		}
		for _, tc := range []struct {
			name string
			sel  map[string]string
			want string
		}{
			{"empty key", map[string]string{"": "v"}, `key ""`},
			{"bad key char", map[string]string{"bad key": "v"}, `key "bad key"`},
			{"bad prefix", map[string]string{"Bad_Prefix/name": "v"}, `key "Bad_Prefix/name"`},
			{"key too long", map[string]string{strings.Repeat("k", 64): "v"}, "key"},
			{"key prefix too long", map[string]string{overLongPrefix() + "/pool": "v"}, "key"},
			{"bad value char", map[string]string{"pool": "a b"}, `value for key "pool"`},
			{"value too long", map[string]string{"pool": strings.Repeat("v", 64)}, `value for key "pool"`},
			{"value leading dash", map[string]string{"pool": "-agents"}, `value for key "pool"`},
		} {
			err := ValidateNodeSelector(tc.sel)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: ValidateNodeSelector = %v, want error containing %q", tc.name, err, tc.want)
			}
		}
	})

	t.Run("tolerations", func(t *testing.T) {
		valid := []api.K8sToleration{
			{Operator: "Exists"},
			{Operator: "Exists", Effect: "NoExecute"},
			{Key: "dedicated", Value: "agents"},
			{Key: "dedicated", Operator: "Equal", Value: "agents", Effect: "NoSchedule"},
			{Key: "dedicated", Operator: "Equal", Effect: "PreferNoSchedule"},
			{Key: "nvidia.com/gpu", Operator: "Exists", Effect: "NoSchedule"},
		}
		if err := ValidateTolerations(valid); err != nil {
			t.Fatalf("ValidateTolerations(valid) = %v", err)
		}
		if err := ValidateTolerations(nil); err != nil {
			t.Fatalf("ValidateTolerations(nil) = %v", err)
		}
		for _, tc := range []struct {
			name string
			tol  api.K8sToleration
			want string
		}{
			{"unknown operator", api.K8sToleration{Key: "k", Operator: "In"}, "operator"},
			{"lower-case operator", api.K8sToleration{Key: "k", Operator: "exists"}, "operator"},
			{"unknown effect", api.K8sToleration{Key: "k", Operator: "Exists", Effect: "NoRun"}, "effect"},
			{"empty key with Equal", api.K8sToleration{Operator: "Equal", Value: "v"}, "must be Exists when the key is empty"},
			{"empty key with empty operator", api.K8sToleration{}, "must be Exists when the key is empty"},
			{"Exists with value", api.K8sToleration{Key: "k", Operator: "Exists", Value: "v"}, "must be empty when the operator is Exists"},
			{"bad key", api.K8sToleration{Key: "bad key", Operator: "Exists"}, `key "bad key"`},
			{"bad Equal value", api.K8sToleration{Key: "k", Operator: "Equal", Value: "a b"}, `value "a b"`},
			{"key name too long", api.K8sToleration{Key: strings.Repeat("k", 64), Operator: "Exists"}, `].key "`},
			{"key prefix too long", api.K8sToleration{Key: overLongPrefix() + "/k", Operator: "Exists"}, `].key "`},
			{"Equal value too long", api.K8sToleration{Key: "k", Operator: "Equal", Value: strings.Repeat("v", 64)}, `].value "`},
		} {
			err := ValidateTolerations([]api.K8sToleration{{Key: "ok", Operator: "Exists"}, tc.tol})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "tolerations[1]") {
				t.Errorf("%s: ValidateTolerations = %v, want error for tolerations[1] containing %q", tc.name, err, tc.want)
			}
		}
	})

	t.Run("runtime class", func(t *testing.T) {
		for _, ok := range []string{"", "gvisor", "kata-qemu", "runsc.v1"} {
			if err := ValidateRuntimeClassName(ok); err != nil {
				t.Errorf("ValidateRuntimeClassName(%q) = %v", ok, err)
			}
		}
		for _, bad := range []string{"GVisor", "gvisor_x", "-gvisor", "gvisor-", "a b", strings.Repeat("a", 254)} {
			if err := ValidateRuntimeClassName(bad); err == nil {
				t.Errorf("ValidateRuntimeClassName(%q) = nil, want error", bad)
			}
		}
	})
}

// overLongPrefix returns a 254-character DNS subdomain, one over the
// 253-character limit for a label key prefix.
func overLongPrefix() string {
	p := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." +
		strings.Repeat("c", 63) + "." + strings.Repeat("d", 62)
	if len(p) != 254 {
		panic("overLongPrefix length")
	}
	return p
}
