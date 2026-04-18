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
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// GroveSyncState mirrors grove_sync_state (V42). Same composite-PK note as
// GroveContributor: Ent surrogate id + UNIQUE on (grove_id, broker_id).
type GroveSyncState struct {
	ent.Schema
}

func (GroveSyncState) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "grove_sync_state"}}
}

func (GroveSyncState) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("grove_id").NotEmpty(),
		field.String("broker_id").Default(""),
		field.Time("last_sync_time").Optional().Nillable(),
		field.String("last_commit_sha").Optional(),
		field.Int("file_count").Default(0),
		field.Int64("total_bytes").Default(0),
	}
}

func (GroveSyncState) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("grove_id", "broker_id").Unique(),
		index.Fields("grove_id").StorageKey("idx_grove_sync_state_grove"),
	}
}
