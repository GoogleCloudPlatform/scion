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

// IntegrationConfig stores non-secret per-integration configuration as JSON.
// Secrets are managed separately via the SecretBackend. This table is hub-owned
// and used by Mode 3 (HA) integrations that read config from Postgres instead
// of local YAML files.
type IntegrationConfig struct {
	ent.Schema
}

// Fields of the IntegrationConfig.
func (IntegrationConfig) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("integration").
			NotEmpty().
			Unique(),
		field.String("config").
			Default("{}"),
		field.Bool("enabled").
			Default(true),
		field.String("updated_by").
			Optional(),
		field.Time("create_time").
			Default(time.Now).
			Immutable(),
		field.Time("update_time").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the IntegrationConfig.
func (IntegrationConfig) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("integration").Unique(),
	}
}

// Annotations of the IntegrationConfig.
func (IntegrationConfig) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "integration_configs"},
	}
}
