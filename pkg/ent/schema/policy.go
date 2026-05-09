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

// AccessPolicy holds the schema definition for the AccessPolicy entity.
// Named AccessPolicy (rather than Policy) to avoid conflict with the
// Ent predeclared "Policy" identifier.
type AccessPolicy struct {
	ent.Schema
}

// Fields of the AccessPolicy.
func (AccessPolicy) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty(),
		field.String("description").
			Optional(),
		field.Enum("scope_type").
			Values("hub", "project", "resource"),
		field.String("scope_id").
			Optional(),
		field.String("resource_type").
			NotEmpty(),
		field.String("resource_id").
			Optional(),
		field.JSON("actions", []string{}).
			Optional(),
		field.Enum("effect").
			Values("allow", "deny"),
		field.JSON("conditions", &PolicyConditions{}).
			Optional(),
		field.Int("priority").
			Default(0),
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
	}
}

// Edges of the AccessPolicy.
func (AccessPolicy) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("bindings", PolicyBinding.Type),
	}
}
