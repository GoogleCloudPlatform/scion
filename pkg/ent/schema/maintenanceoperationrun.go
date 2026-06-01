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
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// MaintenanceOperationRun holds the schema definition for the
// MaintenanceOperationRun entity, mapping the legacy SQLite
// `maintenance_operation_runs` table.
//
// operation_key references maintenance_operations(key) — a non-id unique
// column — so it is modeled as a plain string rather than an Ent edge (which
// binds to id). The FK is reconstructed at the port/migration layer.
type MaintenanceOperationRun struct {
	ent.Schema
}

// Fields of the MaintenanceOperationRun.
func (MaintenanceOperationRun) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("operation_key").
			NotEmpty(),
		field.String("status").
			Default("running"),
		field.Time("started_at").
			Default(time.Now).
			Immutable(),
		field.Time("completed_at").
			Optional().
			Nillable(),
		field.String("started_by").
			Optional(),
		field.String("result").
			Optional(),
		field.String("log").
			Default(""),
	}
}

// Indexes of the MaintenanceOperationRun.
func (MaintenanceOperationRun) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("operation_key"),
	}
}

// Annotations of the MaintenanceOperationRun.
func (MaintenanceOperationRun) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "maintenance_operation_runs"},
	}
}
