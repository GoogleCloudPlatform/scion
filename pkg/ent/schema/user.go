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
		// pending is used only by federated rows: an account recorded for a
		// user of a trusted external issuer before that user's first
		// verified request. It is distinct from invited, which feeds the
		// local invite and sign-up flow.
		field.Enum("status").
			Values("active", "suspended", "invited", "pending").
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
		// identities: test_fixture (POST /api/v1/test-identities) and the
		// admin kinds test_hub_admin and test_super_admin (POST
		// /api/v1/test-identities/admin). It is Immutable: it is written
		// once at create and no update path can change it in either
		// direction. Only the store's CreateTestFixtureUser writes
		// test_fixture and only CreateTestAdminUser the admin kinds; the
		// general CreateUser refuses all three. federated is a local
		// account for a user of a trusted external issuer; only
		// CreateFederatedUser writes it.
		field.Enum("kind").
			Values("human", "test_fixture", "test_hub_admin", "test_super_admin", "federated").
			Default("human").
			Immutable(),
		// expires_at, issued_by and purpose are set only on test identity
		// rows (every kind other than human), at create, and never change
		// afterwards.
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
		// issuance_id and issuer_credential_id are set only on admin-kind
		// rows, at create, and never change afterwards.
		field.String("issuance_id").
			Optional().
			Nillable().
			Immutable().
			Comment("Issuance ID of an admin-kind test identity (audit correlation)"),
		field.String("issuer_credential_id").
			Optional().
			Nillable().
			Immutable().
			Comment("ID of the issuer's user access token for an admin-kind test identity; null for a session issuer"),
		// federated_email and home_issuer are set only on federated rows,
		// at create, and never change. A federated row's email column holds
		// a synthetic address in the reserved .invalid TLD, so no
		// email-keyed lookup can match it; the user's real address (lower
		// case) lives here, scoped by the issuer that vouches for it.
		field.String("federated_email").
			Optional().
			Nillable().
			Immutable().
			Comment("Lower-cased email of a federated user, as asserted by home_issuer"),
		field.String("home_issuer").
			Optional().
			Nillable().
			Immutable().
			Comment("Canonical issuer URL of a federated user"),
	}
}

// Annotations of the User.
//
// The "users_test_fixture_expiry_check" and "users_test_admin_expiry_check"
// CHECKs back the rule that a test identity row always carries an expiry
// (the store and the auth path also treat a missing expiry as expired).
// See the parenthesization note on UserAccessToken.Annotations: Atlas
// inserts the string verbatim.
func (User) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{
			Table: "users",
			Checks: map[string]string{
				"users_test_fixture_expiry_check": "(kind <> 'test_fixture' OR expires_at IS NOT NULL)",
				// The admin kinds carry an expiry too. A separate CHECK,
				// so the migration only adds a constraint.
				"users_test_admin_expiry_check": "(kind NOT IN ('test_hub_admin', 'test_super_admin') OR expires_at IS NOT NULL)",
				// A federated row always names its issuer and email.
				"users_federated_identity_check": "(kind <> 'federated' OR (home_issuer IS NOT NULL AND federated_email IS NOT NULL))",
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
		// One federated account per (issuer, email).
		index.Fields("home_issuer", "federated_email").
			Unique().
			Annotations(entsql.IndexWhere("kind = 'federated'")),
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
		// A test identity's single-use web-login code goes with the
		// user row (ptone/scion#4240, phase WS1).
		edge.To("test_identity_login_code", TestIdentityLoginCode.Type).
			Unique().
			Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}
