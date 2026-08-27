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
	"github.com/google/uuid"
)

// MessageAddressee holds the schema definition for the MessageAddressee entity.
// Each row records one principal (user or agent) that a message is addressed to,
// how the addressing was resolved (explicit mention, body mention, default agent,
// etc.), and the current delivery state.
//
// message_id is modeled as a plain UUID column rather than an Ent edge to keep
// this schema independent of existing entities; edge wiring is deferred to a
// later phase.
type MessageAddressee struct {
	ent.Schema
}

// Fields of the MessageAddressee.
func (MessageAddressee) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("message_id", uuid.UUID{}),
		field.Enum("principal_kind").
			Values("user", "agent"),
		field.String("principal_id").
			NotEmpty(),
		field.Enum("via").
			Values("explicit", "body-mention", "default-agent", "direct"),
		field.Enum("delivery_state").
			Values("pending", "delivered", "failed").
			Default("pending"),
		field.String("failure_reason").
			Optional().
			Nillable(),
	}
}

// Indexes of the MessageAddressee.
func (MessageAddressee) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("message_id", "principal_kind", "principal_id").
			Unique(),
		index.Fields("message_id"),
		index.Fields("principal_kind", "principal_id", "delivery_state"),
	}
}

// Annotations of the MessageAddressee.
func (MessageAddressee) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "message_addressees"},
	}
}
