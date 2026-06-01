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
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// SubscriptionTemplate holds the schema definition for the SubscriptionTemplate
// entity, mapping the legacy SQLite `subscription_templates` table.
//
// project_id is nullable (the SQLite column defaulted to ” for global-scoped
// templates); the (project_id, name) uniqueness is enforced via a unique index.
type SubscriptionTemplate struct {
	ent.Schema
}

// Fields of the SubscriptionTemplate.
func (SubscriptionTemplate) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty(),
		field.String("scope").
			Default("project"),
		field.String("trigger_activities").
			NotEmpty(),
		field.UUID("project_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.String("created_by").
			NotEmpty(),
	}
}

// Indexes of the SubscriptionTemplate.
func (SubscriptionTemplate) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "name").
			Unique(),
	}
}

// Annotations of the SubscriptionTemplate.
func (SubscriptionTemplate) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "subscription_templates"},
	}
}
