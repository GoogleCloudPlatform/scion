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

// UserDeletionAgent is one blocking agent of a forced user deletion
// (UserDeletion) and the cascade's latest view of it. agent_id has no
// foreign key: the agent row may be hard-deleted while the record stays.
//
// outcome is pending (not claimed), stopping (a delete is in flight),
// failed (the agent's delete failed; last_error says why), stopped (the
// delete engine confirmed runtime teardown) or removed (the row is gone
// and the cascade did not confirm teardown).
type UserDeletionAgent struct {
	ent.Schema
}

// Fields of the UserDeletionAgent.
func (UserDeletionAgent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("deletion_id", uuid.UUID{}).
			Immutable(),
		field.UUID("agent_id", uuid.UUID{}).
			Immutable(),
		field.String("slug").
			Default(""),
		field.String("project_id").
			Default(""),
		field.String("outcome").
			NotEmpty().
			Comment("pending, stopping, failed, stopped or removed (store.UserDeletionOutcome*)"),
		field.Int("attempts").
			Default(0).
			NonNegative(),
		field.Text("last_error").
			Default(""),
		field.Time("updated_at").
			Default(time.Now),
	}
}

// Edges of the UserDeletionAgent.
func (UserDeletionAgent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("deletion", UserDeletion.Type).
			Ref("agents").
			Field("deletion_id").
			Unique().
			Required().
			Immutable(),
	}
}

// Indexes of the UserDeletionAgent.
func (UserDeletionAgent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("deletion_id", "agent_id").
			Unique(),
		// The delete engine's finish looks up the deletions an agent
		// belongs to.
		index.Fields("agent_id"),
	}
}

// Annotations of the UserDeletionAgent.
func (UserDeletionAgent) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "user_deletion_agents"},
	}
}
