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

// RoleDefinition holds the schema definition for the RoleDefinition entity.
// A role definition is a named collection of permissions that can be bound
// to principals via RoleBinding.
type RoleDefinition struct {
	ent.Schema
}

// Fields of the RoleDefinition.
func (RoleDefinition) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty(),
		field.String("description").
			Optional(),
		field.Enum("scope_type").
			Values("system", "project"),
		field.JSON("permissions", []string{}).
			Optional(),
		field.Bool("system").
			Default(false),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the RoleDefinition.
func (RoleDefinition) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("name", "scope_type").Unique(),
	}
}

// Edges of the RoleDefinition.
func (RoleDefinition) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("role_bindings", RoleBinding.Type),
	}
}
