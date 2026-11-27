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

// UsageEvent holds one row in the append-only operational usage_events ledger
// (Tokenomics design §3.1, §4.2 item 2). Unknown token counts are stored as
// SQL NULL (never fabricated as 0).
type UsageEvent struct {
	ent.Schema
}

// Fields of the UsageEvent.
func (UsageEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("hub_id").
			NotEmpty().
			Immutable(),
		field.String("event_id").
			NotEmpty().
			Immutable(),
		field.String("run_id").
			NotEmpty().
			Immutable(),
		field.String("project_id").
			NotEmpty().
			Immutable(),
		field.String("agent_id").
			NotEmpty().
			Immutable(),
		field.String("billing_user_id").
			Optional().
			Nillable().
			Immutable(),
		field.String("broker_id").
			Optional().
			Default("").
			Immutable(),
		// NOT NULL DEFAULT '' so unknown harness has one canonical encoding ("").
		field.String("harness").
			Default("").
			Immutable(),
		field.String("model").
			NotEmpty().
			Immutable(),
		field.String("model_source").
			NotEmpty().
			Immutable(),
		field.String("usage_source").
			NotEmpty().
			Immutable(),
		field.String("source_kind").
			Optional().
			Default("").
			Immutable(),
		field.String("source_identity").
			Optional().
			Default("").
			Immutable(),
		field.String("native_request_id").
			Optional().
			Nillable().
			Immutable(),
		field.String("session_id_hash").
			Optional().
			Nillable().
			Immutable(),
		field.String("status").
			NotEmpty().
			Immutable(),
		field.Int64("calls").
			Default(0).
			Immutable(),
		// Three-state nullable canonical token fields: unknown = NULL, never 0.
		field.Int64("input").
			Optional().
			Nillable().
			Immutable(),
		field.Int64("output").
			Optional().
			Nillable().
			Immutable(),
		field.Int64("cache_read").
			Optional().
			Nillable().
			Immutable(),
		field.Int64("cache_write").
			Optional().
			Nillable().
			Immutable(),
		field.Int64("cache_write_1h").
			Optional().
			Nillable().
			Immutable(),
		field.Int64("reasoning").
			Optional().
			Nillable().
			Immutable(),
		field.String("token_status").
			NotEmpty().
			Immutable(),
		field.JSON("malformed_fields", []string{}).
			Optional().
			Immutable(),
		field.Int64("reported_cost_micros").
			Optional().
			Nillable().
			Immutable(),
		field.String("reported_cost_currency").
			Optional().
			Nillable().
			Immutable(),
		field.String("reported_cost_source").
			Optional().
			Nillable().
			Immutable(),
		field.Time("event_ts").
			Immutable(),
		field.Time("received_at").
			Default(time.Now).
			Immutable(),
		field.String("producer_epoch").
			Optional().
			Default("").
			Immutable(),
		field.Int64("producer_seq").
			Default(0).
			Immutable(),
		field.Bool("anomalous").
			Default(false).
			Immutable(),
		field.JSON("charged_group_ids", []string{}).
			Optional().
			Immutable(),
		field.String("relocated_from_hub_id").
			Optional().
			Nillable().
			Immutable(),
	}
}

// Edges of the UsageEvent.
func (UsageEvent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("run_attribution", RunAttribution.Type).
			Ref("usage_events").
			Field("run_id").
			Unique().
			Required().
			Immutable(),
	}
}

// Indexes of the UsageEvent.
func (UsageEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("hub_id", "event_id").
			Unique(),
		index.Fields("project_id", "event_ts"),
		index.Fields("billing_user_id", "event_ts"),
		index.Fields("agent_id", "event_ts"),
		index.Fields("received_at", "event_id"),
		index.Fields("run_id"),
		index.Fields("event_ts"),
	}
}

// Annotations of the UsageEvent.
func (UsageEvent) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "usage_events"},
	}
}
