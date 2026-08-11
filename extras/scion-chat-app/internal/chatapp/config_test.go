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

package chatapp

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDuration_UnmarshalYAML_HumanReadable(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{`"100ms"`, 100 * time.Millisecond},
		{`"5s"`, 5 * time.Second},
		{`"2m30s"`, 2*time.Minute + 30*time.Second},
		{`"1h"`, time.Hour},
		{`"500us"`, 500 * time.Microsecond},
		{`"0s"`, 0},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			var d Duration
			if err := yaml.Unmarshal([]byte(tc.input), &d); err != nil {
				t.Fatalf("UnmarshalYAML(%s) failed: %v", tc.input, err)
			}
			if d.Duration != tc.want {
				t.Errorf("UnmarshalYAML(%s) = %v, want %v", tc.input, d.Duration, tc.want)
			}
		})
	}
}

func TestDuration_UnmarshalYAML_Int64Nanoseconds(t *testing.T) {
	// YAML integer → nanoseconds fallback for backward compatibility.
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"100000000", 100 * time.Millisecond}, // 100ms in ns
		{"5000000000", 5 * time.Second},       // 5s in ns
		{"0", 0},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			var d Duration
			if err := yaml.Unmarshal([]byte(tc.input), &d); err != nil {
				t.Fatalf("UnmarshalYAML(%s) failed: %v", tc.input, err)
			}
			if d.Duration != tc.want {
				t.Errorf("UnmarshalYAML(%s) = %v, want %v", tc.input, d.Duration, tc.want)
			}
		})
	}
}

func TestDuration_UnmarshalYAML_InvalidString(t *testing.T) {
	var d Duration
	err := yaml.Unmarshal([]byte(`"not-a-duration"`), &d)
	if err == nil {
		t.Error("expected error for invalid duration string, got nil")
	}
}

func TestDuration_UnmarshalYAML_InStruct(t *testing.T) {
	// Verify Duration works embedded in a config struct, as it would in
	// a real YAML config file.
	type testConfig struct {
		Delay Duration `yaml:"delay"`
	}

	input := `delay: "250ms"`
	var cfg testConfig
	if err := yaml.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("UnmarshalYAML in struct failed: %v", err)
	}
	if cfg.Delay.Duration != 250*time.Millisecond {
		t.Errorf("got %v, want 250ms", cfg.Delay.Duration)
	}
}
