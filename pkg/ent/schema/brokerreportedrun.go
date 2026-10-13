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

package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// BrokerReportedRun records that a runtime broker other than an agent's
// assigned broker reported a runtime entry of the agent carrying run_id
// (its scion.run_id label), as of seen_at, the hub-clock time of the last
// heartbeat that reported it (ptone/scion#3661). One row per (agent, run,
// broker), upserted on seen_at by every hub replica that handles such a
// heartbeat, so the stranded-run adoption can tell whether any broker
// reported the agent's current run within its quiet period. Entries on
// the assigned broker get no row: the adoption reads them from the
// heartbeat it is processing. Rows older than the quiet period are pruned
// by the heartbeat handling, and removed with the agent (ON DELETE
// CASCADE on the agent edge, see Agent.Edges).
type BrokerReportedRun struct {
	ent.Schema
}

// Fields of the BrokerReportedRun.
func (BrokerReportedRun) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("agent_id", uuid.UUID{}).
			Immutable(),
		field.String("run_id").
			NotEmpty().
			Immutable(),
		field.String("broker_id").
			NotEmpty().
			Immutable(),
		field.Time("seen_at"),
	}
}

// Edges of the BrokerReportedRun.
func (BrokerReportedRun) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("agent", Agent.Type).
			Ref("reported_runs").
			Field("agent_id").
			Unique().
			Required().
			Immutable(),
	}
}

// Indexes of the BrokerReportedRun.
func (BrokerReportedRun) Indexes() []ent.Index {
	return []ent.Index{
		// The table's key: upserts conflict on it.
		index.Fields("agent_id", "run_id", "broker_id").
			Unique(),
		// The prune of rows older than the quiet period.
		index.Fields("seen_at"),
	}
}
