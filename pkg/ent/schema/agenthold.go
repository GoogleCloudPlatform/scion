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
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// membershipLossTriggers are the events that ask the hub to re-evaluate a
// user's project access. Shared by AgentHold.trigger and
// MembershipLossCheck.trigger; keep in sync with store.MembershipLossTrigger.
var membershipLossTriggers = []string{
	"member_remove",
	"member_role_change",
	"member_principal_delete",
	"admin_binding_delete",
	"ownership_transfer",
	"group_change",
	"binding_expiry",
	"system_scope_change",
	"restore_check",
	"reconcile",
}

// AgentHold holds the schema definition for the AgentHold entity: one row
// per (agent, root principal) recording that the agent is held because the
// root principal's access to the agent's project ended. An agent is held
// while any of its rows is active (cleared_at IS NULL). The rows live in
// their own table so that agent row writes (phase, status, PATCH) never
// change them.
type AgentHold struct {
	ent.Schema
}

// Fields of the AgentHold.
func (AgentHold) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("agent_id", uuid.UUID{}).
			Immutable(),
		// project_id is the agent's project, denormalised for per-project
		// listing.
		field.UUID("project_id", uuid.UUID{}).
			Immutable(),
		field.Enum("cause").
			Values("owner_access_ended").
			Immutable(),
		field.String("root_principal_type").
			NotEmpty().
			Immutable(),
		field.String("root_principal_id").
			NotEmpty().
			Immutable(),
		// via_agent_id is the descendant-walk parent that led to this agent;
		// NULL for an agent reached directly from the root principal.
		field.UUID("via_agent_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable(),
		field.Enum("trigger").
			Values(membershipLossTriggers...).
			Immutable(),
		field.String("actor_kind").
			Default("").
			Immutable(),
		field.String("actor_id").
			Default("").
			Immutable(),
		field.String("correlation_id").
			Default("").
			Immutable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("cleared_at").
			Optional().
			Nillable(),
		field.String("cleared_by_kind").
			Default(""),
		field.String("cleared_by_id").
			Default(""),
		field.String("clear_reason").
			Default(""),
	}
}

// Edges of the AgentHold.
func (AgentHold) Edges() []ent.Edge {
	return []ent.Edge{
		// Hard-deleting the agent removes its holds (ON DELETE CASCADE on
		// the agent edge, see Agent.Edges).
		edge.From("agent", Agent.Type).
			Ref("holds").
			Field("agent_id").
			Unique().
			Required().
			Immutable(),
	}
}

// Indexes of the AgentHold.
func (AgentHold) Indexes() []ent.Index {
	return []ent.Index{
		// At most one active hold per (agent, root principal). Inserts use
		// ON CONFLICT ... DO NOTHING against this index, which makes
		// repeated inserts idempotent; cleared rows are kept as history.
		index.Fields("agent_id", "root_principal_id").
			Unique().
			Annotations(entsql.IndexWhere("cleared_at IS NULL")),
		// Point read: does this agent have an active hold?
		index.Fields("agent_id").
			StorageKey("agenthold_agent_id_active").
			Annotations(entsql.IndexWhere("cleared_at IS NULL")),
		// Per-project listing.
		index.Fields("project_id", "cleared_at"),
	}
}

// Annotations of the AgentHold.
func (AgentHold) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "agent_holds"},
	}
}
