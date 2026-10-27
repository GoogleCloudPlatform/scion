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
	"github.com/google/uuid"
)

// HubEncryptionMigration is one encryption migration job
// (ptone/scion#1530 §6). Counts and the cursor are progress only; they
// never substitute for a final full scan. Rows hold no payloads and no key
// material.
type HubEncryptionMigration struct {
	ent.Schema
}

// Annotations of the HubEncryptionMigration.
func (HubEncryptionMigration) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "hub_encryption_migrations"},
	}
}

// Fields of the HubEncryptionMigration.
func (HubEncryptionMigration) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Immutable(),
		field.Int64("source_generation").
			Immutable().
			Comment("Policy generation the migration started from"),
		field.String("target_kid").
			Immutable().
			NotEmpty(),
		field.Enum("state").
			Values("running", "interrupted", "verified"),
		field.String("last_scanned_id").
			Default("").
			Comment("Progress cursor (secret ID); never proof of completion"),
		field.Bool("plaintext_read_admitted").
			Default(false).
			Comment("Migration-only plaintext read admission; cleared when verified"),
		field.Int64("scanned").
			Default(0),
		field.Int64("rewritten").
			Default(0),
		field.Int64("failures").
			Default(0),
		field.Time("created").
			Immutable(),
		field.Time("updated"),
	}
}
