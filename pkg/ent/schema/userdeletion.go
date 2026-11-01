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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// UserDeletion is the tombstone of a forced user deletion
// (ptone/scion#2143): one row per user, written in the same transaction
// that revokes the user's authority and sets the user's status to
// deleting. Its ID is the operation ID recorded on every delegation edge
// and service-account assignment that transaction deactivated. The row
// outlives the user row, so a deleted user's ID keeps resolving as
// deleted and is never reused; user_id is unique and has no foreign key.
//
// state is stopping until every blocking agent is stopped (runtime
// teardown confirmed) or removed (row gone without that confirmation),
// then completed, in the transaction that deletes the user row.
type UserDeletion struct {
	ent.Schema
}

// Fields of the UserDeletion.
func (UserDeletion) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("user_id", uuid.UUID{}).
			Unique().
			Immutable().
			Comment("The deleted user; no foreign key, the row outlives the user row"),
		field.String("state").
			NotEmpty().
			Comment("stopping or completed (store.UserDeletionState*)"),
		field.String("requested_by_kind").
			Default("").
			Immutable(),
		field.String("requested_by_id").
			Default("").
			Immutable(),
		field.String("credential_kind").
			Default("").
			Immutable(),
		field.Time("requested_at").
			Immutable(),
		field.Time("completed_at").
			Optional().
			Nillable(),
		field.Time("last_reconciled_at").
			Optional().
			Nillable(),
		field.Text("before_summary").
			Default("").
			Immutable().
			Comment("JSON: the user's role, status and kind before the request (no email)"),
		field.Text("orphaned_by_revoke").
			Default("").
			Immutable().
			Comment("JSON: edges the request deactivated whose delegate is not a stopped agent"),
		field.Text("last_error").
			Default("").
			Comment("Latest reconcile error, shown to operators"),
	}
}

// Edges of the UserDeletion.
func (UserDeletion) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("agents", UserDeletionAgent.Type).
			Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}

// Indexes of the UserDeletion.
func (UserDeletion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("state"),
	}
}

// Annotations of the UserDeletion.
func (UserDeletion) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "user_deletions"},
	}
}
