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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// DecisionFacetEntry is a per-(project, conversation, kind, actor) summary row
// backing Decisions facet reads, project-agent lookups, and
// ProjectsWithDecisions (design-scale §3.2, ptone/scion#8982).
//
// Rows with NULL chat_key on decisions are not summarized. ref_count tracks
// contributing rows (decisions for kind=asker; non-superseded answers with a
// non-empty answered_by_user_id for kind=answerer). Answerer rows whose
// ref_count reaches 0 remain with ref_count=0 and are filtered out on read.
type DecisionFacetEntry struct {
	ent.Schema
}

// Fields of the DecisionFacetEntry.
func (DecisionFacetEntry) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}).
			Immutable(),
		// chat_key is never NULL; decisions with NULL chat_key are not summarized.
		field.String("chat_key").
			NotEmpty().
			MaxLen(256).
			Immutable(),
		field.Enum("kind").
			Values("asker", "answerer").
			Immutable(),
		// actor_id is the asking agent UUID (kind=asker) or answering user ID
		// (kind=answerer).
		field.String("actor_id").
			NotEmpty().
			Immutable(),
		// name is the newest contributing row's asker_agent_name / answered_by_name
		// snapshot (tie-break: greater name).
		field.String("name").
			Default(""),
		// email is the newest contributing answer's answered_by_email snapshot
		// (answerer only; empty for asker).
		field.String("email").
			Default(""),
		// latest_at is the newest contributing row's created_at.
		field.Time("latest_at"),
		// ref_count is the number of contributing rows (>= 0).
		field.Int("ref_count").
			Default(0).
			NonNegative(),
	}
}

// Indexes of the DecisionFacetEntry.
func (DecisionFacetEntry) Indexes() []ent.Index {
	return []ent.Index{
		// Natural key for ON CONFLICT upserts and conversation-scoped facet reads.
		index.Fields("project_id", "chat_key", "kind", "actor_id").
			Unique(),
		// Project-scoped asker lookups (ListProjectDecisionAgentsForProjects,
		// ProjectsWithDecisions).
		index.Fields("project_id", "kind", "actor_id"),
	}
}

// Annotations of the DecisionFacetEntry.
func (DecisionFacetEntry) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "decision_facet_entries"},
	}
}
