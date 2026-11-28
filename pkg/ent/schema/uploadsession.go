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

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// UploadSession holds the durable state for a resumable, HA-safe chunked web
// upload (ptone/scion#8654 §3.4). Any hub replica may serve any request for a
// session by claiming a short lease on the row with a conditional UPDATE.
type UploadSession struct {
	ent.Schema
}

// Fields of the UploadSession.
func (UploadSession) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			NotEmpty().
			Immutable(),
		field.String("hub_id").
			NotEmpty().
			Immutable(),
		field.String("owner_kind").
			NotEmpty().
			Immutable(),
		field.String("owner_id").
			NotEmpty().
			Immutable(),
		field.Enum("target_kind").
			Values("chat_attachment", "workspace", "shared_dir").
			Immutable(),
		field.String("scope_project_id").
			Optional().
			Nillable().
			Immutable(),
		field.String("scope_shared_dir").
			Optional().
			Nillable().
			Immutable(),
		field.String("dest_path").
			Optional().
			Nillable().
			Immutable(),
		field.Bool("overwrite").
			Default(false).
			Immutable(),
		field.String("file_name").
			NotEmpty().
			Immutable(),
		field.String("media_type").
			Default("").
			Immutable(),
		field.Int64("declared_size").
			NonNegative().
			Immutable(),
		field.Int64("received").
			Default(0).
			NonNegative(),
		field.Bytes("hash_state").
			Optional(),
		field.Bytes("head").
			Optional().
			Nillable(),
		field.JSON("parts", []string{}).
			Optional(),
		field.String("staging_ref").
			NotEmpty().
			Immutable(),
		field.Enum("state").
			Values("uploading", "finalizing", "completed", "failed", "aborted", "expired").
			Default("uploading"),
		field.String("lease_token").
			Optional().
			Nillable(),
		field.Time("lease_until").
			Optional().
			Nillable(),
		field.Int("takeovers").
			Default(0).
			NonNegative(),
		field.JSON("result", json.RawMessage{}).
			Optional(),
		field.String("artifact_id").
			Optional().
			Nillable().
			Immutable(),
		field.Time("idle_expires_at"),
		field.Time("hard_expires_at").
			Immutable(),
		field.Time("created_at").
			Immutable(),
		field.Time("updated_at"),
	}
}

// Indexes of the UploadSession.
func (UploadSession) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("owner_kind", "owner_id", "state"),
		index.Fields("state", "idle_expires_at"),
		index.Fields("state", "hard_expires_at"),
	}
}

// Annotations of the UploadSession.
func (UploadSession) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "upload_sessions"},
	}
}
