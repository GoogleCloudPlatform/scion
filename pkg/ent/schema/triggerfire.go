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

// TriggerFire holds one per-(event, trigger) claim and history record
// (ptone/scion#7961, Stage 1 §4.4). InitiatorAttributionMixin copies the
// trigger's attribution and authorization revision at claim time.
type TriggerFire struct {
	ent.Schema
}

// Mixin of the TriggerFire.
func (TriggerFire) Mixin() []ent.Mixin {
	return []ent.Mixin{
		InitiatorAttributionMixin{},
	}
}

// Fields of the TriggerFire.
func (TriggerFire) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}).
			Immutable(),
		field.UUID("trigger_id", uuid.UUID{}).
			Immutable(),
		field.UUID("event_id", uuid.UUID{}).
			Immutable(),
		field.String("subject").
			MaxLen(256).
			Immutable(),
		field.String("action_type").
			NotEmpty().
			Immutable(),
		field.String("status").
			Default("firing"),
		field.String("error").
			Optional(),
		field.String("result_ref").
			Optional(),
		field.String("matched_through").
			MaxLen(16).
			Optional(),
		field.String("decided_dialect").
			MaxLen(32).
			Optional(),
		field.Text("rule_trace").
			MaxLen(4096).
			Optional(),
		field.UUID("target_agent_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Time("wait_until").
			Optional().
			Nillable(),
		field.Int("attempts").
			Default(0),
		field.String("executor_kind").
			Default(""),
		field.String("executor_id").
			Default(""),
		field.Time("lease_until"),
		field.Time("not_before").
			Optional().
			Nillable(),
		field.Time("deferred_at").
			Optional().
			Nillable(),
		field.Time("started_at").
			Immutable(),
		field.Time("finished_at").
			Optional().
			Nillable(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Edges of the TriggerFire.
func (TriggerFire) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("exec_run", ScheduledExecRun.Type).
			Unique().
			Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}

// Indexes of the TriggerFire.
func (TriggerFire) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("trigger_id", "created"),
		index.Fields("event_id", "trigger_id").
			Unique(),
		index.Fields("trigger_id", "subject", "status"),
		index.Fields("status", "lease_until"),
		index.Fields("target_agent_id", "status"),
		index.Fields("project_id", "status"),
		index.Fields("result_ref"),
	}
}

// Annotations of the TriggerFire.
func (TriggerFire) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "trigger_fires"},
	}
}
