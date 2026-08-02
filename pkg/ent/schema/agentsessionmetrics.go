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

// AgentSessionMetrics holds the schema definition for the AgentSessionMetrics
// entity, storing pre-aggregated session-level telemetry reported by sciontool
// on session-end.
type AgentSessionMetrics struct {
	ent.Schema
}

// Fields of the AgentSessionMetrics.
func (AgentSessionMetrics) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("agent_id").
			NotEmpty(),
		field.String("grove_id").
			NotEmpty(),
		field.String("session_id").
			NotEmpty(),
		field.Time("started_at"),
		field.Time("ended_at").
			Optional().
			Nillable(),
		field.String("status").
			Optional(),
		field.Int("turn_count").
			Optional().
			Default(0),
		field.String("model").
			Optional(),
		field.Int64("tokens_input").
			Optional().
			Default(0),
		field.Int64("tokens_output").
			Optional().
			Default(0),
		field.Int64("tokens_cached").
			Optional().
			Default(0),
		field.Int64("tokens_reasoning").
			Optional().
			Default(0),
		// tool_calls stored as JSON
		field.JSON("tool_calls", map[string]any{}).
			Optional(),
		// languages stored as JSON
		field.JSON("languages", []string{}).
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the AgentSessionMetrics.
func (AgentSessionMetrics) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id"),
		index.Fields("grove_id"),
		index.Fields("started_at"),
	}
}

// Annotations of the AgentSessionMetrics.
func (AgentSessionMetrics) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "agent_session_metrics"},
	}
}
