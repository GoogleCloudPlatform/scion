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

package entadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// parseEffectiveConfig decodes a stored effective_config. A NULL column is
// nil; so is invalid JSON, which is logged (like applied_config) rather than
// returned, because entAgentToStore has no error path.
func parseEffectiveConfig(agentID string, raw *string) *store.AgentEffectiveConfig {
	if raw == nil || *raw == "" {
		return nil
	}
	var cfg store.AgentEffectiveConfig
	if err := json.Unmarshal([]byte(*raw), &cfg); err != nil {
		slog.Error("agent store: effective_config is not valid JSON, ignoring it",
			"agent_id", agentID, "error", err)
		return nil
	}
	return &cfg
}

// SetAgentEffectiveConfig records the broker's effective configuration eff
// for run runID (WP1 slice 3 addendum v12 section 2). It is the only writer
// of agents.effective_config. It writes only while the row is live and not
// delete-held, its runtime_broker_id is eff.BrokerID, and its run_id is
// runID (and eff.RunID is runID); otherwise it writes nothing and returns
// Written=false with the first failing check as StaleReason (gone,
// deleted_or_held, broker_mismatch, run_mismatch). An echo equal to the
// stored one (ignoring ReportedAt) is Written with no write. It never
// touches state_version or updated, so a caller's later UpdateAgent CAS is
// not invalidated.
//
// The write is observed as AgentWriteEffectiveConfig (a config-resolution
// write) in the same transaction. It is not part of store.Store: the hub
// type-asserts for it.
func (s *AgentStore) SetAgentEffectiveConfig(ctx context.Context, agentID, runID string, eff store.AgentEffectiveConfig) (_ store.EffectiveConfigResult, err error) {
	uid, err := parseUUID(agentID)
	if err != nil {
		return store.EffectiveConfigResult{}, err
	}
	obs := s.lifecycle.load()
	client, tx, done, err := s.lifecycleWriteTx(ctx, obs)
	if err != nil {
		return store.EffectiveConfigResult{}, err
	}
	defer done()
	defer markOwnTxErr(&err, tx, s.entTx)

	// With an observer the read is locked (or the transaction serialized)
	// and the first attempt decides. With none, the read is not locked:
	// a write that matches no row re-reads and re-classifies; only a row
	// that keeps changing under the write (its updated moved, no fence
	// failed) exhausts the attempts.
	for attempt := 0; attempt < effectiveConfigAttempts; attempt++ {
		now := time.Now()
		row, err := effectiveConfigRow(ctx, client, uid)
		if err != nil {
			return store.EffectiveConfigResult{}, err
		}
		var before *store.Agent
		if row != nil {
			before = entAgentToStore(row)
		}
		res := classifyEffectiveEcho(before, runID, eff, now)
		if res.StaleReason != "" {
			return res, nil
		}
		if before.EffectiveConfig != nil && before.EffectiveConfig.SameEcho(eff) {
			res.Written = true
			return res, nil
		}
		if effectiveConfigRaceHook != nil {
			effectiveConfigRaceHook()
		}
		eff.ReportedAt = now.UTC()
		raw, err := json.Marshal(eff)
		if err != nil {
			return store.EffectiveConfigResult{}, fmt.Errorf("encode effective_config: %w", err)
		}
		// The predicate carries every fence (with no observer there is no
		// transaction). updated is pinned to the value read, so the echo
		// does not move it (the schema's UpdateDefault would stamp it),
		// and state_version is not touched: a caller's later whole-row
		// UpdateAgent CAS stays valid.
		n, err := client.Agent.Update().
			Where(agent.IDEQ(uid), agent.RunIDEQ(runID), agent.RuntimeBrokerIDEQ(eff.BrokerID), runIDWritable(now),
				agent.UpdatedEQ(row.Updated)).
			SetEffectiveConfig(string(raw)).
			SetUpdated(row.Updated).
			Save(ctx)
		if err != nil {
			return store.EffectiveConfigResult{}, mapError(err)
		}
		if n == 0 {
			continue
		}
		if obs != nil {
			after, err := lifecycleAfterRow(ctx, client, uid)
			if err != nil {
				return store.EffectiveConfigResult{}, err
			}
			if err := observeAgentWrite(ctx, obs, client, tx, store.AgentWrite{
				Op: store.AgentWriteEffectiveConfig, Before: before, After: after, Now: now,
			}); err != nil {
				return store.EffectiveConfigResult{}, err
			}
		}
		if err := commitLifecycleWriteTx(tx, s.entTx); err != nil {
			return store.EffectiveConfigResult{}, err
		}
		res.Written = true
		return res, nil
	}
	return store.EffectiveConfigResult{}, fmt.Errorf("%w: agent %s changed under every effective_config write attempt", store.ErrVersionConflict, agentID)
}

// effectiveConfigAttempts bounds SetAgentEffectiveConfig's read-and-write
// attempts.
const effectiveConfigAttempts = 3

// effectiveConfigRaceHook, when non-nil, is called between
// SetAgentEffectiveConfig's classifying read and its conditional write, so
// a test can land a concurrent write in that window (possible only with no
// observer installed, when the read is not in a transaction). It is read
// without synchronisation: tests that set it must not run in parallel
// (t.Parallel) with any test that calls SetAgentEffectiveConfig.
var effectiveConfigRaceHook func()

// effectiveConfigRow reads the full row of uid, locked for update where the
// database supports row locks, or nil when it does not exist. Unlike
// lifecycleBeforeRow it reads with no observer installed too: the fences
// are classified from it either way.
func effectiveConfigRow(ctx context.Context, client *ent.Client, uid uuid.UUID) (*ent.Agent, error) {
	q := client.Agent.Query().Where(agent.IDEQ(uid))
	if client.Driver().Dialect() == dialect.Postgres {
		q = q.ForUpdate()
	}
	row, err := q.Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mapError(err)
	}
	return row, nil
}

// classifyEffectiveEcho checks the fences of SetAgentEffectiveConfig on row
// in precedence order and names the first that fails ("" when all pass),
// with the row's run, launch and launch run.
func classifyEffectiveEcho(row *store.Agent, runID string, eff store.AgentEffectiveConfig, now time.Time) store.EffectiveConfigResult {
	if row == nil {
		return store.EffectiveConfigResult{StaleReason: store.StaleEchoGone}
	}
	res := store.EffectiveConfigResult{RowRunID: row.RunID, RowLaunchID: row.LaunchID, RowLaunchRunID: row.LaunchRunID}
	switch {
	case !row.DeletedAt.IsZero() || row.DeletionHoldsRow(now):
		res.StaleReason = store.StaleEchoDeletedOrHeld
	case eff.BrokerID == "" || row.RuntimeBrokerID != eff.BrokerID:
		res.StaleReason = store.StaleEchoBrokerMismatch
	case runID == "" || row.RunID != runID || eff.RunID != runID:
		res.StaleReason = store.StaleEchoRunMismatch
	}
	return res
}
