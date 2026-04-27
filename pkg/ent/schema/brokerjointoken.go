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
)

// BrokerJoinToken mirrors broker_join_tokens (V9). PK is broker_id.
type BrokerJoinToken struct {
	ent.Schema
}

func (BrokerJoinToken) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "broker_join_tokens"}}
}

func (BrokerJoinToken) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("broker_id"),
		field.String("token_hash").Unique().NotEmpty(),
		field.Time("expires_at"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.String("created_by").NotEmpty(),
	}
}

func (BrokerJoinToken) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("token_hash").StorageKey("idx_broker_join_tokens_hash"),
		index.Fields("expires_at").StorageKey("idx_broker_join_tokens_expires"),
	}
}
