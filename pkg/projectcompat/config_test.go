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

package projectcompat

import (
	"os"
	"testing"
)

func TestIsProjectIDConfigKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{ConfigProjectIDKey, true},
		{ConfigGroveIDKey, false},
		{"hub.endpoint", false},
	}
	for _, tt := range tests {
		if got := IsProjectIDConfigKey(tt.key); got != tt.want {
			t.Fatalf("IsProjectIDConfigKey(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

func TestIsHubProjectIDConfigKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{ConfigHubProjectIDKey, true},
		{ConfigHubProjectIDJSON, true},
		{ConfigHubGroveIDKey, false},
		{"hub.groveId", false},
		{"hub.endpoint", false},
	}
	for _, tt := range tests {
		if got := IsHubProjectIDConfigKey(tt.key); got != tt.want {
			t.Fatalf("IsHubProjectIDConfigKey(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

func TestProjectIDFromEnv(t *testing.T) {
	t.Setenv(EnvProjectID, "canonical")
	t.Setenv(EnvGroveID, "legacy")
	if got := ProjectIDFromEnv(os.Getenv); got != "canonical" {
		t.Fatalf("ProjectIDFromEnv() = %q, want canonical", got)
	}
	t.Setenv(EnvProjectID, "")
	if got := ProjectIDFromEnv(os.Getenv); got != "legacy" {
		t.Fatalf("ProjectIDFromEnv() fallback = %q, want legacy", got)
	}
}

func TestEnvProjectIDConfigKey(t *testing.T) {
	tests := []struct {
		name                 string
		hubProjectAsTopLevel bool
		want                 string
		ok                   bool
	}{
		{EnvProjectID, true, ConfigProjectIDKey, true},
		{EnvGroveID, true, ConfigProjectIDKey, true},
		{EnvHubProjectID, true, ConfigProjectIDKey, true},
		{EnvProjectID, false, ConfigProjectIDKey, true},
		{EnvGroveID, false, ConfigGroveIDKey, true},
		{EnvHubProjectID, false, ConfigHubProjectIDKey, true},
		// SCION_HUB_GROVE_ID: removed, no replacement case. Must stay
		// (false, "", false) — a caller falling through to a generic
		// mapping for a "false" result here would silently revive the
		// removed variable via koanf.go/settings_v1.go's generic mapper.
		{"SCION_HUB_GROVE_ID", false, "", false},
		{"SCION_HUB_ENDPOINT", false, "", false},
	}

	for _, tt := range tests {
		got, ok := EnvProjectIDConfigKey(tt.name, tt.hubProjectAsTopLevel)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("EnvProjectIDConfigKey(%q, %v) = (%q, %v), want (%q, %v)", tt.name, tt.hubProjectAsTopLevel, got, ok, tt.want, tt.ok)
		}
	}
}
