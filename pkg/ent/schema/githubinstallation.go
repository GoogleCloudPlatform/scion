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
)

// GithubInstallation holds the schema definition for the GithubInstallation
// entity, mapping the legacy SQLite `github_installations` table.
//
// The primary key is the GitHub-provided installation_id (a real integer id,
// NOT a UUID), so id is an int64 stored in the installation_id column with no
// generated default. repositories is a raw JSON string.
type GithubInstallation struct {
	ent.Schema
}

// Fields of the GithubInstallation.
func (GithubInstallation) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id").
			StorageKey("installation_id").
			Immutable(),
		field.String("account_login").
			NotEmpty(),
		field.String("account_type").
			Default("Organization"),
		field.Int64("app_id"),
		field.String("repositories").
			Default("[]"),
		field.String("status").
			Default("active"),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Annotations of the GithubInstallation.
func (GithubInstallation) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "github_installations"},
	}
}
