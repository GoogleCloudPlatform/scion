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

		// harness_config is a queryable shadow of applied_config's
		// "harnessConfig" key, kept in sync by every write to applied_config
		// (CreateAgent/UpdateAgent — see agent_store.go's harnessConfigOf
		// helper) and reconciled at every startup for any row that hasn't
		// caught up (CompositeStore.ReconcileHarnessConfigColumn). It exists
		// solely so the CLI --harness filter (AgentFilter.HarnessConfig) can
		// use a plain, dialect-independent equality predicate instead of
		// parsing/pattern-matching the applied_config JSON document at query
		// time (ptone/scion#2146). It is not part of store.Agent
		// — nothing outside the HarnessConfig filter predicate reads it; the
		// enriched, response-facing store.Agent.HarnessConfig field is
		// unrelated and still derived from applied_config at response time,
		// unchanged.
		//
		// NULL vs "": NULL means "never written by a binary that knows this
		// column exists" — the reconcile's job is to find and fix exactly
		// those rows. Every write that DOES know about the column
		// (CreateAgent, UpdateAgent, the reconcile itself) always writes a
		// real value, including "" for "no harness configured, or nothing
		// usable could be extracted" — never NULL. "" can never match a
		// --harness filter (the filter predicate is only emitted for a
		// non-empty requested value), so this distinction is invisible to
		// callers; it exists purely so the reconcile query
		// (`harness_config IS NULL`) actually converges to empty once every
		// row has been visited by a column-aware binary, instead of
		// re-selecting and re-parsing every no-harness/invalid/legacy-key
		// row on every single boot forever (ptone/scion#2146).
		field.String("harness_config").
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
		// run_id is the identity of the agent's current or most recent run
		// (ptone/scion#2550): minted by the Hub for every create, start and
		// restart dispatch, persisted before the broker call, and carried
		// on the runtime entry as the scion.run_id label. A delete sends it
		// so the broker never removes a different run of the same name. ""
		// for rows that have not been dispatched since run IDs existed.
		field.String("run_id").
			Optional().
			Default(""),
		// previous_run_ids are the runs this agent's runtime entries may
		// still carry besides run_id (ptone/scion#3097), oldest first: each
		// run-ID write appends the run it replaced, and a write that settles
		// the run (the broker reported or replaced the entry, or the dispatch
		// reverted) clears them. A delete names each of them as well as
		// run_id, so a start that never landed does not leave the previous
		// entry behind. Empty for a settled run.
		field.Strings("previous_run_ids").
			Optional(),
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

		// --- Backend-driven agent delete (design ptone/scion#2483 §2.1) ---
		// A leased, sticky delete marker. Every write goes through
		// AgentStore.UpdateAgentDeletion (never UpdateAgent), which bumps
		// state_version so a stale whole-row writer gets ErrVersionConflict.
		//
		// deletion_state is "" (no delete), "deleting", "finalizing" or
		// "failed".
		field.String("deletion_state").
			Optional().
			Default(""),
		// deletion_claim is the claim epoch, bumped on every successful claim.
		field.Int64("deletion_claim").
			Default(0),
		// deletion_lease_at is the lease expiry the live engine renews. A
		// deleting/finalizing row whose lease has passed reads as failed.
		field.Time("deletion_lease_at").
			Optional().
			Nillable(),
		field.Time("deletion_started_at").
			Optional().
			Nillable(),
		field.Time("deletion_failed_at").
			Optional().
			Nillable(),
		// deletion_code is the failure code (runtime_error, conflict,
		// in_doubt, revoke_failed, finalize_failed, ...).
		field.String("deletion_code").
			Optional().
			Default(""),
		field.String("deletion_error").
			Optional().
			Default(""),
		// deletion_prior is JSON {phase, activity, launchId}, captured once
		// per delete attempt so a failed delete can restore it.
		field.String("deletion_prior").
			Optional().
			Default(""),
		// deletion_request is JSON {deleteFiles, removeBranch, soft, force,
		// requestedBy}.
		field.String("deletion_request").
			Optional().
			Default(""),

		// --- Run intent ---
		// run_intent records whether the user (or the system acting for the
		// user) wants this agent running: "running" or "stopped". NULL means
		// unknown; nothing treats a NULL intent as wanting the agent to run.
		// It is written only by AgentStore.SetRunIntent and RevertRunIntent
		// (and the one-time boot backfill), never by UpdateAgent or
		// CreateAgent, and writing it never bumps state_version.
		field.String("run_intent").
			Optional().
			Nillable(),
		// run_intent_at is the store-clock time of the last run_intent
		// write. It strictly increases per row, so it orders intent writes.
		field.Time("run_intent_at").
			Optional().
			Nillable(),

		// --- Start claim ---
		// An owned, leased claim taken before any start is dispatched, so at
		// most one start (or queued-stop drain) acts on an agent at a time.
		// The start_claim_* columns are written only by the AgentStore
		// start-claim methods (start_claim.go), never by UpdateAgent or
		// CreateAgent, and writing them never bumps state_version.
		//
		// start_claim_id is NULL when no claim is held.
		field.String("start_claim_id").
			Optional().
			Nillable(),
		// start_claim_kind: user, restart, wake, create, recovery,
		// reincarnate or stop.
		field.String("start_claim_kind").
			Optional().
			Default(""),
		// start_claim_state: live (the holder renews its lease) or
		// unconfirmed (the outcome is unknown; held until the runtime shows
		// what happened).
		field.String("start_claim_state").
			Optional().
			Default(""),
		// start_claim_owner is the hub instance that holds a live claim.
		field.String("start_claim_owner").
			Optional().
			Default(""),
		// start_claim_target is the runtime target the start is expected to
		// use, for agents that have no recorded target yet.
		field.String("start_claim_target").
			Optional().
			Default(""),
		// Store-clock times: when the claim was taken, when its lease ends,
		// when it became unconfirmed, and the end of the unconfirmed hold.
		field.Time("start_claim_at").
			Optional().
			Nillable(),
		field.Time("start_claim_lease_until").
			Optional().
			Nillable(),
		field.Time("start_claim_unconfirmed_at").
			Optional().
			Nillable(),
		field.Time("start_claim_hold_until").
			Optional().
			Nillable(),
		// start_claim_launch_id is the launch a create claim was linked to
		// when that launch began, so only that launch's end settles it.
		field.String("start_claim_launch_id").
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
		// Lookup of agents on a broker by run intent.
		index.Fields("runtime_broker_id", "run_intent"),
		// The start-claim reaper's scan: only rows holding a claim.
		index.Fields("start_claim_state", "start_claim_lease_until").
			Annotations(
				entsql.IndexWhere("start_claim_id IS NOT NULL"),
			),
		// Partial index backing CompositeStore.ReconcileHarnessConfigColumn's
		// every-boot scan (GoogleCloudPlatform/scion#2153), which queries
		// exactly Where(HarnessConfigIsNil(), AppliedConfigNotNil()) ordered
		// by id. The WHERE clause matches that predicate exactly, so the
		// index holds only rows still needing reconciliation — it shrinks
		// toward empty as they're caught up, instead of growing with the
		// whole table forever the way an unconditional index on
		// harness_config would. Same shape as this file's launch_deadline
		// index above and notification.go's dispatched-false index.
		//
		// Deliberately does not serve the CLI --harness filter
		// (agent.HarnessConfigEQ in agent_store.go): that predicate only
		// ever matches a non-empty harness value, which this index excludes
		// by construction. An index for that filter is a separate, still-open
		// question — it would need to combine with the AuthorizedProjectIDs
		// project scope every --harness query already carries, and the
		// agents table has no project_id index today for it to pair with —
		// not something this reconcile-only index should be widened to cover
		// speculatively.
		index.Fields("id").
			StorageKey("agent_harness_config_reconcile_pending").
			Annotations(
				entsql.IndexWhere("harness_config IS NULL AND applied_config IS NOT NULL"),
			),
	}
}
