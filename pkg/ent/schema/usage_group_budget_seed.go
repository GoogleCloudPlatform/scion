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

// UsageGroupBudgetSeed persists per-group charging-episode seed state
// (Tokenomics P3.3, D6a).
//
// A charging episode for group_id starts when the group's first active pooled
// binding (any scope, any window kind) activates, and ends (ended_at set) when
// its last active pooled binding deactivates. Exactly one seed is computed per
// episode over [seed_from_utc, cutoff_at), written to usage_group_rollups_daily
// under origin="seed:<episode_id>" for both (day, G, "") and (day, G, <project>)
// so episodes never overwrite each other and day/month windows read the same
// rows without colliding.
type UsageGroupBudgetSeed struct {
	ent.Schema
}

// Fields of the UsageGroupBudgetSeed.
func (UsageGroupBudgetSeed) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("group_id").
			NotEmpty().
			Immutable(),
		field.Time("activated_at").
			Immutable(),
		// max(start of current UTC month, previous episode's ended_at).
		field.Time("seed_from_utc").
			Immutable(),
		// activated_at + 2m (covers peer replicas with a stale 60s cache).
		field.Time("cutoff_at").
			Immutable(),
		// NULL while the charging episode is open; set when the group's last
		// active pooled binding deactivates.
		field.Time("ended_at").
			Optional().
			Nillable(),
		field.JSON("frozen_member_ids", []string{}).
			Optional(),
		field.Bool("member_snapshot_set").
			Default(false),
		field.Bool("finalized").
			Default(false),
		field.Int64("state_version").
			Default(1),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the UsageGroupBudgetSeed.
func (UsageGroupBudgetSeed) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id", "activated_at").
			Unique().
			StorageKey("usage_group_budget_seeds_group_act_idx"),
		index.Fields("group_id", "ended_at").
			StorageKey("usage_group_budget_seeds_group_ended_idx"),
		index.Fields("finalized", "cutoff_at").
			StorageKey("usage_group_budget_seeds_incomplete_idx"),
	}
}

// Annotations of the UsageGroupBudgetSeed.
func (UsageGroupBudgetSeed) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "usage_group_budget_seeds"},
	}
}
