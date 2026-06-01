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

// AllowListEntry holds the schema definition for the AllowListEntry entity,
// mapping the legacy SQLite `allow_list` table.
//
// email was UNIQUE COLLATE NOCASE in SQLite. Postgres has no NOCASE collation,
// so a plain unique index is declared here; case-insensitive matching (citext
// or a lower(email) functional index) is a port-layer concern.
type AllowListEntry struct {
	ent.Schema
}

// Fields of the AllowListEntry.
func (AllowListEntry) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("email").
			Unique().
			NotEmpty(),
		field.String("note").
			Default(""),
		field.String("added_by").
			NotEmpty(),
		field.String("invite_id").
			Optional(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the AllowListEntry.
func (AllowListEntry) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("created", "id"),
	}
}

// Annotations of the AllowListEntry.
func (AllowListEntry) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "allow_list"},
	}
}
