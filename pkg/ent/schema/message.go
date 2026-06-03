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

// Message holds the schema definition for the Message entity, mapping the legacy
// SQLite `messages` table.
//
// sender_id/recipient_id/agent_id/group_id are kept as plain strings (they hold
// heterogeneous principal identifiers and defaulted to ” in SQLite), while
// project_id is a required UUID.
type Message struct {
	ent.Schema
}

// Fields of the Message.
func (Message) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}),
		field.String("sender").
			NotEmpty(),
		field.String("sender_id").
			Optional(),
		field.String("recipient").
			NotEmpty(),
		field.String("recipient_id").
			Optional(),
		field.String("msg").
			NotEmpty(),
		field.String("type").
			Default("instruction"),
		field.Bool("urgent").
			Default(false),
		field.Bool("broadcasted").
			Default(false),
		field.Bool("read").
			Default(false),
		field.String("agent_id").
			Optional(),
		field.String("group_id").
			Optional(),
		// dispatch_state tracks cross-node delivery (the message row IS the durable
		// dispatch intent): pending|dispatched|failed. The owning node CAS-flips
		// pending->dispatched after tunneling. Default keeps existing rows valid.
		field.String("dispatch_state").
			Default("pending"),
		field.Time("dispatched_at").
			Optional().
			Nillable(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the Message.
func (Message) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id"),
		index.Fields("recipient", "recipient_id"),
		index.Fields("created"),
	}
}

// Annotations of the Message.
func (Message) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "messages"},
	}
}
