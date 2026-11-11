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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ProjectVisitorExclusion holds the schema definition for a project visitor
// exclusion: a user whose visitor binding created by a project visitor rule
// was revoked, so the rule does not create it again. Removing the rule
// deletes its exclusions.
type ProjectVisitorExclusion struct {
	ent.Schema
}

// Fields of the ProjectVisitorExclusion.
func (ProjectVisitorExclusion) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("rule_id", uuid.UUID{}).
			Immutable().
			Comment("The project visitor rule the user is excluded from"),
		field.String("user_id").
			NotEmpty().
			Immutable().
			Comment("The federated account the rule no longer binds"),
		field.String("created_by").
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the ProjectVisitorExclusion.
func (ProjectVisitorExclusion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("rule_id", "user_id").Unique(),
	}
}
