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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// SubscriptionTemplate mirrors subscription_templates (V33).
type SubscriptionTemplate struct {
	ent.Schema
}

func (SubscriptionTemplate) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "subscription_templates"}}
}

func (SubscriptionTemplate) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("name").NotEmpty(),
		field.String("scope").Default("grove"),
		field.Text("trigger_activities"),
		field.String("grove_id").Default(""),
		field.String("created_by").NotEmpty(),
	}
}

func (SubscriptionTemplate) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("grove_id").StorageKey("idx_sub_templates_grove"),
	}
}
