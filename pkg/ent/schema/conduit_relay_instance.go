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
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// RelayInstance is one row per running Conduit relay process (design
// conduit v2.1 §3.4, table relay_instances). The primary key is the relay's
// instance_id; generation increases strictly on every RegisterRelay for the
// same instance_id, so a previous incarnation of the same instance can be
// fenced by generation-CAS. Re-registration of an existing row is stored+1
// (database-derived, clock-immune); a new row (first registration or after
// a prune) is seeded with the registry clock's Unix milliseconds, so
// generations stay monotonic across a prune provided clocks are not wrong
// by more than the prune horizon. Session rows reference it with ON DELETE CASCADE.
//
// Rows are NOT deleted by the stale-relay reaper (it deletes the relay's
// sessions only): keeping the row is what keeps generation monotonic for a
// reused instance_id. See pkg/conduit/registry and
// pkg/store/entadapter/conduit_registry_store.go.
type RelayInstance struct {
	ent.Schema
}

// Fields of the RelayInstance.
func (RelayInstance) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("instance_id").
			NotEmpty().
			Immutable().
			Comment("Relay instance id (hub instanceID or waypoint id)"),
		field.Int64("generation").
			Comment("Strictly increasing per RegisterRelay for this instance_id"),
		field.String("internal_endpoint").
			Default("").
			Comment("URL other instances dial for the internal relay API; empty = unaddressable"),
		field.String("public_endpoint").
			Optional().
			Nillable().
			Comment("Optional URL clients may dial directly (waypoint)"),
		field.Time("started_at"),
		field.Time("last_seen").
			Comment("Refreshed every 15s by the owning relay; stale after 60s"),
		field.Bool("draining").
			Default(false),
	}
}

// Edges of the RelayInstance.
func (RelayInstance) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("sessions", ConduitSession.Type).
			Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}

// Annotations of the RelayInstance.
func (RelayInstance) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "relay_instances"},
	}
}
