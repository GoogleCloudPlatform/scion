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

// UserExecContainer holds the schema definition for the UserExecContainer
// entity: at most one row per user (1:1 via the unique user edge), storing the
// UUID of the agent registered as the user's default execution container.
//
// agent_id is intentionally not a foreign key: agents are soft-deleted and
// cross-project, and registration validity is resolved lazily at read time.
type UserExecContainer struct {
	ent.Schema
}

// Fields of the UserExecContainer.
func (UserExecContainer) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("user_id", uuid.UUID{}).
			Unique().
			Immutable(),
		field.UUID("agent_id", uuid.UUID{}).
			Comment("Registered execution container agent UUID (not a foreign key; resolved lazily at read time)"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the UserExecContainer.
func (UserExecContainer) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("exec_container").
			Field("user_id").
			Unique().
			Required().
			Immutable(),
	}
}

// Indexes of the UserExecContainer.
func (UserExecContainer) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id"),
	}
}

// Annotations of the UserExecContainer.
func (UserExecContainer) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "user_exec_containers"},
	}
}
