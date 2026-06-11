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

// SkillVersion holds the schema definition for the SkillVersion entity.
type SkillVersion struct {
	ent.Schema
}

// Fields of the SkillVersion.
func (SkillVersion) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("skill_id").
			NotEmpty(),
		field.String("version").
			NotEmpty(),
		field.Enum("status").
			Values("draft", "published", "deprecated", "archived").
			Default("draft"),
		field.String("content_hash").
			Optional(),
		field.String("files").
			Optional(),
		field.String("publisher_id").
			Optional(),
		field.String("deprecation_message").
			Optional(),
		field.String("replacement_uri").
			Optional(),
		field.Int64("download_count").
			Default(0),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the SkillVersion.
func (SkillVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("skill_id", "version").Unique(),
		index.Fields("skill_id", "status"),
	}
}

// Annotations of the SkillVersion.
func (SkillVersion) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "skill_versions"},
	}
}
