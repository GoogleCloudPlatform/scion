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

func TestIsHubManagedAgent(t *testing.T) {
	t.Run("not set", func(t *testing.T) {
		t.Setenv("SCION_AGENT_ID", "")
		if IsHubManagedAgent() {
			t.Error("IsHubManagedAgent() = true, want false when SCION_AGENT_ID is empty")
		}
	})

	t.Run("set to non-empty value", func(t *testing.T) {
		t.Setenv("SCION_AGENT_ID", "agent-uuid-123")
		if !IsHubManagedAgent() {
			t.Error("IsHubManagedAgent() = false, want true when SCION_AGENT_ID is set")
		}
	})
}
