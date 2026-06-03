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
	"github.com/google/uuid"
)

// BrokerDispatch holds the schema definition for the BrokerDispatch entity — the
// durable intent table for the "DB as state machine, NOTIFY as the signal"
// dispatch model (design §5.2). A row records a lifecycle/create-time command
// targeted at a broker; the socket-holding node reconciles it (claim → run local
// tunnel op → mark done/failed). `args`/`result` are TEXT (JSON) to stay
// dialect-neutral and keep secrets out of NOTIFY payloads.
type BrokerDispatch struct {
	ent.Schema
}

// Fields of the BrokerDispatch.
func (BrokerDispatch) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("broker_id", uuid.UUID{}),
		// agent_id is null for project-scoped ops (e.g. create-with-gather).
		field.UUID("agent_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.String("agent_slug").
			Optional(),
		field.UUID("project_id", uuid.UUID{}).
			Optional().
			Nillable(),
		// op: start|stop|restart|delete|finalize_env|check_prompt|create|message
		field.String("op").
			NotEmpty(),
		// args: JSON; bulky/secret-bearing fields (resolvedEnv, resolvedSecrets,
		// inlineConfig, structured bodies) live here, NOT in the NOTIFY payload.
		field.String("args").
			Optional(),
		// state: pending|in_progress|done|failed
		field.String("state").
			Default("pending"),
		// result: JSON; for ops that return data (check_prompt, env-gather).
		field.String("result").
			Optional(),
		// claimed_by: hub instanceID that reconciled this intent.
		field.String("claimed_by").
			Optional(),
		field.Int("attempts").
			Default(0),
		field.String("error").
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
		field.Time("deadline_at").
			Optional().
			Nillable(),
	}
}

// Indexes of the BrokerDispatch.
func (BrokerDispatch) Indexes() []ent.Index {
	return []ent.Index{
		// Drain query: WHERE broker_id=$X AND state='pending'.
		index.Fields("broker_id", "state"),
	}
}

// Annotations of the BrokerDispatch.
func (BrokerDispatch) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "broker_dispatch"},
	}
}
