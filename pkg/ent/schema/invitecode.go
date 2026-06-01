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

// InviteCode holds the schema definition for the InviteCode entity, mapping the
// legacy SQLite `invite_codes` table. code_hash is the unique lookup key and is
// marked Sensitive.
type InviteCode struct {
	ent.Schema
}

// Fields of the InviteCode.
func (InviteCode) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("code_hash").
			Sensitive().
			Unique().
			NotEmpty(),
		field.String("code_prefix").
			NotEmpty(),
		field.Int("max_uses").
			Default(1),
		field.Int("use_count").
			Default(0),
		field.Time("expires_at"),
		field.Bool("revoked").
			Default(false),
		field.String("created_by").
			NotEmpty(),
		field.String("note").
			Default(""),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the InviteCode.
func (InviteCode) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("expires_at"),
	}
}

// Annotations of the InviteCode.
func (InviteCode) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "invite_codes"},
	}
}
