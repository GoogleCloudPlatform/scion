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

// RuntimeOrphan records a runtime object no Hub agent owns, as reported by
// a broker heartbeat's complete inventory (ptone/scion#7930): one row per
// (kind, broker_id, runtime_target, project_id, slug, agent_id), upserted
// on every heartbeat that reports it and removed once missed_inventories
// complete inventories in a row no longer list it. project_id and agent_id
// are the values the broker reported, not foreign keys: an orphan's project
// or agent may be unknown to the Hub. The action_* columns are reserved for
// a later orphan action phase and are not written yet.
type RuntimeOrphan struct {
	ent.Schema
}

// Fields of the RuntimeOrphan.
func (RuntimeOrphan) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		// kind is instance (a runtime entry) or nfs_tree (reserved).
		field.String("kind").NotEmpty().Immutable(),
		field.String("broker_id").NotEmpty().Immutable(),
		field.String("runtime_target").Default("").Immutable(),
		field.String("project_id").Default("").Immutable(),
		field.String("slug").Default("").Immutable(),
		field.String("agent_id").Default("").Immutable(),
		field.String("run_id").Default(""),
		field.String("reason").NotEmpty(),
		field.String("observed_phase").Default(""),
		field.Time("first_seen_at").Immutable(),
		field.Time("last_seen_at"),
		// missed_inventories counts the complete inventories in a row of
		// the row's target that did not list it.
		field.Int("missed_inventories").Default(0),
		field.String("action_state").Default("none"),
		field.Time("action_at").Optional().Nillable(),
		field.String("action_error").Default(""),
	}
}

// Indexes of the RuntimeOrphan.
func (RuntimeOrphan) Indexes() []ent.Index {
	return []ent.Index{
		// The row's identity: upserts conflict on it.
		index.Fields("kind", "broker_id", "runtime_target", "project_id", "slug", "agent_id").
			Unique(),
	}
}
