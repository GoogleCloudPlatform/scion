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
	"github.com/google/uuid"
)

// DecisionKeyCounter is the per-agent counter of hub-assigned decision keys
// (choices, ptone/scion#7514). next is the number the hub issues next; it
// only grows, in the transaction that stores the choice prompt, so numbers
// are never reused, even after a decision is replaced. An agent may repost
// a number below next (one it was given); any other all-digit key is
// refused.
type DecisionKeyCounter struct {
	ent.Schema
}

// Fields of the DecisionKeyCounter.
func (DecisionKeyCounter) Fields() []ent.Field {
	return []ent.Field{
		// The agent's ID is the primary key.
		field.UUID("id", uuid.UUID{}).
			StorageKey("agent_id").
			Immutable(),
		field.Int64("next").
			Positive(),
	}
}

// Annotations of the DecisionKeyCounter.
func (DecisionKeyCounter) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "decision_key_counters"},
	}
}
