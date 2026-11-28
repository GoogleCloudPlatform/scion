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

// MessagePolicyDecision holds the schema definition for content-free message
// policy evaluation records (consumer design §5.8, ptone/scion#8047).
//
// Invariant: NO message body or attachment text is ever stored on this table.
// Only message_id (NULL when a message is rejected before persistence) is kept.
// Retention is bounded at 7 days and 100k rows per project.
type MessagePolicyDecision struct {
	ent.Schema
}

// Fields of the MessagePolicyDecision.
func (MessagePolicyDecision) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}).
			Immutable(),
		field.UUID("policy_id", uuid.UUID{}).
			Immutable(),
		field.String("policy_name").
			Default("").
			MaxLen(63).
			Immutable().
			Comment("Snapshot of policy name at evaluation time."),
		field.Int64("policy_revision").
			Positive().
			Immutable(),
		field.Enum("mode").
			Values("shadow", "enforce").
			Immutable(),
		field.String("surface").
			NotEmpty().
			MaxLen(64).
			Immutable(),
		field.String("sender_kind").
			Default("").
			MaxLen(32).
			Immutable(),
		field.String("sender_id").
			Default("").
			MaxLen(128).
			Immutable(),
		field.String("recipient_kind").
			Default("").
			MaxLen(32).
			Immutable(),
		field.String("recipient_id").
			Default("").
			MaxLen(128).
			Immutable(),
		field.UUID("message_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable().
			Comment("Persisted message UUID when admitted/shadow; NULL when rejected before persistence. Never message text."),
		field.Enum("class").
			Values("pass", "violation", "uncertain", "failed").
			Immutable(),
		field.Enum("action").
			Values("admitted", "rejected", "shadow").
			Immutable(),
		field.Text("reason").
			Default("").
			Immutable().
			Comment("Rendered policy reason or content-free diagnostic; never message body text."),
		field.String("cel_error_reason").
			Default("").
			MaxLen(64).
			Immutable().
			Comment("CEL/engine failure reason code when class is failed (e.g. missing_input, budget_exhausted, cost_limit, timeout)."),
		field.Text("evaluation").
			Default("").
			Immutable().
			Comment("Optional JSON projection of systemone.Evaluation (empty in M1; content-free)."),
		field.Text("chain").
			Default("").
			Immutable().
			Comment("Optional content-free JSON provenance of rule-chain evaluation (deciding step, through, engine_class, consumer_handling, step trace)."),
		field.Int64("elapsed_ms").
			Default(0).
			NonNegative().
			Immutable(),
		field.Time("created_at").
			Default(nowUTC).
			Immutable(),
	}
}

// Edges of the MessagePolicyDecision.
func (MessagePolicyDecision) Edges() []ent.Edge {
	return nil
}

// Indexes of the MessagePolicyDecision.
func (MessagePolicyDecision) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "policy_id", "created_at", "id"),
		index.Fields("project_id", "created_at", "id"),
		index.Fields("created_at", "id"),
	}
}

// Annotations of the MessagePolicyDecision.
func (MessagePolicyDecision) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "message_policy_decisions"},
	}
}
