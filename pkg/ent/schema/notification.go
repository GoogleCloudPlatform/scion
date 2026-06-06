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
	}
}

// Annotations of the Notification.
func (Notification) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "notifications"},
	}
}
