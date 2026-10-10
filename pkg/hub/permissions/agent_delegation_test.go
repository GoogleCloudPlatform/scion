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

package permissions

import "testing"

// TestAgentDelegableRegistry_KeysAreRegisteredPermissions: every policy key
// is a permission in Registry, and every boundary kind is valid.
func TestAgentDelegableRegistry_KeysAreRegisteredPermissions(t *testing.T) {
	known := make(map[string]bool, len(Registry))
	for _, p := range Registry {
		known[p.ID] = true
	}
	for id, kinds := range AgentDelegableRegistry {
		if !known[id] {
			t.Errorf("delegable permission %q is not in Registry", id)
		}
		if len(kinds) == 0 {
			t.Errorf("delegable permission %q lists no boundary kind", id)
		}
		for _, k := range kinds {
			if k != BoundaryKindProject && k != BoundaryKindHub {
				t.Errorf("delegable permission %q lists unknown boundary kind %q", id, k)
			}
		}
	}
}

// TestAgentDelegable_ExactKeysOnly: delegability is exact; absent
// permissions and absent boundary kinds are not delegable.
func TestAgentDelegable_ExactKeysOnly(t *testing.T) {
	if !AgentDelegable("agent.read", BoundaryKindHub) || !AgentDelegable("agent.read", BoundaryKindProject) {
		t.Fatal("agent.read must be delegable under project and hub boundaries")
	}
	for _, id := range []string{"agent.create", "agent.delete", "agent.lifecycle", "agent.attach", "agent.message", "project.read", "agent.list", "agent", "agent.*", ""} {
		if AgentDelegable(id, BoundaryKindHub) || AgentDelegable(id, BoundaryKindProject) {
			t.Errorf("%q must not be delegable", id)
		}
	}
	if AgentDelegable("agent.read", BoundaryKind("other")) {
		t.Error("an unknown boundary kind must not be delegable")
	}
}
