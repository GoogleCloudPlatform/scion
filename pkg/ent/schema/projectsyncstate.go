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

// ProjectSyncState holds the schema definition for the ProjectSyncState entity,
// mapping the legacy SQLite `project_sync_state` table (was `grove_sync_state`
// before the V50 grove→project rename).
//
// SQLite used a composite primary key (project_id, broker_id) with no id column;
// Ent prefers a single id, so a surrogate UUID id is added and the original key
// is enforced via a unique index. broker_id is a plain string (it defaulted to
// ” in SQLite for project-wide sync state).
type ProjectSyncState struct {
	ent.Schema
}

// Fields of the ProjectSyncState.
func (ProjectSyncState) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}),
		field.String("broker_id").
			Default(""),
		field.Time("last_sync_time").
			Optional().
			Nillable(),
		field.String("last_commit_sha").
			Optional(),
		field.Int("file_count").
			Default(0),
		field.Int64("total_bytes").
			Default(0),
	}
}

// Indexes of the ProjectSyncState.
func (ProjectSyncState) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "broker_id").
			Unique(),
	}
}

// Annotations of the ProjectSyncState.
func (ProjectSyncState) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "project_sync_state"},
	}
}
