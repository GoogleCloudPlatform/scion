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

// ThreadGroupMember holds the schema definition for the ThreadGroupMember
// entity (ptone/scion#4424): one group conversation filed into one thread
// group.
//
// group_id, project_id and conversation_id are plain UUID columns with no Ent
// edges. project_id is denormalized from the group so project cleanup is a
// single statement, backed by the project_id index. The unique
// conversation_id index makes a thread belong to at most one project group;
// filing it elsewhere moves it.
type ThreadGroupMember struct {
	ent.Schema
}

// Fields of the ThreadGroupMember.
func (ThreadGroupMember) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("group_id", uuid.UUID{}),
		field.UUID("project_id", uuid.UUID{}),
		field.UUID("conversation_id", uuid.UUID{}),
		field.String("added_by"),
		field.Time("added_at").
			Default(time.Now),
	}
}

// Indexes of the ThreadGroupMember.
func (ThreadGroupMember) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("conversation_id").
			Unique(),
		index.Fields("group_id"),
		// Project cleanup (DeleteThreadGroupsForProject, run inside
		// CompositeStore.DeleteProject) deletes by project_id.
		index.Fields("project_id"),
	}
}

// Annotations of the ThreadGroupMember.
func (ThreadGroupMember) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "thread_group_members"},
	}
}
