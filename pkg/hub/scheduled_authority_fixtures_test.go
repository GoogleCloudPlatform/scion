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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// seedScheduleAuthorAgent stores the agent that authzHelperAgent names, in
// projectID, so the agent can author schedules: a schedule revision records
// the author's effect ceiling, which for an agent is computed from its
// stored row. The agent has no edge, the shape of an agent created before
// the edge backfill (which these tests leave incomplete).
func seedScheduleAuthorAgent(t *testing.T, s store.Store, projectID string) {
	t.Helper()
	require.NoError(t, s.CreateAgent(context.Background(), &store.Agent{
		ID:            authzHelperAgentID,
		Slug:          "schedule-author-agent",
		Name:          "schedule-author-agent",
		ProjectID:     projectID,
		Phase:         "running",
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
	}))
}
