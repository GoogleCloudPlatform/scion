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

// Trigger holds the schema definition for a project-scoped event trigger
// definition (ptone/scion#7961, Stage 1 §4.2). Its attribution, authority
// ceiling, and fixture provenance mixins match Schedule verbatim (§4.6).
type Trigger struct {
	ent.Schema
}

// Mixin of the Trigger.
func (Trigger) Mixin() []ent.Mixin {
	return []ent.Mixin{
		InitiatorAttributionMixin{},
		EffectCeilingMixin{Prefix: "authority_"},
		FixtureProvenanceMixin{},
	}
}

// Fields of the Trigger.
func (Trigger) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}),
		field.String("name").
			NotEmpty(),
		field.String("description").
			Optional(),
		field.String("source_kind").
			NotEmpty(),
		field.String("source_id").
			Default(""),
		field.Text("match_types").
			NotEmpty(),
		field.String("match_dialect").
			Default("cel"),
		field.Text("match_rule").
			Default(""),
		field.Text("match_rules").
			Default("[]"),
		field.String("action_type").
			NotEmpty(),
		field.Text("action_payload").
			Default("{}"),
		field.String("subject_dialect").
			Default("cel"),
		field.Text("subject_rule").
			Default(""),
		field.String("subject_mode").
			Default("new"),
		field.String("status").
			Default("active"),
		field.String("status_reason").
			Optional(),
		field.Time("last_fire_at").
			Optional().
			Nillable(),
		field.String("last_fire_status").
			Optional(),
		field.String("last_fire_error").
			Optional(),
		field.Int("fire_count").
			Default(0),
		field.Int("error_count").
			Default(0),
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

// Indexes of the Trigger.
func (Trigger) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "name").
			Unique(),
		index.Fields("project_id", "source_kind", "source_id", "status"),
		index.Fields("initiator_principal_id"),
	}
}

// Annotations of the Trigger.
func (Trigger) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "triggers", Checks: fixtureStampCheck("triggers")},
	}
}
