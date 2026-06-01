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

// ApiKey holds the schema definition for the ApiKey entity, mapping the legacy
// SQLite `api_keys` table.
//
// NOTE: api_keys is a legacy table superseded by user_access_tokens (V34). It is
// schematized here for completeness/migration fidelity; confirm with the
// coordinator whether it is still in active use.
type ApiKey struct {
	ent.Schema
}

// Fields of the ApiKey.
func (ApiKey) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("user_id", uuid.UUID{}),
		field.String("name").
			Optional(),
		field.String("prefix").
			Optional(),
		field.String("key_hash").
			Sensitive().
			Unique().
			NotEmpty(),
		field.String("scopes").
			Optional(),
		field.Bool("revoked").
			Default(false),
		field.Time("expires_at").
			Optional().
			Nillable(),
		field.Time("last_used").
			Optional().
			Nillable(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the ApiKey.
func (ApiKey) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id"),
	}
}

// Annotations of the ApiKey.
func (ApiKey) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "api_keys"},
	}
}
