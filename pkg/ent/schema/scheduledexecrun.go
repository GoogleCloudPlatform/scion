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

// ScheduledExecRun holds the schema definition for the ScheduledExecRun
// entity: one row per admitted run of a scheduled exec event
// (ptone/scion#7883, exec-schedules design "Schema: scheduled_exec_runs").
//
// The primary key is the scheduled event's id (column event_id): one run per
// event. There is deliberately no foreign key or ent edge to
// scheduled_events: the soft-delete purge deletes a run in the same
// transaction as its event, before the event, and never relies on a cascade.
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
			StorageKey("event_id").
			Immutable(),
		// schedule_id is the recurring schedule of the event, NULL for a
		// one-shot event (the overlap scope).
		field.UUID("schedule_id", uuid.UUID{}).
			Optional().
			Nillable().
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
		field.String("runtime").
			Default("").
			Immutable(),
		// launch_id is the agent's launch (run id) at admission (D9-S3):
		// status polls are routed to that launch only.
		field.String("launch_id").
			Default("").
			Immutable(),
	}
}

// Indexes of the ScheduledExecRun.
func (ScheduledExecRun) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("schedule_id", "finished_at"),
		index.Fields("target_agent_id", "finished_at"),
		index.Fields("project_id"),
		index.Fields("finished_at"),
	}
}

// Annotations of the ScheduledExecRun.
func (ScheduledExecRun) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "scheduled_exec_runs"},
	}
}
