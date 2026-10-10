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
)

// HubInstance is one row per hub process (table hub_instances, health
// dashboard F3 design §5.1). Each hub replica writes only its own row, keyed
// by Server.InstanceID(), from its registry loop (pkg/hub/hub_instance_registry.go).
// The health summary reads the table to list every hub instance; it is never
// used for control-plane decisions.
//
// Every timestamp is the store clock (Postgres now(), SQLite Go clock) of
// the write, so clock skew between replicas does not matter.
//
// Indexes: primary key only. last_seen is updated every tick and is
// deliberately not indexed, so the update stays a HOT update on Postgres.
// The table holds at most a few hundred rows; every read is a full scan.
type HubInstance struct {
	ent.Schema
}

// Fields of the HubInstance.
func (HubInstance) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("instance_id").
			NotEmpty().
			Immutable().
			Comment("Server.InstanceID() of the hub process"),
		field.String("label").
			Default("").
			Comment("Short display name (pod name, Cloud Run revision or host name); never a key"),
		field.String("version").
			Default(""),
		field.Time("started_at").
			Comment("Store clock at this process's first successful upsert"),
		field.Time("last_seen").
			Comment("Store clock of the last write; not indexed"),
		field.Time("stopped_at").
			Optional().
			Nillable().
			Comment("Set by a clean shutdown; a row with stopped_at is never live"),
		field.String("status").
			Default("").
			Comment("healthy, degraded or unhealthy"),
		field.JSON("checks", map[string]string{}).
			Optional().
			Comment("Normalised check map: fixed values only, at most 16 entries"),
		field.JSON("stats", json.RawMessage{}).
			Optional().
			Comment("Bounded per-instance figures (DB pool, integrations); empty until written"),
	}
}

// Annotations of the HubInstance.
func (HubInstance) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "hub_instances"},
	}
}
