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

// BrokerJoinToken holds the schema definition for the BrokerJoinToken entity,
// mapping the legacy SQLite `broker_join_tokens` table.
//
// The primary key is broker_id (one active join token per runtime broker), so
// the id field is stored in the broker_id column with no generated default.
type BrokerJoinToken struct {
	ent.Schema
}

// Fields of the BrokerJoinToken.
func (BrokerJoinToken) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			StorageKey("broker_id").
			Immutable(),
		field.String("token_hash").
			Unique().
			NotEmpty(),
		field.Time("expires_at"),
		field.String("created_by").
			NotEmpty(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the BrokerJoinToken.
func (BrokerJoinToken) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("expires_at"),
	}
}

// Annotations of the BrokerJoinToken.
func (BrokerJoinToken) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "broker_join_tokens"},
	}
}
