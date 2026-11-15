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
)

// DurableQueueItem is one item in the generic durable delivery queue
// (pkg/durablequeue). Rows are inserted inside the producer's transaction,
// leased and processed by a per-kind drainer, and deleted on completion or
// dropped by age, row-count, or poison bounds.
//
// The primary key is the auto-increment seq (SQLite allows AUTOINCREMENT
// only on an INTEGER PRIMARY KEY); (kind, dedup_key) is unique. Rows carry
// no foreign keys so queued items outlive the resources they reference.
type DurableQueueItem struct {
	ent.Schema
}

// Fields of the DurableQueueItem.
func (DurableQueueItem) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id").
			StorageKey("seq"),
		field.String("kind").
			NotEmpty().
			MaxLen(64).
			Immutable(),
		field.String("dedup_key").
			NotEmpty().
			MaxLen(256).
			Immutable(),
		field.String("project_id").
			Default("").
			Immutable(),
		field.Text("payload").
			Immutable(),
		field.Time("not_before").
			Immutable(),
		field.Time("created_at").
			Immutable(),
		field.Int("attempts").
			Default(0),
		field.Int("permanent_failures").
			Default(0),
		field.Time("next_attempt_at"),
		field.String("lease_owner").
			Default(""),
		field.Time("lease_until").
			Optional().
			Nillable(),
		field.String("last_error").
			Default("").
			MaxLen(256),
	}
}

// Indexes of the DurableQueueItem.
func (DurableQueueItem) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("kind", "dedup_key").
			Unique(),
		index.Fields("kind", "next_attempt_at", "id"),
		index.Fields("kind", "project_id", "id"),
		index.Fields("kind", "created_at"),
		index.Fields("project_id"),
	}
}

// Annotations of the DurableQueueItem.
func (DurableQueueItem) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "durable_queue_items"},
	}
}
