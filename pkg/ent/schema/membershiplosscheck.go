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

// MembershipLossCheck holds the schema definition for the
// MembershipLossCheck entity: a durable work item asking the hub to
// re-evaluate one user's project access and the agents rooted at that user.
// A row is written in the same transaction as the membership change that
// triggered it, claimed with a lease by a processor, and deleted when the
// processor completes it.
type MembershipLossCheck struct {
	ent.Schema
}

// Fields of the MembershipLossCheck.
func (MembershipLossCheck) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("user_id").
			NotEmpty().
			Immutable(),
		// project_id NULL means every project in which the user roots
		// agents.
		field.String("project_id").
			Optional().
			Nillable().
			Immutable(),
		field.Enum("trigger").
			Values(membershipLossTriggers...).
			Immutable(),
		field.String("actor_kind").
			Default("").
			Immutable(),
		field.String("actor_id").
			Default("").
			Immutable(),
		field.String("correlation_id").
			Default("").
			Immutable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Int("attempts").
			Default(0),
		field.String("last_error").
			Default(""),
		// lease_until is set by a claim; the row is claimable again once
		// it is NULL or in the past.
		field.Time("lease_until").
			Optional().
			Nillable(),
	}
}

// Indexes of the MembershipLossCheck.
func (MembershipLossCheck) Indexes() []ent.Index {
	return []ent.Index{
		// Claim scan: oldest claimable rows first.
		index.Fields("lease_until", "created_at"),
		index.Fields("user_id"),
	}
}

// Annotations of the MembershipLossCheck.
func (MembershipLossCheck) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "membership_loss_checks"},
	}
}
