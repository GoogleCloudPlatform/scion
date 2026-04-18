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

// Notification mirrors the notifications raw SQL table (V18).
type Notification struct {
	ent.Schema
}

func (Notification) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "notifications"}}
}

func (Notification) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("subscription_id").NotEmpty(),
		field.String("agent_id").NotEmpty(),
		field.String("grove_id").NotEmpty(),
		field.String("subscriber_type").NotEmpty(),
		field.String("subscriber_id").NotEmpty(),
		field.String("status").NotEmpty(),
		field.Text("message"),
		field.Bool("dispatched").Default(false),
		field.Bool("acknowledged").Default(false),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (Notification) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("subscriber_type", "subscriber_id").StorageKey("idx_notifications_subscriber"),
		index.Fields("grove_id").StorageKey("idx_notifications_grove"),
	}
}
