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

//go:build !no_sqlite

package hub

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDeleteAgent_SameProjectPeerAllowedWithLifecycleScope pins existing,
// intentional behavior rather than changing it: an agent holding
// project:agent:lifecycle may delete any agent within its own project, not
// only its own descendants. authorizeAgentLifecycle's doc comment calls this
// breadth deliberate (design Q3), and performAgentDelete's AgentIdentity
// branch applies the identical two-part check (scope + same project, no
// ancestry test). This is not part of #1908 -- it is the same rule on both
// the global and project-scoped delete routes, so there is nothing route-
// specific to fix here. The test exists so that if this model changes later
// (tracked separately, see #1097), the change shows up as a diff to an
// explicit assertion instead of silently drifting between the two routes.
func TestDeleteAgent_SameProjectPeerAllowedWithLifecycleScope(t *testing.T) {
	t.Run("global route", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodDelete, "/api/v1/agents/"+f.sibling.ID, nil, ScopeAgentLifecycle)
		assert.NotEqual(t, http.StatusForbidden, rec.Code,
			"an agent holding project:agent:lifecycle may delete a same-project peer, not just its own descendants (#1097); got %d: %s",
			rec.Code, rec.Body.String())
	})

	t.Run("project-scoped route", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodDelete, "/api/v1/projects/"+f.proj.ID+"/agents/"+f.sibling.ID, nil, ScopeAgentLifecycle)
		assert.NotEqual(t, http.StatusForbidden, rec.Code,
			"the project-scoped delete route must apply the same #1097 breadth as the global route; got %d: %s",
			rec.Code, rec.Body.String())
	})
}
