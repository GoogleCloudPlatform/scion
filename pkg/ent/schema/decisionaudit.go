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

// DecisionAudit holds the schema definition for the DecisionAudit entity.
// Records authorization decisions made by the AuthzService for auditing.
type DecisionAudit struct {
	ent.Schema
}

// Fields of the DecisionAudit.
func (DecisionAudit) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.Time("timestamp").
			Default(time.Now).
			Immutable(),
		field.String("principal_kind").
			NotEmpty(),
		field.String("principal_id").
			NotEmpty(),
		field.String("credential_id").
			Optional(),
		field.String("credential_type").
			Optional(),
		field.String("route").
			Optional(),
		field.String("resource_type").
			NotEmpty(),
		field.String("resource_id").
			Optional(),
		field.String("permission").
			NotEmpty(),
		field.String("result").
			NotEmpty(),
		field.String("reason").
			NotEmpty(),
		field.String("matched_policy").
			Optional(),
		field.String("matched_grant").
			Optional(),
		field.String("policy_id").
			Optional(),
		field.String("correlation_id").
			Optional(),
		field.Bool("sampled").
			Default(false),
	}
}

// Indexes of the DecisionAudit.
func (DecisionAudit) Indexes() []ent.Index {
	return []ent.Index{
		// Time-range queries and retention cleanup
		index.Fields("timestamp"),
		// Principal queries
		index.Fields("principal_kind", "principal_id"),
		// Credential queries
		index.Fields("credential_id"),
		// Route queries
		index.Fields("route"),
		// Resource queries
		index.Fields("resource_type", "resource_id"),
		// Decision queries (allow vs deny)
		index.Fields("result"),
		// Request correlation
		index.Fields("correlation_id"),
	}
}

// Edges of the DecisionAudit.
func (DecisionAudit) Edges() []ent.Edge {
	return nil
}
