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

// ConduitPrincipalEpoch is the durable per-principal connection_epoch
// counter (design conduit v2.1 §3.4, table conduit_principal_epochs). It is
// bumped with INSERT ... ON CONFLICT (principal_kind, principal_id) DO
// UPDATE SET epoch = epoch + 1 RETURNING epoch in the same transaction as
// the conduit_sessions insert, so the epoch never regresses when session
// rows are reaped.
//
// Rows are never reaped. An agent's row may be deleted together with the
// agent (agent uuids are never reused); broker rows are kept because broker
// IDs are reused.
//
// The design's PRIMARY KEY (principal_kind, principal_id) is expressed as a
// UNIQUE index plus ent's required surrogate integer id (ent has no
// composite primary keys for regular schemas); the unique index is the
// upsert conflict target, so the semantics are identical.
type ConduitPrincipalEpoch struct {
	ent.Schema
}

// Fields of the ConduitPrincipalEpoch.
func (ConduitPrincipalEpoch) Fields() []ent.Field {
	return []ent.Field{
		field.String("principal_kind").
			NotEmpty().
			Immutable(),
		field.String("principal_id").
			NotEmpty().
			Immutable(),
		field.Int64("epoch"),
	}
}

// Indexes of the ConduitPrincipalEpoch.
func (ConduitPrincipalEpoch) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("principal_kind", "principal_id").Unique(),
	}
}

// Annotations of the ConduitPrincipalEpoch.
func (ConduitPrincipalEpoch) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "conduit_principal_epochs"},
	}
}
