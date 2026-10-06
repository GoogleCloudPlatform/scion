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
	"maps"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestBuiltinDefaultResources(t *testing.T) {
	got := BuiltinDefaultResources()

	if got == nil {
		t.Fatal("BuiltinDefaultResources returned nil")
	}
	if got.Limits.CPU != "2" {
		t.Errorf("limits.cpu = %q, want \"2\"", got.Limits.CPU)
	}
	// Decision C: no hard memory limit by default. A hard --memory limit
	// OOM-kills the largest process in the cgroup, which is often the harness
	// rather than the build that grew.
	if got.Limits.Memory != "" {
		t.Errorf("limits.memory = %q, want empty (no hard memory cap by default)", got.Limits.Memory)
	}
	if got.Requests.CPU != "" || got.Requests.Memory != "" {
		t.Errorf("requests = %+v, want empty", got.Requests)
	}
}

// TestBuiltinDefaultResources_FreshAllocation guards against the spec being
// promoted to a package-level var. MergeResourceSpec returns its base pointer
// unchanged when the override is nil, so a shared value would be mutable by
// every caller that merges on top of it.
func TestBuiltinDefaultResources_FreshAllocation(t *testing.T) {
	a := BuiltinDefaultResources()
	b := BuiltinDefaultResources()

	if a == b {
		t.Fatal("BuiltinDefaultResources returned the same pointer twice; must allocate per call")
	}

	a.Limits.CPU = "64"
	if b.Limits.CPU != "2" {
		t.Errorf("mutating one result changed another: got %q, want \"2\"", b.Limits.CPU)
	}
	if BuiltinDefaultResources().Limits.CPU != "2" {
		t.Error("mutating a result changed subsequent calls")
	}
}

// TestBuiltinDefaultCPUMatchesK8sFallback pins the built-in CPU limit to the
// Kubernetes adapter's own fallback (k8s_runtime.go), so the two runtimes do not
// silently drift apart. If the K8s fallback changes, this test should be updated
// deliberately, together with a decision about whether divergence is intended.
func TestBuiltinDefaultCPUMatchesK8sFallback(t *testing.T) {
	const k8sFallbackCPULimit = "2"

	if got := BuiltinDefaultResources().Limits.CPU; got != k8sFallbackCPULimit {
		t.Errorf("built-in CPU limit %q diverges from the Kubernetes fallback %q", got, k8sFallbackCPULimit)
	}
}

