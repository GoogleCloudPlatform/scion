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

// Message holds the schema definition for the Message entity, mapping the legacy
// SQLite `messages` table.
//
// sender_id/recipient_id/agent_id/group_id are kept as plain strings (they hold
// heterogeneous principal identifiers and defaulted to ” in SQLite), while
// project_id is a required UUID.
type Message struct {
	ent.Schema
}

// Fields of the Message.
func (Message) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}),
		field.String("sender").
			NotEmpty(),
		field.String("sender_id").
			Optional(),
		field.String("recipient").
			NotEmpty(),
		field.String("recipient_id").
			Optional(),
		// msg may be empty: an attachment-only chat message has no text.
		// Send handlers enforce their own body rules.
		field.String("msg"),
		field.String("type").
			Default("instruction"),
		field.Bool("urgent").
			Default(false),
		field.Bool("broadcasted").
			Default(false),
		field.Bool("read").
			Default(false),
		field.String("agent_id").
			Optional(),
		field.String("group_id").
			Optional(),
		// dispatch_state tracks cross-node delivery:
		// pending|dispatched|failed|deferred|no_recipient (see the
		// store.MessageDispatch* constants).
		// After Phase 4 (no-queuing delivery), every writer sets the state
		// explicitly. Only ExecuteAgentDM writes "pending", for the length of its
		// dispatch; a row stays pending only after a deadline-ambiguous dispatch,
		// by design (AC-4 of #1689), and brokerMessageSweepHandler reports and
		// expires it. The schema default applies only to a writer that leaves
		// the state empty, which the store logs (ptone/scion#4827).
		field.String("dispatch_state").
			Default("pending"),
		field.String("dispatch_failure_reason").
			Optional().
			Nillable(),
		field.Time("dispatched_at").
			Optional().
			Nillable(),
		// channel identifies the integration that originated or targets
		// this message (e.g. "web", "discord", "telegram").
		field.String("channel").
			Optional().
			MaxLen(64),
		// thread_id groups messages into a conversation thread. By
		// convention, web-originated messages use "agent:<agentID>".
		field.String("thread_id").
			Optional().
			MaxLen(256),
		// conversation_id links this message to a Conversation record.
		// Populated by backfill (Phase 4) and dual-write (Phase 5).
		field.UUID("conversation_id", uuid.UUID{}).
			Optional().
			Nillable(),
		// sender_project_id / recipient_project_id carry server-derived
		// cross-project provenance (store.Message.SenderProjectID /
		// RecipientProjectID). Nullable: human senders and legacy rows have
		// no project-level provenance. Stamped at admission and never
		// updated afterwards.
		field.UUID("sender_project_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable(),
		field.UUID("recipient_project_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable(),
		// authorizing_user_id / delegation_grant_id attribute a message
		// sent with an agent delegated credential
		// (.design/agent-delegation.md §14.5): the grant issuer and the
		// grant. The sender columns name the actor agent. Nullable: every
		// other message has neither. Stamped at admission and never
		// updated afterwards.
		field.String("authorizing_user_id").
			Optional().
			Nillable().
			Immutable(),
		field.String("delegation_grant_id").
			Optional().
			Nillable().
			Immutable(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the Message.
func (Message) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id"),
		index.Fields("recipient", "recipient_id"),
		index.Fields("created"),
		index.Fields("conversation_id"),
		// Native chat tail and history reads must not sort the entire DM on
		// every unread refresh. Both read-switch paths order by created, id.
		index.Fields("conversation_id", "channel", "created", "id"),
		index.Fields("thread_id", "channel", "created", "id"),
		// Project-scoped pages (the conversation backfill, project message
		// listings) filter on project_id and order by created, id with a
		// (created, id) keyset cursor. Without this index every page reads
		// and sorts all of the project's messages.
		index.Fields("project_id", "created", "id"),
		// Participant reads (the agent DM inter-agent view, a user's message
		// listing) filter on recipient_id = X OR sender_id = X across all
		// projects. ListMessages reads each branch as its own query, and the
		// trailing created, id serves its ORDER BY ... LIMIT in index order,
		// so a page reads at most limit+1 rows per branch however busy the
		// participant is. The COUNT of the OR uses both indexes (a
		// multi-index OR on SQLite) instead of scanning the whole table.
		index.Fields("sender_id", "created", "id"),
		index.Fields("recipient_id", "created", "id"),
		// The inter-agent view's legacy lookup filters on project_id and
		// the sender text ("agent:<slug>") and orders by created, id.
		index.Fields("project_id", "sender", "created", "id"),
	}
}

// Annotations of the Message.
func (Message) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "messages"},
	}
}
