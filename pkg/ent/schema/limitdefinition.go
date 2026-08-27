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

// LimitDefinition holds the schema definition for the LimitDefinition entity.
// A limit definition describes a configurable resource quota that can be
// bound to subjects via EntitlementBinding.
type LimitDefinition struct {
	ent.Schema
}

// Fields of the LimitDefinition.
func (LimitDefinition) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty().
			Unique(),
		field.String("resource_type").
			NotEmpty(),
		field.String("unit").
			Default("count"),
		field.String("description").
			Optional().
			Default(""),
		field.Int64("default_value").
			Default(0),
		field.Bool("system").
			Default(false).
			Immutable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the LimitDefinition.
func (LimitDefinition) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("resource_type"),
	}
}

// Edges of the LimitDefinition.
func (LimitDefinition) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("entitlement_bindings", EntitlementBinding.Type),
		edge.To("usage_reservations", UsageReservation.Type),
	}
}
