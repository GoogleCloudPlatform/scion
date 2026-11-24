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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ScheduledExecRun holds the schema definition for the ScheduledExecRun
// entity: one row per admitted exec run, owned by either a scheduled event
// (event_id) or a trigger fire (fire_id), never both
// (ptone/scion#7883, ptone/scion#8241).
//
// Exactly one of event_id or fire_id is non-NULL (enforced by a table CHECK
// constraint). Each owner column is unique and immutable, with a cascading
// foreign key to its owner table. overlap_key scopes per-schedule or
// per-trigger overlap protection without overloading schedule_id (which keeps
// its recurring-schedule accounting meaning and is always NULL for trigger runs).
//
// The row is written before exec-start is sent and is the durable record of
// the run (SEC-P6): the initiator, revision, executor and command digest
// never change after insert. It never holds argv or command output (D2 = b).
type ScheduledExecRun struct {
	ent.Schema
}

// Fields of the ScheduledExecRun.
func (ScheduledExecRun) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("event_id", uuid.UUID{}).
			Optional().
			Nillable().
			Unique().
			Immutable(),
		field.UUID("fire_id", uuid.UUID{}).
			Optional().
			Nillable().
			Unique().
			Immutable(),
		// schedule_id is the recurring schedule of the event, NULL for a
		// one-shot event or a trigger-owned run.
		field.UUID("schedule_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable(),
		// overlap_key scopes overlap protection ("schedule:<id>" or
		// "trigger:<id>") without overloading schedule_id.
		field.String("overlap_key").
			Default("").
			MaxLen(128).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}).
			Immutable(),
		field.UUID("target_agent_id", uuid.UUID{}).
			Immutable(),
		field.String("initiator_principal_kind").
			Default("").
			Immutable(),
		field.String("initiator_principal_id").
			Default("").
			Immutable(),
		field.String("initiator_credential_kind").
			Default("").
			Immutable(),
		field.String("initiator_credential_id").
			Default("").
			Immutable(),
		field.Int("authorization_revision").
			Default(0).
			Immutable(),
		field.String("executor_kind").
			Default("").
			Immutable(),
		field.String("executor_id").
			Default("").
			Immutable(),
		// command_sha256 is the hex sha256 of argv as a canonical JSON
		// array (agentcontrol.ExecArgvDigest).
		field.String("command_sha256").
			MaxLen(64).
			Immutable(),
		field.Int("argc").
			Immutable(),
		field.Int("timeout_seconds").
			Immutable(),
		field.Time("started_at").
			Default(time.Now).
			Immutable(),
		// not_after is the start-admission deadline sent as
		// not_after_unix_ms, frozen at admission: a delayed fire never
		// refreshes it, so sciontool refuses any start received after
		// not_after + tolerance, and recovery can treat a run never seen
		// started as absent after that (ptone/scion#7883 P3-R1).
		field.Time("not_after").
			Immutable(),
		// lease_until is started_at + timeout + 120s, fixed at admission.
		field.Time("lease_until").
			Immutable(),
		field.Time("finished_at").
			Optional().
			Nillable(),
		field.Enum("outcome").
			Values("starting", "running", "ok", "nonzero", "timeout", "skipped", "error", "abandoned").
			Default("starting"),
		field.Int("exit_code").
			Optional().
			Nillable(),
		field.Int64("duration_ms").
			Optional().
			Nillable(),
		// output_bytes counts the bytes sciontool drained and discarded.
		field.Int64("output_bytes").
			Optional().
			Nillable(),
		// capture_mode is the output capture mode requested at admission ("",
		// "json", or "tail"), immutable after insert.
		field.String("capture_mode").
			Default("").
			Immutable(),
		// output_status records what happened to captured output when
		// capture_mode is non-empty (captured, truncated, too_large,
		// invalid_json, empty, expired, lost).
		field.String("output_status").
			Default(""),
		field.String("runtime").
			Default("").
			Immutable(),
		// launch_id is the agent's launch (run id) at admission (D9-S3):
		// status polls are routed to that launch only.
		field.String("launch_id").
			Default("").
			Immutable(),
		// last_polled_at is when the sweeper last claimed the run for a
		// status poll (started_at at insert).
		field.Time("last_polled_at"),
		// poll_seq orders unfinished runs of one target agent by when the
		// sweeper last claimed them (0 at insert, monotonic across claims;
		// ptone/scion#8096).
		field.Int64("poll_seq").
			Default(0),
	}
}

// Edges of the ScheduledExecRun.
func (ScheduledExecRun) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("event", ScheduledEvent.Type).
			Ref("exec_run").
			Field("event_id").
			Unique().
			Immutable(),
		edge.From("trigger_fire", TriggerFire.Type).
			Ref("exec_run").
			Field("fire_id").
			Unique().
			Immutable(),
	}
}

// Indexes of the ScheduledExecRun.
func (ScheduledExecRun) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("schedule_id", "finished_at"),
		index.Fields("overlap_key", "finished_at"),
		index.Fields("target_agent_id", "finished_at"),
		index.Fields("target_agent_id", "finished_at", "poll_seq"),
		index.Fields("project_id"),
		index.Fields("finished_at", "lease_until"),
		index.Fields("finished_at", "last_polled_at"),
	}
}

// Annotations of the ScheduledExecRun.
func (ScheduledExecRun) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{
			Table: "scheduled_exec_runs",
			Checks: map[string]string{
				"scheduled_exec_runs_single_owner": "((event_id IS NOT NULL AND fire_id IS NULL) OR (event_id IS NULL AND fire_id IS NOT NULL))",
			},
		},
	}
}
