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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AgentIdentityKey holds the schema definition for the AgentIdentityKey
// entity. Each agent reserves one row per distinct key in
// {slug, slugify(displayName)}; the UNIQUE(project_id, key) index makes
// per-project uniqueness across both an agent's slug and its display name a
// database invariant rather than a check-then-write race. Rows persist while
// the owning agent is soft-deleted (mirroring Agent's own non-partial
// (slug, project_id) unique index) and are removed only on hard delete.
//
// agent_id is a plain field rather than an ent edge, matching the
// AgentCredential/AgentSessionMetrics/AgentReincarnation precedent: a DB-level
// FK would block hard-deleting an agent the moment it has any identity-key
// row. Instead, composite.go's DeleteAgent and DeleteProject cascade-delete
// this table's rows explicitly, the same way DeleteAgent already does for
// AgentReincarnation.
type AgentIdentityKey struct {
	ent.Schema
}

// Fields of the AgentIdentityKey.
func (AgentIdentityKey) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}),
		field.String("key").
			NotEmpty(),
		field.UUID("agent_id", uuid.UUID{}),
	}
}

// Indexes of the AgentIdentityKey.
func (AgentIdentityKey) Indexes() []ent.Index {
	return []ent.Index{
		// The invariant: a key is unique within a project, across every
		// agent's slug and display-name keys combined.
		index.Fields("project_id", "key").
			Unique(),
		// Lookup/replace/delete all of one agent's keys.
		index.Fields("agent_id"),
	}
}

// Edges of the AgentIdentityKey.
func (AgentIdentityKey) Edges() []ent.Edge {
	return nil
}
