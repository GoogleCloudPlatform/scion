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
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ProjectContributor holds the schema definition for the ProjectContributor
// entity, mapping the legacy SQLite `project_contributors` table (was
// `grove_contributors` before the V50 grove→project rename). It records which
// runtime brokers contribute to / provide for a project.
//
// SQLite used a composite primary key (project_id, broker_id) with no id column;
// Ent prefers a single id, so a surrogate UUID id is added and the original key
// is enforced via a unique index. profiles is a raw JSON string.
type ProjectContributor struct {
	ent.Schema
}

// Fields of the ProjectContributor.
func (ProjectContributor) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}),
		field.UUID("broker_id", uuid.UUID{}),
		field.String("broker_name").
			NotEmpty(),
		field.String("mode").
			Default("connected"),
		field.String("status").
			Default("offline"),
		field.String("profiles").
			Optional(),
		field.Time("last_seen").
			Optional().
			Nillable(),
		field.String("local_path").
			Optional(),
		field.String("linked_by").
			Optional(),
		field.Time("linked_at").
			Optional().
			Nillable(),
	}
}

// Indexes of the ProjectContributor.
func (ProjectContributor) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "broker_id").
			Unique(),
		index.Fields("broker_id"),
	}
}

// Annotations of the ProjectContributor.
func (ProjectContributor) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "project_contributors"},
	}
}
