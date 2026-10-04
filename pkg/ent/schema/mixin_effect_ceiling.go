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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/mixin"
)

// EffectCeilingMixin adds the frozen effect-ceiling columns recorded with an
// authority-producing write (store.EffectCeiling). Prefix is prepended to
// every column name, so one row can carry a ceiling under a distinct name
// without colliding with neighbouring mixins.
//
// Every column defaults to the store.EffectCeilingUnrecorded zero value: a
// row written before these columns existed reads back with an empty
// ceiling_kind, which consumers treat as unrecorded and never as an
// unrestricted ceiling.
type EffectCeilingMixin struct {
	mixin.Schema

	// Prefix is prepended to every column name ("" for delegation edges).
	Prefix string
}

var _ ent.Mixin = EffectCeilingMixin{}

// Fields of the EffectCeilingMixin.
func (m EffectCeilingMixin) Fields() []ent.Field {
	return []ent.Field{
		// "" (unrecorded) | "bounded" | "principal".
		field.String(m.Prefix + "ceiling_kind").
			Default(""),
		// permissions.CeilingVersion for a bounded ceiling; 0 otherwise.
		field.Int32(m.Prefix + "ceiling_version").
			Default(0),
		// JSON array of permission IDs for a bounded ceiling. NULL for
		// principal and unrecorded; an empty JSON array allows nothing.
		field.String(m.Prefix + "ceiling_permission_ids").
			Optional().
			Nillable(),
		// permissions.BoundaryKind of the source credential; "" for principal.
		field.String(m.Prefix + "ceiling_boundary_kind").
			Default(""),
		field.String(m.Prefix + "ceiling_boundary_project_id").
			Default(""),
		// Expiry of the source UAT, when the source credential is a UAT.
		// Descriptive only.
		field.Time(m.Prefix + "ceiling_source_expires_at").
			Optional().
			Nillable(),
	}
}
