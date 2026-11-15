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

// SystemOneInstance holds the schema definition for the SystemOneInstance
// entity: a Hub-scoped System One provider instance record (runtime design
// §4.2). Only secret name references are stored here — never secret values.
type SystemOneInstance struct {
	ent.Schema
}

// Fields of the SystemOneInstance.
func (SystemOneInstance) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty().
			MaxLen(63).
			Comment("Unique canonical slug identifying the instance (e.g. typesafe-prod)."),
		field.String("adapter").
			NotEmpty().
			Comment("Registered adapter name (e.g. typesafe, google-genai, openai-compat, anthropic, bedrock)."),
		field.String("model").
			Default("").
			Comment("Adapter-specific model identifier; empty for fixed-model adapters such as TypeSafe."),
		field.String("endpoint").
			NotEmpty().
			Comment("Base URL for provider calls; required so no vendor URL is hardcoded in core."),
		field.String("location").
			Default("").
			Comment("Cloud region where applicable (e.g. Vertex, Bedrock)."),
		// Full Auth.Kind enum from runtime design §4.2 so future credential
		// kinds require no schema migration.
		field.Enum("auth_kind").
			Values("none", "api_key", "google_access_token", "google_id_token", "azure_entra").
			Default("api_key"),
		field.String("auth_secret_name").
			Default("").
			Comment("Hub secret name (api_key), GSA ID (google_*), or app secret name (azure_entra); never a secret value."),
		field.String("auth_header_name").
			Default("").
			Comment("Optional HTTP header override when auth_kind is api_key."),
		field.JSON("auth_scopes", []string{}).
			Optional().
			Comment("OAuth scopes when auth_kind is google_access_token."),
		field.String("auth_audience").
			Default("").
			Comment("ID token audience or Entra resource URI."),
		field.JSON("options", map[string]string{}).
			Optional().
			Comment("Non-secret adapter options validated by the adapter."),
		// Full InstanceLimits from runtime design §4.2 stored as scalar
		// columns so P2 rate/token enforcement needs no schema migration.
		field.Int("max_in_flight").
			Default(0).
			Comment("Per-replica concurrency semaphore limit (0 uses runtime default)."),
		field.Float("rate_per_sec").
			Default(0).
			Comment("Per-replica token-bucket rate limit (0 = unlimited)."),
		field.Int("max_input_tokens").
			Default(0).
			Comment("Approximate input token guard before calling provider (0 = adapter cap)."),
		field.Int64("call_timeout_ns").
			Default(0).
			Comment("Per-call timeout in nanoseconds (0 uses runtime default)."),
		field.Bool("allow_private_network").
			Default(false).
			Comment("Explicit admin opt-in to bypass private-IP egress block."),
		field.Bool("enabled").
			Default(true),
		field.Int64("revision").
			Default(1).
			Comment("Bumped on any change to adapter, model, endpoint, auth, options, or limits."),
		field.String("created_by").
			Default(""),
		field.String("updated_by").
			Default(""),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the SystemOneInstance.
func (SystemOneInstance) Edges() []ent.Edge {
	return nil
}

// Indexes of the SystemOneInstance.
func (SystemOneInstance) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("name").Unique(),
		index.Fields("adapter"),
		index.Fields("enabled"),
	}
}
