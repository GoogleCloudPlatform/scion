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
)

// LaunchReaperState is the single-row table backing the T1 async-create launch
// reaper's cluster-wide arming state (design t1-async-create-v11.md §3.3,
// §3.7). There is exactly one row, id "agent-launch-reaper", created on
// demand by the first reaper tick to run — CreateBulk(Create().SetID(...)).
// OnConflictColumns(id).DoNothing(), both dialects; see armLaunchReaper's doc
// comment for why CreateBulk rather than a plain Create. It is written only
// by RunLaunchReaperTick, and only on the store clock (storeNow): never by
// any Go wall-clock read outside a tick's transaction, and never
// read/written by anything else.
//
// A nil OkAt or ArmedSince means disarmed. The arm/disarm decision is made in
// Go from the row plus storeNow (never through SQL NULL semantics or CASE
// expressions), so the same code runs on Postgres and SQLite.
type LaunchReaperState struct {
	ent.Schema
}

// Fields of the LaunchReaperState.
func (LaunchReaperState) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			Immutable(),
		// ok_at is set to storeNow at the end of every tick that completes
		// (commits) successfully, by whichever replica held the transaction-
		// scoped advisory lock for that tick. A tick that fails or cannot
		// acquire the lock leaves it unchanged.
		field.Time("ok_at").
			Optional().
			Nillable(),
		// armed_since stores the storeNow at which the current disarm window
		// started: it is (re)set whenever a tick (re)disarms — ok_at is stale
		// by more than reaperInterval+5s, or this is the row's first tick.
		// The cluster becomes armed once storeNow - armed_since >= 8x the
		// keepalive interval; staleness-based reaping only runs while armed.
		field.Time("armed_since").
			Optional().
			Nillable(),
	}
}
