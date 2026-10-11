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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/mixin"
	"github.com/google/uuid"
)

// FixtureProvenanceMixin adds the creation-time test-identity provenance
// stamp (store.FixtureProvenance) to agents, projects, schedules and groups.
//
// fixture_root_id is the hub test identity (users.kind = test_fixture) at the
// root of the principal chain that created the row; fixture_issuer_id is
// that identity's issued_by user. Both are NULL for a row whose creating
// chain does not root at a test identity, and for every row written before
// these columns existed (there is no backfill).
//
// Both columns are Immutable: they are written once, in the row's create,
// and ent generates no setter for them on any update builder. The hub
// computes them on the server (pkg/hub/fixture_provenance.go); no request
// type binds them.
type FixtureProvenanceMixin struct {
	mixin.Schema
}

var _ ent.Mixin = FixtureProvenanceMixin{}

// Fields of the FixtureProvenanceMixin.
func (FixtureProvenanceMixin) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("fixture_root_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable().
			Comment("Test identity at the root of the creating principal chain; set once at create"),
		field.UUID("fixture_issuer_id", uuid.UUID{}).
			Optional().
			Nillable().
			Immutable().
			Comment("issued_by of the root test identity; set once at create"),
	}
}
