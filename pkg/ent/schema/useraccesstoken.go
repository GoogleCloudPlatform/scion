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

// UserAccessToken mirrors the user_access_tokens raw SQL table (V34).
type UserAccessToken struct {
	ent.Schema
}

func (UserAccessToken) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "user_access_tokens"}}
}

func (UserAccessToken) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("user_id").NotEmpty(),
		field.String("name").NotEmpty(),
		field.String("prefix").NotEmpty(),
		field.String("key_hash").Unique().NotEmpty(),
		field.String("grove_id").NotEmpty(),
		field.String("scopes").NotEmpty(),
		field.Bool("revoked").Default(false),
		field.Time("expires_at").Optional().Nillable(),
		field.Time("last_used").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (UserAccessToken) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id").StorageKey("idx_uat_user_id"),
		index.Fields("key_hash").StorageKey("idx_uat_key_hash"),
	}
}
