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

// DiscordPendingLink holds the schema definition for the DiscordPendingLink
// entity. It stores pending Discord account link codes so that a code
// registered on one hub instance can be verified on another.
type DiscordPendingLink struct {
	ent.Schema
}

// Fields of the DiscordPendingLink.
func (DiscordPendingLink) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("code").
			Unique().
			NotEmpty(),
		field.String("discord_user_id").
			NotEmpty(),
		field.String("status").
			Default("pending"),
		field.String("user_id").
			Default(""),
		field.String("user_email").
			Default(""),
		field.Time("expires_at"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the DiscordPendingLink.
func (DiscordPendingLink) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("expires_at"),
		index.Fields("discord_user_id"),
	}
}

// Annotations of the DiscordPendingLink.
func (DiscordPendingLink) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "discord_pending_links"},
	}
}
