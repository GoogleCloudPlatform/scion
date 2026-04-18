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
)

// GroveContributor mirrors grove_contributors (V1 + V3 + V10). Raw SQL uses a
// composite PK (grove_id, broker_id); Ent requires a surrogate id field, so
// we add one and enforce the composite as a UNIQUE index. Functionally
// equivalent for lookups and FK references.
type GroveContributor struct {
	ent.Schema
}

func (GroveContributor) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "grove_contributors"}}
}

func (GroveContributor) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("grove_id").NotEmpty(),
		field.String("broker_id").NotEmpty(),
		field.String("broker_name").NotEmpty(),
		field.String("mode").Default("connected"),
		field.String("status").Default("offline"),
		field.Text("profiles").Optional(),
		field.Time("last_seen").Optional().Nillable(),
		field.String("local_path").Optional(),
		field.String("linked_by").Optional(),
		field.Time("linked_at").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (GroveContributor) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("grove_id", "broker_id").Unique(),
	}
}
