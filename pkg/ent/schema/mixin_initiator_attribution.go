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

// InitiatorAttributionMixin is the single ent mixin shared by Schedule and
// ScheduledEvent (tracker E.2b, ptone/scion#2127; rulings "E.2b field names",
// committed and adopted verbatim by B.3 — do not rename). One mixin
// guarantees the two schemas expose an identical attribution column set, so
// recurrence propagation is a single struct assignment and B.3 can read the
// same shape from either row.
//
// Every field is additive (Optional().Nillable()): a row written before
// E.2b has every column NULL. NULL/0 in attribution_version is the explicit
// legacy_unknown marker (design check (c)) — read it through
// store.InitiatorAttribution / the scheduledInitiator helper in pkg/hub,
// never directly, so a legacy row is never mistaken for an interactive
// credential.
//
// B.3's ceiling column(s) live on the same Schedule/ScheduledEvent rows,
// added separately by B.3 and updated in the same transaction as an
// attribution replacement. They are not part of this mixin.
type InitiatorAttributionMixin struct {
	mixin.Schema
}

var _ ent.Mixin = (*InitiatorAttributionMixin)(nil)

// Fields of the InitiatorAttributionMixin.
func (InitiatorAttributionMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String("initiator_principal_kind").
			Optional().
			Nillable(),
		field.String("initiator_principal_id").
			Optional().
			Nillable(),
		// session | uat | agent | dev_local | legacy_unknown — a smaller,
		// committed domain than hub.CredentialKind; see
		// store.InitiatorCredentialKind*.
		field.String("initiator_credential_kind").
			Optional().
			Nillable(),
		field.String("initiator_credential_id").
			Optional().
			Nillable(),
		// Bounded JSON snapshot (name, boundary, purpose, labels), built with
		// the same sanitization/bounding primitives E.2a's audit path uses
		// (sanitizeForLog / boundedLabelsJSON) so a legacy row and a live
		// audit row degrade identically.
		field.String("initiator_credential_snapshot").
			Optional().
			Nillable(),
		// NULL/0 = legacy_unknown; 1 = written by E.2b.
		field.Int("attribution_version").
			Optional().
			Nillable(),
		// Incremented atomically with each attribution replacement (ruling
		// Q2). Schedules own the counter; events copy the schedule's current
		// value at materialization and never change it afterward.
		field.Int("authorization_revision").
			Optional().
			Nillable(),
	}
}
