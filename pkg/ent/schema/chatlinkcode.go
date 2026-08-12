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

// ChatLinkCode stores pending and confirmed chat account-link codes in the
// database so they survive across Hub instances. This replaces the per-instance
// in-memory maps that caused ~50% failure rates at min-instances=2.
type ChatLinkCode struct {
	ent.Schema
}

// Fields of the ChatLinkCode.
func (ChatLinkCode) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("code_hash").
			Sensitive().
			Unique().
			NotEmpty().
			Comment("SHA-256 hash of the plaintext link code"),
		field.String("user_identifier").
			NotEmpty().
			Comment("Platform-specific user ID (Telegram user ID, Discord user ID, Teams AAD object ID)"),
		field.Enum("provider").
			Values("telegram", "discord", "teams").
			Comment("Chat provider: telegram, discord, or teams"),
		field.Enum("status").
			Default("pending").
			Values("pending", "confirmed").
			Comment("Link status: pending or confirmed"),
		field.String("user_id").
			Optional().
			Nillable().
			Comment("Scion user ID, set when a logged-in user confirms the code"),
		field.String("user_email").
			Optional().
			Nillable().
			Comment("Scion user email, set when a logged-in user confirms the code"),
		field.Time("expires_at").
			Comment("When this link code expires"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the ChatLinkCode.
func (ChatLinkCode) Indexes() []ent.Index {
	return []ent.Index{
		// Look up by provider + user_identifier (to remove old codes for same user).
		index.Fields("provider", "user_identifier"),
		// Efficient TTL eviction of expired entries.
		index.Fields("expires_at"),
	}
}

// Annotations of the ChatLinkCode.
func (ChatLinkCode) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "chat_link_codes"},
	}
}
