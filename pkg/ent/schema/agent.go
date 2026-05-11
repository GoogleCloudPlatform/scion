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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Agent holds the schema definition for the Agent entity.
// Only principal-relevant fields are included; operational fields
// (ContainerStatus, RuntimeState, etc.) will be added when the
// agent entity is fully migrated to Ent.
type Agent struct {
	ent.Schema
}

// Fields of the Agent.
func (Agent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("slug").
			NotEmpty(),
		field.String("name").
			NotEmpty(),
		field.String("template").
			Optional(),
		field.UUID("project_id", uuid.UUID{}).
			StorageKey("project_id"),
		field.Enum("status").
			Values("created", "provisioning", "cloning", "starting", "running", "suspended", "stopping", "stopped", "error").
			Default("created"),
		field.UUID("created_by", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("owner_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Bool("delegation_enabled").
			Default(false),
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

// Edges of the Agent.
func (Agent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).
			Ref("agents").
			Field("project_id").
			Required().
			Unique(),
		edge.From("creator", User.Type).
			Ref("created_agents").
			Field("created_by").
			Unique(),
		edge.From("owner", User.Type).
			Ref("owned_agents").
			Field("owner_id").
			Unique(),
		edge.From("memberships", GroupMembership.Type).
			Ref("agent"),
		edge.From("policy_bindings", PolicyBinding.Type).
			Ref("agent"),
	}
}

// Indexes of the Agent.
func (Agent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug", "project_id").
			Unique(),
	}
}
