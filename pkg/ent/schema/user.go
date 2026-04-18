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

// User holds the schema definition for the User entity.
type User struct {
	ent.Schema
}

// Annotations set the table name.
func (User) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "users"},
	}
}

// Fields of the User.
func (User) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("email").
			Unique().
			NotEmpty(),
		field.String("display_name").
			NotEmpty(),
		field.String("avatar_url").
			Optional(),
		field.Enum("role").
			Values("admin", "member", "viewer").
			Default("member"),
		field.Enum("status").
			Values("active", "suspended").
			Default("active"),
		field.JSON("preferences", &UserPreferences{}).
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("last_login").
			Optional().
			Nillable(),
		// V27: last_seen for presence tracking.
		field.Time("last_seen").
			Optional().
			Nillable(),
	}
}

// Edges of the User.
func (User) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("created_agents", Agent.Type),
		edge.To("owned_agents", Agent.Type),
		edge.To("owned_groups", Group.Type),
		edge.From("memberships", GroupMembership.Type).
			Ref("user"),
		edge.From("policy_bindings", PolicyBinding.Type).
			Ref("user"),
	}
}

// Indexes of the User, named to match raw SQL DDL.
func (User) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("email").
			StorageKey("idx_users_email"),
	}
}
