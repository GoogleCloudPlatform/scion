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

// Template holds the schema definition for the Template entity, mapping the
// legacy SQLite `templates` table (final post-V50 state; grove_id renamed to
// project_id).
//
// JSON columns (config, files) are kept as raw strings to stay dialect-neutral.
// Foreign references (project_id, scope_id, owner ids) are modeled as plain
// strings rather than edges so global/unscoped rows with empty values port
// cleanly; edges can be added later when the full entity migrates.
type Template struct {
	ent.Schema
}

// Fields of the Template.
func (Template) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty(),
		field.String("slug").
			NotEmpty(),
		field.String("display_name").
			Optional(),
		field.String("description").
			Optional(),
		// harness may be empty: a directory template that declares no harness type
		// leaves this blank; the raw-SQL store allowed it and BootstrapTemplatesFromDir
		// relies on storing such templates rather than skipping them.
		field.String("harness"),
		field.String("default_harness_config").
			Optional(),
		field.String("image").
			Optional(),
		field.String("config").
			Optional(),
		field.String("content_hash").
			Optional(),
		field.String("scope").
			Default("global"),
		field.String("scope_id").
			Optional(),
		field.String("project_id").
			Optional(),
		field.String("storage_uri").
			Optional(),
		field.String("storage_bucket").
			Optional(),
		field.String("storage_path").
			Optional(),
		field.String("files").
			Optional(),
		field.String("base_template").
			Optional(),
		field.Enum("status").
			Values("pending", "active", "archived").
			Default("active"),
		field.String("owner_id").
			Optional(),
		field.String("created_by").
			Optional(),
		field.String("updated_by").
			Optional(),
		field.String("source_url").
			Optional(),
		field.String("visibility").
			Default("private"),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the Template.
func (Template) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug", "scope", "scope_id").Unique(),
		index.Fields("harness"),
		index.Fields("status"),
		index.Fields("content_hash"),
	}
}

// Annotations of the Template.
func (Template) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "templates"},
	}
}
