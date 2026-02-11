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
	"github.com/google/uuid"
)

// Group holds the schema definition for the Group entity.
type Group struct {
	ent.Schema
}

// Fields of the Group.
func (Group) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty(),
		field.String("slug").
			Unique().
			NotEmpty(),
		field.String("description").
			Optional(),
		field.Enum("group_type").
			Values("explicit", "grove_agents").
			Default("explicit"),
		field.UUID("grove_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.JSON("labels", map[string]string{}).
			Optional(),
		field.JSON("annotations", map[string]string{}).
			Optional(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
		field.String("created_by").
			Optional(),
		field.UUID("owner_id", uuid.UUID{}).
			Optional().
			Nillable(),
	}
}

// Edges of the Group.
func (Group) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("memberships", GroupMembership.Type),
		edge.To("child_groups", Group.Type).
			From("parent_groups"),
		edge.From("owner", User.Type).
			Ref("owned_groups").
			Field("owner_id").
			Unique(),
		edge.To("grove", Grove.Type).
			Field("grove_id").
			Unique(),
		edge.From("policy_bindings", PolicyBinding.Type).
			Ref("group"),
	}
}
