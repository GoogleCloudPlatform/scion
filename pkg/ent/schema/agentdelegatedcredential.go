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

// AgentDelegatedCredential holds the schema definition for a delegated
// credential (.design/agent-delegation.md §9.1, §18.1): a short-lived opaque
// bearer produced by exchanging one agent delegation grant. Only the SHA-256
// of the bearer is stored (key_hash); the plaintext is returned once, in the
// exchange response.
type AgentDelegatedCredential struct {
	ent.Schema
}

// Fields of the AgentDelegatedCredential.
func (AgentDelegatedCredential) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("grant_id").
			NotEmpty().
			Immutable(),
		field.String("agent_id").
			NotEmpty().
			Immutable(),
		field.String("key_hash").
			Sensitive().
			Unique().
			NotEmpty().
			Immutable(),
		// prefix is the fixed "scion_adt_" marker only; no part of the
		// random body is stored.
		field.String("prefix").
			NotEmpty().
			Immutable(),
		field.String("audience").
			NotEmpty().
			Immutable(),
		// ceiling_permission_ids is a JSON array, a subset of the grant's.
		field.String("ceiling_permission_ids").
			NotEmpty().
			Immutable(),
		// exchange_agent_credential_id is the agent credential row verified
		// at exchange; revoking it ends this credential.
		field.String("exchange_agent_credential_id").
			NotEmpty().
			Immutable(),
		field.Time("issued_at").
			Default(time.Now).
			Immutable(),
		field.Time("expires_at").
			Immutable(),
		field.Time("revoked_at").
			Optional().
			Nillable(),
		field.String("revoke_reason").
			Optional().
			Nillable(),
		field.Time("last_seen_at").
			Optional().
			Nillable(),
	}
}

// Indexes of the AgentDelegatedCredential.
func (AgentDelegatedCredential) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("grant_id"),
		index.Fields("agent_id"),
		index.Fields("grant_id", "revoked_at", "expires_at"),
	}
}

// Edges of the AgentDelegatedCredential.
func (AgentDelegatedCredential) Edges() []ent.Edge {
	return nil
}

// Annotations of the AgentDelegatedCredential.
func (AgentDelegatedCredential) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "agent_delegated_credentials"},
	}
}
