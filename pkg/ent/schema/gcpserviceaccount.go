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
)

// GCPServiceAccount mirrors gcp_service_accounts (V30 + V44).
type GCPServiceAccount struct {
	ent.Schema
}

func (GCPServiceAccount) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "gcp_service_accounts"}}
}

func (GCPServiceAccount) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("scope").NotEmpty(),
		field.String("scope_id").NotEmpty(),
		field.String("email").NotEmpty(),
		field.String("project_id").NotEmpty(),
		field.String("display_name").Default(""),
		field.Text("default_scopes").Default(""),
		field.Bool("verified").Default(false),
		field.Time("verified_at").Optional().Nillable(),
		field.String("created_by").Default(""),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Bool("managed").Default(false),
		field.String("managed_by").Default(""),
	}
}

func (GCPServiceAccount) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("email", "scope", "scope_id").Unique(),
		index.Fields("scope", "scope_id").StorageKey("idx_gcp_sa_scope"),
	}
}
