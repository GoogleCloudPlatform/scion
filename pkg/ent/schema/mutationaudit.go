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

// MutationAudit holds the schema definition for the MutationAudit entity.
// Records authorization-relevant mutations (policy changes, membership changes, etc.) for auditing.
type MutationAudit struct {
	ent.Schema
}

// Fields of the MutationAudit.
func (MutationAudit) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.Time("timestamp").
			Default(time.Now).
			Immutable(),
		field.String("mutation_type").
			NotEmpty(),
		field.String("actor_principal_kind").
			NotEmpty(),
		field.String("actor_principal_id").
			NotEmpty(),
		field.String("actor_credential_id").
			Optional(),
		field.String("actor_credential_type").
			Optional(),
		field.String("target_type").
			NotEmpty(),
		field.String("target_id").
			NotEmpty(),
		field.String("before_summary").
			Optional(),
		field.String("after_summary").
			Optional(),
		field.String("can_delegate_result").
			Optional(),
		field.String("can_delegate_reason").
			Optional(),
	}
}

// Indexes of the MutationAudit.
func (MutationAudit) Indexes() []ent.Index {
	return []ent.Index{
		// Time-range queries and retention cleanup
		index.Fields("timestamp"),
		// Mutation type queries
		index.Fields("mutation_type"),
		// Actor queries
		index.Fields("actor_principal_kind", "actor_principal_id"),
		// Actor credential queries
		index.Fields("actor_credential_id"),
		// Target queries
		index.Fields("target_type", "target_id"),
	}
}

// Edges of the MutationAudit.
func (MutationAudit) Edges() []ent.Edge {
	return nil
}
