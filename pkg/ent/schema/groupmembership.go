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

// GroupMembership holds the schema definition for the GroupMembership entity.
// It serves as a through-table linking principals (users or agents) to groups,
// with role and audit metadata.
type GroupMembership struct {
	ent.Schema
}

// Fields of the GroupMembership.
func (GroupMembership) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.Enum("role").
			Values("member", "admin", "owner").
			Default("member"),
		field.String("added_by").
			Optional(),
		field.Time("added_at").
			Default(time.Now).
			Immutable(),
		field.UUID("group_id", uuid.UUID{}),
		field.UUID("user_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("agent_id", uuid.UUID{}).
			Optional().
			Nillable(),
	}
}

// Edges of the GroupMembership.
func (GroupMembership) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("group", Group.Type).
			Ref("memberships").
			Field("group_id").
			Required().
			Unique(),
		edge.To("user", User.Type).
			Field("user_id").
			Unique(),
		edge.To("agent", Agent.Type).
			Field("agent_id").
			Unique(),
	}
}

// Indexes of the GroupMembership.
func (GroupMembership) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id", "user_id").
			Unique(),
		index.Fields("group_id", "agent_id").
			Unique(),
	}
}
