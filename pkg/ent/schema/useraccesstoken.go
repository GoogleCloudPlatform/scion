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

// UserAccessToken holds the schema definition for the UserAccessToken entity,
// mapping the legacy SQLite `user_access_tokens` table.
//
// user_id and project_id are required UUID foreign keys (modeled as plain
// columns, no Ent edges); scopes is a raw JSON string. key_hash is the unique
// lookup key and is marked Sensitive.
type UserAccessToken struct {
	ent.Schema
}

// Fields of the UserAccessToken.
func (UserAccessToken) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("user_id", uuid.UUID{}),
		field.String("name").
			NotEmpty(),
		field.String("prefix").
			NotEmpty(),
		field.String("key_hash").
			Sensitive().
			Unique().
			NotEmpty(),
		field.UUID("project_id", uuid.UUID{}),
		field.String("scopes").
			NotEmpty(),
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

// Indexes of the UserAccessToken.
func (UserAccessToken) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id"),
		index.Fields("project_id"),
	}
}

// Annotations of the UserAccessToken.
func (UserAccessToken) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "user_access_tokens"},
	}
}
