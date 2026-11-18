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

// WebhookEndpoint holds the schema definition for a project-owned receiving
// webhook address (ptone/scion#7961, Stage 1 §4.1). The HMAC signing secret is
// stored out of band as a hub-scoped SecretTypeInternal row in secrets, never
// on this table.
type WebhookEndpoint struct {
	ent.Schema
}

// Mixin of the WebhookEndpoint. FixtureProvenanceMixin holds the creation-time
// test-identity provenance stamp (ptone/scion#4240, Phase 3a; ptone/scion#8028).
func (WebhookEndpoint) Mixin() []ent.Mixin {
	return []ent.Mixin{
		FixtureProvenanceMixin{},
	}
}

// Fields of the WebhookEndpoint.
func (WebhookEndpoint) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}),
		field.String("name").
			NotEmpty(),
		field.String("description").
			Optional(),
		field.String("owner_principal_kind").
			NotEmpty().
			Immutable(),
		field.String("owner_principal_id").
			NotEmpty().
			Immutable(),
		field.String("owner_credential_kind").
			NotEmpty().
			Immutable(),
		field.Int("secret_version").
			Default(1),
		field.String("signature_mode").
			Default("scion_v1"),
		field.String("status").
			Default("active"),
		field.String("created_by").
			Optional(),
		field.Time("last_received_at").
			Optional().
			Nillable(),
		field.Int("receive_count").
			Default(0),
		field.Int("reject_count").
			Default(0),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the WebhookEndpoint.
func (WebhookEndpoint) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "name").
			Unique(),
	}
}

// Annotations of the WebhookEndpoint.
func (WebhookEndpoint) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "webhook_endpoints", Checks: fixtureStampCheck("webhook_endpoints")},
	}
}
