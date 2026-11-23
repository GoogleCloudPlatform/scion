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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AgentSuspendScreen holds the terminal screen captured just before an agent's
// most recent hub-initiated suspend (ptone/scion#8530). At most one row per
// agent (enforced by a unique index on agent_id); replaced by each suspend,
// deleted with the agent via ON DELETE CASCADE. Kept out of the agents table
// so ListAgents/GetAgent/heartbeat writes never read or write it.
type AgentSuspendScreen struct {
	ent.Schema
}

// Fields of the AgentSuspendScreen.
func (AgentSuspendScreen) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("agent_id", uuid.UUID{}).
			Immutable(),
		// ANSI-preserving, valid UTF-8, NUL-free, <= 64 KiB (hub-enforced).
		field.Text("content"),
		field.Bool("truncated").
			Default(false),
		field.Time("captured_at"),
	}
}

// Edges of the AgentSuspendScreen.
func (AgentSuspendScreen) Edges() []ent.Edge {
	return []ent.Edge{
		// Hard-deleting the agent removes its saved screen (ON DELETE CASCADE
		// on the agent edge, see Agent.Edges).
		edge.From("agent", Agent.Type).
			Ref("suspend_screen").
			Field("agent_id").
			Unique().
			Required().
			Immutable(),
	}
}

// Indexes of the AgentSuspendScreen.
func (AgentSuspendScreen) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id").
			Unique(),
	}
}

// Annotations of the AgentSuspendScreen.
func (AgentSuspendScreen) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "agent_suspend_screens"},
	}
}
