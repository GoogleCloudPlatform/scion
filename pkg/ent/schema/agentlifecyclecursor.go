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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AgentLifecycleCursor holds one agent's lifecycle-event sequencing state
// (WP1 design section 4): the dense per-agent transition revision, the
// metadata revision and digest, and the fingerprint of the last observed
// row. It is operational state, not an analytical record: one row per
// agent, removed with the agent (hard delete, purge, project delete) or by
// the orphan sweep.
//
// The key is the agent ID with no foreign key (agent_id and project_id are
// uuid, like agents.id and agents.project_id, so the orphan sweep compares
// like types on PostgreSQL; review r6 N1). The open_run* and pending_*
// columns are written only by the run rules of a later slice; they exist
// now so that slice needs no migration.
type AgentLifecycleCursor struct {
	ent.Schema
}

// Fields of the AgentLifecycleCursor.
func (AgentLifecycleCursor) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			StorageKey("agent_id").
			Immutable(),
		field.UUID("project_id", uuid.UUID{}),
		field.Int64("transition_revision").
			Default(0),
		field.Int64("metadata_revision").
			Default(0),
		field.String("metadata_digest").
			Default(""),
		field.String("last_fingerprint").
			Default(""),
		// last_deleted: the last observed row had deleted_at set.
		field.Bool("last_deleted").
			Default(false),
		field.String("open_run").
			Default(""),
		field.String("open_run_generation").
			Default(""),
		field.Int64("open_run_metadata_revision").
			Default(0),
		field.Time("pending_run_change_at").
			Optional().
			Nillable(),
		field.String("reincarnation_base_run_id").
			Default(""),
		field.String("pending_adopted_from").
			Default(""),
		field.Time("pending_adopted_at").
			Optional().
			Nillable(),
		field.String("pending_adoption_source").
			Default(""),
		field.String("creator_principal_type").
			Default(""),
		field.String("creator_principal_id").
			Default(""),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the AgentLifecycleCursor.
func (AgentLifecycleCursor) Indexes() []ent.Index {
	return []ent.Index{
		// DeleteProject removes every cursor of the project's agents.
		index.Fields("project_id"),
	}
}

// Annotations of the AgentLifecycleCursor.
func (AgentLifecycleCursor) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "agent_lifecycle_cursors"},
	}
}
