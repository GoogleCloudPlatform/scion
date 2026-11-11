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
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AgentServiceAccountAssignment records who authorized an agent's assign-mode
// GCP service account, with the recorded provenance and frozen effect ceiling
// of that principal. At most one row per agent is active. There is no edge
// (no foreign key) to agents, matching delegation edges, so the history
// survives a hard delete.
type AgentServiceAccountAssignment struct {
	ent.Schema
}

// Mixin of the AgentServiceAccountAssignment: recorded provenance, the frozen
// effect ceiling and the deactivation record. All columns default to the
// unrecorded zero values.
func (AgentServiceAccountAssignment) Mixin() []ent.Mixin {
	return []ent.Mixin{
		AuthorityProvenanceMixin{},
		EffectCeilingMixin{},
		DeactivationMixin{},
	}
}

// Fields of the AgentServiceAccountAssignment.
func (AgentServiceAccountAssignment) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		// agent_id is the agent the service account is assigned to.
		field.String("agent_id").
			NotEmpty().
			Immutable(),
		// project_id is the agent's project at the time of the write.
		field.String("project_id").
			Default("").
			Immutable(),
		// service_account_id is the GCP service account record ID.
		field.String("service_account_id").
			NotEmpty().
			Immutable(),
		// origin names the write that recorded the row; descriptive only.
		field.String("origin").
			Default("").
			Immutable(),
		// active is true for the agent's current assignment.
		field.Bool("active").
			Default(true),
		// created is the time the row was recorded.
		field.Time("created").
			Default(time.Now).
			Immutable(),
		// updated is the time of the last change (deactivation or
		// reactivation).
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the AgentServiceAccountAssignment.
func (AgentServiceAccountAssignment) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id", "active"),
		// At most one active row per agent.
		index.Fields("agent_id").
			Unique().
			Annotations(entsql.IndexWhere("active = true")),
	}
}

// Edges of the AgentServiceAccountAssignment.
func (AgentServiceAccountAssignment) Edges() []ent.Edge {
	return nil
}
