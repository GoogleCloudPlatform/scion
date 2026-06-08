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
)

// LifecycleHookAgentPhase tracks the last-processed lifecycle-hook phase per
// agent. Used for HA transition de-duplication: across multiple hub instances,
// the single instance whose compare-and-set succeeds "wins" and fires hooks;
// all others see changed=false and skip.
//
// This entity replaces the raw-SQL lifecycle_hook_agent_phase table from the
// reference implementation; it uses ent's sql/upsert feature for atomic CAS.
type LifecycleHookAgentPhase struct {
	ent.Schema
}

// Fields of the LifecycleHookAgentPhase.
func (LifecycleHookAgentPhase) Fields() []ent.Field {
	return []ent.Field{
		// agent_id is the primary key (string UUID of the agent). Unique
		// ensures one row per agent.
		field.String("agent_id").
			NotEmpty().
			Immutable().
			Unique(),
		field.String("last_phase").
			NotEmpty(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Annotations of the LifecycleHookAgentPhase.
func (LifecycleHookAgentPhase) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "lifecycle_hook_agent_phases"},
	}
}
