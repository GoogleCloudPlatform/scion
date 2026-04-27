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

// RuntimeBroker mirrors the runtime_brokers raw SQL table. Uses a string
// primary key (not UUID) because the existing raw SQL column is TEXT and
// some brokers use non-UUID ids.
type RuntimeBroker struct {
	ent.Schema
}

// Annotations set the table name.
func (RuntimeBroker) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "runtime_brokers"},
	}
}

// Fields of the RuntimeBroker.
func (RuntimeBroker) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
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
		field.Text("capabilities").
			Optional(),
		field.Text("supported_harnesses").
			Optional(),
		field.Text("resources").
			Optional(),
		field.Text("runtimes").
			Optional(),
		field.Text("labels").
			Optional(),
		field.Text("annotations").
			Optional(),
		field.String("endpoint").
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
		field.String("created_by").
			Optional(),
		field.Bool("auto_provide").
			Default(false),
	}
}

// Indexes match raw SQL DDL.
func (RuntimeBroker) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug").
			StorageKey("idx_runtime_brokers_slug"),
		index.Fields("status").
			StorageKey("idx_runtime_brokers_status"),
	}
}
