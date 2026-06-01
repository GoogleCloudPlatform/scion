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

// Schedule holds the schema definition for the Schedule entity, mapping the
// legacy SQLite `schedules` table.
//
// payload is a raw JSON string. The SQLite store used a partial index on
// next_run_at (WHERE status='active'); a plain index is declared here to stay
// dialect-neutral.
type Schedule struct {
	ent.Schema
}

// Fields of the Schedule.
func (Schedule) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}),
		field.String("name").
			NotEmpty(),
		field.String("cron_expr").
			NotEmpty(),
		field.String("event_type").
			NotEmpty(),
		field.String("payload").
			Default("{}"),
		field.String("status").
			Default("active"),
		field.Time("next_run_at").
			Optional().
			Nillable(),
		field.Time("last_run_at").
			Optional().
			Nillable(),
		field.String("last_run_status").
			Optional(),
		field.String("last_run_error").
			Optional(),
		field.Int("run_count").
			Default(0),
		field.Int("error_count").
			Default(0),
		field.String("created_by").
			Optional(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the Schedule.
func (Schedule) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "name").
			Unique(),
		index.Fields("next_run_at"),
	}
}

// Annotations of the Schedule.
func (Schedule) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "schedules"},
	}
}
