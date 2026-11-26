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

// ExecRunOutput holds the bounded captured output of an exec run, stored in a
// separate table from scheduled_exec_runs so list/history/admin queries never
// hydrate output bytes and so other run owners can share the table by run_id.
//
// There is intentionally no foreign key: each owner deletes its own output
// rows explicitly in the same transaction as its run purge.
type ExecRunOutput struct {
	ent.Schema
}

// Fields of the ExecRunOutput.
func (ExecRunOutput) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			StorageKey("run_id").
			Immutable(),
		field.Enum("owner_kind").
			Values("scheduled_exec", "action_run").
			Immutable(),
		field.UUID("project_id", uuid.UUID{}).
			Immutable(),
		field.String("capture_mode").
			Immutable(),
		field.Bytes("content").
			MaxLen(64 * 1024).
			Immutable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the ExecRunOutput.
func (ExecRunOutput) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("owner_kind", "project_id"),
	}
}

// Annotations of the ExecRunOutput.
func (ExecRunOutput) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "exec_run_outputs"},
	}
}
