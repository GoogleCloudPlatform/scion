// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestSavedAgentInfoGetters(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), config.DotScion)
	agentName := "saved-agent"
	agentHome := config.GetAgentHomePath(projectDir, agentName)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	data, err := json.Marshal(api.AgentInfo{
		Profile:       "balanced",
		HarnessConfig: "claude",
		Phase:         "stopped",
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	tests := []struct {
		name string
		get  func(string, string) string
		want string
	}{
		{name: "profile", get: GetSavedProfile, want: "balanced"},
		{name: "harness config", get: GetSavedHarnessConfig, want: "claude"},
		{name: "phase", get: GetSavedPhase, want: "stopped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.get(agentName, projectDir); got != tt.want {
				t.Fatalf("getter() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSavedAgentInfoGettersReturnEmptyForUnreadableMetadata(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), config.DotScion)
	agentName := "saved-agent"

	getters := []func(string, string) string{
		GetSavedProfile,
		GetSavedHarnessConfig,
		GetSavedPhase,
	}
	for _, get := range getters {
		if got := get(agentName, projectDir); got != "" {
			t.Fatalf("getter() with missing metadata = %q, want empty", got)
		}
	}

	agentHome := config.GetAgentHomePath(projectDir, agentName)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), []byte("{"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	for _, get := range getters {
		if got := get(agentName, projectDir); got != "" {
			t.Fatalf("getter() with malformed metadata = %q, want empty", got)
		}
	}
}
