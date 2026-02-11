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

// PolicyBinding holds the schema definition for the PolicyBinding entity.
// It links principals (users, agents, or groups) to policies.
type PolicyBinding struct {
	ent.Schema
}

// Fields of the PolicyBinding.
func (PolicyBinding) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.Enum("principal_type").
			Values("user", "agent", "group"),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.String("created_by").
			Optional(),
		field.UUID("policy_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("user_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("group_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("agent_id", uuid.UUID{}).
			Optional().
			Nillable(),
	}
}

// Edges of the PolicyBinding.
func (PolicyBinding) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("policy", AccessPolicy.Type).
			Ref("bindings").
			Field("policy_id").
			Unique(),
		edge.To("user", User.Type).
			Field("user_id").
			Unique(),
		edge.To("group", Group.Type).
			Field("group_id").
			Unique(),
		edge.To("agent", Agent.Type).
			Field("agent_id").
			Unique(),
	}
}
