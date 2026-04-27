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

// MaintenanceOperation mirrors maintenance_operations (V41).
type MaintenanceOperation struct {
	ent.Schema
}

func (MaintenanceOperation) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "maintenance_operations"}}
}

func (MaintenanceOperation) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("key").Unique().NotEmpty(),
		field.String("title").NotEmpty(),
		field.Text("description").Default(""),
		field.String("category").NotEmpty(),
		field.String("status").Default("pending"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("started_at").Optional().Nillable(),
		field.Time("completed_at").Optional().Nillable(),
		field.String("started_by").Optional(),
		field.Text("result").Optional(),
		field.Text("metadata").Default("{}"),
	}
}

func (MaintenanceOperation) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("category").StorageKey("idx_maintenance_ops_category"),
		index.Fields("status").StorageKey("idx_maintenance_ops_status"),
	}
}
