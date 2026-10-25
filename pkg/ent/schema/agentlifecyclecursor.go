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

// AgentLifecycleCursor holds one agent's lifecycle-event sequencing state
// (WP1 design section 4): the dense per-agent transition revision, the
// metadata revision and digest, and the fingerprint of the last observed
// row. It is operational state, not an analytical record: one row per
// agent, removed with the agent (hard delete, purge, project delete) or by
// the orphan sweep.
//
// The key is the agent ID with no foreign key (agent_id and project_id are
// uuid, like agents.id and agents.project_id, so the orphan sweep compares
// like types on PostgreSQL; review r6 N1). The open_run* and pending_*
// columns are written only by the run rules of a later slice; they exist
// now so that slice needs no migration.
type AgentLifecycleCursor struct {
	ent.Schema
}

// Fields of the AgentLifecycleCursor.
func (AgentLifecycleCursor) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			StorageKey("agent_id").
			Immutable(),
		field.UUID("project_id", uuid.UUID{}),
		field.Int64("transition_revision").
			Default(0),
		field.Int64("metadata_revision").
			Default(0),
		field.String("metadata_digest").
			Default(""),
		field.String("last_fingerprint").
			Default(""),
		// last_deleted: the last observed row had deleted_at set.
		field.Bool("last_deleted").
			Default(false),
		field.String("open_run").
			Default(""),
		field.String("open_run_generation").
			Default(""),
		field.Int64("open_run_metadata_revision").
			Default(0),
		field.Time("pending_run_change_at").
			Optional().
			Nillable(),
		field.String("reincarnation_base_run_id").
			Default(""),
		field.String("pending_adopted_from").
			Default(""),
		field.Time("pending_adopted_at").
			Optional().
			Nillable(),
		field.String("pending_adoption_source").
			Default(""),
		// pending_adopted_to is the run the pending adoption made current
		// (the adopted launch_id); the provenance rides only on an event
		// that names this run (wp1s2 review r1 R1).
		field.String("pending_adopted_to").
			Default(""),
		field.String("creator_principal_type").
			Default(""),
		field.String("creator_principal_id").
			Default(""),
		// requested_template is the agent's requested template name,
		// recorded at created from the committed row (WP1 slice 3
		// addendum v12 section 4a), before any broker echo can rewrite
		// agents.template. "" for a cursor created later (legacy rows):
		// requested_template is then omitted.
		field.String("requested_template").
			Default(""),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),

		// Persistent run attribution (WP1 attribution design v2.8, section
		// 3.2). Every column is additive with a fail-closed default: ""
		// reads as a LEGACY cursor (attribution_state) with an UNKNOWN
		// quarantine history (quarantine_mode), and a NULL stamp.
		//
		// attribution_state: "" LEGACY (written by a binary without this
		// design), "clear" or "held".
		field.String("attribution_state").
			Default(""),
		// attribution_stamp is written from the same value as updated_at
		// in every bump this design makes; a cursor update by a writer
		// that does not maintain the attribution leaves them unequal. A
		// consistency check, not proof of provenance (design 7.1).
		field.Time("attribution_stamp").
			Optional().
			Nillable(),
		// last_run and last_generation are the row's run and generation
		// as of the last observed write.
		field.String("last_run").
			Default(""),
		field.String("last_generation").
			Default(""),
		// attributed_run and attributed_generation: in "held", the identity
		// agent-state events may name for the held run ("" run: explicit
		// UNATTRIBUTED); in "clear", the undo identity of the newest
		// quarantined run.
		field.String("attributed_run").
			Default(""),
		field.String("attributed_generation").
			Default(""),
		// quarantined_runs: the runs this cursor held unproven, newest
		// first, comma-separated, at most store.MaxPreviousRunIDs.
		field.String("quarantined_runs").
			Default(""),
		// quarantine_mode: "" UNKNOWN (sticky: the list may be incomplete)
		// or "complete".
		field.String("quarantine_mode").
			Default(""),
		// reincarnation_mint_run witnesses the observed establishing mint
		// of a claimed, in-flight reincarnation.
		field.String("reincarnation_mint_run").
			Default(""),
		// untrusted_open_run, untrusted_open_run_generation and
		// untrusted_open_run_rev mark the exact open segment opened or
		// bound while its run was unproven.
		field.String("untrusted_open_run").
			Default(""),
		field.String("untrusted_open_run_generation").
			Default(""),
		field.Int64("untrusted_open_run_rev").
			Default(0),
	}
}

// Indexes of the AgentLifecycleCursor.
func (AgentLifecycleCursor) Indexes() []ent.Index {
	return []ent.Index{
		// DeleteProject removes every cursor of the project's agents.
		index.Fields("project_id"),
	}
}

// Annotations of the AgentLifecycleCursor.
func (AgentLifecycleCursor) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "agent_lifecycle_cursors"},
	}
}
