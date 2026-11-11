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
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// PendingSecret holds the schema definition for the PendingSecret entity,
// mapping the `pending_secrets` table. A row is a user-scope secret written
// by an agent that waits for the owning user's approval. It is never read by
// any delivery path: only approval copies it into `secrets`.
//
// value_ref is the secret backend's handle to the held value (an encrypted
// envelope for the local backend). It is Sensitive so it is never logged or
// serialized.
//
// The provenance fields (agent_id, agent_slug, project_id, project_name,
// created) record the writing agent and are immutable. A replacement by the
// same agent inserts a new row (new id) that carries created over.
type PendingSecret struct {
	ent.Schema
}

// Fields of the PendingSecret.
func (PendingSecret) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("key").
			NotEmpty().
			Immutable(),
		field.Enum("secret_type").
			Values("environment", "variable", "file").
			Default("environment").
			Immutable(),
		field.String("target").
			Optional().
			Immutable(),
		field.String("scope").
			NotEmpty().
			Immutable(),
		field.String("scope_id").
			NotEmpty().
			Immutable(),
		field.Bool("allow_progeny").
			Default(false).
			Immutable(),
		field.Enum("injection_mode").
			Values("always", "as_needed").
			Default("as_needed").
			Immutable(),
		field.String("description").
			Optional().
			Immutable(),
		field.String("value_ref").
			NotEmpty().
			Sensitive().
			Immutable(),
		field.Int("value_len").
			NonNegative().
			Immutable(),
		field.String("agent_id").
			NotEmpty().
			Immutable(),
		field.String("agent_slug").
			NotEmpty().
			Immutable(),
		field.String("project_id").
			NotEmpty().
			Immutable(),
		field.String("project_name").
			NotEmpty().
			Immutable(),
		field.Time("created").
			Immutable(),
		field.Time("updated").
			Immutable(),
		field.String("replaces_secret_id").
			Optional().
			Immutable(),
		field.Int("replaces_version").
			Optional().
			Immutable(),
		field.Time("expires_at").
			Immutable(),
	}
}

// Indexes of the PendingSecret.
func (PendingSecret) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("key", "scope", "scope_id").
			Unique(),
		index.Fields("scope", "scope_id"),
		index.Fields("agent_id"),
		index.Fields("expires_at"),
	}
}

// Annotations of the PendingSecret.
func (PendingSecret) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "pending_secrets"},
	}
}
