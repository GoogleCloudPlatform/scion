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

// Secret mirrors the secrets raw SQL table (V4 + V13/V14/V23/V43/V45).
type Secret struct {
	ent.Schema
}

func (Secret) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "secrets"}}
}

func (Secret) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("key").NotEmpty(),
		field.Text("encrypted_value"),
		field.String("scope").NotEmpty(),
		field.String("scope_id").NotEmpty(),
		field.String("description").Optional(),
		field.Int("version").Default(1),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.String("created_by").Optional(),
		field.String("updated_by").Optional(),
		field.String("secret_type").Default("environment"),
		field.String("target").Optional(),
		field.String("secret_ref").Optional(),
		field.String("injection_mode").Default("as_needed"),
		field.Bool("allow_progeny").Default(false),
	}
}

func (Secret) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("key", "scope", "scope_id").Unique().StorageKey("idx_secrets_key_scope"),
		index.Fields("scope", "scope_id").StorageKey("idx_secrets_scope"),
	}
}
