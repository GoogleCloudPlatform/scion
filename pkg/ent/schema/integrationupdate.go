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

// IntegrationUpdate tracks update requests for HA (Mode 3) integrations.
// When an admin triggers an update, a row is created in "requested" state;
// the standalone integration service acknowledges, performs the update, and
// writes back the final state.
type IntegrationUpdate struct {
	ent.Schema
}

// Fields of the IntegrationUpdate.
func (IntegrationUpdate) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("integration").
			NotEmpty(),
		field.Enum("state").
			Values("requested", "acknowledged", "updating", "completed", "failed").
			Default("requested"),
		field.String("detail").
			Optional(),
		field.String("new_version").
			Optional(),
		field.String("requested_by").
			Optional(),
		field.Time("create_time").
			Default(time.Now).
			Immutable(),
		field.Time("update_time").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the IntegrationUpdate.
func (IntegrationUpdate) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("integration"),
		index.Fields("integration", "create_time"),
	}
}

// Annotations of the IntegrationUpdate.
func (IntegrationUpdate) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "integration_updates"},
	}
}
