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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AccessConstraint holds the schema definition for the AccessConstraint entity.
// An access constraint is a named maximum-permissions boundary that can only
// reduce otherwise granted authority.
type AccessConstraint struct {
	ent.Schema
}

// Fields of the AccessConstraint.
func (AccessConstraint) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty(),

		// Subject selector fields — exactly one of the three kinds.
		field.Enum("subject_kind").
			Values("principal", "group_closure", "all_principals"),
		field.String("subject_principal_type").
			Optional().
			Nillable().
			Comment("user, agent, or group — required when subject_kind=principal"),
		field.String("subject_principal_id").
			Optional().
			Nillable().
			Comment("ID of the exact principal — required when subject_kind=principal"),
		field.String("subject_group_id").
			Optional().
			Nillable().
			Comment("Group whose closure is constrained — required when subject_kind=group_closure"),

		// Scope fields.
		field.Enum("scope_type").
			Values("system", "project"),
		field.String("scope_id").
			Default("").
			Comment("Empty for system scope, project ID for project scope"),

		// Maximum permissions — the allowlist of permission IDs.
		field.JSON("maximum_permissions", []string{}).
			Comment("Permission IDs that constrained principals may hold"),

		// Constraint condition: typed time window (v1 only).
		field.Time("not_before").
			Optional().
			Nillable().
			Comment("Constraint is inactive before this time"),
		field.Time("expires_at").
			Optional().
			Nillable().
			Comment("Constraint is inactive after this time"),

		// Disabled flag for offline recovery.
		field.Bool("disabled").
			Default(false).
			Comment("True when deactivated by offline recovery"),

		// Revision tracking — monotonic counter incremented on every update.
		field.Int64("revision").
			Default(1).
			Comment("Monotonic revision counter, incremented on every update"),

		// Purpose — required human-readable description of why this constraint exists.
		field.String("purpose").
			NotEmpty().
			Default("system constraint").
			Comment("Human-readable description of why this constraint exists"),

		// Audit fields.
		field.String("created_by").
			Optional(),
		field.String("updated_by").
			Optional().
			Nillable().
			Comment("Principal who last modified this constraint"),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the AccessConstraint.
func (AccessConstraint) Indexes() []ent.Index {
	return []ent.Index{
		// Unique constraint name per scope.
		index.Fields("name", "scope_type", "scope_id").Unique(),
		// Batched lookup: find constraints for a subject kind and scope.
		index.Fields("subject_kind", "scope_type", "scope_id"),
		// Find constraints targeting an exact principal.
		index.Fields("subject_principal_type", "subject_principal_id"),
		// Find constraints targeting a group closure.
		index.Fields("subject_group_id"),
	}
}

// Edges of the AccessConstraint.
func (AccessConstraint) Edges() []ent.Edge {
	return nil
}
