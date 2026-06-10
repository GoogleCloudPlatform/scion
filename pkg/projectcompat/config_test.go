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

import "testing"

func TestCanonicalConfigKey(t *testing.T) {
	tests := []struct {
		key       string
		canonical string
		legacy    bool
	}{
		{ConfigProjectIDKey, ConfigProjectIDKey, false},
		{ConfigGroveIDKey, ConfigProjectIDKey, true},
		{ConfigHubProjectIDKey, ConfigHubProjectIDKey, false},
		{ConfigHubProjectIDJSON, ConfigHubProjectIDKey, false},
		{ConfigHubGroveIDKey, ConfigHubProjectIDKey, true},
		{ConfigHubGroveIDJSON, ConfigHubProjectIDKey, true},
		{"hub.endpoint", "hub.endpoint", false},
	}

	for _, tt := range tests {
		canonical, legacy := CanonicalConfigKey(tt.key)
		if canonical != tt.canonical || legacy != tt.legacy {
			t.Fatalf("CanonicalConfigKey(%q) = (%q, %v), want (%q, %v)", tt.key, canonical, legacy, tt.canonical, tt.legacy)
		}
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
		{EnvHubGroveID, true, ConfigProjectIDKey, true},
		{EnvProjectID, false, ConfigProjectIDKey, true},
		{EnvGroveID, false, ConfigGroveIDKey, true},
		{EnvHubProjectID, false, ConfigHubProjectIDKey, true},
		{EnvHubGroveID, false, ConfigHubGroveIDKey, true},
		{"SCION_HUB_ENDPOINT", false, "", false},
	}

	for _, tt := range tests {
		got, ok := EnvProjectIDConfigKey(tt.name, tt.hubProjectAsTopLevel)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("EnvProjectIDConfigKey(%q, %v) = (%q, %v), want (%q, %v)", tt.name, tt.hubProjectAsTopLevel, got, ok, tt.want, tt.ok)
		}
	}
}
