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

// RoleBinding holds the schema definition for the RoleBinding entity.
// A role binding connects a principal (user, agent, or group) to a role definition,
// optionally scoped to a specific project.
type RoleBinding struct {
	ent.Schema
}

// Fields of the RoleBinding.
func (RoleBinding) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("role_definition_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Enum("principal_type").
			Values("user", "agent", "group"),
		field.String("principal_id").
			NotEmpty(),
		field.Enum("scope_type").
			Values("system", "project"),
		field.String("scope_id").
			Default(""),
		field.String("created_by").
			Optional(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the RoleBinding.
func (RoleBinding) Indexes() []ent.Index {
	return []ent.Index{
		// Find all bindings for a principal
		index.Fields("principal_type", "principal_id"),
		// Find all bindings for a role definition
		index.Fields("role_definition_id"),
		// Find all bindings for a scope (e.g. a project)
		index.Fields("scope_type", "scope_id"),
		// Prevent duplicate bindings
		index.Fields("role_definition_id", "principal_type", "principal_id", "scope_type", "scope_id").Unique(),
	}
}

// Edges of the RoleBinding.
func (RoleBinding) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("role_definition", RoleDefinition.Type).
			Ref("role_bindings").
			Field("role_definition_id").
			Unique(),
	}
}
