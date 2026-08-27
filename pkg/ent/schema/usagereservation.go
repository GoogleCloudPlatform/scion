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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// UsageReservation holds the schema definition for the UsageReservation entity.
// A usage reservation tracks a single unit of quota consumption against a
// limit definition. Active reservations (released_at IS NULL) count toward
// the quota; released reservations are retained for auditing.
type UsageReservation struct {
	ent.Schema
}

// Fields of the UsageReservation.
func (UsageReservation) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("limit_definition_id", uuid.UUID{}).
			Comment("FK to LimitDefinition"),
		field.String("subject_id").
			NotEmpty(),
		field.Enum("scope_type").
			Values("system", "project"),
		field.String("scope_id").
			Default(""),
		field.String("resource_id").
			NotEmpty(),
		field.Int64("reserved").
			Default(1),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("released_at").
			Optional().
			Nillable(),
	}
}

// Indexes of the UsageReservation.
func (UsageReservation) Indexes() []ent.Index {
	return []ent.Index{
		// Active reservations for quota enforcement counting.
		// Partial index: only non-released reservations are indexed.
		index.Fields("limit_definition_id", "scope_type", "scope_id", "subject_id").
			Annotations(entsql.IndexWhere("released_at IS NULL")),
		// Unique active reservation per resource.
		// Partial index: released reservations are excluded so the same
		// resource can be re-reserved after release.
		index.Fields("limit_definition_id", "resource_id").
			Unique().
			Annotations(entsql.IndexWhere("released_at IS NULL")),
	}
}

// Edges of the UsageReservation.
func (UsageReservation) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("limit_definition", LimitDefinition.Type).
			Ref("usage_reservations").
			Field("limit_definition_id").
			Required().
			Unique(),
	}
}
