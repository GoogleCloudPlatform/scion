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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
)

// SecretRequest is one version of a requested agent-scoped secret (ptone/scion#8747).
// It serves as both the request lifecycle state and the durable request log:
// it NEVER stores secret values, and has no foreign key to agents so rows
// outlive agent hard deletion (S5). Project deletion removes its rows.
type SecretRequest struct {
	ent.Schema
}

// Fields of the SecretRequest.
func (SecretRequest) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}).
			Immutable(),
		// agent_id is the requesting agent (the agent the fulfilled secret is
		// bound to). No FK edge: rows outlive the agent (S5).
		field.UUID("agent_id", uuid.UUID{}).
			Immutable(),
		// agent_name is a snapshot captured at send time for display after
		// agent deletion.
		field.String("agent_name").
			Immutable(),
		field.String("key").
			NotEmpty().
			MaxLen(messages.MaxSecretRequestKeyLen).
			Immutable(),
		field.String("key_norm").
			NotEmpty().
			MaxLen(messages.MaxSecretRequestKeyLen).
			Immutable(),
		field.Int("version").
			Positive().
			Immutable(),
		field.Text("description").
			Default("").
			Immutable(),
		field.Enum("secret_type").
			Values("environment", "file", "variable").
			Default("environment").
			Immutable(),
		field.String("target").
			Optional().
			Nillable().
			MaxLen(messages.MaxSecretRequestTargetLen).
			Immutable(),
		field.UUID("message_id", uuid.UUID{}).
			Immutable(),
		field.Int("position").
			NonNegative().
			Immutable(),
		// intro_length is the byte offset in the prompt message's msg where
		// the markdown fallback starts (position 0 row only; 0 means no intro).
		field.Int("intro_length").
			Default(0).
			NonNegative().
			Immutable(),
		field.UUID("conversation_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable(),
		field.String("chat_key").
			Optional().
			Nillable().
			MaxLen(256).
			Immutable(),
		field.Enum("status").
			Values("pending", "fulfilling", "fulfilled", "replaced", "withdrawn", "dismissed", "expired").
			Default("pending"),
		field.UUID("replaced_by_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Time("replaced_at").
			Optional().
			Nillable(),
		field.Text("withdrawn_reason").
			Optional().
			Nillable(),
		field.String("withdrawn_by").
			Optional().
			Nillable().
			MaxLen(32),
		field.String("resolved_by_user_id").
			Optional().
			Nillable().
			MaxLen(128),
		field.String("resolved_by_name").
			Optional().
			Nillable().
			MaxLen(256),
		field.String("resolved_by_email").
			Optional().
			Nillable().
			MaxLen(256),
		field.Text("dismiss_reason").
			Optional().
			Nillable(),
		field.Enum("surface").
			Values("native", "standalone_page").
			Optional().
			Nillable(),
		field.String("idempotency_key").
			Optional().
			Nillable().
			MaxLen(msgaction.MaxIdempotencyKeyLen),
		field.UUID("response_message_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Int("secret_version").
			Optional().
			Nillable(),
		field.Time("claimed_at").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("expires_at").
			Immutable(),
		field.Time("resolved_at").
			Optional().
			Nillable(),
	}
}

// Indexes of the SecretRequest.
func (SecretRequest) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id", "key_norm", "version").
			Unique(),
		index.Fields("agent_id", "status"),
		index.Fields("status", "expires_at"),
		index.Fields("status", "claimed_at"),
		index.Fields("message_id", "position"),
		index.Fields("project_id", "created_at"),
	}
}

// Annotations of the SecretRequest.
func (SecretRequest) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "secret_requests"},
	}
}
