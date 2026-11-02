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

package runtime

import (
	"context"
	"embed"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

type mockHarness struct {
	command []string
	env     map[string]string
}

func (h *mockHarness) Name() string { return "mock" }
func (h *mockHarness) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	return api.HarnessAdvancedCapabilities{}
}
func (h *mockHarness) GetCommand(task string, resume bool, baseArgs []string) []string {
	return h.command
}
func (h *mockHarness) GetEnv(agentName, agentHome, unixUsername string) map[string]string {
	return h.env
}
func (h *mockHarness) DefaultConfigDir() string                                        { return "" }
func (h *mockHarness) SkillsDir() string                                               { return "" }
func (h *mockHarness) HasSystemPrompt(agentHome string) bool                           { return false }
func (h *mockHarness) Provision(ctx context.Context, name, dir, home, ws string) error { return nil }
func (h *mockHarness) GetInterruptKey() string                                         { return "C-c" }
func (h *mockHarness) GetInterruptSequence() []string                                  { return nil }
func (h *mockHarness) DeliverySpec() api.DeliverySpec                                  { return api.DeliverySpec{} }
func (h *mockHarness) ReadySpec() api.ReadySpec                                        { return api.ReadySpec{} }
func (h *mockHarness) GetHarnessEmbedsFS() (embed.FS, string)                          { return embed.FS{}, "" }
func (h *mockHarness) InjectAgentInstructions(agentHome string, content []byte) error  { return nil }
func (h *mockHarness) InjectSystemPrompt(agentHome string, content []byte) error       { return nil }
func (h *mockHarness) GetTelemetryEnv() map[string]string                              { return nil }
func (h *mockHarness) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error)      { return nil, nil }
