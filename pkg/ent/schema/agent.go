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

// Agent holds the schema definition for the Agent entity. Columns are
// annotated to match the raw SQL naming in pkg/store/sqlite/sqlite.go so that
// a single database can back both the legacy raw-SQL store and Ent.
type Agent struct {
	ent.Schema
}

// Annotations set the table name.
func (Agent) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "agents"},
	}
}

// Fields of the Agent.
func (Agent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		// Raw SQL column: agent_id (the URL-safe per-grove identifier).
		field.String("slug").
			NotEmpty().
			StorageKey("agent_id"),
		field.String("name").
			NotEmpty(),
		// Raw SQL column is TEXT NOT NULL; we keep it Optional at the Ent
		// level so shadow records created by the CompositeStore don't need
		// to set it. The schema-diff gate treats this as tolerated drift.
		field.String("template").
			Optional(),
		field.UUID("grove_id", uuid.UUID{}),

		// Metadata (JSON blobs, raw columns are TEXT with no default).
		field.JSON("labels", map[string]string{}).
			Optional(),
		field.JSON("annotations", map[string]string{}).
			Optional(),

		// Lifecycle (V20 split status into phase/activity/tool_name).
		field.String("phase").
			Default("created"),
		field.String("activity").
			Default(""),
		field.String("tool_name").
			Default(""),

		field.String("connection_state").
			Default("unknown"),
		field.String("container_status").
			Optional(),
		field.String("runtime_state").
			Optional(),

		// Stalled detection (V25).
		field.String("stalled_from_activity").
			Default(""),

		// Limits tracking (V26).
		field.Int("current_turns").
			Default(0),
		field.Int("current_model_calls").
			Default(0),
		field.Time("started_at").
			Optional().
			Nillable(),

		// Runtime configuration.
		field.String("image").
			Optional(),
		field.Bool("detached").
			Default(true),
		field.String("runtime").
			Optional(),
		field.UUID("runtime_broker_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Bool("web_pty_enabled").
			Default(false),
		field.String("task_summary").
			Optional(),
		field.String("message").
			Optional(),

		// Applied configuration (JSON blob, opaque at the Ent layer).
		field.JSON("applied_config", AgentAppliedConfig{}).
			Optional(),

		// Timestamps (raw columns: created_at/updated_at).
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
		field.Time("last_seen").
			Optional().
			Nillable(),
		field.Time("last_activity_event").
			Optional().
			Nillable(),
		field.Time("deleted_at").
			Optional().
			Nillable(),

		// Ownership.
		field.UUID("created_by", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("owner_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.String("visibility").
			Default("private"),
		// DelegationEnabled is an Ent-only field (no raw SQL column). Used by
		// the policy engine to mark agents whose creator relationship is
		// policy-addressable. A follow-up raw-SQL migration will add this
		// column so the schema-diff gate can pass.
		field.Bool("delegation_enabled").
			Default(false),

		// Ancestry chain for transitive access control (V37). JSON array.
		field.JSON("ancestry", []string{}).
			Optional(),

		// Optimistic locking.
		field.Int64("state_version").
			Default(1),
	}
}

// Edges of the Agent.
func (Agent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("grove", Grove.Type).
			Ref("agents").
			Field("grove_id").
			Required().
			Unique(),
		edge.From("creator", User.Type).
			Ref("created_agents").
			Field("created_by").
			Unique(),
		edge.From("owner", User.Type).
			Ref("owned_agents").
			Field("owner_id").
			Unique(),
		edge.From("memberships", GroupMembership.Type).
			Ref("agent"),
		edge.From("policy_bindings", PolicyBinding.Type).
			Ref("agent"),
	}
}

// Indexes of the Agent. Named to match the raw SQL DDL so the schema-diff
// gate in pkg/store/entstore/schemacoverage_test.go can match indexes by name.
func (Agent) Indexes() []ent.Index {
	return []ent.Index{
		// idx_agents_grove_slug UNIQUE (grove_id, agent_id)
		index.Fields("grove_id", "slug").
			Unique().
			StorageKey("idx_agents_grove_slug"),
		// idx_agents_grove (grove_id)
		index.Fields("grove_id").
			StorageKey("idx_agents_grove"),
		// idx_agents_runtime_broker (runtime_broker_id)
		index.Fields("runtime_broker_id").
			StorageKey("idx_agents_runtime_broker"),
		// idx_agents_phase (phase)
		index.Fields("phase").
			StorageKey("idx_agents_phase"),
		// idx_agents_deleted (deleted_at) WHERE deleted_at IS NOT NULL
		index.Fields("deleted_at").
			StorageKey("idx_agents_deleted").
			Annotations(entsql.IndexWhere("deleted_at IS NOT NULL")),
	}
}
