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

// NotificationSubscription holds the schema definition for the
// NotificationSubscription entity, mapping the legacy SQLite
// `notification_subscriptions` table.
//
// trigger_activities is kept as a raw JSON string to stay dialect-neutral. The
// SQLite store enforced uniqueness via a COALESCE-based partial index over
// (scope, agent_id, subscriber_type, subscriber_id, project_id); that
// expression index is not dialect-neutral, so a plain composite index is
// declared here and the unique-with-NULL semantics are deferred to the port
// layer / migration.
type NotificationSubscription struct {
	ent.Schema
}

// Fields of the NotificationSubscription.
func (NotificationSubscription) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("scope").
			Default("agent"),
		field.UUID("agent_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.String("subscriber_type").
			Default("agent"),
		field.String("subscriber_id").
			NotEmpty(),
		field.UUID("project_id", uuid.UUID{}),
		field.String("trigger_activities").
			NotEmpty(),
		field.String("created_by").
			NotEmpty(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the NotificationSubscription.
func (NotificationSubscription) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("scope", "agent_id", "subscriber_type", "subscriber_id", "project_id"),
		index.Fields("project_id"),
	}
}

// Annotations of the NotificationSubscription.
func (NotificationSubscription) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "notification_subscriptions"},
	}
}
