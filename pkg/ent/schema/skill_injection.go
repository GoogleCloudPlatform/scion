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

// SkillInjection represents one entry in an injected-skills list for a
// project, user, or (future) other scope. Hub scope is handled via hub_settings.
type SkillInjection struct{ ent.Schema }

// Fields of the SkillInjection.
func (SkillInjection) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.Enum("scope").Values("project", "user"), // closed set; hub scope is via hub_settings
		field.String("scope_id"),                      // project UUID or user UUID
		field.String("skill_uri"),                     // full skill URI (may include version pin)
		field.String("skill_as").Optional(),           // alias
		field.Bool("optional").Default(false),
		field.Bool("allow_progeny").Default(false),
		field.Int("sort_order").Default(0),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.String("created_by").Optional(),
	}
}

// Indexes of the SkillInjection.
func (SkillInjection) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("scope", "scope_id"),
		// Prevent duplicate skill URIs within the same scope+scopeID.
		index.Fields("scope", "scope_id", "skill_uri").Unique(),
	}
}
