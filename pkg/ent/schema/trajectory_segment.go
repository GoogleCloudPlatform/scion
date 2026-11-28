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
)

// TrajectorySegment holds one L0 raw trajectory byte-range segment row in
// trajectory_segments (Tokenomics design §5.1, §5.5; P5 wire contract).
// Unique on segment identity (agent_id, run_id, harness, native_relative_path,
// generation, byte_start).
type TrajectorySegment struct {
	ent.Schema
}

// Fields of the TrajectorySegment.
func (TrajectorySegment) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			NotEmpty().
			Immutable(),
		field.String("hub_id").
			NotEmpty().
			Immutable(),
		field.String("origin_hub_id").
			Optional().
			Default(""),
		field.String("project_id").
			NotEmpty().
			Immutable(),
		field.String("agent_id").
			NotEmpty().
			Immutable(),
		field.String("run_id").
			NotEmpty().
			Immutable(),
		field.String("harness").
			NotEmpty().
			Immutable(),
		field.String("native_relative_path").
			NotEmpty().
			Immutable(),
		field.String("artifact_kind").
			NotEmpty().
			Immutable(),
		field.Int("generation").
			NonNegative().
			Immutable(),
		field.Int64("byte_start").
			NonNegative().
			Immutable(),
		field.Int64("byte_end").
			Positive(),
		field.String("raw_sha256").
			NotEmpty(),
		field.Int64("compressed_size").
			Positive(),
		field.Int64("charged_bytes").
			NonNegative().
			Default(0),
		field.String("compressed_sha256").
			NotEmpty(),
		field.Bool("incomplete_tail").
			Default(false),
		field.String("session_id").
			Optional().
			Default(""),
		field.String("object_path").
			NotEmpty().
			Immutable(),
		field.String("manifest_path").
			Optional().
			Default(""),
		field.String("status").
			NotEmpty().
			Default("pending"),
		field.String("normalize_status").
			NotEmpty().
			Default("pending"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("committed_at").
			Optional().
			Nillable(),
		field.Time("prepare_expires_at"),
	}
}

// Indexes of the TrajectorySegment.
func (TrajectorySegment) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id", "run_id", "harness", "native_relative_path", "generation", "byte_start").
			Unique(),
		index.Fields("project_id", "agent_id", "run_id"),
		index.Fields("agent_id", "created_at"),
	}
}

// Annotations of the TrajectorySegment.
func (TrajectorySegment) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "trajectory_segments"},
	}
}
