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
)

// HubEncryptionPolicy is the singleton database encryption policy
// (ptone/scion#1530 §6): exactly one row, id 1, created by the first
// policy transition. Once it exists, every payload-changing secret write
// is fenced to its allowed writer key IDs. It holds key IDs only, never
// key material.
type HubEncryptionPolicy struct {
	ent.Schema
}

// Annotations of the HubEncryptionPolicy.
func (HubEncryptionPolicy) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "hub_encryption_policy"},
	}
}

// Fields of the HubEncryptionPolicy.
func (HubEncryptionPolicy) Fields() []ent.Field {
	return []ent.Field{
		field.Int("id").
			Immutable().
			Comment("Always 1 (singleton)"),
		field.Int64("generation").
			Positive().
			Comment("Incremented by every policy transition (compare-and-swap)"),
		field.Int("min_reader_version").
			Default(2),
		field.JSON("allowed_write_kids", []string{}).
			Comment("Key IDs a payload write may use (enc:v2: only)"),
		field.JSON("required_read_kids", []string{}).
			Comment("Key IDs every ring-aware instance must be able to read"),
		field.String("migration_id").
			Optional().
			Nillable().
			Comment("UUID of the bound encryption migration, if any"),
		field.Enum("state").
			Values("prepared", "rewrapping", "verified"),
		field.Time("updated_at").
			Comment("UTC"),
	}
}
