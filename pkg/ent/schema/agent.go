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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Agent holds the schema definition for the Agent entity.
//
// The agent entity carries both the principal-relevant fields used by the
// authorization layer (created_by, owner_id, delegation_enabled, visibility)
// and the full set of operational fields required to back store.Agent through
// the Ent adapter (P2-port-agent). Together they give the Ent-backed agent
// store parity with the former raw-SQL store implementation.
type Agent struct {
	ent.Schema
}

// Fields of the Agent.
func (Agent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("slug").
			NotEmpty(),
		field.String("name").
			NotEmpty(),
		field.String("template").
			Optional(),
		field.UUID("project_id", uuid.UUID{}).
			StorageKey("project_id"),
		field.Enum("status").
			Values("created", "provisioning", "cloning", "starting", "running", "suspended", "stopping", "stopped", "error").
			Default("created"),
		// created_by and owner_id are polymorphic *principal* references: the
		// creator/owner may be a user OR another agent (an agent that spawns a
		// sub-agent records its own ID here). They therefore carry no foreign-key
		// edge to the users table — a User-typed FK rejected every agent-created
		// sub-agent with a foreign-key violation. Consumers that need the user
		// behind the ID must look it up by ID and tolerate "no such user".
		field.UUID("created_by", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("owner_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Bool("delegation_enabled").
			Default(false),
		field.String("visibility").
			Default("private"),

		// --- Metadata (stored as JSON) ---
		field.JSON("labels", map[string]string{}).
			Optional(),
		field.JSON("annotations", map[string]string{}).
			Optional(),

		// --- Runtime status ---
		field.String("phase").
			Optional(),
		field.String("activity").
			Optional(),
		field.String("tool_name").
			Optional(),
		field.String("connection_state").
			Optional(),
		field.String("container_status").
			Optional(),
		field.String("runtime_state").
			Optional(),
		field.String("stalled_from_activity").
			Optional(),

		// --- Limits tracking ---
		field.Int("current_turns").
			Default(0),
		field.Int("current_model_calls").
			Default(0),

		// --- Runtime configuration ---
		field.String("image").
			Optional(),
		field.Bool("detached").
			Default(false),
		field.String("runtime").
			Optional(),
		field.String("runtime_broker_id").
			Optional(),
		field.Bool("web_pty_enabled").
			Default(false),
		field.String("task_summary").
			Optional(),
		field.String("message").
			Optional(),

		// applied_config is the agent's resolved configuration, persisted as a
		// JSON document (store.AgentAppliedConfig). Stored as text to keep the
		// Ent schema decoupled from the store package's struct definition.
		field.Text("applied_config").
			Optional(),

		// ancestry is the ordered chain of ancestor principal IDs used for
		// transitive access control. Stored as a JSON array so the dialect-aware
		// json_each / json_array_elements_text membership filter can be applied.
		field.JSON("ancestry", []string{}).
			Optional(),

		// --- Timestamps ---
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
		field.Time("last_seen").
			Optional().
			Nillable(),
		field.Time("last_activity_event").
			Optional().
			Nillable(),
		field.Time("started_at").
			Optional().
			Nillable(),
		// deleted_at backs soft-delete: a non-nil value excludes the agent from
		// default listings (filtered via the DeletedAtIsNil Ent predicate).
		field.Time("deleted_at").
			Optional().
			Nillable(),

		// --- Optimistic locking ---
		// state_version is incremented on every UpdateAgent and used as a CAS
		// guard to detect concurrent modifications under multi-replica Postgres.
		field.Int64("state_version").
			Default(1),
	}
}

// Edges of the Agent.
func (Agent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).
			Ref("agents").
			Field("project_id").
			Required().
			Unique(),
		edge.From("memberships", GroupMembership.Type).
			Ref("agent"),
		edge.From("policy_bindings", PolicyBinding.Type).
			Ref("agent"),
	}
}

// Indexes of the Agent.
func (Agent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug", "project_id").
			Unique(),
	}
}
