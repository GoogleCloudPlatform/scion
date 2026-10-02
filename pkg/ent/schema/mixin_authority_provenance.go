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

// AuthorityProvenanceMixin adds the columns that record who authorized an
// authority-producing write and with which credential
// (store.AuthorityProvenance).
//
// provenance_version 0 marks a row with no recorded provenance (every row
// written before these columns existed); 1 marks the v1 layout. The
// initiator_* columns use the same names and value domain as
// InitiatorAttributionMixin.
type AuthorityProvenanceMixin struct {
	mixin.Schema
}

var _ ent.Mixin = AuthorityProvenanceMixin{}

// Fields of the AuthorityProvenanceMixin.
func (AuthorityProvenanceMixin) Fields() []ent.Field {
	return []ent.Field{
		field.Int("provenance_version").
			Default(0),
		// "user" | "agent".
		field.String("source_principal_kind").
			Default(""),
		field.String("source_principal_id").
			Default(""),
		// session | dev_local | uat | agent | scheduler | system_migration.
		field.String("source_credential_kind").
			Default(""),
		// UAT ID or agent credential JTI; "" for session and dev_local.
		field.String("source_credential_id").
			Default(""),
		// Scheduler writes only: fired scheduled event, recurring schedule
		// and the authorization revision the event snapshotted.
		field.String("source_event_id").
			Default(""),
		field.String("source_schedule_id").
			Optional().
			Nillable(),
		field.Int("source_authorization_revision").
			Default(0),
		field.String("initiator_principal_kind").
			Default(""),
		field.String("initiator_principal_id").
			Default(""),
		field.String("initiator_credential_kind").
			Default(""),
		field.String("initiator_credential_id").
			Default(""),
	}
}
