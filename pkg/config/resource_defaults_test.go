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
