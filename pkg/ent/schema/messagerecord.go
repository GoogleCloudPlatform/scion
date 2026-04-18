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

// MessageRecord mirrors the messages raw SQL table (V39). Named
// "MessageRecord" rather than "Message" to avoid collision with Ent
// internals (Ent's mutation framework uses Message as a conventional name).
type MessageRecord struct {
	ent.Schema
}

func (MessageRecord) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "messages"}}
}

func (MessageRecord) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("grove_id").NotEmpty(),
		field.String("sender").NotEmpty(),
		field.String("sender_id").Default(""),
		field.String("recipient").NotEmpty(),
		field.String("recipient_id").Default(""),
		field.Text("msg"),
		field.String("type").Default("instruction"),
		field.Bool("urgent").Default(false),
		field.Bool("broadcasted").Default(false),
		field.Bool("read").Default(false),
		field.String("agent_id").Default(""),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (MessageRecord) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("grove_id").StorageKey("idx_messages_grove"),
		index.Fields("recipient_id", "read").StorageKey("idx_messages_recipient"),
		index.Fields("agent_id").StorageKey("idx_messages_agent"),
		index.Fields("sender_id").StorageKey("idx_messages_sender"),
		index.Fields("created_at").
			StorageKey("idx_messages_created").
			Annotations(entsql.Desc()),
	}
}
