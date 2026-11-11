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
	"github.com/google/uuid"
)

// UserTerminalWorkspace holds the schema definition for the
// UserTerminalWorkspace entity: one row per user, storing the ordered list of
// open terminal agents the terminal viewer (/terminals) restores on open.
type UserTerminalWorkspace struct {
	ent.Schema
}

// Fields of the UserTerminalWorkspace.
func (UserTerminalWorkspace) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("user_id", uuid.UUID{}).
			Unique().
			Immutable(),
		field.JSON("agent_ids", []string{}).
			Comment("Ordered, lowercase canonical agent UUIDs; max 32; unique within the list"),
		field.String("frontmost_agent_id").
			Optional().
			Nillable().
			Comment("Must be a member of agent_ids when set"),
		field.Int("schema_version").
			Default(1),
		field.Int64("revision").
			Default(0).
			Comment("Incremented on every write; informational (last-writer-wins)"),
		field.Time("update_time").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the UserTerminalWorkspace.
func (UserTerminalWorkspace) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("terminal_workspace").
			Field("user_id").
			Unique().
			Required().
			Immutable(),
	}
}

// Annotations of the UserTerminalWorkspace.
func (UserTerminalWorkspace) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "user_terminal_workspaces"},
	}
}
