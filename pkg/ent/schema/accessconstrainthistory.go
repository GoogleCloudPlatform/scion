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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AccessConstraintHistory is the bounded, purpose-specific read model for a
// live access constraint. It intentionally stores typed fields rather than a
// rendered audit envelope.
type AccessConstraintHistory struct {
	ent.Schema
}

func (AccessConstraintHistory) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("event_id").
			NotEmpty().
			Immutable(),
		field.UUID("constraint_id", uuid.UUID{}).Immutable(),
		field.Time("occurred_at").Immutable(),
		field.String("operation").NotEmpty().Immutable(),
		field.String("actor_kind").Optional().Nillable().Immutable(),
		field.String("actor_id").Optional().Nillable().Immutable(),
		field.String("correlation_id").Optional().Nillable().Immutable(),
		field.String("batch_operation_id").Optional().Nillable().Immutable(),
		field.Int64("before_revision").Optional().Nillable().Immutable(),
		field.Int64("after_revision").Optional().Nillable().Immutable(),
		field.String("classification").Optional().Nillable().Immutable(),
		field.String("preview_id").Optional().Nillable().Immutable(),
		field.String("draft_hash").Optional().Nillable().Immutable(),
		field.String("impact_counts_json").Optional().Nillable().Immutable(),
		field.String("changed_fields_json").Optional().Nillable().Immutable(),
	}
}

func (AccessConstraintHistory) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("constraint", AccessConstraint.Type).
			Ref("history").
			Field("constraint_id").
			Required().
			Immutable().
			Unique(),
	}
}

func (AccessConstraintHistory) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("constraint_id", "occurred_at", "id").
			Annotations(entsql.DescColumns("occurred_at", "event_id")),
		index.Fields("occurred_at"),
	}
}

func (AccessConstraintHistory) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "access_constraint_history"},
	}
}
