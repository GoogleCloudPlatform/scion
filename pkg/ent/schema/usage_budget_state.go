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

// UsageBudgetState tracks per-(limit, scope, window) raw-token budget
// evaluation state with optimistic CAS concurrency control on state_version
// (Tokenomics design §4.2 item 6 and §4.3).
type UsageBudgetState struct {
	ent.Schema
}

// Fields of the UsageBudgetState.
func (UsageBudgetState) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("limit_definition_id", uuid.UUID{}).
			Immutable(),
		field.String("scope_kind").
			NotEmpty().
			Immutable(),
		field.String("scope_id").
			NotEmpty().
			Immutable(),
		field.String("project_scope_id").
			Default("").
			Immutable(),
		field.Time("window_start_utc").
			Immutable(),
		field.Time("window_end_utc").
			Immutable(),
		field.String("state").
			Default("ok"),
		field.Int64("consumed_at_eval").
			Default(0),
		field.Int64("cap_at_eval").
			Default(0),
		field.Int64("thresholds_notified").
			Default(0),
		field.Time("ack_anomalous_through").
			Optional().
			Nillable(),
		field.Time("override_until").
			Optional().
			Nillable(),
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

// Indexes of the UsageBudgetState.
func (UsageBudgetState) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("limit_definition_id", "scope_kind", "scope_id", "project_scope_id", "window_start_utc").
			Unique().
			StorageKey("usage_budget_state_key"),
		index.Fields("window_end_utc"),
		index.Fields("scope_kind", "scope_id", "project_scope_id", "window_start_utc").
			StorageKey("usage_budget_state_scope_window"),
	}
}

// Annotations of the UsageBudgetState.
func (UsageBudgetState) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "usage_budget_state"},
	}
}
