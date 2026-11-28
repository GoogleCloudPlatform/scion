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

// UsageGroupBudgetSeed persists per-(group, project_scope, window_kind, window_start)
// D6 mid-window seeding state (activation timestamp, +2m cutoff, frozen member
// snapshot, finalization flag, and optimistic state_version) so multiple Hub
// replicas never overwrite each other's seed records or recompute finalized
// seeds (Tokenomics P3.3, D6).
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
		field.String("project_scope_id").
			Default("").
			Immutable(),
		// "day" or "month" so day and month windows never collide on the 1st of a month.
		field.String("window_kind").
			NotEmpty().
			Immutable(),
		field.Time("window_start_utc").
			Immutable(),
		field.Time("window_end_utc").
			Immutable(),
		field.Time("activated_at"),
		field.Time("cutoff_at"),
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
		index.Fields("group_id", "project_scope_id", "window_kind", "window_start_utc").
			Unique().
			StorageKey("usage_group_budget_seeds_key"),
		index.Fields("finalized", "window_end_utc"),
	}
}

// Annotations of the UsageGroupBudgetSeed.
func (UsageGroupBudgetSeed) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "usage_group_budget_seeds"},
	}
}
