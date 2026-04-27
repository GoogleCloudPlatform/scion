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

// ScheduledEvent mirrors scheduled_events (V19 + V32 schedule_id).
type ScheduledEvent struct {
	ent.Schema
}

func (ScheduledEvent) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "scheduled_events"}}
}

func (ScheduledEvent) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("grove_id").NotEmpty(),
		field.String("event_type").NotEmpty(),
		field.Time("fire_at"),
		field.Text("payload"),
		field.String("status").Default("pending"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.String("created_by").Optional(),
		field.Time("fired_at").Optional().Nillable(),
		field.Text("error").Optional(),
		field.String("schedule_id").Default(""),
	}
}

func (ScheduledEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status").StorageKey("idx_scheduled_events_status"),
		index.Fields("fire_at").
			StorageKey("idx_scheduled_events_fire_at").
			Annotations(entsql.IndexWhere("status = 'pending'")),
		index.Fields("grove_id").StorageKey("idx_scheduled_events_grove"),
	}
}
