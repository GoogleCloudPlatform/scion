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
	"github.com/google/uuid"
)

// BrokerSecret holds the schema definition for the BrokerSecret entity, mapping
// the legacy SQLite `broker_secrets` table.
//
// The primary key is broker_id (one secret per runtime broker), so the id field
// is stored in the broker_id column with no generated default. secret_key is a
// binary HMAC key (SQLite BLOB → Postgres bytea) and is marked Sensitive.
type BrokerSecret struct {
	ent.Schema
}

// Fields of the BrokerSecret.
func (BrokerSecret) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			StorageKey("broker_id").
			Immutable(),
		field.Bytes("secret_key").
			Sensitive().
			NotEmpty(),
		field.String("algorithm").
			Default("hmac-sha256"),
		field.Time("rotated_at").
			Optional().
			Nillable(),
		field.Time("expires_at").
			Optional().
			Nillable(),
		field.String("status").
			Default("active"),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Annotations of the BrokerSecret.
func (BrokerSecret) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "broker_secrets"},
	}
}
