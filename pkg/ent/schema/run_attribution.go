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
)

// RunAttribution holds the Hub-frozen immutable attribution snapshot for one
// agent run_id (Tokenomics design §3.2, §4.2 item 1). Created atomically with
// run-ID minting or adoption via INSERT ... ON CONFLICT (run_id) DO NOTHING.
type RunAttribution struct {
	ent.Schema
}

// Fields of the RunAttribution.
func (RunAttribution) Fields() []ent.Field {
	return []ent.Field{
		// run_id is the primary key.
		field.String("id").
			StorageKey("run_id").
			NotEmpty().
			Immutable(),
		// launch_id is filled once when BeginLaunch/BeginStartLaunch binds
		// launch_run_id to this run_id.
		field.String("launch_id").
			Optional().
			Default(""),
		field.Int("generation").
			Default(1).
			Immutable(),
		field.String("prior_run_id").
			Optional().
			Default("").
			Immutable(),
		field.String("hub_id").
			Optional().
			Default("").
			Immutable(),
		field.String("project_id").
			NotEmpty().
			Immutable(),
		field.String("agent_id").
			NotEmpty().
			Immutable(),
		field.String("agent_slug").
			Optional().
			Default("").
			Immutable(),
		field.String("broker_id").
			Optional().
			Default("").
			Immutable(),
		// billing_user_id is NULL for principal-less agents (schedules,
		// webhooks, system agents).
		field.String("billing_user_id").
			Optional().
			Nillable().
			Immutable(),
		// owner_id and created_by are polymorphic principal IDs (user or agent).
		field.String("owner_id").
			Optional().
			Nillable().
			Immutable(),
		field.String("created_by").
			Optional().
			Nillable().
			Immutable(),
		field.String("harness").
			Optional().
			Default("").
			Immutable(),
		field.String("harness_config").
			Optional().
			Default("").
			Immutable(),
		field.String("template").
			Optional().
			Default("").
			Immutable(),
		// Fill-once digests and build info from broker resolved-config reports.
		field.String("harness_config_digest").
			Optional().
			Nillable(),
		field.String("template_digest").
			Optional().
			Nillable(),
		field.String("harness_build").
			Optional().
			Nillable(),
		field.JSON("labels_at_launch", map[string]string{}).
			Optional().
			Immutable(),
		field.String("metering_writer_mode").
			Optional().
			Default("").
			Immutable(),
		field.String("usage_live_source").
			Optional().
			Default("").
			Immutable(),
		field.String("capture_policy").
			Optional().
			Default("").
			Immutable(),
		field.String("notice_version").
			Optional().
			Default("").
			Immutable(),
		field.Bool("backfill_existing").
			Default(false).
			Immutable(),
		field.Bool("legal_hold").
			Default(false),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Edges of the RunAttribution.
func (RunAttribution) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("usage_events", UsageEvent.Type),
	}
}

// Indexes of the RunAttribution.
func (RunAttribution) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id", "created_at"),
		index.Fields("project_id", "created_at"),
		index.Fields("billing_user_id", "created_at"),
	}
}

// Annotations of the RunAttribution.
func (RunAttribution) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "run_attribution"},
	}
}
