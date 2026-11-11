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
// user_id is a required UUID foreign key (modeled as a plain column, no Ent
// edge). project_id is a UUID foreign key that is required iff boundary_kind
// is "project" (see boundary_kind's doc comment and store.UserAccessToken.
// ValidateBoundary, which enforces this in Go); scopes is a raw JSON string.
// key_hash is the unique lookup key and is marked Sensitive.
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
		// project_id is set iff boundary_kind is "project"; NULL for a hub
		// boundary. A missing or blank project_id is never read as hub — see
		// store.UserAccessToken.ValidateBoundary. Optional and Nillable so a
		// hub-boundary token persists a NULL value rather than an empty
		// string masquerading as "no project".
		field.UUID("project_id", uuid.UUID{}).
			Optional().
			Nillable(),
		// boundary_kind is the credential-side boundary this token was
		// issued under: "project" or "hub" (permissions.BoundaryKind).
		// Defaults to "project" so that a column-add migration backfills
		// every pre-existing row (which always had a non-null project_id)
		// correctly, and so that a future insert that forgets to set this
		// field cannot silently become a hub-boundary token: at worst it
		// becomes "project" with a NULL project_id, which load-time
		// validation (store.UserAccessToken.ValidateBoundary) rejects
		// rather than treating as authoritative.
		field.String("boundary_kind").
			NotEmpty().
			Default("project"),
		field.String("scopes").
			NotEmpty(),
		// ceiling_version and ceiling_permission_ids persist the normalized,
		// frozen permission ceiling. ceiling_version defaults to 0
		// (permissions.CeilingVersionUnspecified) for every row created
		// before this column existed. ceiling_permission_ids is Nillable so
		// "never backfilled" (NULL) is distinguishable from "backfilled to an
		// explicit empty list" (an empty JSON array, which denies rather than
		// meaning unrestricted) — see store.UserAccessToken.NormalizedCeiling.
		// Newly minted (CeilingVersionV1+) tokens always set both.
		field.Int32("ceiling_version").
			Default(0),
		field.String("ceiling_permission_ids").
			Optional().
			Nillable(),
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

		// Descriptive credential metadata. Optional/Nillable additive
		// columns: NULL means no metadata (always true for rows created
		// before these columns existed). Immutable() means these fields are
		// immutable after issuance, and the generated update builders
		// enforce that at the store layer too, not just by convention in
		// the service layer.
		field.String("purpose").
			Optional().
			Nillable().
			Immutable(),
		field.String("labels").
			Optional().
			Nillable().
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
//
// The "user_access_tokens_boundary_kind_check" CHECK enforces the same
// kind/project-id-presence invariant as store.UserAccessToken.ValidateBoundary
// at the database level: a "project" boundary requires a non-null
// project_id, and a "hub" boundary requires a null one. It is Go-side
// validation's backstop, not a substitute for it — every code path still
// calls ValidateBoundary at create and at load.
func (UserAccessToken) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{
			Table: "user_access_tokens",
			// Atlas inserts this string verbatim after CHECK, with no
			// additional wrapping paren of its own (unlike a hand-written
			// SQL CHECK(...) call): the OR must therefore be enclosed here,
			// or SQLite parses only the first parenthesized group as the
			// whole check and rejects the trailing " OR (...)" as a syntax
			// error immediately after the table's CREATE TABLE statement.
			Checks: map[string]string{
				"user_access_tokens_boundary_kind_check": "((boundary_kind = 'project' AND project_id IS NOT NULL) OR (boundary_kind = 'hub' AND project_id IS NULL))",
			},
		},
	}
}
