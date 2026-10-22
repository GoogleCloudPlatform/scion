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

// AgentLifecycleOutbox is the bounded, drained operational outbox of
// serialized agent.lifecycle events (WP1 design 4, 5.3). A row is inserted
// in the transaction that commits the transition and deleted once Cloud
// Logging acknowledges it, or dropped (and counted) by the age, row-count
// and poison bounds. It is never a queryable history.
//
// The primary key is the auto-increment seq (SQLite allows AUTOINCREMENT
// only on an INTEGER PRIMARY KEY); event_id is unique. agent_id has no
// foreign key, so a deletion's event outlives the agent row.
type AgentLifecycleOutbox struct {
	ent.Schema
}

// Fields of the AgentLifecycleOutbox.
func (AgentLifecycleOutbox) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id").
			StorageKey("seq"),
		field.String("event_id").
			Unique().
			Immutable(),
		field.UUID("agent_id", uuid.UUID{}).
			Immutable(),
		field.Int64("transition_revision").
			Immutable(),
		// payload is the exact serialized event; retries send these bytes.
		field.Text("payload").
			Immutable(),
		field.Time("created_at").
			Immutable(),
		field.Int("attempts").
			Default(0),
		// permanent_failures counts only failures that are permanent for
		// this payload; transient and sink-blocked failures never count.
		field.Int("permanent_failures").
			Default(0),
		field.Time("next_attempt_at"),
		field.String("lease_owner").
			Default(""),
		field.Time("lease_until").
			Optional().
			Nillable(),
		field.String("last_error").
			Default("").
			MaxLen(256),
	}
}

// Indexes of the AgentLifecycleOutbox.
func (AgentLifecycleOutbox) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id"),
		index.Fields("next_attempt_at"),
		index.Fields("created_at"),
	}
}

// Annotations of the AgentLifecycleOutbox.
func (AgentLifecycleOutbox) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "agent_lifecycle_outbox"},
	}
}
