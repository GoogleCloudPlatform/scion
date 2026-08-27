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

// ConversationParticipant holds the schema definition for the
// ConversationParticipant entity. It is a through-table linking principals
// (users or agents) to conversations, with role and temporal metadata.
//
// conversation_id is modeled as a plain UUID column rather than an Ent edge
// to keep this schema independent of existing entities; edge wiring is deferred
// to a later phase.
type ConversationParticipant struct {
	ent.Schema
}

// Fields of the ConversationParticipant.
func (ConversationParticipant) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("conversation_id", uuid.UUID{}),
		field.Enum("principal_kind").
			Values("user", "agent"),
		field.String("principal_id").
			NotEmpty(),
		field.Enum("role").
			Values("member", "observer").
			Default("member"),
		field.Time("joined_at").
			Default(time.Now).
			Immutable(),
		field.Time("left_at").
			Optional().
			Nillable(),
	}
}

// Indexes of the ConversationParticipant.
func (ConversationParticipant) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("conversation_id", "principal_kind", "principal_id").
			Unique(),
		index.Fields("principal_id"),
	}
}

// Annotations of the ConversationParticipant.
func (ConversationParticipant) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "conversation_participants"},
	}
}
