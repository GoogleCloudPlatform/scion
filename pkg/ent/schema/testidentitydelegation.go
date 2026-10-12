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
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// TestIdentityDelegation is a workload-bound issuer delegation
// (ptone/scion#4240, phase X2): a human issuer (the grantor) delegates
// member-tier test identity issuance to one workload identity, pinned by
// its Google subject. The delegation is the credential: a request proves
// it with a Google ID token whose sub is workload_sub and whose audience is
// the hub's issuer audience. Authority at every mint is the grantor's live
// bindings intersected with scope.
//
// Only the store's dedicated delegation methods write this table: create
// and revoke. Every field except revoked_at and revoked_by is immutable;
// revocation sets those once.
type TestIdentityDelegation struct {
	ent.Schema
}

// Fields of the TestIdentityDelegation.
func (TestIdentityDelegation) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("grantor_id").
			NotEmpty().
			Immutable().
			Comment("User ID of the human issuer who created the delegation"),
		field.String("workload_sub").
			NotEmpty().
			Immutable().
			Comment("Pinned Google subject (service account unique id) of the delegated workload"),
		field.String("scope").
			NotEmpty().
			Immutable().
			Comment("The single delegated scope, e.g. test_identity:issue"),
		field.Int("member_cap").
			Positive().
			Immutable().
			Comment("Live test identities this delegation may hold at once"),
		field.String("label").
			Default("").
			Immutable().
			Comment("Grantor-supplied label for the test window"),
		field.Time("created_at").
			Immutable(),
		field.Time("expires_at").
			Immutable().
			Comment("Server-computed end of the delegation's lifetime"),
		field.Time("revoked_at").
			Optional().
			Nillable(),
		field.String("revoked_by").
			Optional().
			Nillable(),
	}
}

// Indexes of the TestIdentityDelegation.
func (TestIdentityDelegation) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("grantor_id"),
	}
}

// Annotations of the TestIdentityDelegation.
func (TestIdentityDelegation) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "test_identity_delegations"},
	}
}
