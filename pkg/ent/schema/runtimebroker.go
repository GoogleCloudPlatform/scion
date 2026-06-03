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

// RuntimeBroker holds the schema definition for the RuntimeBroker entity,
// mapping the legacy SQLite `runtime_brokers` table.
//
// JSON-bearing columns (capabilities, supported_harnesses, resources, runtimes,
// labels, annotations) are kept as raw strings to stay dialect-neutral and match
// the existing store's raw-marshaling behavior during the dual-write phase.
type RuntimeBroker struct {
	ent.Schema
}

// Fields of the RuntimeBroker.
func (RuntimeBroker) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty(),
		field.String("slug").
			NotEmpty(),
		// type/mode are vestigial columns in the legacy store (the SQLite store
		// always writes ""); kept Optional for column parity rather than required.
		field.String("type").
			Optional(),
		field.String("mode").
			Default("connected"),
		field.String("version").
			Optional(),
		// lock_version is an internal optimistic-concurrency token (not surfaced
		// on store.RuntimeBroker, which already uses "version" for the broker
		// software version). The heartbeat and full-update paths compare-and-set
		// this column to serialize concurrent writers without SELECT ... FOR
		// UPDATE, so the same logic is correct on both SQLite (tests) and
		// Postgres (production).
		field.Int64("lock_version").
			Default(0),
		field.String("status").
			Default("offline"),
		field.String("connection_state").
			Default("disconnected"),
		field.Time("last_heartbeat").
			Optional().
			Nillable(),
		field.String("capabilities").
			Optional(),
		field.String("supported_harnesses").
			Optional(),
		field.String("resources").
			Optional(),
		field.String("runtimes").
			Optional(),
		field.String("labels").
			Optional(),
		field.String("annotations").
			Optional(),
		field.String("endpoint").
			Optional(),
		field.String("created_by").
			Optional(),
		field.Bool("auto_provide").
			Default(false),
		field.String("connected_hub_id").
			Optional().
			Nillable(),
		field.String("connected_session_id").
			Optional().
			Nillable(),
		field.Time("connected_at").
			Optional().
			Nillable(),
		field.Time("created").
			Default(time.Now).
			Immutable(),
		field.Time("updated").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the RuntimeBroker.
func (RuntimeBroker) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug"),
		index.Fields("status"),
	}
}

// Annotations of the RuntimeBroker.
func (RuntimeBroker) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "runtime_brokers"},
	}
}
