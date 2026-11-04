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

// Notification holds the schema definition for the Notification entity, mapping
// the legacy SQLite `notifications` table.
//
// Foreign keys (subscription_id, agent_id, project_id) are modeled as plain
// UUID columns rather than Ent edges to keep this periphery schema independent;
// edge wiring is deferred to a later pass.
type Notification struct {
	ent.Schema
}

// Fields of the Notification.
func (Notification) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("subscription_id", uuid.UUID{}),
		field.UUID("agent_id", uuid.UUID{}),
		field.UUID("project_id", uuid.UUID{}),
		field.String("subscriber_type").
			NotEmpty(),
		field.String("subscriber_id").
			NotEmpty(),
		field.String("status").
			NotEmpty(),
		field.String("message").
			NotEmpty(),
		field.Bool("dispatched").
			Default(false),
		field.Bool("acknowledged").
			Default(false),
		// dispatch_attempts counts delivery attempts made for an
		// agent-targeted notification (primary dispatch and sweep or
		// broker-connect retries). Rows written before the column existed
		// read 0: their attempts were not recorded.
		field.Int("dispatch_attempts").
			NonNegative().
			Default(0),
		// dispatch_outcome is the terminal delivery outcome: "delivered" or
		// "failed" (given up; see store.NotificationOutcomeFailed). Empty
		// means no outcome is recorded: the notification is pending or in
		// flight, or the row predates the column, in which case the
		// dispatched flag keeps its old meaning.
		field.String("dispatch_outcome").
			Default(""),
		// dispatch_reason says why a "failed" outcome was recorded (see
		// the store.NotificationReason* constants). Empty for any other
		// outcome and for rows that predate the column.
		field.String("dispatch_reason").
			Default(""),
		// next_attempt_at is the earliest time the dispatch sweep may
		// claim the notification again after a failed delivery (capped
		// exponential backoff). NULL means at once.
		field.Time("next_attempt_at").
			Optional().
			Nillable(),
		// kind says what produced the notification (a
		// store.NotificationKind* constant). Empty means not classified:
		// every row written before the column existed, and every row from
		// writers that do not set it. Empty does not mean "agent-scoped":
		// a GC must select rows by agent_id or subscriber_type, never by
		// kind.
		field.String("kind").
			Default(""),
		// resource_type and resource_id name the thing the notification
		// is about (for example store.NotificationResourceArtifact and an
		// artifact id). Both are empty when the notification is not about
		// a resource.
		field.String("resource_type").
			Default(""),
		field.String("resource_id").
			Default(""),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the Notification.
func (Notification) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("subscription_id"),
		index.Fields("project_id", "subscriber_type", "subscriber_id"),
		// Partial index for the undispatched-notification sweep: covers
		// queries filtering on subscriber_type + created WHERE dispatched = false.
		index.Fields("subscriber_type", "created").
			Annotations(entsql.IndexWhere("dispatched = false")),
	}
}

// Annotations of the Notification.
func (Notification) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "notifications"},
	}
}
