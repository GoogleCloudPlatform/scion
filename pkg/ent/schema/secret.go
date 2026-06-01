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

// Secret holds the schema definition for the Secret entity, mapping the legacy
// SQLite `secrets` table. Secrets are polymorphically scoped (hub/user/project/
// runtime_broker) via (scope, scope_id), so no FK edges are declared.
//
// encrypted_value stores the encrypted secret payload as TEXT (base64), not a
// BLOB, and is marked Sensitive so it is never logged or serialized.
type Secret struct {
	ent.Schema
}

// Fields of the Secret.
func (Secret) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("key").
			NotEmpty(),
		field.String("encrypted_value").
			Sensitive(),
		field.String("secret_ref").
			Optional(),
		field.Enum("secret_type").
			Values("environment", "variable", "file", "internal").
			Default("environment"),
		field.String("target").
			Optional(),
		field.String("scope").
			NotEmpty(),
		field.String("scope_id"),
		field.String("description").
			Optional(),
		field.Enum("injection_mode").
			Values("always", "as_needed").
			Default("as_needed"),
		field.Bool("allow_progeny").
			Default(false),
		field.Int("version").
			Default(1),
		field.String("created_by").
			Optional(),
		field.String("updated_by").
			Optional(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the Secret.
func (Secret) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("key", "scope", "scope_id").
			Unique(),
		index.Fields("scope", "scope_id"),
	}
}

// Annotations of the Secret.
func (Secret) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "secrets"},
	}
}