func TestShouldEnforceResourceDefaults(t *testing.T) {
	enabled := true
	disabled := false

	tests := []struct {
		name string
		vs   *VersionedSettings
		want bool
	}{
		{"nil settings defaults to enabled", nil, true},
		{"nil runtime section defaults to enabled", &VersionedSettings{}, true},
		{
			"nil flag defaults to enabled",
			&VersionedSettings{Runtime: &V1RuntimeDefaultsConfig{}},
			true,
		},
		{
			"explicit true",
			&VersionedSettings{Runtime: &V1RuntimeDefaultsConfig{EnforceResourceDefaults: &enabled}},
			true,
		},
		{
			"explicit false is the kill switch",
			&VersionedSettings{Runtime: &V1RuntimeDefaultsConfig{EnforceResourceDefaults: &disabled}},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldEnforceResourceDefaults(tt.vs); got != tt.want {
				t.Errorf("ShouldEnforceResourceDefaults() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBuiltinDefaultsMergeBehaviour covers the merge the provisioner performs:
// the built-in spec is the base and any higher tier wins field-by-field.
func TestBuiltinDefaultsMergeBehaviour(t *testing.T) {
	tests := []struct {
		name       string
		existing   *api.ResourceSpec
		wantCPU    string
		wantMemory string
	}{
		{
			name:     "no existing spec gets the built-in CPU limit",
			existing: nil,
			wantCPU:  "2",
		},
		{
			name:     "empty existing spec gets the built-in CPU limit",
			existing: &api.ResourceSpec{},
			wantCPU:  "2",
		},
		{
			name:     "explicit CPU limit wins over the built-in",
			existing: &api.ResourceSpec{Limits: api.ResourceList{CPU: "8"}},
			wantCPU:  "8",
		},
		{
			name:       "memory-only config keeps its memory and still gains a CPU limit",
			existing:   &api.ResourceSpec{Limits: api.ResourceList{Memory: "4Gi"}},
			wantCPU:    "2",
			wantMemory: "4Gi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeResourceSpec(BuiltinDefaultResources(), tt.existing)
			if got.Limits.CPU != tt.wantCPU {
				t.Errorf("limits.cpu = %q, want %q", got.Limits.CPU, tt.wantCPU)
			}
			if got.Limits.Memory != tt.wantMemory {
				t.Errorf("limits.memory = %q, want %q", got.Limits.Memory, tt.wantMemory)
			}
		})
	}
}

// TestRuntimeDefaultsRoundTrip verifies the kill switch survives a YAML
// round-trip through VersionedSettings, and that an unset switch does not
// emit a `runtime:` block (which would trip the schema's additionalProperties
// checks in the migration validator).
func TestRuntimeDefaultsRoundTrip(t *testing.T) {
	yamlIn := []byte("schema_version: \"1\"\nruntime:\n  enforce_resource_defaults: false\n")

	var vs VersionedSettings
	if err := yaml.Unmarshal(yamlIn, &vs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if vs.Runtime == nil || vs.Runtime.EnforceResourceDefaults == nil {
		t.Fatal("runtime.enforce_resource_defaults did not unmarshal")
	}
	if *vs.Runtime.EnforceResourceDefaults {
		t.Error("expected enforce_resource_defaults=false")
	}
	if ShouldEnforceResourceDefaults(&vs) {
		t.Error("kill switch set to false but defaults still enforced")
	}

	// Re-marshalling must round-trip the flag.
	out, err := yaml.Marshal(&vs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "enforce_resource_defaults: false") {
		t.Errorf("flag lost on re-marshal:\n%s", out)
	}

	// An unset switch must not emit a runtime block at all.
	empty, err := yaml.Marshal(&VersionedSettings{SchemaVersion: "1"})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if strings.Contains(string(empty), "runtime:") {
		t.Errorf("empty settings emitted a runtime block:\n%s", empty)
	}
}

func TestApplyBuiltinDefaultResources(t *testing.T) {
	tests := []struct {
		name          string
		res           *api.ResourceSpec
		k8s           *api.K8sResources
		wantLimitCPU  string
		wantReqCPU    string
		wantK8sLimits map[string]string // nil: k8s returned unchanged
	}{
		{name: "nil spec", wantLimitCPU: "2"},
		{name: "empty spec", res: &api.ResourceSpec{}, wantLimitCPU: "2"},
		{name: "request below builtin", res: &api.ResourceSpec{Requests: api.ResourceList{CPU: "1"}}, wantLimitCPU: "2", wantReqCPU: "1"},
		{name: "request equal to builtin", res: &api.ResourceSpec{Requests: api.ResourceList{CPU: "2"}}, wantLimitCPU: "2", wantReqCPU: "2"},
		{name: "request equal to builtin in millicores", res: &api.ResourceSpec{Requests: api.ResourceList{CPU: "2000m"}}, wantLimitCPU: "2", wantReqCPU: "2000m"},
		{name: "request above builtin raises limit", res: &api.ResourceSpec{Requests: api.ResourceList{CPU: "4"}}, wantLimitCPU: "4", wantReqCPU: "4"},
		{name: "two-digit request above builtin", res: &api.ResourceSpec{Requests: api.ResourceList{CPU: "10"}}, wantLimitCPU: "10", wantReqCPU: "10"},
		{name: "millicore request above builtin copied verbatim", res: &api.ResourceSpec{Requests: api.ResourceList{CPU: "3500m"}}, wantLimitCPU: "3500m", wantReqCPU: "3500m"},
		{name: "fractional request just above builtin", res: &api.ResourceSpec{Requests: api.ResourceList{CPU: "2.5"}}, wantLimitCPU: "2.5", wantReqCPU: "2.5"},
		{name: "explicit limit never changed", res: &api.ResourceSpec{Requests: api.ResourceList{CPU: "4"}, Limits: api.ResourceList{CPU: "1"}}, wantLimitCPU: "1", wantReqCPU: "4"},
		{name: "unparseable request gives no raise", res: &api.ResourceSpec{Requests: api.ResourceList{CPU: "lots"}}, wantLimitCPU: "2", wantReqCPU: "lots"},
		{
			name:          "k8s request above builtin sets k8s limit only",
			k8s:           &api.K8sResources{Requests: map[string]string{"cpu": "4"}},
			wantLimitCPU:  "2",
			wantK8sLimits: map[string]string{"cpu": "4"},
		},
		{
			name:          "k8s request in millicores equal to builtin gives no raise",
			k8s:           &api.K8sResources{Requests: map[string]string{"cpu": "2000m"}},
			wantLimitCPU:  "2",
			wantK8sLimits: nil,
		},
		{
			name:          "k8s request keeps other k8s limits",
			k8s:           &api.K8sResources{Requests: map[string]string{"cpu": "6"}, Limits: map[string]string{"nvidia.com/gpu": "1"}},
			wantLimitCPU:  "2",
			wantK8sLimits: map[string]string{"cpu": "6", "nvidia.com/gpu": "1"},
		},
		{
			name:          "both requests each raise their own limit",
			res:           &api.ResourceSpec{Requests: api.ResourceList{CPU: "3"}},
			k8s:           &api.K8sResources{Requests: map[string]string{"cpu": "5"}},
			wantLimitCPU:  "3",
			wantReqCPU:    "3",
			wantK8sLimits: map[string]string{"cpu": "5"},
		},
		{
			name:         "k8s request not above raised generic limit",
			res:          &api.ResourceSpec{Requests: api.ResourceList{CPU: "4"}},
			k8s:          &api.K8sResources{Requests: map[string]string{"cpu": "3"}},
			wantLimitCPU: "4",
			wantReqCPU:   "4",
		},
		{
			name:         "explicit k8s limit never changed",
			k8s:          &api.K8sResources{Requests: map[string]string{"cpu": "4"}, Limits: map[string]string{"cpu": "3"}},
			wantLimitCPU: "2",
		},
		{
			name:         "explicit generic limit skips k8s raise too",
			res:          &api.ResourceSpec{Limits: api.ResourceList{CPU: "1"}},
			k8s:          &api.K8sResources{Requests: map[string]string{"cpu": "4"}},
			wantLimitCPU: "1",
		},
		{
			name:         "unparseable k8s request gives no raise",
			k8s:          &api.K8sResources{Requests: map[string]string{"cpu": "many"}},
			wantLimitCPU: "2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resBefore api.ResourceSpec
			if tt.res != nil {
				resBefore = *tt.res
			}
			k8sBefore := cloneK8s(tt.k8s)

			gotRes, gotK8s := ApplyBuiltinDefaultResources(tt.res, tt.k8s)

			if gotRes == nil {
				t.Fatal("returned nil resources")
			}
			if gotRes.Limits.CPU != tt.wantLimitCPU {
				t.Errorf("limits.cpu = %q, want %q", gotRes.Limits.CPU, tt.wantLimitCPU)
			}
			if gotRes.Requests.CPU != tt.wantReqCPU {
				t.Errorf("requests.cpu = %q, want %q", gotRes.Requests.CPU, tt.wantReqCPU)
			}
			if tt.wantK8sLimits == nil {
				if gotK8s != tt.k8s {
					t.Errorf("k8s resources replaced, want unchanged input; got %+v", gotK8s)
				}
			} else {
				if gotK8s == tt.k8s {
					t.Fatal("k8s resources returned as the input pointer, want a raised copy")
				}
				if !maps.Equal(gotK8s.Limits, tt.wantK8sLimits) {
					t.Errorf("k8s limits = %v, want %v", gotK8s.Limits, tt.wantK8sLimits)
				}
				if !maps.Equal(gotK8s.Requests, tt.k8s.Requests) {
					t.Errorf("k8s requests = %v, want %v", gotK8s.Requests, tt.k8s.Requests)
				}
			}

			// Inputs must never be mutated.
			if tt.res != nil && *tt.res != resBefore {
				t.Errorf("input spec mutated: got %+v, want %+v", *tt.res, resBefore)
			}
			if !equalK8s(tt.k8s, k8sBefore) {
				t.Errorf("input k8s resources mutated: got %+v, want %+v", tt.k8s, k8sBefore)
			}
		})
	}
}

// With limits.cpu already set, the inputs come back as the same pointers.
func TestApplyBuiltinDefaultResources_ExplicitLimitReturnsInputs(t *testing.T) {
	res := &api.ResourceSpec{Limits: api.ResourceList{CPU: "8"}}
	k8s := &api.K8sResources{Requests: map[string]string{"cpu": "16"}}
	gotRes, gotK8s := ApplyBuiltinDefaultResources(res, k8s)
	if gotRes != res || gotK8s != k8s {
		t.Errorf("want inputs returned unchanged, got %+v / %+v", gotRes, gotK8s)
	}
}

func cloneK8s(k *api.K8sResources) *api.K8sResources {
	if k == nil {
		return nil
	}
	return &api.K8sResources{Requests: maps.Clone(k.Requests), Limits: maps.Clone(k.Limits)}
}

func equalK8s(a, b *api.K8sResources) bool {
	if a == nil || b == nil {
		return a == b
	}
	return maps.Equal(a.Requests, b.Requests) && maps.Equal(a.Limits, b.Limits)
}
