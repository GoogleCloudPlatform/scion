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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// MessagePolicy holds the schema definition for project-scoped message
// admission policies (consumer design §5.3, ptone/scion#8047).
//
// Rows carry project_id as a plain UUID column with no database-level foreign
// key; CompositeStore.DeleteProject cascades deletion explicitly.
type MessagePolicy struct {
	ent.Schema
}

func nowUTC() time.Time {
	return time.Now().UTC()
}

// Fields of the MessagePolicy.
func (MessagePolicy) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("project_id", uuid.UUID{}).
			Immutable(),
		field.String("name").
			NotEmpty().
			MaxLen(store.MaxMessagePolicyNameLen).
			Comment("Project-unique canonical slug identifying the policy (e.g. no-prose-choices)."),
		field.Text("description").
			Default(""),
		field.Bool("enabled").
			Default(true).
			Comment("Whether the policy participates in outbound message evaluation."),
		field.Enum("mode").
			Values("shadow", "enforce").
			Default("shadow").
			Comment("Evaluation mode; defaults to shadow at creation."),
		field.JSON("scope", store.MessagePolicyScope{}).
			Comment("Surfaces and sender/recipient selectors governed by this policy."),
		field.Text("violation_rule").
			NotEmpty().
			Comment("Required CEL bool expression (<= 4096 bytes); true means policy violated."),
		field.Text("system_one_rule").
			Default("").
			Comment("Optional System One rule JSON (<= 16 KiB); empty in M1."),
		field.JSON("system_one_refs", []string{}).
			Optional().
			Comment("Derived System One instance and pool names referenced by system_one_rule."),
		field.String("on_uncertain").
			Default("").
			MaxLen(16).
			Comment("Outcome when evaluation class is uncertain (admit or reject; empty allowed for CEL-only policies)."),
		field.Enum("on_failed").
			Values("admit", "reject").
			Comment("Required fallback outcome when evaluation class is failed."),
		field.JSON("on_failed_reasons", map[string]string{}).
			Optional().
			Comment("Optional per-FailedError reason overrides (admit or reject); unlisted reasons fall back to on_failed."),
		field.Text("reject_reason").
			Default("").
			Comment("Rejection reason template (<= 1 KiB); never interpolates message body text."),
		field.Text("reject_remediation").
			Default("").
			Comment("Optional advisory remediation template (<= 1 KiB); never interpolates message body text."),
		field.Int64("revision").
			Default(1).
			Positive().
			Comment("Incremented on every policy edit; scopes CEL program cache and decision records."),
		field.String("created_by").
			Default(""),
		field.String("updated_by").
			Default(""),
		field.Time("created_at").
			Default(nowUTC).
			Immutable(),
		field.Time("updated_at").
			Default(nowUTC).
			UpdateDefault(nowUTC),
	}
}

// Edges of the MessagePolicy.
func (MessagePolicy) Edges() []ent.Edge {
	return nil
}

// Indexes of the MessagePolicy.
func (MessagePolicy) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "name").Unique(),
		index.Fields("project_id", "enabled", "name"),
	}
}

// Annotations of the MessagePolicy.
func (MessagePolicy) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "message_policies"},
	}
}
