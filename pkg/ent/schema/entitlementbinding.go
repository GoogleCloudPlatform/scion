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

// EntitlementBinding holds the schema definition for the EntitlementBinding entity.
// An entitlement binding associates a limit definition with a subject (user,
// group, or system default), optionally scoped to a project.
type EntitlementBinding struct {
	ent.Schema
}

// Fields of the EntitlementBinding.
func (EntitlementBinding) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("limit_definition_id", uuid.UUID{}).
			Comment("FK to LimitDefinition"),
		field.Enum("subject_type").
			Values("user", "group", "system_default"),
		field.String("subject_id").
			Default(""),
		field.Enum("scope_type").
			Values("system", "project"),
		field.String("scope_id").
			Default(""),
		field.Int64("value"),
		field.String("created_by").
			Optional().
			Default("").
			Immutable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the EntitlementBinding.
func (EntitlementBinding) Indexes() []ent.Index {
	return []ent.Index{
		// Unique constraint: one binding per (limit, subject, scope).
		index.Fields("limit_definition_id", "subject_type", "subject_id", "scope_type", "scope_id").
			Unique(),
	}
}

// Edges of the EntitlementBinding.
func (EntitlementBinding) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("limit_definition", LimitDefinition.Type).
			Ref("entitlement_bindings").
			Field("limit_definition_id").
			Required().
			Unique(),
	}
}
