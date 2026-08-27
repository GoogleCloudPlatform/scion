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

// Conversation holds the schema definition for the Conversation entity.
// A conversation is the container for a thread of messages between participants
// (users and/or agents). It may be a direct (1:1) conversation or a group
// conversation, and may originate from a native or external surface (Discord,
// Slack, etc.).
//
// Foreign keys (project_id, default_agent_id) are modeled as plain UUID columns
// rather than Ent edges to keep this schema independent of existing entities;
// edge wiring is deferred to a later phase.
type Conversation struct {
	ent.Schema
}

// Fields of the Conversation.
func (Conversation) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Enum("kind").
			Values("direct", "group"),
		field.Enum("surface").
			Values("native", "discord", "slack", "telegram", "gchat", "teams"),
		field.String("external_ref").
			Optional().
			Default(""),
		field.String("parent_ref").
			Optional().
			Default(""),
		field.String("display_name").
			Optional().
			Default(""),
		field.UUID("default_agent_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Enum("drift_state").
			Values("active", "orphaned", "unresolvable").
			Default("active"),
		field.Time("last_activity_at").
			Default(time.Now),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("archived_at").
			Optional().
			Nillable(),
		field.Time("deleted_at").
			Optional().
			Nillable(),
	}
}

// Indexes of the Conversation.
func (Conversation) Indexes() []ent.Index {
	return []ent.Index{
		// Partial unique index: only one active (non-deleted) conversation per
		// (surface, external_ref) pair when external_ref is non-empty.
		// Both SQLite and Postgres support WHERE clauses on indexes.
		index.Fields("surface", "external_ref").
			Unique().
			Annotations(
				entsql.IndexWhere("external_ref <> '' AND deleted_at IS NULL"),
			),
		index.Fields("project_id"),
		index.Fields("kind"),
	}
}

// Annotations of the Conversation.
func (Conversation) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "conversations"},
	}
}
