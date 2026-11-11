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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// TestIdentityLoginCode is a single-use web-login code for a hub-issued test
// identity (ptone/scion#4240, phase WS1). An issuer mints one with
// POST /api/v1/test-identities/{id}/web-login-code; a browser driver
// redeems it once at POST /auth/test-identity/redeem for a web session as
// that identity.
//
// Only the SHA-256 of the code is stored. A user holds at most one row:
// user_id is unique, and minting a new code deletes the old row first, so a
// second mint invalidates the first code. The row goes with its user (ON
// DELETE CASCADE). Only the dedicated UserStore methods write it.
type TestIdentityLoginCode struct {
	ent.Schema
}

// Fields of the TestIdentityLoginCode.
func (TestIdentityLoginCode) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("user_id", uuid.UUID{}).
			Unique().
			Immutable().
			Comment("The test identity this code signs in as; at most one code per identity"),
		field.String("code_hash").
			Sensitive().
			Unique().
			NotEmpty().
			Immutable().
			Comment("Hex SHA-256 of the code; the code itself is never stored"),
		field.String("issued_by").
			NotEmpty().
			Immutable().
			Comment("User ID of the issuer that minted the code"),
		field.String("issuer_credential_id").
			Default("").
			Immutable().
			Comment("Credential ID the issuer used, when it had one"),
		field.Time("expires_at").
			Immutable().
			Comment("The code cannot be redeemed at or after this time"),
		field.Time("consumed_at").
			Optional().
			Nillable().
			Comment("Set once, when the code is redeemed"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the TestIdentityLoginCode.
func (TestIdentityLoginCode) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("expires_at"),
	}
}

// Edges of the TestIdentityLoginCode.
func (TestIdentityLoginCode) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("test_identity_login_code").
			Field("user_id").
			Unique().
			Required().
			Immutable(),
	}
}

// Annotations of the TestIdentityLoginCode.
func (TestIdentityLoginCode) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "test_identity_login_codes"},
	}
}
