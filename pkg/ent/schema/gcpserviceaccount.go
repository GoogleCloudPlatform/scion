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

// GCPServiceAccount holds the schema definition for the GCPServiceAccount
// entity, mapping the legacy SQLite `gcp_service_accounts` table.
//
// Accounts are polymorphically scoped via (scope, scope_id); default_scopes is
// a raw string (JSON/CSV) kept dialect-neutral.
type GCPServiceAccount struct {
	ent.Schema
}

// Fields of the GCPServiceAccount.
func (GCPServiceAccount) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("scope").
			NotEmpty(),
		field.String("scope_id").
			NotEmpty(),
		field.String("email").
			NotEmpty(),
		// project_id holds the GCP *cloud project* identifier (e.g.
		// "my-project-123"), which is a free-form string, not a UUID.
		field.String("project_id").
			NotEmpty(),
		field.String("display_name").
			Default(""),
		field.String("default_scopes").
			Default(""),
		field.Bool("verified").
			Default(false),
		field.Time("verified_at").
			Optional().
			Nillable(),
		field.String("created_by").
			Default(""),
		field.Bool("managed").
			Default(false),
		field.String("managed_by").
			Default(""),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the GCPServiceAccount.
func (GCPServiceAccount) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("email", "scope", "scope_id").
			Unique(),
		index.Fields("scope", "scope_id"),
	}
}

// Annotations of the GCPServiceAccount.
func (GCPServiceAccount) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "gcp_service_accounts"},
	}
}
