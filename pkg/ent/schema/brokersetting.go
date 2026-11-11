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

// BrokerSetting stores general per-broker settings as a single JSON document,
// one row per runtime broker (ptone/scion#2061 P2, ptone/scion#2177). The
// first key in the document is maxAgents, a per-broker override of the
// max_agents_per_broker quota (see pkg/hub/brokersettings). The table is
// separate from runtime_brokers so that heartbeats and registration, which
// rewrite runtime_brokers constantly, never contend with settings writes
// (design.md §5.1).
type BrokerSetting struct {
	ent.Schema
}

// Fields of the BrokerSetting.
func (BrokerSetting) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("broker_id").
			NotEmpty(),
		field.JSON("value", json.RawMessage{}).
			Comment("BrokerSettings document; jsonb on Postgres, TEXT on SQLite"),
		field.Int64("revision").
			Default(1).
			Comment("Optimistic concurrency token; incremented on every update"),
		field.String("updated_by").
			Optional().
			Comment("Identity ID (store.Identity.ID(), e.g. a user ID) of whoever last wrote this document"),
		field.Time("create_time").
			Default(time.Now).
			Immutable(),
		field.Time("update_time").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the BrokerSetting.
func (BrokerSetting) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("broker_id").Unique(),
	}
}

// Annotations of the BrokerSetting.
func (BrokerSetting) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "broker_settings"},
	}
}
