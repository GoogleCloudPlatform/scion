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

package harness

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestAdvancedCapabilitiesDefaults(t *testing.T) {
	tests := []struct {
		name                string
		harness             string
		expectMaxTurns      api.SupportLevel
		expectMaxModelCalls api.SupportLevel
		expectMaxDuration   api.SupportLevel
		expectAuthFile      api.SupportLevel
		expectVertexAI      api.SupportLevel
		expectSystemPrompt  api.SupportLevel
	}{
		{
			name:                "gemini",
			harness:             "gemini",
			expectMaxTurns:      api.SupportYes,
			expectMaxModelCalls: api.SupportYes,
			expectMaxDuration:   api.SupportYes,
			expectAuthFile:      api.SupportYes,
			expectVertexAI:      api.SupportYes,
			expectSystemPrompt:  api.SupportYes,
		},
		{
			name:                "claude",
			harness:             "claude",
			expectMaxTurns:      api.SupportYes,
			expectMaxModelCalls: api.SupportNo,
			expectMaxDuration:   api.SupportYes,
			expectAuthFile:      api.SupportYes,
			expectVertexAI:      api.SupportYes,
			expectSystemPrompt:  api.SupportYes,
		},
		{
			name:                "opencode",
			harness:             "opencode",
			expectMaxTurns:      api.SupportNo,
			expectMaxModelCalls: api.SupportNo,
			expectMaxDuration:   api.SupportYes,
			expectAuthFile:      api.SupportYes,
			expectVertexAI:      api.SupportNo,
			expectSystemPrompt:  api.SupportPartial,
		},
		{
			name:                "codex",
			harness:             "codex",
			expectMaxTurns:      api.SupportNo,
			expectMaxModelCalls: api.SupportNo,
			expectMaxDuration:   api.SupportYes,
			expectAuthFile:      api.SupportYes,
			expectVertexAI:      api.SupportNo,
			expectSystemPrompt:  api.SupportNo,
		},
		{
			name:                "generic",
			harness:             "missing-harness-name",
			expectMaxTurns:      api.SupportNo,
			expectMaxModelCalls: api.SupportNo,
			expectMaxDuration:   api.SupportYes,
			expectAuthFile:      api.SupportYes,
			expectVertexAI:      api.SupportYes,
			expectSystemPrompt:  api.SupportPartial,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caps := New(tc.harness).AdvancedCapabilities()
			if caps.Limits.MaxTurns.Support != tc.expectMaxTurns {
				t.Fatalf("max_turns = %q, want %q", caps.Limits.MaxTurns.Support, tc.expectMaxTurns)
			}
			if caps.Limits.MaxModelCalls.Support != tc.expectMaxModelCalls {
				t.Fatalf("max_model_calls = %q, want %q", caps.Limits.MaxModelCalls.Support, tc.expectMaxModelCalls)
			}
			if caps.Limits.MaxDuration.Support != tc.expectMaxDuration {
				t.Fatalf("max_duration = %q, want %q", caps.Limits.MaxDuration.Support, tc.expectMaxDuration)
			}
			if caps.Auth.AuthFile.Support != tc.expectAuthFile {
				t.Fatalf("auth_file = %q, want %q", caps.Auth.AuthFile.Support, tc.expectAuthFile)
			}
			if caps.Auth.VertexAI.Support != tc.expectVertexAI {
				t.Fatalf("vertex_ai = %q, want %q", caps.Auth.VertexAI.Support, tc.expectVertexAI)
			}
			if caps.Prompts.SystemPrompt.Support != tc.expectSystemPrompt {
				t.Fatalf("system_prompt = %q, want %q", caps.Prompts.SystemPrompt.Support, tc.expectSystemPrompt)
			}
		})
	}
}
