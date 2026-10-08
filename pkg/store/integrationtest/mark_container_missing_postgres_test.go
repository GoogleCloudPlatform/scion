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

//go:build integration

package integrationtest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarkAgentContainerMissing_Phases_Postgres is the Postgres counterpart
// of pkg/store/entadapter's SQLite tests for the phases
// MarkAgentContainerMissing accepts (ptone/scion#2669): running and stopping
// agents are marked (a preempted reason is kept), a stopped agent is not.
// It requires SCION_TEST_POSTGRES_URL and skips otherwise.
func TestMarkAgentContainerMissing_Phases_Postgres(t *testing.T) {
	requirePG(t)
	ctx := context.Background()
	cs := newStore(t)
	p := seedProject(t, cs)
	cutoff := time.Now().Add(-5 * time.Minute)

	create := func(slug, phase, reason string) string {
		a := makeAgent(p.ID, slug)
		a.Phase = phase
		a.RuntimeBrokerID = "broker-1"
		a.LastSeen = time.Now().Add(-time.Hour)
		a.ExitReason = reason
		require.NoError(t, cs.CreateAgent(ctx, a))
		return a.ID
	}

	for _, tc := range []struct {
		slug, phase, reason string
		wantMarked          bool
		wantReason          string
	}{
		{"pg-running", "running", "", true, "container_missing"},
		{"pg-stopping", "stopping", "", true, "container_missing"},
		{"pg-stopping-preempted", "stopping", "preempted", true, "preempted"},
		{"pg-stopped", "stopped", "", false, ""},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			id := create(tc.slug, tc.phase, tc.reason)
			got, err := cs.MarkAgentContainerMissing(ctx, id, "broker-1", cutoff, "gone")
			require.NoError(t, err)
			stored, err := cs.GetAgent(ctx, id)
			require.NoError(t, err)
			if !tc.wantMarked {
				assert.Nil(t, got)
				assert.Equal(t, tc.phase, stored.Phase)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, "error", stored.Phase)
			assert.Equal(t, tc.wantReason, stored.ExitReason)
		})
	}
}
