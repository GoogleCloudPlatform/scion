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

// ScheduledExecPollCursor persists each target agent's recovery-sweep turn
// (ptone/scion#8096). Keyed by target_agent_id rather than derived from
// individual scheduled_exec_runs rows so a target's round-robin position
// neither depends on run creation timestamps nor rewinds when polled runs
// finish or are purged.
type ScheduledExecPollCursor struct {
	ent.Schema
}

// Fields of the ScheduledExecPollCursor.
func (ScheduledExecPollCursor) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("target_agent_id", uuid.UUID{}).
			Unique().
			Immutable(),
		field.Int64("turn_seq").
			Default(0),
		field.Time("last_polled_at"),
	}
}

// Indexes of the ScheduledExecPollCursor.
func (ScheduledExecPollCursor) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("turn_seq", "target_agent_id"),
		index.Fields("last_polled_at"),
	}
}

// Annotations of the ScheduledExecPollCursor.
func (ScheduledExecPollCursor) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "scheduled_exec_poll_cursors"},
	}
}
