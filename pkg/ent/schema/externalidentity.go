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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ExternalIdentity holds the schema definition for the ExternalIdentity entity.
// An external identity binding maps an (external provider, issuer, subject)
// triple to a local Hub user. This is used by the GE Google credential exchange
// to persist stable cross-login identity linkage.
type ExternalIdentity struct {
	ent.Schema
}

// Annotations of the ExternalIdentity.
func (ExternalIdentity) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "external_identities"},
	}
}

// Fields of the ExternalIdentity.
func (ExternalIdentity) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		// provider identifies the external identity provider (e.g. "google").
		field.String("provider").
			NotEmpty().
			Immutable().
			Comment("External identity provider name"),
		// issuer is the canonical issuer URL (e.g. "https://accounts.google.com").
		field.String("issuer").
			NotEmpty().
			Immutable().
			Comment("Canonical issuer URL"),
		// subject is the stable, provider-specific user identifier.
		field.String("subject").
			NotEmpty().
			Immutable().
			Comment("Provider-stable subject identifier"),
		// user_id is the FK to the local Hub user.
		field.UUID("user_id", uuid.UUID{}).
			Comment("FK to local Hub user"),
		// email is informational — the email at binding time. Does not drive
		// lookup and is updated if the upstream email changes.
		field.String("email").
			Optional().
			Comment("Email at binding time (informational)"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the ExternalIdentity.
func (ExternalIdentity) Indexes() []ent.Index {
	return []ent.Index{
		// Unique constraint: one binding per (provider, issuer, subject).
		// This enforces that a given external identity maps to exactly one
		// local user, enabling conflict-safe concurrent binding creation.
		index.Fields("provider", "issuer", "subject").
			Unique(),
		// Index for looking up all bindings for a given user.
		index.Fields("user_id"),
	}
}

// Edges of the ExternalIdentity.
func (ExternalIdentity) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("external_identities").
			Field("user_id").
			Required().
			Unique(),
	}
}
