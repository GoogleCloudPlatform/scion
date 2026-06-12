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

// SkillRegistry holds the schema definition for the SkillRegistry entity.
type SkillRegistry struct {
	ent.Schema
}

// Fields of the SkillRegistry.
func (SkillRegistry) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty().
			Unique(),
		field.String("endpoint").
			NotEmpty(),
		field.String("description").
			Optional().
			Default(""),
		field.Enum("type").
			Values("hub", "gcp").
			Default("hub"),
		field.Enum("trust_level").
			Values("trusted", "pinned").
			Default("pinned"),
		field.String("auth_token").
			Optional().
			Sensitive(),
		field.String("resolve_path").
			Optional().
			Default("/api/v1/skills/resolve"),
		field.String("pinned_hashes").
			Optional(),
		field.Enum("status").
			Values("active", "disabled").
			Default("active"),
		field.String("created_by").
			Optional(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the SkillRegistry.
func (SkillRegistry) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("name").Unique(),
		index.Fields("status"),
	}
}

// Annotations of the SkillRegistry.
func (SkillRegistry) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "skill_registries"},
	}
}
