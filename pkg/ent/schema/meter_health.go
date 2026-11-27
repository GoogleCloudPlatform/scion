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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// MeterHealth tracks per-run producer receipt liveness, sequence continuity,
// anomaly counts, and schema drift (Tokenomics design §4.2 item 7).
type MeterHealth struct {
	ent.Schema
}

// Fields of the MeterHealth.
func (MeterHealth) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("agent_id").
			NotEmpty().
			Immutable(),
		field.String("run_id").
			NotEmpty().
			Immutable(),
		field.String("producer_epoch").
			NotEmpty().
			Immutable(),
		field.Time("last_receipt_at"),
		field.Int64("last_contiguous_seq").
			Default(0),
		field.Int64("max_seq_seen").
			Default(0),
		field.JSON("missing_ranges", []store.SeqRange{}).
			Optional(),
		field.Time("last_source_activity_at").
			Optional().
			Nillable(),
		field.JSON("supported_signals", []string{}).
			Optional(),
		field.Int64("anomalous_count").
			Default(0),
		field.Bool("schema_drift_detected").
			Default(false),
		field.String("final_state").
			Default(store.MeterHealthFinalStateOpen),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the MeterHealth.
func (MeterHealth) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id", "run_id", "producer_epoch").
			Unique().
			StorageKey("meter_health_agent_run_epoch"),
		index.Fields("run_id"),
		index.Fields("final_state", "updated_at"),
		index.Fields("updated_at"),
	}
}

// Annotations of the MeterHealth.
func (MeterHealth) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "meter_health"},
	}
}
