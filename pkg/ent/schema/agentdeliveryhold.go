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

// AgentDeliveryHold is the per-agent hold marker for deliver-message
// hold-until-session re-drive (ptone/scion#7881; table agent_delivery_holds).
//
// One row exists per agent that currently has messages in dispatch_state
// 'awaiting_session': created in the same transaction as the agent's first
// held message, updated as additional messages enter or leave
// 'awaiting_session', and deleted in the same transaction that drains or
// expires the agent's last held message.
type AgentDeliveryHold struct {
	ent.Schema
}

// Fields of the AgentDeliveryHold.
func (AgentDeliveryHold) Fields() []ent.Field {
	return []ent.Field{
		// id is stored as agent_id (PRIMARY KEY).
		field.String("id").
			StorageKey("agent_id").
			NotEmpty().
			Immutable(),
		field.Time("hold_until"),
		field.String("launch_id").
			Optional().
			Default("").
			MaxLen(128),
		field.Int("held_count").
			Default(0),
		field.Time("created").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the AgentDeliveryHold.
func (AgentDeliveryHold) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("hold_until"),
		index.Fields("created"),
	}
}

// Annotations of the AgentDeliveryHold.
func (AgentDeliveryHold) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "agent_delivery_holds"},
	}
}
