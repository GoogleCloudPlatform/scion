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

// ProjectPreStartHook holds the schema definition for the ProjectPreStartHook
// entity. Each row is a named, versioned shell script that can be associated
// with a project (scope="project") or with the hub as a whole (scope="hub")
// and staged into the agent container's pre-start hook directory
// (pre-start.d/30-project-custom) before the agent process starts.
//
// One hook has status="active" per project at any given time, plus at most one
// active hub-scoped hook; the rest are "archived". This invariant is enforced
// by the store layer (not the DB schema) so that the implementation stays
// portable across SQLite and Postgres.
//
// Hub-scoped rows carry an empty project_id. The project-scoped hook (if any)
// takes precedence over the hub-scoped hook at agent-create time.
type ProjectPreStartHook struct {
	ent.Schema
}

// Fields of the ProjectPreStartHook.
func (ProjectPreStartHook) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		// scope distinguishes project-scoped hooks from hub-wide hooks.
		// Hub-scoped hooks act as the fallback for projects with no active
		// project-scoped hook.
		field.Enum("scope").
			Values("project", "hub").
			Default("project"),
		// project_id is empty for hub-scoped hooks. It is Optional (rather
		// than NotEmpty) for that reason; the store layer requires a
		// non-empty value for project-scoped hooks.
		field.String("project_id").
			Optional().
			Default(""),
		field.String("name").
			NotEmpty(),
		field.String("slug").
			NotEmpty(),
		field.String("description").
			Optional(),
		// script holds the raw script content (e.g. #!/bin/sh ...).
		// Size is capped at 64 KB by the Hub API layer, not here.
		field.String("script").
			NotEmpty(),
		field.Enum("status").
			Values("active", "archived").
			Default("active"),
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

// Indexes of the ProjectPreStartHook.
func (ProjectPreStartHook) Indexes() []ent.Index {
	return []ent.Index{
		// slug must be unique within a scope+project. Including scope keeps
		// hub-scoped hooks (project_id="") from colliding with project-scoped
		// hooks that happen to share a slug.
		index.Fields("scope", "project_id", "slug").Unique(),
		// Efficient lookup of all active hooks for a project.
		index.Fields("project_id", "status"),
		// Efficient lookup of the active hub-scoped hook.
		index.Fields("scope", "status"),
	}
}

// Annotations of the ProjectPreStartHook.
func (ProjectPreStartHook) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "project_pre_start_hooks"},
	}
}
