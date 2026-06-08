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

// LifecycleHook holds the schema definition for the LifecycleHook entity.
// A LifecycleHook is a Hub database record, authored by hub administrators,
// that fires an HTTP/webhook action when a matching agent crosses an
// authoritative phase transition (trigger). It is a sibling of AccessPolicy.
type LifecycleHook struct {
	ent.Schema
}

// Fields of the LifecycleHook.
func (LifecycleHook) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty(),
		field.Enum("scope_type").
			Values("hub", "project").
			Default("hub"),
		field.String("scope_id").
			Optional(),
		field.JSON("selector", &LifecycleHookSelector{}).
			Optional(),
		field.Enum("trigger").
			Values("running", "suspended", "stopped", "error"),
		field.JSON("action", &LifecycleHookAction{}).
			Optional(),
		field.String("execution_identity").
			Optional(),
		field.Bool("enabled").
			Default(true),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
		field.String("created_by").
			Optional(),
		// state_version provides optimistic-locking, mirroring the existing
		// agent optimistic-locking pattern.
		field.Int64("state_version").
			Default(1),
	}
}

// Indexes of the LifecycleHook.
func (LifecycleHook) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("scope_type", "scope_id"),
		index.Fields("trigger"),
		index.Fields("enabled"),
	}
}
