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
)

// MaintenanceOperationRun mirrors maintenance_operation_runs (V41). FK to
// maintenance_operations(key), not operations(id).
type MaintenanceOperationRun struct {
	ent.Schema
}

func (MaintenanceOperationRun) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "maintenance_operation_runs"}}
}

func (MaintenanceOperationRun) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("operation_key").NotEmpty(),
		field.String("status").Default("running"),
		field.Time("started_at").Default(time.Now).Immutable(),
		field.Time("completed_at").Optional().Nillable(),
		field.String("started_by").Optional(),
		field.Text("result").Optional(),
		field.Text("log").Default(""),
	}
}

func (MaintenanceOperationRun) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("operation_key").StorageKey("idx_maintenance_runs_key"),
		index.Fields("started_at").
			StorageKey("idx_maintenance_runs_started").
			Annotations(entsql.Desc()),
	}
}
