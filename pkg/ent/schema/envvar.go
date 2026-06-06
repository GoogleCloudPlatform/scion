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

// EnvVar holds the schema definition for the EnvVar entity, mapping the legacy
// SQLite `env_vars` table. Like Secret, it is polymorphically scoped via
// (scope, scope_id) with no FK edges.
type EnvVar struct {
	ent.Schema
}

// Fields of the EnvVar.
func (EnvVar) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("key").
			NotEmpty(),
		field.String("value"),
		field.String("scope").
			NotEmpty(),
		field.String("scope_id"),
		field.String("description").
			Optional(),
		field.Bool("sensitive").
			Default(false),
		field.Enum("injection_mode").
			Values("always", "as_needed").
			Default("as_needed"),
		field.Bool("secret").
			Default(false),
		field.String("created_by").
			Optional(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the EnvVar.
func (EnvVar) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("key", "scope", "scope_id").
			Unique(),
		index.Fields("scope", "scope_id"),
	}
}

// Annotations of the EnvVar.
func (EnvVar) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "env_vars"},
	}
}
