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

// UsageRollupDaily holds daily aggregated Track 1 usage counters
// (Tokenomics design §4.2 item 3). billing_user_id stores "" for runs with
// NULL billing_user_id so the composite unique index behaves uniformly across
// Postgres and SQLite.
type UsageRollupDaily struct {
	ent.Schema
}

// Fields of the UsageRollupDaily.
func (UsageRollupDaily) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.Time("bucket_start").
			Immutable(),
		field.String("project_id").
			NotEmpty().
			Immutable(),
		field.String("billing_user_id").
			Default("").
			Immutable(),
		field.String("agent_id").
			NotEmpty().
			Immutable(),
		// Every column in usage_rollups_daily_key must be NOT NULL so
		// ON CONFLICT DO UPDATE upserts always collide on the same logical key.
		field.String("harness").
			Default("").
			Immutable(),
		field.String("model").
			Default("").
			Immutable(),
		field.String("usage_source").
			NotEmpty().
			Immutable(),
		field.Int64("calls").
			Default(0),
		field.Int64("error_calls").
			Default(0),
		field.Int64("input").
			Default(0),
		field.Int64("output").
			Default(0),
		field.Int64("cache_read").
			Default(0),
		field.Int64("cache_write").
			Default(0),
		field.Int64("cache_write_1h").
			Default(0),
		field.Int64("reasoning").
			Default(0),
		field.Int64("reported_cost_micros").
			Default(0),
		field.Int64("events").
			Default(0),
		field.Int64("null_token_events").
			Default(0),
		// Per-bucket presence (non-null) counters allow reads to distinguish
		// measured 0 from all-NULL (unknown) and flag partially-known buckets
		// without scanning usage_events (§3.1). presence_tracked_events marks
		// events ingested with presence tracking; pre-existing legacy rows have
		// presence_tracked_events = 0 so sum == 0 safely reads as null.
		field.Int64("input_known_events").
			Default(0),
		field.Int64("output_known_events").
			Default(0),
		field.Int64("cache_read_known_events").
			Default(0),
		field.Int64("cache_write_known_events").
			Default(0),
		field.Int64("cache_write_1h_known_events").
			Default(0),
		field.Int64("reasoning_known_events").
			Default(0),
		field.Int64("presence_tracked_events").
			Default(0),
		field.Time("first_event_ts"),
		field.Time("last_event_ts"),
	}
}

// Indexes of the UsageRollupDaily.
func (UsageRollupDaily) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("bucket_start", "project_id", "billing_user_id", "agent_id", "harness", "model", "usage_source").
			Unique().
			StorageKey("usage_rollups_daily_key"),
		index.Fields("project_id", "bucket_start"),
		index.Fields("billing_user_id", "bucket_start"),
		index.Fields("agent_id", "bucket_start"),
	}
}

// Annotations of the UsageRollupDaily.
func (UsageRollupDaily) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "usage_rollups_daily"},
	}
}
