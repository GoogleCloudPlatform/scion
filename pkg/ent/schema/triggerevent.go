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

// TriggerEvent holds the durable inbox record of an accepted trigger event
// (ptone/scion#7961, Stage 1 §4.3). Rows carry no foreign keys to endpoints or
// triggers so history outlives deleted definitions until retention purge.
type TriggerEvent struct {
	ent.Schema
}

// Fields of the TriggerEvent.
func (TriggerEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}).
			Immutable(),
		field.String("source_kind").
			NotEmpty().
			Immutable(),
		field.String("source_id").
			NotEmpty().
			Immutable(),
		field.String("type").
			NotEmpty().
			Immutable(),
		field.String("subject").
			NotEmpty().
			Immutable(),
		field.String("dedup_key").
			NotEmpty().
			Immutable(),
		field.String("body_sha256").
			Default("").
			Immutable(),
		field.Time("occurred_at").
			Immutable(),
		field.Time("received_at").
			Immutable(),
		field.Text("attributes").
			Default("{}").
			Immutable(),
		field.Text("lists").
			Default("{}").
			Immutable(),
		field.Text("headers").
			Default("{}").
			Immutable(),
		field.Text("payload").
			Default("").
			Immutable(),
		field.Int("payload_bytes").
			Default(0).
			Immutable(),
		field.String("status").
			Default("pending"),
		field.Int("match_count").
			Default(0),
		field.Int("fire_count").
			Default(0),
		field.String("error").
			Optional(),
		field.Time("processed_at").
			Optional().
			Nillable(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the TriggerEvent.
func (TriggerEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "received_at"),
		index.Fields("source_kind", "source_id", "dedup_key").
			Unique(),
		// Partial unique index (Stage 3 rev 1.8 §4.3/§4.4): deduplicates raw
		// signed body bytes per (project_id, source_kind) when body_sha256 is
		// populated, closing fresh-GUID replay on unsigned X-GitHub-Delivery.
		index.Fields("project_id", "source_kind", "body_sha256").
			Unique().
			Annotations(
				entsql.IndexWhere("body_sha256 <> ''"),
			),
		index.Fields("status", "created"),
	}
}

// Annotations of the TriggerEvent.
func (TriggerEvent) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "trigger_events"},
	}
}
