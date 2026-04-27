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

// EnvVar mirrors the env_vars raw SQL table (V4 + V12).
type EnvVar struct {
	ent.Schema
}

func (EnvVar) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "env_vars"}}
}

func (EnvVar) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("key").NotEmpty(),
		field.String("value"),
		field.String("scope").NotEmpty(),
		field.String("scope_id").NotEmpty(),
		field.String("description").Optional(),
		field.Bool("sensitive").Default(false),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.String("created_by").Optional(),
		field.String("injection_mode").Default("as_needed"),
		field.Bool("secret").Default(false),
	}
}

func (EnvVar) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("key", "scope", "scope_id").Unique().StorageKey("idx_env_vars_key_scope"),
		index.Fields("scope", "scope_id").StorageKey("idx_env_vars_scope"),
	}
}
