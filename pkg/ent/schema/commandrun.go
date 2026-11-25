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

// CommandRun is the single execution attempt of an approved command-request
// version (ptone/scion#9278 §4.3). Its id is the Conduit run_id minted at
// approval, and UNIQUE(request_id) is the DB-level guarantee against double
// execution. No foreign keys.
type CommandRun struct {
	ent.Schema
}

// Fields of the CommandRun.
func (CommandRun) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("request_id", uuid.UUID{}).
			Unique().
			Immutable(),
		field.String("command_sha256").
			NotEmpty().
			MaxLen(64).
			Immutable(),
		field.Int("script_len").
			Positive().
			Immutable(),
		field.String("approver_user_id").
			NotEmpty().
			MaxLen(128).
			Immutable(),
		field.UUID("exec_agent_id", uuid.UUID{}).
			Immutable(),
		field.UUID("exec_project_id", uuid.UUID{}).
			Immutable(),
		field.String("launch_id").
			Default(""),
		field.Enum("status").
			Values("queued", "waking", "dispatching", "running", "succeeded", "failed", "unknown", "not_started").
			Default("queued"),
		field.String("reason").
			Default("").
			MaxLen(128),
		field.Enum("wake").
			Values("none", "resume").
			Default("none"),
		field.Bool("share_output").
			Default(false).
			Immutable(),
		field.Int("timeout_s").
			Positive().
			Immutable(),
		field.String("capture_mode").
			Default("tail").
			MaxLen(32).
			Immutable(),
		field.String("workdir").
			Default("").
			MaxLen(messages.MaxCommandRequestWorkdirLen).
			Immutable(),
		field.Int("exit_code").
			Optional().
			Nillable(),
		field.Int64("duration_ms").
			Optional().
			Nillable(),
		field.Int64("output_bytes").
			Optional().
			Nillable(),
		field.String("output_status").
			Default("").
			MaxLen(32),
		field.Time("not_after").
			Optional().
			Nillable(),
		field.Time("lease_until"),
		field.Time("last_polled_at").
			Optional().
			Nillable(),
		field.Time("poll_claimed_at").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("dispatched_at").
			Optional().
			Nillable(),
		field.Time("finished_at").
			Optional().
			Nillable(),
		field.Time("outcome_delivered_at").
			Optional().
			Nillable(),
		field.UUID("outcome_message_id", uuid.UUID{}).
			Optional().
			Nillable(),
	}
}

// Indexes of the CommandRun.
func (CommandRun) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("finished_at", "lease_until"),
		index.Fields("exec_agent_id", "finished_at"),
		index.Fields("approver_user_id", "created_at"),
		index.Fields("finished_at", "outcome_delivered_at"),
	}
}

// Annotations of the CommandRun.
func (CommandRun) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "command_runs"},
	}
}
