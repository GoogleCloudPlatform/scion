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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Agent holds the schema definition for the Agent entity.
//
// The agent entity carries both the principal-relevant fields used by the
// authorization layer (created_by, owner_id, delegation_enabled)
// and the full set of operational fields required to back store.Agent through
// the Ent adapter (P2-port-agent). Together they give the Ent-backed agent
// store parity with the former raw-SQL store implementation.
type Agent struct {
	ent.Schema
}

// Fields of the Agent.
func (Agent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("slug").
			NotEmpty(),
		field.String("name").
			NotEmpty(),
		field.String("template").
			Optional(),
		field.UUID("project_id", uuid.UUID{}).
			StorageKey("project_id"),
		field.Enum("status").
			Values("created", "provisioning", "cloning", "starting", "running", "suspended", "stopping", "stopped", "error").
			Default("created"),
		// created_by and owner_id are polymorphic *principal* references: the
		// creator/owner may be a user OR another agent (an agent that spawns a
		// sub-agent records its own ID here). They therefore carry no foreign-key
		// edge to the users table — a User-typed FK rejected every agent-created
		// sub-agent with a foreign-key violation. Consumers that need the user
		// behind the ID must look it up by ID and tolerate "no such user".
		field.UUID("created_by", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("owner_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Bool("delegation_enabled").
			Default(false),
		field.Enum("message_mode").
			Values("none", "lineage", "branch", "project", "hub").
			Default("project"),

		// --- Metadata (stored as JSON) ---
		field.JSON("labels", map[string]string{}).
			Optional(),
		field.JSON("annotations", map[string]string{}).
			Optional(),

		// --- Runtime status ---
		field.String("phase").
			Optional(),
		field.String("activity").
			Optional(),
		field.String("tool_name").
			Optional(),
		field.String("connection_state").
			Optional(),
		field.String("container_status").
			Optional(),
		field.Int("exit_code").
			Optional().
			Nillable(),
		field.String("exit_reason").
			Optional(),
		field.String("runtime_state").
			Optional(),
		field.String("stalled_from_activity").
			Optional(),

		// --- Limits tracking ---
		field.Int("current_turns").
			Default(0),
		field.Int("current_model_calls").
			Default(0),

		// --- Runtime configuration ---
		field.String("image").
			Optional(),
		field.Bool("detached").
			Default(false),
		field.String("runtime").
			Optional(),
		field.String("runtime_broker_id").
			Optional(),
		field.Bool("web_pty_enabled").
			Default(false),
		field.JSON("exposed_ports", []store.ExposedPort{}).
			Optional(),
		field.String("task_summary").
			Optional(),
		field.String("message").
			Optional(),

		// applied_config is the agent's resolved configuration, persisted as a
		// JSON document (store.AgentAppliedConfig). Stored as text to keep the
		// Ent schema decoupled from the store package's struct definition.
		field.Text("applied_config").
			Optional(),

		// ancestry is the ordered chain of ancestor principal IDs used for
		// transitive access control. Stored as a JSON array so the dialect-aware
		// json_each / json_array_elements_text membership filter can be applied.
		field.JSON("ancestry", []string{}).
			Optional(),

		// --- Timestamps ---
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
		field.Time("last_seen").
			Optional().
			Nillable(),
		field.Time("last_activity_event").
			Optional().
			Nillable(),
		field.Time("started_at").
			Optional().
			Nillable(),
		// deleted_at backs soft-delete: a non-nil value excludes the agent from
		// default listings (filtered via the DeletedAtIsNil Ent predicate).
		field.Time("deleted_at").
			Optional().
			Nillable(),

		// --- Optimistic locking ---
		// state_version is incremented on every UpdateAgent and used as a CAS
		// guard to detect concurrent modifications under multi-replica Postgres.
		field.Int64("state_version").
			Default(1),

		// --- Reincarnation (design: agent-reincarnate, ptone/scion#1821) ---
		// generation counts completed `scion reincarnate` migrations of this
		// agent row; a brand-new agent starts at 1. It is incremented only by
		// the reincarnation worker on a completed migration.
		field.Int("generation").
			Default(1),
		// reincarnation_state tracks an in-flight reincarnation and is kept
		// separate from `phase` so existing phase consumers are unaffected;
		// phase still moves through stopping/provisioning/starting/running as
		// normal during a reincarnation. Empty means no reincarnation is in
		// flight. A non-empty value here is also what gates the (future,
		// Phase 2) migration message delivery gate.
		field.String("reincarnation_state").
			Optional().
			Default(""),
		// reincarnation_updated_at is bumped ONLY by reincarnation-owned
		// writes (the claim, each worker step, and every terminal write) —
		// unlike `updated`, which every broker heartbeat's UpdateAgentStatus
		// also bumps (runtimebroker/heartbeat.go lists containers via `docker
		// ps -a`, so a stopped-but-present container still heartbeats). The
		// replica-safe sweep's agent-state backstop (design §3.4 Amendment
		// A6.6) keys on this column instead of `updated`, so a heartbeat
		// arriving for an orphaned claim can no longer keep it from ever
		// looking stale. Nil means no reincarnation has ever touched this
		// agent.
		field.Time("reincarnation_updated_at").
			Optional().
			Nillable(),

		// --- T1 async agent create (design t1-async-create-v11.md §3.3) ---
		// launch_async_opt_in records whether the client that created (or is
		// finalizing) this agent opted into non-blocking launch. The Hub flag
		// hub.asyncAgentLaunch is the kill switch; this is the per-request
		// half of the gate.
		field.Bool("launch_async_opt_in").
			Default(false),
		// launch_id is the current or most recent launch's identity. It is
		// kept after the launch ends, so a late report can recognize a
		// superseded launch and every reader has a stable ID to correlate
		// against, even once launch_state has moved to "ended".
		field.String("launch_id").
			Optional().
			Default(""),
		// launch_state is "active" while a launch is in flight, "ended" once
		// it has reached a terminal outcome, or "" for an agent that has
		// never had a launch (pre-T1 rows, or rows created before P1b-3 turns
		// async on).
		field.String("launch_state").
			Optional().
			Default(""),
		// launch_end_reason records why the most recent launch ended:
		// succeeded, running_observed, failed, timed_out, lost,
		// not_launched, or superseded.
		field.String("launch_end_reason").
			Optional().
			Default(""),
		// launch_kind is "create", "start" or "restart". Only "create" is
		// writable in P1a (BeginLaunch is restricted to phase in
		// {created, provisioning}); start/restart land in P6.
		field.String("launch_kind").
			Optional().
			Default(""),
		// launch_deadline is storeNow + the launch's timeout, set by
		// BeginLaunch. Present only while a launch is active in spirit, but
		// the column is never cleared on end — it is simply not read once
		// launch_state != "active". Covered by a partial index (below) so the
		// reaper's deadline scan stays cheap.
		field.Time("launch_deadline").
			Optional().
			Nillable(),
		// launch_last_report_at is bumped by every accepted report
		// (including keepalives) and is the input to the reaper's staleness
		// selection.
		field.Time("launch_last_report_at").
			Optional().
			Nillable(),
		// launch_owner is the broker instance ID (LaunchInstanceID) that
		// claimed this launch. Empty until the first non-terminal report is
		// accepted from some instance.
		field.String("launch_owner").
			Optional().
			Default(""),
		// launch_seq orders progress/checkpoint/claim reports for the current
		// launch so a replayed or reordered report can be recognized as a
		// duplicate. Terminal reports bypass it.
		field.Int64("launch_seq").
			Default(0),
		// launch_step is the most recent human-readable step name reported
		// for the current launch, surfaced on AgentLaunch.Step.
		field.String("launch_step").
			Optional().
			Default(""),
		// launch_error is the failure code of the current or most recent
		// launch (e.g. launch_timeout, broker_lost, launch_stopped,
		// agent_error). Cleared whenever phase becomes "running" (T3): an
		// agent that has ever run is never treated as an incomplete create
		// again, whatever happens to it later.
		field.String("launch_error").
			Optional().
			Default(""),
	}
}

// Edges of the Agent.
func (Agent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).
			Ref("agents").
			Field("project_id").
			Required().
			Unique(),
		edge.From("memberships", GroupMembership.Type).
			Ref("agent"),
		edge.From("policy_bindings", PolicyBinding.Type).
			Ref("agent"),
	}
}

// Indexes of the Agent.
func (Agent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug", "project_id").
			Unique(),
		// Partial index backing the T1 launch reaper's deadline scan (design
		// §3.3): a range scan on launch_deadline restricted to in-flight
		// launches, so it stays cheap regardless of table size. Same shape as
		// pkg/ent/schema/conversation.go's partial unique index and
		// usagereservation.go's partial indexes.
		index.Fields("launch_deadline").
			Annotations(
				entsql.IndexWhere("launch_state = 'active'"),
			),
		index.Fields("launch_id"),
	}
}
