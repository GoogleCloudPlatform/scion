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

// Schedule mirrors the schedules raw SQL table (V32).
type Schedule struct {
	ent.Schema
}

func (Schedule) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "schedules"}}
}

func (Schedule) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("grove_id").NotEmpty(),
		field.String("name").NotEmpty(),
		field.String("cron_expr").NotEmpty(),
		field.String("event_type").NotEmpty(),
		field.Text("payload").Default("{}"),
		field.String("status").Default("active"),
		field.Time("next_run_at").Optional().Nillable(),
		field.Time("last_run_at").Optional().Nillable(),
		field.String("last_run_status").Optional(),
		field.Text("last_run_error").Optional(),
		field.Int("run_count").Default(0),
		field.Int("error_count").Default(0),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.String("created_by").Optional(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Schedule) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("grove_id", "name").Unique(),
		index.Fields("grove_id").StorageKey("idx_schedules_grove"),
		index.Fields("next_run_at").
			StorageKey("idx_schedules_next_run").
			Annotations(entsql.IndexWhere("status = 'active'")),
	}
}
