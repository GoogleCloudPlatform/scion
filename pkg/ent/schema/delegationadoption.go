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

// DelegationAdoption records one examined delegation edge of a
// delegation-provenance adoption: the boot migration's cohort snapshot, or
// an admin commit. It is evidence only; the authorization path never reads
// it. Each written adoption deactivates the original unrecorded edge and
// inserts a recorded edge, and this row links the two.
type DelegationAdoption struct {
	ent.Schema
}

// Fields of the DelegationAdoption.
func (DelegationAdoption) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		// cohort_id is the boot snapshot ID, or the plan ID of an admin commit.
		field.String("cohort_id").
			NotEmpty(),
		// "boot_migration" | "admin_commit".
		field.String("origin").
			NotEmpty(),
		field.Int("policy_version").
			Default(0),
		// The unrecorded edge this record adopts. NULL for a recognized edge
		// whose original row cannot be resolved unambiguously.
		field.String("original_edge_id").
			Optional().
			Nillable(),
		// The recorded edge written (adopted) or found (recognized).
		field.String("adopted_edge_id").
			Optional().
			Nillable(),
		// Copied from the examined edge, for reporting.
		field.String("delegate_id").
			Default(""),
		field.String("delegator_type").
			Default(""),
		field.String("delegator_id").
			Default(""),
		field.String("scope_id").
			Default(""),
		field.String("role").
			Default(""),
		// Depth of the delegate below its root principal (1 = created by a
		// user). Adoption runs in ascending depth.
		field.Int("depth").
			Default(0),
		// pending | adopted | recognized | recognized_above_policy |
		// excluded | skipped_changed | reverted.
		field.String("status").
			NotEmpty(),
		// Exclusion or skip reason code.
		field.String("reason").
			Default(""),
		field.String("before_fingerprint").
			Default(""),
		field.String("after_summary").
			Default(""),
		field.String("actor_kind").
			Default(""),
		field.String("actor_id").
			Default(""),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the DelegationAdoption.
func (DelegationAdoption) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("cohort_id", "original_edge_id").
			Unique(),
		// An edge written by an adoption is linked by exactly one adopted
		// record. Recognized records only point at an existing edge.
		index.Fields("adopted_edge_id").
			Unique().
			Annotations(entsql.IndexWhere("status = 'adopted'")),
		index.Fields("delegate_id", "status"),
		index.Fields("cohort_id", "status"),
	}
}

// Edges of the DelegationAdoption.
func (DelegationAdoption) Edges() []ent.Edge {
	return nil
}
