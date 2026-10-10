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

package entc

import "testing"

// MigrateData bulk-inserts each listed entity into a destination that
// AutoMigrate has already given every unique index. AgentSessionMetrics has
// a unique (agent_id, session_id, started_at) index, and source databases
// written by older Hubs can hold rows that collide on it, which would abort
// the cutover. It is not copied today, so nothing collides. Adding it to
// migrationEntities requires skipping colliding rows (keeping the earliest
// by created_at, then id, as CompositeStore.deduplicateAgentSessionMetrics
// does) and counting them in the row-count check; update this test then.
func TestMigrationEntities_SessionMetricsNeedCollisionHandling(t *testing.T) {
	for _, name := range migrationEntities {
		if name == "AgentSessionMetrics" {
			t.Fatal("AgentSessionMetrics is in migrationEntities: MigrateData must skip rows " +
				"that collide on its unique (agent_id, session_id, started_at) index before it can be copied")
		}
	}
}
