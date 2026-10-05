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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// AgentRecovery holds per-agent runtime observations recorded from complete
// broker inventories: whether the agent's container or pod was present and
// running, present and terminal, or absent, and whether the broker reported
// a start in flight for it. One row per observed agent; the id is the agent
// ID. It is written only by AgentStore.RecordRecoveryObservations and is
// deleted with the agent.
type AgentRecovery struct {
	ent.Schema
}

// Fields of the AgentRecovery.
func (AgentRecovery) Fields() []ent.Field {
	return []ent.Field{
		// id is the agent ID.
		field.String("id").
			NotEmpty().
			Immutable(),
		field.String("broker_id").
			Optional().
			Default(""),
		// observed_state: present_running, present_terminal or absent.
		field.String("observed_state").
			Optional().
			Default(""),
		// observed_target is the runtime target whose complete inventory
		// produced the observation.
		field.String("observed_target").
			Optional().
			Default(""),
		// observed_at is the store-clock time of the heartbeat that produced
		// the observation.
		field.Time("observed_at").
			Optional().
			Nillable(),
		// first_absent_at is the store-clock time of the first observation in
		// the current run of absent observations; NULL when not absent.
		field.Time("first_absent_at").
			Optional().
			Nillable(),
		// observed_in_flight is true when the same heartbeat listed a start
		// in flight for the agent.
		field.Bool("observed_in_flight").
			Default(false),
	}
}

// Indexes of the AgentRecovery.
func (AgentRecovery) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("broker_id"),
	}
}
