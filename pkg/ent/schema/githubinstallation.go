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

// GitHubInstallation mirrors github_installations (V35). The PK is an
// int64 column named installation_id — distinct from every other Hub table.
type GitHubInstallation struct {
	ent.Schema
}

func (GitHubInstallation) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "github_installations"}}
}

func (GitHubInstallation) Fields() []ent.Field {
	return []ent.Field{
		// Override Ent's default id with an int64 column named installation_id.
		field.Int64("id").StorageKey("installation_id"),
		field.String("account_login").NotEmpty(),
		field.String("account_type").Default("Organization"),
		field.Int64("app_id"),
		field.Text("repositories").Default("[]"),
		field.String("status").Default("active"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (GitHubInstallation) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("account_login").StorageKey("idx_github_installations_account"),
		index.Fields("status").StorageKey("idx_github_installations_status"),
	}
}
