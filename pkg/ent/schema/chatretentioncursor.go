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
)

// ChatRetentionCursor is the resume point of one chat retention phase
// (design chat message retention §6.2, §6.6): the (created, id) of the last
// message the oldest-first walk returned, the time the cursor was last reset
// to the start, and the last cutoff the walk reached.
type ChatRetentionCursor struct {
	ent.Schema
}

// Fields of the ChatRetentionCursor.
func (ChatRetentionCursor) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("phase").
			NotEmpty().
			Immutable().
			Comment(`Retention phase, e.g. "main"`),
		field.Time("cursor_created").
			Optional().
			Nillable().
			Comment("created of the last message returned; NULL = start of the walk"),
		field.String("cursor_id").
			Default("").
			Comment("id of the last message returned (tiebreak on created)"),
		field.Time("reset_at").
			Optional().
			Nillable().
			Comment("When the cursor was last reset to the start (daily)"),
		field.Time("last_applied_cutoff").
			Optional().
			Nillable().
			Comment("Cutoff of the last tick whose walk reached it"),
		field.Time("updated_at"),
	}
}

// Annotations of the ChatRetentionCursor.
func (ChatRetentionCursor) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "chat_retention_cursors"},
	}
}
