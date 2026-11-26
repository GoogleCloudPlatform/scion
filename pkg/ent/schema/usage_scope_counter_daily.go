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

// UsageScopeCounterDaily holds O(1) exact daily quota counters per scope
// (Tokenomics design §4.2 item 4). scope_kind is one of project, user, or
// user_project (where scope_id is "<user_id>:<project_id>").
type UsageScopeCounterDaily struct {
	ent.Schema
}

// Fields of the UsageScopeCounterDaily.
func (UsageScopeCounterDaily) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("scope_kind").
			NotEmpty().
			Immutable(),
		field.String("scope_id").
			NotEmpty().
			Immutable(),
		field.Time("day_utc").
			Immutable(),
		field.Int64("tokens_summable").
			Default(0),
		field.Int64("calls").
			Default(0),
		field.Int64("unmeasured_calls").
			Default(0),
	}
}

// Indexes of the UsageScopeCounterDaily.
func (UsageScopeCounterDaily) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("scope_kind", "scope_id", "day_utc").
			Unique().
			StorageKey("usage_scope_counters_daily_key"),
		index.Fields("day_utc"),
	}
}

// Annotations of the UsageScopeCounterDaily.
func (UsageScopeCounterDaily) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "usage_scope_counters_daily"},
	}
}
