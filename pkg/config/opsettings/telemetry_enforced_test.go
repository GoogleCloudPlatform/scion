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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

func TestTelemetrySettings_Resolve(t *testing.T) {
	on := true
	off := false

	cases := []struct {
		name    string
		in      TelemetrySettings
		wantErr string
	}{
		{
			name: "default empty",
			in:   TelemetrySettings{},
		},
		{
			name: "disabled and not enforced",
			in: TelemetrySettings{
				V1TelemetryConfig: config.V1TelemetryConfig{Enabled: &off},
				Enforced:          false,
			},
		},
		{
			name: "enabled and not enforced",
			in: TelemetrySettings{
				V1TelemetryConfig: config.V1TelemetryConfig{Enabled: &on},
				Enforced:          false,
			},
		},
		{
			name: "enabled and enforced",
			in: TelemetrySettings{
				V1TelemetryConfig: config.V1TelemetryConfig{Enabled: &on},
				Enforced:          true,
			},
		},
		{
			name: "enforced with enabled false fails",
			in: TelemetrySettings{
				V1TelemetryConfig: config.V1TelemetryConfig{Enabled: &off},
				Enforced:          true,
			},
			wantErr: "telemetry.enforced requires telemetry.enabled=true",
		},
		{
			name: "enforced with enabled nil fails",
			in: TelemetrySettings{
				Enforced: true,
			},
			wantErr: "telemetry.enforced requires telemetry.enabled=true",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.in.Resolve()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Resolve() error = nil, want %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Resolve() error = %q, want substring %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() unexpected error: %v", err)
			}
			if got.Enforced != tc.in.Enforced {
				t.Errorf("Resolve().Enforced = %v, want %v", got.Enforced, tc.in.Enforced)
			}
		})
	}
}

func TestValidate_TelemetryEnforced(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		wantErr  string
		wantPath string
	}{
		{
			name: "enabled true and enforced true",
			doc:  `{"enabled": true, "enforced": true}`,
		},
		{
			name: "enabled false and enforced false",
			doc:  `{"enabled": false, "enforced": false}`,
		},
		{
			name:     "enforced true with enabled false",
			doc:      `{"enabled": false, "enforced": true}`,
			wantErr:  "telemetry.enforced requires telemetry.enabled=true",
			wantPath: "enforced",
		},
		{
			name:     "enforced true with enabled omitted",
			doc:      `{"enforced": true}`,
			wantErr:  "telemetry.enforced requires telemetry.enabled=true",
			wantPath: "enforced",
		},
		{
			name:    "enforced wrong type",
			doc:     `{"enabled": true, "enforced": "yes"}`,
			wantErr: "boolean",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := Validate("telemetry", json.RawMessage(tc.doc))
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("Validate() unexpected errors: %+v", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("Validate() returned no errors, want %q", tc.wantErr)
			}
			var found bool
			for _, e := range errs {
				if strings.Contains(e.Message, tc.wantErr) && (tc.wantPath == "" || e.Path == tc.wantPath) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("Validate() errors = %+v, want path=%q containing %q", errs, tc.wantPath, tc.wantErr)
			}
			if tc.name == "enforced wrong type" && len(errs) != 1 {
				t.Fatalf("Validate() returned %d errors for wrong-type enforced, want 1: %+v", len(errs), errs)
			}
		})
	}
}

func TestExtractSectionFromKoanf_TelemetryStripsDBOnlyEnforced(t *testing.T) {
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(map[string]interface{}{
		"telemetry.enabled":        true,
		"telemetry.enforced":       true,
		"telemetry.cloud.endpoint": "otel.example.com:4317",
	}, "."), nil); err != nil {
		t.Fatalf("load koanf: %v", err)
	}

	raw, err := ExtractSectionFromKoanf(k, "telemetry")
	if err != nil {
		t.Fatalf("ExtractSectionFromKoanf: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal extracted doc: %v", err)
	}
	if _, hasEnforced := m["enforced"]; hasEnforced {
		t.Fatalf("extracted bootstrap telemetry doc must not contain DB-only key 'enforced': %s", raw)
	}
	if enabled, ok := m["enabled"].(bool); !ok || !enabled {
		t.Fatalf("extracted doc lost enabled=true: %s", raw)
	}
}

func TestEnvOverriddenLayer1Keys_ExcludesDBOnlyEnforced(t *testing.T) {
	got := EnvOverriddenLayer1Keys([]string{"telemetry.enabled", "telemetry.enforced"})
	if len(got) != 1 || got[0] != "telemetry.enabled" {
		t.Fatalf("EnvOverriddenLayer1Keys = %v, want [telemetry.enabled]", got)
	}
	if !IsDBOnlyKey("telemetry.enforced") {
		t.Fatal("IsDBOnlyKey(telemetry.enforced) = false, want true")
	}
	if IsDBOnlyKey("telemetry.enabled") {
		t.Fatal("IsDBOnlyKey(telemetry.enabled) = true, want false")
	}
}
