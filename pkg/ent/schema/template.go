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

// Template mirrors the templates raw SQL table (V1 + V6 additions).
type Template struct {
	ent.Schema
}

func (Template) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "templates"}}
}

func (Template) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("name").NotEmpty(),
		field.String("slug").NotEmpty(),
		field.String("harness").NotEmpty(),
		field.String("image").Optional(),
		field.Text("config").Optional(),
		field.String("scope").Default("global"),
		field.String("grove_id").Optional(),
		field.String("storage_uri").Optional(),
		field.String("owner_id").Optional(),
		field.String("visibility").Default("private"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		// V6 additions.
		field.String("display_name").Optional(),
		field.String("description").Optional(),
		field.String("content_hash").Optional(),
		field.String("scope_id").Optional(),
		field.String("storage_bucket").Optional(),
		field.String("storage_path").Optional(),
		field.Text("files").Optional(),
		field.String("base_template").Optional(),
		field.Bool("locked").Default(false),
		field.String("status").Default("active"),
		field.String("created_by").Optional(),
		field.String("updated_by").Optional(),
		// V46 addition.
		field.String("default_harness_config").Optional(),
	}
}

func (Template) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug", "scope").StorageKey("idx_templates_slug_scope"),
		index.Fields("harness").StorageKey("idx_templates_harness"),
		index.Fields("status").StorageKey("idx_templates_status"),
		index.Fields("content_hash").StorageKey("idx_templates_content_hash"),
		index.Fields("scope", "scope_id").StorageKey("idx_templates_scope_id"),
	}
}
