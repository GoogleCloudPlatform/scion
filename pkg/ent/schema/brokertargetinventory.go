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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// BrokerTargetInventory records, per runtime broker and runtime target, the
// store-clock time of the last heartbeat that listed that target's inventory
// as complete. Observations of agents on the target are current only as of
// this time. It is written only by AgentStore.RecordRecoveryObservations: it
// is not a runtime_brokers column, so writing a broker record back from
// memory cannot roll it back. Rows are deleted with the broker.
type BrokerTargetInventory struct {
	ent.Schema
}

// Fields of the BrokerTargetInventory.
func (BrokerTargetInventory) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("broker_id").
			NotEmpty().
			Immutable(),
		field.String("target").
			NotEmpty().
			Immutable(),
		field.Time("last_complete_inventory_at"),
	}
}

// Indexes of the BrokerTargetInventory.
func (BrokerTargetInventory) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("broker_id", "target").
			Unique(),
	}
}
