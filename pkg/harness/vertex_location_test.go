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

package harness

import "testing"

// captureVertexLocationWarnings replaces the conflict warning hook for the
// duration of the test and returns the recorded (location, region) pairs.
func captureVertexLocationWarnings(t *testing.T) *[][2]string {
	t.Helper()
	var got [][2]string
	orig := warnVertexLocationConflict
	warnVertexLocationConflict = func(location, region string) {
		got = append(got, [2]string{location, region})
	}
	t.Cleanup(func() { warnVertexLocationConflict = orig })
	return &got
}

var vertexLocationCases = []struct {
	name     string
	env      map[string]string
	want     string
	wantWarn bool
}{
	{"only region", map[string]string{"GOOGLE_CLOUD_REGION": "us-central1"}, "us-central1", false},
	{"only location", map[string]string{"GOOGLE_CLOUD_LOCATION": "us-east5"}, "us-east5", false},
	{"both equal", map[string]string{"GOOGLE_CLOUD_LOCATION": "us-east5", "GOOGLE_CLOUD_REGION": "us-east5"}, "us-east5", false},
	{"both differ", map[string]string{"GOOGLE_CLOUD_LOCATION": "us-east5", "GOOGLE_CLOUD_REGION": "us-central1"}, "us-east5", true},
	{"neither", map[string]string{}, "", false},
	{"cloud ml region last", map[string]string{"CLOUD_ML_REGION": "europe-west4"}, "europe-west4", false},
	{"region beats cloud ml region", map[string]string{"GOOGLE_CLOUD_REGION": "us-central1", "CLOUD_ML_REGION": "europe-west4"}, "us-central1", false},
	{"location beats cloud ml region", map[string]string{"GOOGLE_CLOUD_LOCATION": "us-east5", "CLOUD_ML_REGION": "europe-west4"}, "us-east5", false},
}

func TestGatherAuthVertexLocationPrecedence(t *testing.T) {
	for _, tc := range vertexLocationCases {
		t.Run(tc.name, func(t *testing.T) {
			warnings := captureVertexLocationWarnings(t)
			// Broker mode (localSources=false) reads only the env map.
			auth := GatherAuthWithEnv(tc.env, false, nil)
			if auth.GoogleCloudRegion != tc.want {
				t.Errorf("GoogleCloudRegion = %q, want %q", auth.GoogleCloudRegion, tc.want)
			}
			if tc.wantWarn {
				want := [2]string{tc.env["GOOGLE_CLOUD_LOCATION"], tc.env["GOOGLE_CLOUD_REGION"]}
				if len(*warnings) != 1 || (*warnings)[0] != want {
					t.Errorf("warnings = %v, want exactly one %v", *warnings, want)
				}
			} else if len(*warnings) != 0 {
				t.Errorf("warnings = %v, want none", *warnings)
			}
		})
	}
}

// TestVertexLocationConsistentInContainerEnv follows a conflicting pair from
// GatherAuthWithEnv through ContainerScriptHarness.ResolveAuth: every location
// variable the container sees carries the GOOGLE_CLOUD_LOCATION value.
func TestVertexLocationConsistentInContainerEnv(t *testing.T) {
	env := map[string]string{
		"GOOGLE_CLOUD_PROJECT":  "synthetic-project",
		"GOOGLE_CLOUD_LOCATION": "us-east5",
		"GOOGLE_CLOUD_REGION":   "us-central1",
	}
	for _, tc := range []struct {
		name    string
		harness func(t *testing.T) *ContainerScriptHarness
		keys    []string
	}{
		{"generic container script", func(t *testing.T) *ContainerScriptHarness {
			h, _ := newTestContainerScriptHarness(t)
			return h
		}, []string{"GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION"}},
		{"claude vertex", newClaudeHarness, []string{"GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings := captureVertexLocationWarnings(t)
			auth := GatherAuthWithEnv(env, false, nil)
			// Config-driven auth metadata would also forward the raw values.
			auth.EnvVars = map[string]string{
				"GOOGLE_CLOUD_LOCATION": env["GOOGLE_CLOUD_LOCATION"],
				"GOOGLE_CLOUD_REGION":   env["GOOGLE_CLOUD_REGION"],
			}
			resolved, err := tc.harness(t).ResolveAuth(auth)
			if err != nil {
				t.Fatalf("ResolveAuth: %v", err)
			}
			for _, k := range tc.keys {
				if got := resolved.EnvVars[k]; got != "us-east5" {
					t.Errorf("%s = %q, want %q (GOOGLE_CLOUD_LOCATION wins)", k, got, "us-east5")
				}
			}
			if len(*warnings) != 1 {
				t.Errorf("got %d warnings, want 1: %v", len(*warnings), *warnings)
			}
		})
	}
}
