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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AgentReincarnation holds the schema definition for the AgentReincarnation
// entity: the history and durable record of `scion reincarnate` requests
// (design /scion-volumes/scratchpad/projects/agent-migrate/design.md §3.2,
// ptone/scion#1821). One row per reincarnation attempt (including a
// `--rollback`, which is its own row per the design). agent_id is a plain
// field rather than an ent edge, matching the AgentCredential/
// AgentSessionMetrics precedent (composite.go's DeleteAgent cascade-deletes
// these rows explicitly rather than relying on a DB-level FK).
type AgentReincarnation struct {
	ent.Schema
}

// Fields of the AgentReincarnation.
func (AgentReincarnation) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("agent_id").
			NotEmpty().
			Immutable(),
		field.Int("from_generation").
			Immutable(),
		field.Int("to_generation").
			Immutable(),
		// requested_by is a principal ID (user or agent), matching the
		// polymorphic created_by/owner_id convention on Agent itself — no FK
		// edge, since the requester may not be a user row.
		field.String("requested_by").
			Optional(),
		field.Time("requested_at").
			Default(time.Now).
			Immutable(),
		field.Time("completed_at").
			Optional().
			Nillable(),
		field.Enum("state").
			Values("pending", "stopping", "provisioning", "starting", "completed", "failed").
			Default("pending"),
		field.String("error").
			Optional(),
		// previous_applied_config and new_applied_config are full
		// store.AgentAppliedConfig JSON snapshots, stored as text for the same
		// reason agents.applied_config is: keeping the Ent schema decoupled
		// from the store package's struct definition. previous_applied_config
		// is the rollback source (design §3.7).
		field.Text("previous_applied_config").
			Optional(),
		field.Text("new_applied_config").
			Optional(),
		// handoff is the agent-authored handoff text delivered to the new
		// generation's first task, kept here (not on a shared volume) so
		// migration works on every runtime (design §3.2).
		field.Text("handoff").
			Optional(),
	}
}

// Indexes of the AgentReincarnation.
func (AgentReincarnation) Indexes() []ent.Index {
	return []ent.Index{
		// The 409-concurrency check (AC-8) and the hub-restart resume path
		// (Phase 3, AC-12) both query "is there a non-terminal reincarnation
		// for this agent". This composite index also covers plain agent_id
		// lookups (history listing for an agent) since agent_id is its
		// leading column — a separate single-column index would be
		// redundant (p1a-r1 N3).
		index.Fields("agent_id", "state"),
	}
}

// Edges of the AgentReincarnation.
func (AgentReincarnation) Edges() []ent.Edge {
	return nil
}
