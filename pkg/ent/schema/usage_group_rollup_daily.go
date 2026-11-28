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

// UsageGroupRollupDaily holds daily pooled-group usage rollups for budgeted
// groups (Tokenomics design §4.2 item 5, §4.3).
//
// Key: (day_utc, group_id, project_scope_id, origin) where project_scope_id is
// "" for system-wide pools and "<project_id>" for project-scoped pools, and
// origin is "ingest" (written additively only by the usage_events insert tx) or
// "seed" (written idempotently via SET by the pooled-group seeder).
type UsageGroupRollupDaily struct {
	ent.Schema
}

// Fields of the UsageGroupRollupDaily.
func (UsageGroupRollupDaily) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.Time("day_utc").
			Immutable(),
		field.String("group_id").
			NotEmpty().
			Immutable(),
		field.String("project_scope_id").
			Default("").
			Immutable(),
		field.String("origin").
			NotEmpty().
			Immutable(),
		field.Int64("tokens_summable").
			Default(0),
		field.Int64("input").
			Default(0),
		field.Int64("output").
			Default(0),
		field.Int64("cache_read").
			Default(0),
		field.Int64("cache_write").
			Default(0),
		field.Int64("calls").
			Default(0),
		field.Int64("events").
			Default(0),
	}
}

// Indexes of the UsageGroupRollupDaily.
func (UsageGroupRollupDaily) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("day_utc", "group_id", "project_scope_id", "origin").
			Unique().
			StorageKey("usage_group_rollups_daily_key"),
		index.Fields("group_id", "project_scope_id", "day_utc"),
		index.Fields("day_utc"),
	}
}

// Annotations of the UsageGroupRollupDaily.
func (UsageGroupRollupDaily) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "usage_group_rollups_daily"},
	}
}
