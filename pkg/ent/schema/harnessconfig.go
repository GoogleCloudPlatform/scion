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

// HarnessConfig mirrors the harness_configs raw SQL table (V16).
type HarnessConfig struct {
	ent.Schema
}

func (HarnessConfig) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "harness_configs"}}
}

func (HarnessConfig) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("name").NotEmpty(),
		field.String("slug").NotEmpty(),
		field.String("display_name").Optional(),
		field.String("description").Optional(),
		field.String("harness").NotEmpty(),
		field.Text("config").Optional(),
		field.String("content_hash").Optional(),
		field.String("scope").Default("global"),
		field.String("scope_id").Optional(),
		field.String("storage_uri").Optional(),
		field.String("storage_bucket").Optional(),
		field.String("storage_path").Optional(),
		field.Text("files").Optional(),
		field.Bool("locked").Default(false),
		field.String("status").Default("active"),
		field.String("owner_id").Optional(),
		field.String("created_by").Optional(),
		field.String("updated_by").Optional(),
		field.String("visibility").Default("private"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (HarnessConfig) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug", "scope").StorageKey("idx_harness_configs_slug_scope"),
		index.Fields("harness").StorageKey("idx_harness_configs_harness"),
		index.Fields("status").StorageKey("idx_harness_configs_status"),
		index.Fields("content_hash").StorageKey("idx_harness_configs_content_hash"),
		index.Fields("scope", "scope_id").StorageKey("idx_harness_configs_scope_id"),
	}
}
