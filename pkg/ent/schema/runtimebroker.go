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
		field.String("type").
			NotEmpty(),
		field.String("mode").
			Default("connected"),
		field.String("version").
			Optional(),
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
