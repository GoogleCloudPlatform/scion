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

// AgentDelegationGrant holds the schema definition for an agent delegation
// grant (.design/agent-delegation.md §18.1): a durable record, created by an
// interactive user (the issuer), that binds one agent to a boundary, a
// frozen permission ceiling and an expiry. A grant confers nothing until the
// bound agent exchanges it for a delegated credential.
//
// agent_id and issuer_user_id are plain columns with no Ent edge and no
// foreign key, so a grant row survives the agent row being removed (hard
// delete, failed-create cleanup): every use-time check then denies on the
// missing agent, and the row stays for audit.
type AgentDelegationGrant struct {
	ent.Schema
}

// Fields of the AgentDelegationGrant.
func (AgentDelegationGrant) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("agent_id").
			NotEmpty().
			Immutable(),
		// agent_project_id, agent_generation and agent_state_version
		// snapshot the agent row at issuance, under its row lock.
		field.String("agent_project_id").
			NotEmpty().
			Immutable(),
		field.Int("agent_generation").
			Default(0).
			Immutable(),
		field.Int64("agent_state_version").
			Default(0).
			Immutable(),
		field.String("issuer_user_id").
			NotEmpty().
			Immutable(),
		// boundary_kind is "project" or "hub"; boundary_project_id is set
		// iff the kind is "project" (CHECK below).
		field.String("boundary_kind").
			NotEmpty().
			Immutable(),
		field.String("boundary_project_id").
			Optional().
			Nillable().
			Immutable(),
		// ceiling_version is always >= 1 for a grant: version 0 is never
		// written, and a row that carries it denies at exchange and use.
		field.Int32("ceiling_version").
			Immutable(),
		// ceiling_permission_ids is a JSON array of canonical permission IDs.
		field.String("ceiling_permission_ids").
			NotEmpty().
			Immutable(),
		field.String("name").
			NotEmpty().
			Immutable(),
		field.String("purpose").
			Optional().
			Nillable().
			Immutable(),
		// labels is a JSON object; NULL when absent.
		field.String("labels").
			Optional().
			Nillable().
			Immutable(),
		// allow_subdelegation, parent_grant_id and depth leave room for a
		// later subdelegation rule; v1 always writes false, NULL and 0.
		field.Bool("allow_subdelegation").
			Default(false).
			Immutable(),
		field.String("parent_grant_id").
			Optional().
			Nillable().
			Immutable(),
		field.Int("depth").
			Default(0).
			Immutable(),
		field.Int("max_credential_ttl_seconds").
			Positive().
			Immutable(),
		field.Time("expires_at").
			Immutable(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("last_exchanged_at").
			Optional().
			Nillable(),
		field.Time("revoked_at").
			Optional().
			Nillable(),
		field.String("revoked_by").
			Optional().
			Nillable(),
		field.String("revoke_reason").
			Optional().
			Nillable(),
		field.String("issuance_audit_id").
			NotEmpty().
			Immutable(),
		field.String("revocation_audit_id").
			Optional().
			Nillable(),
	}
}

// Indexes of the AgentDelegationGrant.
func (AgentDelegationGrant) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id"),
		index.Fields("issuer_user_id"),
		index.Fields("agent_id", "revoked_at"),
		index.Fields("issuer_user_id", "revoked_at"),
	}
}

// Edges of the AgentDelegationGrant.
func (AgentDelegationGrant) Edges() []ent.Edge {
	return nil
}

// Annotations of the AgentDelegationGrant. The CHECKs back up the Go-side
// validation in the store adapter: a project boundary names a project and a
// hub boundary names none, and the ceiling version is never 0.
func (AgentDelegationGrant) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{
			Table: "agent_delegation_grants",
			Checks: map[string]string{
				"agent_delegation_grants_boundary_check": "((boundary_kind = 'project' AND boundary_project_id IS NOT NULL) OR (boundary_kind = 'hub' AND boundary_project_id IS NULL))",
				"agent_delegation_grants_ceiling_check":  "(ceiling_version >= 1)",
			},
		},
	}
}
