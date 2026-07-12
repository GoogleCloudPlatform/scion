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
	"encoding/json"
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// HubSetting stores operational hub settings as JSON documents, one row per
// section (e.g. "access", "telemetry"). This is the persistence backing for
// the two-tier settings architecture (Layer 1 — cluster-shared operational
// settings stored in Postgres).
type HubSetting struct {
	ent.Schema
}

// Fields of the HubSetting.
func (HubSetting) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("section").
			NotEmpty().
			Unique(),
		field.JSON("value", json.RawMessage{}).
			Comment("Section payload; jsonb on Postgres, TEXT on SQLite"),
		field.Int64("revision").
			Default(1).
			Comment("Optimistic concurrency token; incremented on every update"),
		field.String("updated_by").
			Optional().
			Comment("Email of the admin who last wrote this section"),
		field.Enum("origin").
			Values("seeded", "managed").
			Default("seeded").
			Comment("Tracks whether this section is bootstrap-managed (seeded) or admin-owned (managed)"),
		field.Time("create_time").
			Default(time.Now).
			Immutable(),
		field.Time("update_time").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the HubSetting.
func (HubSetting) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("section").Unique(),
	}
}

// Annotations of the HubSetting.
func (HubSetting) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "hub_settings"},
	}
}
