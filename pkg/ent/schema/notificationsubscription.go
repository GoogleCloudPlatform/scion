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

// NotificationSubscription mirrors notification_subscriptions (V18 + V22/V31).
type NotificationSubscription struct {
	ent.Schema
}

func (NotificationSubscription) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "notification_subscriptions"}}
}

func (NotificationSubscription) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("scope").Default("agent"),
		field.String("agent_id").Optional().Nillable(),
		field.String("subscriber_type").Default("agent"),
		field.String("subscriber_id").NotEmpty(),
		field.String("grove_id").NotEmpty(),
		field.Text("trigger_activities"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.String("created_by").NotEmpty(),
	}
}

func (NotificationSubscription) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id").StorageKey("idx_notification_subs_agent"),
		index.Fields("grove_id").StorageKey("idx_notification_subs_grove"),
		index.Fields("subscriber_type", "subscriber_id").StorageKey("idx_notification_subs_subscriber"),
	}
}
