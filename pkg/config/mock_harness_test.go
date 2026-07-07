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
	"context"
	"embed"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

type MockHarness struct {
	NameVal      string
	ConfigDirVal string
}

func (m *MockHarness) Name() string { return m.NameVal }
func (m *MockHarness) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	return api.HarnessAdvancedCapabilities{Harness: m.NameVal}
}
func (m *MockHarness) GetEnv(agentName string, agentHome string, unixUsername string) map[string]string {
	return nil
}
func (m *MockHarness) GetCommand(task string, resume bool, baseArgs []string) []string { return nil }
func (m *MockHarness) DefaultConfigDir() string                                        { return m.ConfigDirVal }
func (m *MockHarness) SkillsDir() string {
	if m.ConfigDirVal == "" {
		return ""
	}
	return m.ConfigDirVal + "/skills"
}
func (m *MockHarness) HasSystemPrompt(agentHome string) bool { return false }
func (m *MockHarness) Provision(ctx context.Context, agentName, agentDir, agentHome, agentWorkspace string) error {
	return nil
}
func (m *MockHarness) GetInterruptKey() string                { return "C-c" }
func (m *MockHarness) GetInterruptSequence() []string         { return nil }
func (m *MockHarness) GetHarnessEmbedsFS() (embed.FS, string) { return embed.FS{}, "" }
func (m *MockHarness) InjectAgentInstructions(agentHome string, content []byte) error {
	target := filepath.Join(agentHome, "agents.md")
	return os.WriteFile(target, content, 0644)
}
func (m *MockHarness) InjectSystemPrompt(agentHome string, content []byte) error {
	target := filepath.Join(agentHome, ".scion", "system_prompt.md")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	return os.WriteFile(target, content, 0644)
}
func (m *MockHarness) GetTelemetryEnv() map[string]string { return nil }
func (m *MockHarness) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error) {
	return &api.ResolvedAuth{Method: "mock"}, nil
}

func GetMockHarnesses() []api.Harness {
	return []api.Harness{
		&MockHarness{NameVal: "gemini", ConfigDirVal: ".gemini"},
		&MockHarness{NameVal: "claude", ConfigDirVal: ".claude"},
	}
}
