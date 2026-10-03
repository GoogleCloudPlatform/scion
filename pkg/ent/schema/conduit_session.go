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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ConduitSession is one row per live Conduit session (design conduit v2.1
// §3.4, table conduit_sessions). There is deliberately NO uniqueness on
// (principal_kind, principal_id): several live sessions per principal are
// legal. Deletes are CAS on (session_id, relay_instance_id,
// relay_generation). connection_epoch is allocated from
// conduit_principal_epochs in the same transaction as the insert.
type ConduitSession struct {
	ent.Schema
}

// Fields of the ConduitSession.
func (ConduitSession) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("session_id").
			NotEmpty().
			Immutable(),
		field.String("principal_kind").
			NotEmpty().
			Immutable().
			Comment("broker | agent | user | relay-peer"),
		field.String("principal_id").
			NotEmpty().
			Immutable(),
		field.String("project_id").
			Optional().
			Nillable().
			Immutable().
			Comment("For agents; authz scoping without a join"),
		field.String("relay_instance_id").
			NotEmpty().
			Immutable(),
		field.Int64("relay_generation").
			Immutable(),
		field.String("transport").
			NotEmpty().
			Immutable().
			Comment("ws | grpc | h1pair"),
		field.String("endpoint_incarnation").
			Immutable().
			Comment("agent: container incarnation id; broker: process start id"),
		field.String("exec_scope").
			Optional().
			Nillable().
			Immutable().
			Comment("Runtime execution scope; sessions with different exec_scope are never interchangeable"),
		field.Int64("connection_epoch").
			Immutable().
			Comment("Per-principal, strictly increasing; from conduit_principal_epochs"),
		field.Bool("draining").
			Default(false),
		field.JSON("capabilities", json.RawMessage{}).
			Default(json.RawMessage("{}")).
			Comment("Capabilities document (contracts §3); jsonb on Postgres, TEXT on SQLite"),
		field.Time("connected_at").
			Immutable(),
		field.Time("last_seen").
			Comment("Bumped on pong, every 30s"),
	}
}

// Edges of the ConduitSession.
func (ConduitSession) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("relay", RelayInstance.Type).
			Ref("sessions").
			Field("relay_instance_id").
			Unique().
			Required().
			Immutable(),
	}
}

// Indexes of the ConduitSession.
func (ConduitSession) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("principal_kind", "principal_id", "last_seen"),
		index.Fields("relay_instance_id", "relay_generation"),
	}
}

// Annotations of the ConduitSession.
func (ConduitSession) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "conduit_sessions"},
	}
}
