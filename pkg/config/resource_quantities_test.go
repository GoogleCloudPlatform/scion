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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestValidateResourceSpec(t *testing.T) {
	const p = "profiles.gke.resources"
	tests := []struct {
		name     string
		spec     api.ResourceSpec
		wantErr  string // path that must have an error, "" for none
		wantWarn string // substring a warning must contain, "" for none
	}{
		{"empty", api.ResourceSpec{}, "", ""},
		{"typical", api.ResourceSpec{Requests: api.ResourceList{CPU: "7", Memory: "8Gi"}, Limits: api.ResourceList{CPU: "8", Memory: "16Gi"}, Disk: "20Gi"}, "", ""},
		{"millicores", api.ResourceSpec{Requests: api.ResourceList{CPU: "500m"}}, "", ""},
		{"unitless memory 16", api.ResourceSpec{Limits: api.ResourceList{Memory: "16"}}, p + ".limits.memory", ""},
		{"memory below 1Mi with unit", api.ResourceSpec{Requests: api.ResourceList{Memory: "512Ki"}}, p + ".requests.memory", ""},
		{"disk below 1Mi", api.ResourceSpec{Disk: "100"}, p + ".disk", ""},
		{"not a quantity", api.ResourceSpec{Limits: api.ResourceList{Memory: "16GB"}}, p + ".limits.memory", ""},
		{"cpu not a quantity", api.ResourceSpec{Requests: api.ResourceList{CPU: "two"}}, p + ".requests.cpu", ""},
		{"zero cpu", api.ResourceSpec{Limits: api.ResourceList{CPU: "0"}}, p + ".limits.cpu", ""},
		{"negative memory", api.ResourceSpec{Limits: api.ResourceList{Memory: "-1Gi"}}, p + ".limits.memory", ""},
		{"cpu request above limit", api.ResourceSpec{Requests: api.ResourceList{CPU: "8"}, Limits: api.ResourceList{CPU: "7"}}, p + ".requests.cpu", ""},
		{"memory request above limit", api.ResourceSpec{Requests: api.ResourceList{Memory: "2Gi"}, Limits: api.ResourceList{Memory: "1Gi"}}, p + ".requests.memory", ""},
		{"request above limit across units", api.ResourceSpec{Requests: api.ResourceList{Memory: "1G"}, Limits: api.ResourceList{Memory: "900Mi"}}, p + ".requests.memory", ""},
		{"unitless byte count warns", api.ResourceSpec{Limits: api.ResourceList{Memory: "2147483648"}}, "", "2Gi"},
		{"unitless disk warns", api.ResourceSpec{Disk: "10737418240"}, "", "10Gi"},
		{"exactly 1Mi warns low", api.ResourceSpec{Limits: api.ResourceList{Memory: "1Mi"}}, "", "below 64Mi"},
		{"low memory warns", api.ResourceSpec{Requests: api.ResourceList{Memory: "32Mi"}}, "", "below 64Mi"},
		{"64Mi no warning", api.ResourceSpec{Requests: api.ResourceList{Memory: "64Mi"}}, "", ""},
		{"low disk no low-memory warning", api.ResourceSpec{Disk: "2Mi"}, "", ""},
		{"exponent is a unit", api.ResourceSpec{Limits: api.ResourceList{Memory: "2e9"}}, "", ""},
		{"unitless cpu fine", api.ResourceSpec{Limits: api.ResourceList{CPU: "16"}}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := tt.spec
			errs, warnings := ValidateResourceSpec(p, &spec)
			if tt.wantErr == "" && len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if tt.wantErr != "" {
				if len(errs) != 1 || errs[0].Path != tt.wantErr {
					t.Fatalf("want one error at %s, got %v", tt.wantErr, errs)
				}
				if !strings.Contains(errs[0].Message, "such as") && !strings.Contains(errs[0].Message, "limit") {
					t.Errorf("error should give an example or name the limit: %s", errs[0].Message)
				}
			}
			if tt.wantWarn == "" && len(warnings) > 0 {
				t.Fatalf("unexpected warnings: %v", warnings)
			}
			if tt.wantWarn != "" {
				found := false
				for _, w := range warnings {
					if strings.Contains(w.Error(), tt.wantWarn) && strings.HasPrefix(w.Path, p+".") {
						found = true
					}
				}
				if !found {
					t.Errorf("want a warning containing %q, got %v", tt.wantWarn, warnings)
				}
			}
		})
	}
}

func TestValidateResourceSpec_UnitlessMessage(t *testing.T) {
	errs, _ := ValidateResourceSpec("profiles.gke.resources", &api.ResourceSpec{Limits: api.ResourceList{Memory: "16"}})
	if len(errs) != 1 {
		t.Fatalf("errs = %v", errs)
	}
	got := errs[0].Error()
	for _, want := range []string{"profiles.gke.resources.limits.memory", `"16"`, "16 bytes", "16Gi"} {
		if !strings.Contains(got, want) {
			t.Errorf("error %q should contain %q", got, want)
		}
	}
}

func TestValidateResourceQuantities_Paths(t *testing.T) {
	bad := &api.ResourceSpec{Limits: api.ResourceList{Memory: "16"}}
	errs, _ := ValidateResourceQuantities(bad, map[string]V1ProfileConfig{
		"b": {Resources: bad, HarnessOverrides: map[string]V1HarnessOverride{"claude": {Resources: bad}}},
		"a": {Resources: bad},
	})
	var paths []string
	for _, e := range errs {
		paths = append(paths, e.Path)
	}
	want := []string{
		"default_resources.limits.memory",
		"profiles.a.resources.limits.memory",
		"profiles.b.harness_overrides.claude.resources.limits.memory",
		"profiles.b.resources.limits.memory",
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

func TestSettingsWarnings_ResourceQuantities(t *testing.T) {
	data := []byte("schema_version: \"1\"\ndefault_resources:\n  limits:\n    memory: \"16\"\nprofiles:\n  p:\n    runtime: k\n    resources:\n      limits:\n        memory: \"2147483648\"\n")
	warnings := SettingsWarnings(data, "1")
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"default_resources.limits.memory", "profiles.p.resources.limits.memory"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings should name %s: %v", want, warnings)
		}
	}
}
