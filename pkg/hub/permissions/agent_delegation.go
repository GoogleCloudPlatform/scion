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
	"agent.read": {BoundaryKindProject, BoundaryKindHub},
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
