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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// Grove holds the schema definition for the Grove entity.
// This is a minimal schema for edge compilation; operational fields
// will be added when the grove entity is fully migrated to Ent.
type Grove struct {
	ent.Schema
}

// Fields of the Grove.
func (Grove) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty(),
		field.String("slug").
			Unique().
			NotEmpty(),
		field.String("git_remote").
			Optional().
			Unique().
			Nillable(),
		field.JSON("labels", map[string]string{}).
			Optional(),
		field.JSON("annotations", map[string]string{}).
			Optional(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
		field.String("created_by").
			Optional(),
		field.String("owner_id").
			Optional(),
		field.String("visibility").
			Default("private"),
	}
}

// Edges of the Grove.
func (Grove) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("agents", Agent.Type),
		edge.From("groups", Group.Type).
			Ref("grove"),
	}
}
