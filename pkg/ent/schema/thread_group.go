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

// ThreadGroup holds the schema definition for the ThreadGroup entity
// (ptone/scion#4424). A thread group is a named, project-scoped folder of
// group conversations (chat threads) that every project member sees.
//
// project_id is a plain UUID column with no Ent edge, the same as
// Conversation. name_key is the case-folded name computed in Go by
// store.ThreadGroupNameKey; the unique (project_id, name_key) index compares
// it byte for byte, so SQLite and Postgres behave the same with no database
// collation.
type ThreadGroup struct {
	ent.Schema
}

// Fields of the ThreadGroup.
func (ThreadGroup) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}),
		field.String("name").
			NotEmpty(),
		field.String("name_key").
			NotEmpty(),
		field.String("created_by"),
		field.Enum("created_by_kind").
			Values("user", "agent"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the ThreadGroup.
func (ThreadGroup) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "name_key").
			Unique(),
		index.Fields("project_id"),
	}
}

// Annotations of the ThreadGroup.
func (ThreadGroup) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "thread_groups"},
	}
}
