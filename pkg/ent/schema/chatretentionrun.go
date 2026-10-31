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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ChatRetentionRun is the audit record of one chat retention run (design
// chat message retention §6.6). It holds the policy and per-project counts
// only: never a message ID or message content. A sweep run stays open and
// accumulates counts for up to 24 hours.
type ChatRetentionRun struct {
	ent.Schema
}

// Fields of the ChatRetentionRun.
func (ChatRetentionRun) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.Enum("trigger").
			Values("sweep", "manual", "preview").
			Immutable(),
		field.Bool("dry_run").
			Default(false).
			Immutable(),
		field.Time("started_at").
			Immutable(),
		field.Time("finished_at").
			Optional().
			Nillable().
			Comment("End of the last tick or run that wrote this row"),
		field.Enum("status").
			Values("running", "ok", "partial", "failed").
			Default("running"),
		field.String("actor").
			Optional().
			Nillable().
			Comment("User ID for manual and preview runs"),
		field.JSON("policy", json.RawMessage{}).
			Optional().
			Comment("Policy of the last tick: window and cutoff"),
		field.JSON("counts", json.RawMessage{}).
			Optional().
			Comment("Per-project counts of deleted rows; no IDs or content"),
		field.String("error").
			Optional().
			Nillable(),
	}
}

// Indexes of the ChatRetentionRun.
func (ChatRetentionRun) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("started_at"),
	}
}

// Annotations of the ChatRetentionRun.
func (ChatRetentionRun) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "chat_retention_runs"},
	}
}
