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

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// MigrateData bulk-inserts each listed entity into a destination that
// AutoMigrate has already given every unique index. AgentSessionMetrics has a
// unique (agent_id, session_id, started_at) index, and a source written by an
// older Hub can hold rows that collide on it, which would abort the cutover.
// It must therefore be copied, and copied through a row filter that drops the
// colliding rows.
func TestMigrationEntities_SessionMetricsCopiedWithCollisionHandling(t *testing.T) {
	listed := false
	for _, name := range migrationEntities {
		if name == "AgentSessionMetrics" {
			listed = true
		}
	}
	if !listed {
		t.Fatal("AgentSessionMetrics is not in migrationEntities: a cutover would drop every stored session metric")
	}
	if migrationRowFilters["AgentSessionMetrics"] == nil {
		t.Fatal("AgentSessionMetrics has no entry in migrationRowFilters: source rows that collide on its " +
			"unique (agent_id, session_id, started_at) index would abort the copy")
	}
	for name := range migrationRowFilters {
		found := false
		for _, n := range migrationEntities {
			if n == name {
				found = true
			}
		}
		if !found {
			t.Errorf("migrationRowFilters has %q, which is not in migrationEntities", name)
		}
	}
}

// TestDedupAgentSessionMetricsRows pins the keep-earliest rule without a
// database: per (agent, session, segment start) the row with the lowest
// created_at wins, ties broken by the lower id; started_at is compared as an
// instant at microsecond precision; distinct segments and distinct sessions
// are all kept.
func TestDedupAgentSessionMetricsRows(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	created := time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC)
	lowID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	highID := uuid.MustParse("00000000-0000-0000-0000-000000000002")

	row := func(id uuid.UUID, session string, startedAt, createdAt time.Time) *ent.AgentSessionMetrics {
		return &ent.AgentSessionMetrics{ID: id, AgentID: "agent-a", SessionID: session, StartedAt: startedAt, CreatedAt: createdAt}
	}
	later := row(uuid.New(), "s1", start, created.Add(time.Minute))
	earliest := row(uuid.New(), "s1", start, created)
	// Same instant as start, in another location: still the same key.
	sameKeyOtherZone := row(uuid.New(), "s1", start.In(time.FixedZone("x", 3600)), created.Add(2*time.Minute))
	tieHigh := row(highID, "s2", start, created)
	tieLow := row(lowID, "s2", start, created)
	otherSegment := row(uuid.New(), "s1", start.Add(time.Hour), created.Add(time.Hour))
	// Starts half a second apart are distinct segments, so a key at second
	// precision would wrongly merge them.
	subSecondA := row(uuid.New(), "s3", start, created)
	subSecondB := row(uuid.New(), "s3", start.Add(500*time.Millisecond), created)
	// Starts that differ only below a microsecond are one key in Postgres:
	// pgx truncates to microseconds, so both become start+1us (rounding would
	// split them into 1us and 2us). The earlier stored row wins.
	subMicroEarlier := row(uuid.New(), "s4", start.Add(1400*time.Nanosecond), created)
	subMicroLater := row(uuid.New(), "s4", start.Add(1600*time.Nanosecond), created.Add(time.Second))

	in := []*ent.AgentSessionMetrics{later, earliest, sameKeyOtherZone, tieHigh, tieLow, otherSegment,
		subSecondA, subSecondB, subMicroLater, subMicroEarlier}
	rows := make([]reflect.Value, len(in))
	for i, m := range in {
		rows[i] = reflect.ValueOf(m)
	}

	out, dropped := dedupAgentSessionMetricsRows(rows)
	if dropped != 4 {
		t.Errorf("dropped = %d, want 4", dropped)
	}
	var got []*ent.AgentSessionMetrics
	for _, rv := range out {
		got = append(got, rv.Interface().(*ent.AgentSessionMetrics))
	}
	want := []*ent.AgentSessionMetrics{earliest, tieLow, otherSegment, subSecondA, subSecondB, subMicroEarlier}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kept rows = %v, want %v", sessionMetricsIDs(got), sessionMetricsIDs(want))
	}
}

func sessionMetricsIDs(ms []*ent.AgentSessionMetrics) []uuid.UUID {
	out := make([]uuid.UUID, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}
