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

// CommandRequest is one version of an agent's proposal to run a command in a
// named human's execution container (ptone/scion#9278 §4.3).
// It has no foreign keys so rows outlive agent deletion.
type CommandRequest struct {
	ent.Schema
}

// Fields of the CommandRequest.
func (CommandRequest) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}).
			Immutable(),
		field.UUID("agent_id", uuid.UUID{}).
			Immutable(),
		field.String("agent_name").
			Immutable(),
		field.String("key").
			NotEmpty().
			MaxLen(messages.MaxCommandRequestKeyLen).
			Immutable(),
		field.String("key_norm").
			NotEmpty().
			MaxLen(messages.MaxCommandRequestKeyLen).
			Immutable(),
		field.Int("version").
			Positive().
			Immutable(),
		field.Text("description").
			Default("").
			Immutable(),
		field.Text("script").
			NotEmpty().
			Immutable(),
		field.String("workdir").
			Default("").
			MaxLen(messages.MaxCommandRequestWorkdirLen).
			Immutable(),
		field.Int("timeout_s").
			Positive().
			Immutable(),
		field.Bool("share_output_requested").
			Default(false).
			Immutable(),
		field.String("command_sha256").
			NotEmpty().
			MaxLen(64).
			Immutable(),
		field.String("approver_user_id").
			NotEmpty().
			MaxLen(128).
			Immutable(),
		field.UUID("message_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable(),
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
			Values("pending", "approved", "replaced", "withdrawn", "dismissed", "expired").
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
		field.Text("dismiss_reason").
			Optional().
			Nillable(),
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
		field.Time("resolved_at").
			Optional().
			Nillable(),
		field.Enum("surface").
			Values("native", "standalone_page", "cli", "markdown_block").
			Optional().
			Nillable(),
		field.String("idempotency_key").
			Optional().
			Nillable().
			MaxLen(msgaction.MaxIdempotencyKeyLen),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("expires_at").
			Immutable(),
	}
}

// Indexes of the CommandRequest.
func (CommandRequest) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id", "key_norm", "version").
			Unique(),
		index.Fields("agent_id", "status"),
		index.Fields("status", "expires_at"),
		index.Fields("message_id"),
		index.Fields("project_id", "created_at"),
		index.Fields("approver_user_id", "resolved_at"),
		index.Fields("approver_user_id", "status", "expires_at"),
	}
}

// Annotations of the CommandRequest.
func (CommandRequest) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "command_requests"},
	}
}
