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

// User holds the schema definition for the User entity.
type User struct {
	ent.Schema
}

// Fields of the User.
func (User) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		// email was UNIQUE COLLATE NOCASE in the legacy SQLite schema. Postgres
		// has no NOCASE collation, so case-insensitive uniqueness and lookup are
		// enforced at the port layer (entadapter): emails are normalized to
		// lower case on write and matched with EmailEqualFold (lower(email) =
		// lower($1)) on read. The Unique() index below therefore enforces
		// case-insensitive uniqueness because every stored value is normalized.
		// This is equivalent to a lower(email) functional unique index without
		// requiring an expression index, which ent codegen + AutoMigrate cannot
		// emit for both SQLite (tests) and Postgres.
		field.String("email").
			Unique().
			NotEmpty(),
		// display_name is required (NOT NULL) but may be empty, matching the
		// former raw-SQL store (display_name TEXT NOT NULL). Some identity
		// providers omit a display name; the broker/user handlers fall back to
		// the email in that case, so empty values must be storable. A stricter
		// NotEmpty() here would reject those users and break the fallback.
		field.String("display_name"),
		field.String("avatar_url").
			Optional(),
		field.Enum("role").
			Values("admin", "member", "viewer").
			Default("member"),
		field.Enum("status").
			Values("active", "suspended", "invited").
			Default("active"),
		field.JSON("preferences", &UserPreferences{}).
			Optional(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.String("invited_by").
			Optional().
			Nillable().
			Comment("ID or email of the admin who created this invited-user record"),
		field.String("invite_note").
			Optional().
			Nillable().
			Comment("Optional note from the admin who invited this user"),
		field.Time("last_login").
			Optional().
			Nillable(),
		field.Time("last_seen").
			Optional().
			Nillable(),
		field.Int64("session_generation").
			Default(0).
			Comment("Incremented to revoke all sessions for this user"),
		// kind separates ordinary accounts from hub-issued test
		// fixtures (POST /api/v1/test-identities). It is Immutable: it is
		// written once at create and no update path can change it in
		// either direction. Only the store's CreateTestFixtureUser writes
		// test_fixture; the general CreateUser refuses it.
		field.Enum("kind").
			Values("human", "test_fixture").
			Default("human").
			Immutable(),
		// expires_at, issued_by and purpose are set only on test_fixture
		// rows, at create, and never change afterwards.
		field.Time("expires_at").
			Optional().
			Nillable().
			Immutable().
			Comment("Hard expiry of a test_fixture user; required for that kind"),
		field.String("issued_by").
			Optional().
			Nillable().
			Immutable().
			Comment("User ID of the principal that issued this test_fixture user"),
		field.String("purpose").
			Optional().
			Nillable().
			Immutable().
			Comment("Issuer-supplied purpose of this test_fixture user"),
	}
}

// Annotations of the User.
//
// The "users_test_fixture_expiry_check" CHECK backs the rule that a
// test_fixture row always carries an expiry (the store and the auth path
// also treat a missing expiry as expired). See the parenthesization note on
// UserAccessToken.Annotations: Atlas inserts the string verbatim.
func (User) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{
			Table: "users",
			Checks: map[string]string{
				"users_test_fixture_expiry_check": "(kind <> 'test_fixture' OR expires_at IS NOT NULL)",
			},
		},
	}
}

// Indexes of the User.
func (User) Indexes() []ent.Index {
	return []ent.Index{
		// Supports the lastSeen sort option in ListUsers.
		index.Fields("last_seen"),
		// Supports the live test-fixture counts behind the issuance caps.
		index.Fields("kind", "issued_by"),
	}
}

// Edges of the User.
func (User) Edges() []ent.Edge {
	return []ent.Edge{
		// Note: agent.created_by / agent.owner_id are polymorphic principal
		// references (user or agent), so there is intentionally no
		// created_agents / owned_agents edge back to Agent. See pkg/ent/schema/agent.go.
		edge.To("owned_groups", Group.Type),
		edge.From("memberships", GroupMembership.Type).
			Ref("user"),
		edge.From("policy_bindings", PolicyBinding.Type).
			Ref("user"),
		edge.To("external_identities", ExternalIdentity.Type).
			Annotations(entsql.OnDelete(entsql.Cascade)),
		edge.To("terminal_workspace", UserTerminalWorkspace.Type).
			Unique().
			Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}
