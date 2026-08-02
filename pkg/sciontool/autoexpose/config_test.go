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

package autoexpose

import (
	"testing"
	"time"
)

func TestConfigFromEnv_Defaults(t *testing.T) {
	// Clear all auto-expose env vars.
	for _, env := range []string{
		EnvAutoExposePorts, EnvAutoExposeMode, EnvAutoExposeList,
		EnvAutoExposeInterval, EnvAutoExposeMinPort,
	} {
		t.Setenv(env, "")
	}

	cfg := ConfigFromEnv()

	if cfg.Enabled {
		t.Error("expected Enabled=false by default")
	}
	if cfg.Interval != DefaultInterval {
		t.Errorf("Interval = %v, want %v", cfg.Interval, DefaultInterval)
	}
	if cfg.FilterMode != FilterModeAllowlist {
		t.Errorf("FilterMode = %q, want %q", cfg.FilterMode, FilterModeAllowlist)
	}
	if len(cfg.FilterPorts) != 0 {
		t.Errorf("FilterPorts = %v, want empty", cfg.FilterPorts)
	}
	if cfg.MaxPorts != DefaultMaxPorts {
		t.Errorf("MaxPorts = %d, want %d", cfg.MaxPorts, DefaultMaxPorts)
	}
	if cfg.MinPort != DefaultMinPort {
		t.Errorf("MinPort = %d, want %d", cfg.MinPort, DefaultMinPort)
	}
	if len(cfg.DeniedPorts) != 2 {
		t.Errorf("DeniedPorts = %v, want 2 entries (9810, 18380)", cfg.DeniedPorts)
	}
}

func TestConfigFromEnv_Enabled(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"True", true},
		{"TRUE", true},
		{"1", true},
		{"false", false},
		{"0", false},
		{"", false},
		{"yes", false},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv(EnvAutoExposePorts, tt.value)
			cfg := ConfigFromEnv()
			if cfg.Enabled != tt.want {
				t.Errorf("Enabled = %v for %q, want %v", cfg.Enabled, tt.value, tt.want)
			}
		})
	}
}

func TestConfigFromEnv_FilterMode(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{"allowlist", FilterModeAllowlist},
		{"denylist", FilterModeDenylist},
		{"Allowlist", FilterModeAllowlist},
		{"DENYLIST", FilterModeDenylist},
		{"invalid", FilterModeAllowlist}, // falls back to allowlist
		{"", FilterModeAllowlist},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv(EnvAutoExposeMode, tt.value)
			cfg := ConfigFromEnv()
			if cfg.FilterMode != tt.want {
				t.Errorf("FilterMode = %q for %q, want %q", cfg.FilterMode, tt.value, tt.want)
			}
		})
	}
}

func TestConfigFromEnv_PortList(t *testing.T) {
	tests := []struct {
		value string
		want  []int
	}{
		{"3000,5000,8000", []int{3000, 5000, 8000}},
		{"3000", []int{3000}},
		{"3000, 5000, 8000", []int{3000, 5000, 8000}},
		{"", nil},
		{"abc,3000,xyz", []int{3000}},
		{",,3000,,", []int{3000}},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv(EnvAutoExposeList, tt.value)
			cfg := ConfigFromEnv()
			if len(cfg.FilterPorts) != len(tt.want) {
				t.Fatalf("FilterPorts = %v, want %v", cfg.FilterPorts, tt.want)
			}
			for i := range tt.want {
				if cfg.FilterPorts[i] != tt.want[i] {
					t.Errorf("FilterPorts[%d] = %d, want %d", i, cfg.FilterPorts[i], tt.want[i])
				}
			}
		})
	}
}

func TestConfigFromEnv_Interval(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
	}{
		{"5s", 5 * time.Second},
		{"1m", time.Minute},
		{"500ms", time.Second}, // clamped to 1s floor
		{"invalid", DefaultInterval},
		{"", DefaultInterval},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv(EnvAutoExposeInterval, tt.value)
			cfg := ConfigFromEnv()
			if cfg.Interval != tt.want {
				t.Errorf("Interval = %v for %q, want %v", cfg.Interval, tt.value, tt.want)
			}
		})
	}
}

func TestConfigFromEnv_IntervalFloor(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
	}{
		{"100ms", time.Second},  // clamped to 1s
		{"1ms", time.Second},    // clamped to 1s
		{"999ms", time.Second},  // clamped to 1s
		{"1s", time.Second},     // exactly at floor
		{"2s", 2 * time.Second}, // above floor, unchanged
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv(EnvAutoExposeInterval, tt.value)
			cfg := ConfigFromEnv()
			if cfg.Interval != tt.want {
				t.Errorf("Interval = %v for %q, want %v", cfg.Interval, tt.value, tt.want)
			}
		})
	}
}

func TestConfigFromEnv_MinPort(t *testing.T) {
	tests := []struct {
		value string
		want  int
	}{
		{"2048", 2048},
		{"0", 0},
		{"invalid", DefaultMinPort},
		{"", DefaultMinPort},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv(EnvAutoExposeMinPort, tt.value)
			cfg := ConfigFromEnv()
			if cfg.MinPort != tt.want {
				t.Errorf("MinPort = %d for %q, want %d", cfg.MinPort, tt.value, tt.want)
			}
		})
	}
}

func TestConfigFromEnv_FullConfig(t *testing.T) {
	t.Setenv(EnvAutoExposePorts, "true")
	t.Setenv(EnvAutoExposeMode, "denylist")
	t.Setenv(EnvAutoExposeList, "22,80")
	t.Setenv(EnvAutoExposeInterval, "10s")
	t.Setenv(EnvAutoExposeMinPort, "2048")

	cfg := ConfigFromEnv()

	if !cfg.Enabled {
		t.Error("expected Enabled=true")
	}
	if cfg.FilterMode != FilterModeDenylist {
		t.Errorf("FilterMode = %q, want denylist", cfg.FilterMode)
	}
	if len(cfg.FilterPorts) != 2 || cfg.FilterPorts[0] != 22 || cfg.FilterPorts[1] != 80 {
		t.Errorf("FilterPorts = %v, want [22, 80]", cfg.FilterPorts)
	}
	if cfg.Interval != 10*time.Second {
		t.Errorf("Interval = %v, want 10s", cfg.Interval)
	}
	if cfg.MinPort != 2048 {
		t.Errorf("MinPort = %d, want 2048", cfg.MinPort)
	}
}
