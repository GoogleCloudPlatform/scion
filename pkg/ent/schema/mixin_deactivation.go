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

// DeactivationMixin records why, when and by which operation a row was
// deactivated (store.Deactivation). An active row, or one deactivated
// before these columns existed, has an empty cause, a NULL time and an
// empty operation ID.
type DeactivationMixin struct {
	mixin.Schema
}

var _ ent.Mixin = DeactivationMixin{}

// Fields of the DeactivationMixin.
func (DeactivationMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String("deactivation_cause").
			Default(""),
		field.Time("deactivated_at").
			Optional().
			Nillable(),
		// One ID per deactivating operation, so a restore can reactivate
		// exactly the rows that operation deactivated.
		field.String("deactivation_op_id").
			Default(""),
	}
}
