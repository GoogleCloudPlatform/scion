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

// AgentDelegableRegistry is the hub agent-delegation policy
// (.design/agent-delegation.md §13): the reviewed, exact list of permissions
// that may ever be delegated to an agent through an agent delegation grant,
// with the boundary kinds each may be delegated under. It is the only
// source of delegability. A permission that is absent is not delegable, and
// registering a new permission never makes it delegable. There is no
// wildcard or family expansion.
//
// A key enters this table in the same change that admits its first route
// for a delegated credential, so every key has an admitting catalog
// operation (pinned by tests in pkg/hub).
var AgentDelegableRegistry = map[string][]BoundaryKind{
	"agent.read":    {BoundaryKindProject, BoundaryKindHub},
	"project.read":  {BoundaryKindProject, BoundaryKindHub},
	"agent.message": {BoundaryKindProject, BoundaryKindHub},
	// Admitted on the PTY handshake only; the lifecycle operations that
	// share it (exec, env, resetauth, keys) are not admitted.
	"agent.attach": {BoundaryKindProject, BoundaryKindHub},
	// Admitted on the hub agent list (GET /api/v1/agents) only. Each listed
	// item is also decided for agent.read, so a grant without agent.read
	// lists nothing.
	"agent.list": {BoundaryKindProject, BoundaryKindHub},
}

// AgentDelegationSharedKeyExemption names a catalog operation whose base
// permission is a key of AgentDelegableRegistry but which is never admitted
// for an agent delegated credential.
type AgentDelegationSharedKeyExemption struct {
	Permission string
	Operation  string
	Reason     string
}

// AgentDelegationSharedKeyExemptions is the reviewed list of operations that
// share a delegable key and have an effect the policy forbids, but are never
// admitted for an agent delegated credential. The effect guard in pkg/hub
// scans every catalog operation and skips only these exact pairs; a test
// there fails if any of them is ever admitted (.design/agent-delegation.md
// §13).
var AgentDelegationSharedKeyExemptions = []AgentDelegationSharedKeyExemption{
	{
		Permission: "project.read",
		Operation:  "project.broadcast",
		Reason:     "broadcast sends a message to every agent of a project; it shares project.read but is not a delegable use of it",
	},
}

// AgentDelegable reports whether permissionID may be delegated to an agent
// under a grant of boundary kind kind.
func AgentDelegable(permissionID string, kind BoundaryKind) bool {
	for _, k := range AgentDelegableRegistry[permissionID] {
		if k == kind {
			return true
		}
	}
	return false
}
