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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// GroupMembership holds the schema definition for the GroupMembership entity.
// It serves as a through-table linking principals (users or agents) to groups,
// with role and audit metadata.
type GroupMembership struct {
	ent.Schema
}

// Fields of the GroupMembership.
func (GroupMembership) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.Enum("role").
			Values("member", "admin", "owner").
			Default("member"),
		field.String("added_by").
			Optional(),
		field.Time("added_at").
			Default(time.Now).
			Immutable(),
		field.UUID("group_id", uuid.UUID{}),
		field.UUID("user_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("agent_id", uuid.UUID{}).
			Optional().
			Nillable(),
		// expires_at bounds the membership (ptone/scion#4240, admin tier
		// A3): a membership past it confers nothing and is not read as a
		// membership. NULL is unbounded. Grant shaping forces it for
		// memberships added by an admin-kind test identity.
		field.Time("expires_at").
			Optional().
			Nillable().
			Immutable(),
		// issuance_id is the issuance ID of the admin-kind test identity
		// whose request created this row (ptone/scion#4240, admin tier A3),
		// set only by the store's grant shaping (entadapter
		// applyGrantShaping), at create. NULL for every other row. The
		// purge of that identity removes the rows carrying it.
		field.String("issuance_id").
			Optional().
			Nillable().
			Immutable().
			Comment("Issuance ID of the admin-kind test identity that created this row (grant shaping)"),
	}
}

// Edges of the GroupMembership.
func (GroupMembership) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("group", Group.Type).
			Ref("memberships").
			Field("group_id").
			Required().
			Unique(),
		edge.To("user", User.Type).
			Field("user_id").
			Unique(),
		edge.To("agent", Agent.Type).
			Field("agent_id").
			Unique(),
	}
}

// Indexes of the GroupMembership.
func (GroupMembership) Indexes() []ent.Index {
	return []ent.Index{
		// The admin-kind test identity purge selects rows by issuance_id.
		index.Fields("issuance_id"),
		index.Fields("group_id", "user_id").
			Unique(),
		index.Fields("group_id", "agent_id").
			Unique(),
	}
}
