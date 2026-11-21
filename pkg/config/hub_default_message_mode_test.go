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

import "testing"

func TestValidateDefaultMessageMode(t *testing.T) {
	for _, ok := range []string{"", "none", "lineage", "branch", "project"} {
		if err := ValidateDefaultMessageMode(ok); err != nil {
			t.Errorf("ValidateDefaultMessageMode(%q) = %v, want nil", ok, err)
		}
	}
	// hub mode is granted per agent, never as a hub-wide default.
	for _, bad := range []string{"hub", "Project", "bogus", " none"} {
		if err := ValidateDefaultMessageMode(bad); err == nil {
			t.Errorf("ValidateDefaultMessageMode(%q) = nil, want error", bad)
		}
	}
}

func TestDefaultMessageMode_V1RoundTrip(t *testing.T) {
	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{Hub: &V1ServerHubConfig{DefaultMessageMode: "lineage"}})
	if gc.Hub.DefaultMessageMode != "lineage" {
		t.Fatalf("Hub.DefaultMessageMode = %q, want lineage", gc.Hub.DefaultMessageMode)
	}
	v1 := ConvertGlobalToV1ServerConfig(gc)
	if v1.Hub == nil || v1.Hub.DefaultMessageMode != "lineage" {
		t.Fatalf("round trip lost server.hub.default_message_mode: %+v", v1.Hub)
	}
}
