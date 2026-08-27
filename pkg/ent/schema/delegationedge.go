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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// DelegationEdge holds the schema definition for the DelegationEdge entity.
// Records every authority-delegating relationship between principals.
// Used by the live delegation ceiling (Phase 1G) to verify that every
// ancestor in an agent's delegation chain still holds the permissions
// being exercised at decision time.
type DelegationEdge struct {
	ent.Schema
}

// Fields of the DelegationEdge.
func (DelegationEdge) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		// delegator_type is the kind of principal that delegated authority.
		field.Enum("delegator_type").
			Values("user", "agent"),
		// delegator_id is the ID of the delegating principal.
		field.String("delegator_id").
			NotEmpty(),
		// delegate_type is the kind of principal receiving authority.
		field.Enum("delegate_type").
			Values("user", "agent"),
		// delegate_id is the ID of the receiving principal.
		field.String("delegate_id").
			NotEmpty(),
		// scope_type matches RoleBinding scope types: system or project.
		field.Enum("scope_type").
			Values("system", "project"),
		// scope_id is the project ID for project-scoped delegations.
		field.String("scope_id").
			Default(""),
		// role is the role that was delegated (e.g. "full", "baseline", "readonly").
		field.String("role").
			NotEmpty(),
		// active indicates whether this delegation edge is currently active.
		field.Bool("active").
			Default(true),
		// grandfathered indicates whether this edge was created by migration backfill.
		field.Bool("grandfathered").
			Default(false),
		// created is the timestamp of edge creation.
		field.Time("created").
			Default(time.Now).
			Immutable(),
		// updated is the timestamp of last update.
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the DelegationEdge.
func (DelegationEdge) Indexes() []ent.Index {
	return []ent.Index{
		// Primary lookup: find who delegated to this principal.
		index.Fields("delegate_type", "delegate_id"),
		// Reverse lookup: find who this principal delegated to.
		index.Fields("delegator_type", "delegator_id"),
		// Scoped lookups — UNIQUE partial index to enforce the invariant
		// that at most one ACTIVE edge exists per (delegate, scope).
		// Using a partial index (WHERE active = true) avoids blocking
		// revocation: multiple inactive rows are allowed, only one
		// active row per (delegate, scope) combination.
		index.Fields("delegate_type", "delegate_id", "scope_type", "scope_id").
			Unique().
			Annotations(entsql.IndexWhere("active = true")),
		// For revocation propagation queries.
		index.Fields("delegator_type", "delegator_id", "active"),
	}
}

// Edges of the DelegationEdge.
func (DelegationEdge) Edges() []ent.Edge {
	return nil
}
