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

// Decision is one posted version of a decision an agent asked in a choice
// prompt (choices, ptone/scion#7514). A decision is identified by
// (asker_agent_id, key); each repost of the key inserts the next version
// and marks the active one replaced. Rows are append-only apart from the
// status columns and are kept forever: chat retention does not touch them,
// and there is no foreign key to the message, so a purged prompt leaves its
// decisions in place.
type Decision struct {
	ent.Schema
}

// Fields of the Decision.
func (Decision) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}).
			Immutable(),
		// conversation_id is the prompt's conversation; nil for a prompt
		// stored without one.
		field.UUID("conversation_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable(),
		// message_id is the choice-prompt message that carried this version.
		field.UUID("message_id", uuid.UUID{}).
			Immutable(),
		// position is the order of the decision within its message.
		field.Int("position").
			NonNegative().
			Immutable(),
		field.UUID("asker_agent_id", uuid.UUID{}).
			Immutable(),
		// asker_agent_name is a snapshot, for display after the agent is
		// deleted.
		field.String("asker_agent_name").
			Immutable(),
		// key is the decision key as first given; key_norm its lower-cased
		// form, which uniqueness uses.
		field.String("key").
			NotEmpty().
			MaxLen(messages.MaxDecisionKeyLen).
			Immutable(),
		field.String("key_norm").
			NotEmpty().
			MaxLen(messages.MaxDecisionKeyLen).
			Immutable(),
		// key_source tells an agent-chosen key from a hub-assigned number.
		field.Enum("key_source").
			Values("agent", "hub").
			Immutable(),
		field.Int("version").
			Positive().
			Immutable(),
		// question is markdown.
		field.Text("question").
			Immutable(),
		// choices is the canonical [{id, label, description?}].
		field.JSON("choices", []messages.Choice{}).
			Immutable(),
		field.String("default_choice_id").
			Optional().
			Nillable().
			Immutable(),
		// intro_length is the byte offset in the prompt message's msg where
		// the markdown fallback starts (messages.RenderChoiceFallback). It is
		// set on the message's position 0 row only; 0 means no intro.
		field.Int("intro_length").
			Default(0).
			NonNegative().
			Immutable(),
		field.Enum("status").
			Values("pending", "decided", "replaced").
			Default("pending"),
		// replaced_by_id is the row of the version that replaced this one.
		field.UUID("replaced_by_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("decided_at").
			Optional().
			Nillable(),
		field.Time("replaced_at").
			Optional().
			Nillable(),
	}
}

// Indexes of the Decision.
func (Decision) Indexes() []ent.Index {
	return []ent.Index{
		// One row per version of a key. Concurrent reposts of one key
		// serialize on it: the loser retries once.
		index.Fields("asker_agent_id", "key_norm", "version").
			Unique(),
		// The pending cap counts an agent's pending rows; the agent's
		// decision list filters on its status.
		index.Fields("asker_agent_id", "status"),
		// The project's pending decisions.
		index.Fields("project_id", "status"),
		// History pages load a page's decisions by message.
		index.Fields("message_id", "position"),
		index.Fields("conversation_id"),
	}
}

// Annotations of the Decision.
func (Decision) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "decisions"},
	}
}
