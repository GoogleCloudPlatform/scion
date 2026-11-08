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
)

// DecisionAnswer is one answer to a decision version: the decision log
// (choices, ptone/scion#7514). Rows are append-only and kept forever. The
// question, key, version and label are snapshots, so a row reads the same
// after its decision is replaced or its prompt message is purged, and the
// answerer's name and email are snapshots kept after the user is deleted
// (answered_by_user_id is then cleared).
type DecisionAnswer struct {
	ent.Schema
}

// Fields of the DecisionAnswer.
func (DecisionAnswer) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		// decision_row_id is the decision version answered (decisions.id).
		// No foreign key: answers outlive any cleanup of the decision side.
		field.UUID("decision_row_id", uuid.UUID{}).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}).
			Immutable(),
		field.UUID("asker_agent_id", uuid.UUID{}).
			Immutable(),
		field.String("key").
			NotEmpty().
			MaxLen(messages.MaxDecisionKeyLen).
			Immutable(),
		field.Int("version").
			Positive().
			Immutable(),
		field.Text("question_text").
			Immutable(),
		field.String("choice_id").
			NotEmpty().
			MaxLen(messages.MaxDecisionKeyLen).
			Immutable(),
		field.String("choice_label").
			Immutable(),
		// answered_by_user_id is cleared when the user is deleted; the name
		// and email snapshots stay.
		field.String("answered_by_user_id").
			Optional().
			Nillable(),
		field.String("answered_by_name").
			Default("").
			Immutable(),
		field.String("answered_by_email").
			Default("").
			Immutable(),
		// provenance: direct is a user's click; agent_interpreted is an
		// answer the asking agent recorded from a user's free-text reply.
		field.Enum("provenance").
			Values("direct", "agent_interpreted").
			Immutable(),
		// outcome: confirm and override only on a direct row that follows
		// an interpreted one.
		field.Enum("outcome").
			Values("answer", "confirm", "override").
			Default("answer").
			Immutable(),
		// interpreted_by_agent_id and source_message_id are set iff the
		// provenance is agent_interpreted.
		field.UUID("interpreted_by_agent_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable(),
		field.UUID("source_message_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable(),
		// surface is where the user acted.
		field.Enum("surface").
			Values("native", "slack", "google_chat", "discord", "telegram", "teams", "other").
			Immutable(),
		// response_message_id is the choice-response message.
		field.UUID("response_message_id", uuid.UUID{}).
			Immutable(),
		// authorizing_user_id / delegation_grant_id: the same delegated-send
		// provenance as the message table.
		field.String("authorizing_user_id").
			Optional().
			Nillable().
			Immutable(),
		field.String("delegation_grant_id").
			Optional().
			Nillable().
			Immutable(),
		// idempotency_key, when set, makes a retried answer return the row
		// it already recorded (same key, choice and answerer) instead of a
		// conflict.
		field.String("idempotency_key").
			Optional().
			Nillable().
			NotEmpty().
			MaxLen(128).
			Immutable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the DecisionAnswer.
func (DecisionAnswer) Indexes() []ent.Index {
	return []ent.Index{
		// At most one interpreted and one direct answer per version; two
		// concurrent answers of one provenance have exactly one winner.
		index.Fields("decision_row_id", "provenance").
			Unique(),
		// One row per idempotency key on a version. NULL keys are distinct
		// in both SQLite and Postgres, so keyless answers never collide.
		index.Fields("decision_row_id", "idempotency_key").
			Unique(),
		// The project's decision log, newest first.
		index.Fields("project_id", "created_at"),
		// History joins choice-response messages to their answer.
		index.Fields("response_message_id"),
		// User deletion clears answered_by_user_id on the user's answers.
		index.Fields("answered_by_user_id"),
	}
}

// Annotations of the DecisionAnswer.
func (DecisionAnswer) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "decision_answers"},
	}
}
