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
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// MaintenanceOperation holds the schema definition for the MaintenanceOperation
// entity, mapping the legacy SQLite `maintenance_operations` table.
//
// `key` is a stable unique business key. Seed rows that SQLite created via
// hex(randomblob(...)) move to Go-side seeding (out of scope for this schema).
type MaintenanceOperation struct {
	ent.Schema
}

// Fields of the MaintenanceOperation.
func (MaintenanceOperation) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("key").
			Unique().
			NotEmpty(),
		field.String("title").
			NotEmpty(),
		field.String("description").
			Default(""),
		field.String("category").
			NotEmpty(),
		field.String("status").
			Default("pending"),
		field.Time("started_at").
			Optional().
			Nillable(),
		field.Time("completed_at").
			Optional().
			Nillable(),
		field.String("started_by").
			Optional(),
		field.String("result").
			Optional(),
		field.String("metadata").
			Default("{}"),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Annotations of the MaintenanceOperation.
func (MaintenanceOperation) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "maintenance_operations"},
	}
}
