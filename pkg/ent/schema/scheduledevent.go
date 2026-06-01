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

// ScheduledEvent holds the schema definition for the ScheduledEvent entity,
// mapping the legacy SQLite `scheduled_events` table.
//
// payload is a raw JSON string. The SQLite store used a partial index on fire_at
// (WHERE status='pending'); a plain index is declared here to stay
// dialect-neutral. schedule_id is an optional back-reference string (defaulted
// to ” in SQLite).
type ScheduledEvent struct {
	ent.Schema
}

// Fields of the ScheduledEvent.
func (ScheduledEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}),
		field.String("event_type").
			NotEmpty(),
		field.Time("fire_at"),
		field.String("payload").
			NotEmpty(),
		field.String("status").
			Default("pending"),
		field.String("created_by").
			Optional(),
		field.Time("fired_at").
			Optional().
			Nillable(),
		field.String("error").
			Optional(),
		field.String("schedule_id").
			Optional(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the ScheduledEvent.
func (ScheduledEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("fire_at"),
		index.Fields("project_id"),
		index.Fields("status"),
	}
}

// Annotations of the ScheduledEvent.
func (ScheduledEvent) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "scheduled_events"},
	}
}
