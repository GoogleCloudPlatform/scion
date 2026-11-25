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

// TriggerSubjectClaim holds the per-(trigger, subject) live-subject claim row
// used by dispatch_agent triggers with subject mode "skip" (Stage 2 §5.6) and
// Stage 3 "reuse". One row per (trigger_id, subject) is reused across claims.
type TriggerSubjectClaim struct {
	ent.Schema
}

// Fields of the TriggerSubjectClaim.
func (TriggerSubjectClaim) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("trigger_id", uuid.UUID{}).
			Immutable(),
		field.Int("action_index").
			Default(0).
			Immutable(),
		field.String("subject").
			MaxLen(256).
			Immutable(),
		field.String("state").
			Default("active"),
		field.UUID("agent_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("event_id", uuid.UUID{}),
		field.UUID("fire_id", uuid.UUID{}),
		field.Time("claimed_at").
			Default(time.Now),
		field.Time("bound_at").
			Optional().
			Nillable(),
		field.Time("released_at").
			Optional().
			Nillable(),
		field.String("release_reason").
			Optional(),
	}
}

// Indexes of the TriggerSubjectClaim.
func (TriggerSubjectClaim) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("trigger_id", "action_index", "subject").
			Unique(),
		index.Fields("agent_id"),
		index.Fields("state", "released_at"),
	}
}

// Annotations of the TriggerSubjectClaim.
func (TriggerSubjectClaim) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "trigger_subject_claims"},
	}
}
