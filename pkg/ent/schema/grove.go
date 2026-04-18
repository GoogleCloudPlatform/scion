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

// Grove holds the schema definition for the Grove entity.
type Grove struct {
	ent.Schema
}

// Annotations set the table name.
func (Grove) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "groves"},
	}
}

// Fields of the Grove.
func (Grove) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty(),
		field.String("slug").
			Unique().
			NotEmpty(),
		field.String("git_remote").
			Optional().
			Nillable(),
		field.JSON("labels", map[string]string{}).
			Optional(),
		field.JSON("annotations", map[string]string{}).
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
		field.String("created_by").
			Optional(),
		field.String("owner_id").
			Optional(),
		field.String("visibility").
			Default("private"),

		// V2: default runtime broker (FK, ON DELETE SET NULL).
		field.UUID("default_runtime_broker_id", uuid.UUID{}).
			Optional().
			Nillable(),

		// V28: grove-level shared directory config (JSON array).
		field.JSON("shared_dirs", []SharedDir{}).
			Optional(),

		// V35: GitHub App integration (int64 FK to github_installations.installation_id).
		field.Int64("github_installation_id").
			Optional().
			Nillable(),
		field.JSON("github_permissions", &GitHubTokenPermissions{}).
			Optional(),
		field.JSON("github_app_status", &GitHubAppGroveStatus{}).
			Optional(),

		// V36: commit attribution config.
		field.JSON("git_identity", &GitIdentityConfig{}).
			Optional(),
	}
}

// Edges of the Grove.
func (Grove) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("agents", Agent.Type),
	}
}

// Indexes of the Grove, named to match raw SQL DDL.
func (Grove) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug").
			StorageKey("idx_groves_slug"),
		index.Fields("git_remote").
			StorageKey("idx_groves_git_remote"),
		index.Fields("owner_id").
			StorageKey("idx_groves_owner"),
		index.Fields("default_runtime_broker_id").
			StorageKey("idx_groves_default_runtime_broker"),
	}
}
