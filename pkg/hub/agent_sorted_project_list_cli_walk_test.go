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
	"context"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListProjectAgents_CLIWalk_LegacyBindingSurvivesPagination is the
// legacy cursor-walk case: a legacy (no "sort")
// project-agents cursor walk, driven through hubclient.Projects().ListAgents
// the way cmd/project_health.go's walk does (limit=200, following
// NextCursor until empty), must return every agent exactly once under the
// project cursor binding, which applies in both legacy and sorted mode.
// Run for both a user and an agent JWT.
func TestListProjectAgents_CLIWalk_LegacyBindingSurvivesPagination(t *testing.T) {
	f := sortedListSetup(t)

	// Uses 450 agents at limit=200, as
	// cmd/project_health.go's walk does (bulk insert via store.Store.WithTx
	// makes this cheap, and legacy mode's per-page cost is independent of
	// total candidate count, unlike sorted mode).
	const total = 450
	const pageLimit = 200
	agents := f.createAgentsBulk(t, total, "walk", string(state.PhaseStopped), nil)
	want := make(map[string]bool, total)
	for _, a := range agents {
		want[a.ID] = true
	}

	httpSrv := httptest.NewServer(f.srv.Handler())
	defer httpSrv.Close()

	t.Run("user", func(t *testing.T) {
		tokenPair, _, _, err := f.srv.userTokenService.GenerateTokenPair(
			f.owner.ID, f.owner.Email, f.owner.DisplayName, f.owner.Role, ClientTypeWeb,
		)
		require.NoError(t, err)

		c, err := hubclient.New(httpSrv.URL, hubclient.WithBearerToken(tokenPair))
		require.NoError(t, err)

		seen := walkProjectAgents(t, c, f.project.ID, pageLimit)
		assertWalkSawEveryAgentOnce(t, want, seen)
	})

	t.Run("agent JWT", func(t *testing.T) {
		// The agent-JWT project-agents path has no read filter, so any
		// agent in the project can
		// walk every sibling. Use one of the created agents as the walker.
		var walkerID string
		for id := range want {
			walkerID = id
			break
		}
		svc := f.srv.GetAgentTokenService()
		require.NotNil(t, svc)
		tok, err := svc.GenerateAgentToken(walkerID, f.project.ID, []AgentTokenScope{ScopeProjectRead}, nil)
		require.NoError(t, err)

		c, err := hubclient.New(httpSrv.URL, hubclient.WithAgentToken(tok))
		require.NoError(t, err)

		seen := walkProjectAgents(t, c, f.project.ID, pageLimit)
		assertWalkSawEveryAgentOnce(t, want, seen)
	})
}

// walkProjectAgents mirrors cmd/project_health.go's walk: page at limit,
// follow NextCursor until it is empty.
func walkProjectAgents(t *testing.T, c hubclient.Client, projectID string, limit int) []string {
	t.Helper()
	var seen []string
	cursor := ""
	for pages := 0; ; pages++ {
		require.Lessf(t, pages, 20, "walk did not terminate within a sane number of pages")
		resp, err := c.Projects().ListAgents(context.Background(), projectID, &hubclient.ListAgentsOptions{
			Page: apiclient.PageOptions{Limit: limit, Cursor: cursor},
		})
		require.NoError(t, err)
		for _, a := range resp.Agents {
			seen = append(seen, a.ID)
		}
		if !resp.Page.HasMore() {
			break
		}
		cursor = resp.Page.NextCursor
	}
	return seen
}

func assertWalkSawEveryAgentOnce(t *testing.T, want map[string]bool, seen []string) {
	t.Helper()
	counts := make(map[string]int, len(seen))
	for _, id := range seen {
		counts[id]++
	}
	for id := range want {
		assert.Equalf(t, 1, counts[id], "agent %s must be seen exactly once", id)
	}
	for id, n := range counts {
		assert.Truef(t, want[id], "walk returned an unexpected agent %s (seen %d times)", id, n)
	}
	assert.Len(t, seen, len(want), "walk must return exactly the seeded set, no more, no fewer")
}
