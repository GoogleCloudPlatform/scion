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

package opsettings

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

func TestValidate_TelemetryTrajectoryAllowProjectOptOut(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{
			name: "allow_project_opt_out false",
			doc:  `{"trajectory": {"allow_project_opt_out": false}}`,
		},
		{
			name: "allow_project_opt_out true",
			doc:  `{"trajectory": {"allow_project_opt_out": true}}`,
		},
		{
			name: "empty trajectory object",
			doc:  `{"trajectory": {}}`,
		},
		{
			name:    "non-boolean allow_project_opt_out rejected",
			doc:     `{"trajectory": {"allow_project_opt_out": "false"}}`,
			wantErr: "boolean",
		},
		{
			name:    "unknown nested trajectory property rejected",
			doc:     `{"trajectory": {"unknown_key": true}}`,
			wantErr: "additional",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := Validate("telemetry", json.RawMessage(tc.doc))
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("Validate(%s) = %v, want no errors", tc.doc, errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("Validate(%s) succeeded, want error containing %q", tc.doc, tc.wantErr)
			}
			var combined strings.Builder
			for _, e := range errs {
				combined.WriteString(e.Path)
				combined.WriteString(":")
				combined.WriteString(e.Message)
				combined.WriteString("\n")
			}
			if !strings.Contains(strings.ToLower(combined.String()), strings.ToLower(tc.wantErr)) {
				t.Errorf("Validate(%s) errors = %q, want substring %q", tc.doc, combined.String(), tc.wantErr)
			}
		})
	}
}

func TestTelemetryTrajectory_DBOnlyKey(t *testing.T) {
	if !IsDBOnlyKey("telemetry.trajectory.allow_project_opt_out") {
		t.Fatal("IsDBOnlyKey(telemetry.trajectory.allow_project_opt_out) = false, want true")
	}
	if !IsDBOnlyKey("telemetry.trajectory") {
		t.Fatal("IsDBOnlyKey(telemetry.trajectory) = false, want true")
	}

	got := EnvOverriddenLayer1Keys([]string{"telemetry.enabled", "telemetry.trajectory.allow_project_opt_out"})
	for _, k := range got {
		if k == "telemetry.trajectory.allow_project_opt_out" {
			t.Fatalf("EnvOverriddenLayer1Keys returned DB-only key %q", k)
		}
	}

	k := koanf.New(".")
	if err := k.Load(confmap.Provider(map[string]any{
		"telemetry.enabled":                          true,
		"telemetry.trajectory.allow_project_opt_out": false,
	}, "."), nil); err != nil {
		t.Fatalf("koanf load: %v", err)
	}
	raw, err := ExtractSectionFromKoanf(k, "telemetry")
	if err != nil {
		t.Fatalf("ExtractSectionFromKoanf: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal extracted: %v", err)
	}
	if _, hasTraj := m["trajectory"]; hasTraj {
		t.Fatalf("ExtractSectionFromKoanf must exclude DB-only telemetry.trajectory, got %s", raw)
	}
}
