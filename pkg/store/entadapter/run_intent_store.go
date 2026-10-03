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
	"fmt"
	"time"

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file implements the run intent writers: SetRunIntent, RevertRunIntent
// and BackfillRunIntent. They reuse the launch store's hand-built
// transaction (beginLaunchTx) and store clock (storeNow), so every
// run_intent_at value comes from the same clock as the launch timestamps:
// Postgres now() inside the transaction, or the single SQLite process's
// clock. None of them touches state_version.

// SetRunIntent implements store.AgentStore.SetRunIntent.
func (s *AgentStore) SetRunIntent(ctx context.Context, agentID string, intent store.RunIntent) (time.Time, error) {
	if !intent.Valid() {
		return time.Time{}, fmt.Errorf("%w: unknown run intent %q", store.ErrInvalidInput, intent)
	}
	uid, err := parseUUID(agentID)
	if err != nil {
		return time.Time{}, err
	}

	ltx, err := s.beginLaunchTx(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer ltx.cleanup()
	isPG := s.dialect(ctx) == dialect.Postgres
	committed := false
	defer func() {
		if !committed {
			_ = ltx.tx.Rollback()
		}
	}()

	// Lock the row so the clock read and the write below are ordered with
	// every other intent write to it: the commit order is the intent order.
	// SQLite write transactions are serialised already.
	q := ltx.client.Agent.Query().Where(agent.IDEQ(uid))
	if isPG {
		q = q.ForUpdate()
	}
	current, err := q.Select(agent.FieldRunIntentAt).Only(ctx)
	if err != nil {
		return time.Time{}, mapError(err)
	}

	now, err := storeNow(ctx, ltx.tx, isPG)
	if err != nil {
		return time.Time{}, err
	}
	at := nextRunIntentAt(now, current.RunIntentAt)

	if _, err := ltx.client.Agent.UpdateOneID(uid).
		SetRunIntent(string(intent)).
		SetRunIntentAt(at).
		Save(ctx); err != nil {
		return time.Time{}, mapError(err)
	}
	if err := ltx.tx.Commit(); err != nil {
		return time.Time{}, fmt.Errorf("run intent: commit SetRunIntent: %w", err)
	}
	committed = true
	return at, nil
}

// nextRunIntentAt returns max(now, stored + resolution), normalised to the
// stored precision, so run_intent_at strictly increases per row.
func nextRunIntentAt(now time.Time, stored *time.Time) time.Time {
	at := store.NormalizeRunIntentTime(now)
	if stored != nil {
		floor := store.NormalizeRunIntentTime(*stored).Add(store.RunIntentResolution)
		if at.Before(floor) {
			at = floor
		}
	}
	return at
}

// RevertRunIntent implements store.AgentStore.RevertRunIntent.
func (s *AgentStore) RevertRunIntent(ctx context.Context, agentID string, from store.RunIntent, fromAt time.Time, to store.RunIntent) (bool, error) {
	if !from.Valid() || !to.Valid() {
		return false, fmt.Errorf("%w: unknown run intent %q -> %q", store.ErrInvalidInput, from, to)
	}
	uid, err := parseUUID(agentID)
	if err != nil {
		return false, err
	}

	ltx, err := s.beginLaunchTx(ctx)
	if err != nil {
		return false, err
	}
	defer ltx.cleanup()
	isPG := s.dialect(ctx) == dialect.Postgres
	committed := false
	defer func() {
		if !committed {
			_ = ltx.tx.Rollback()
		}
	}()

	q := ltx.client.Agent.Query().Where(agent.IDEQ(uid))
	if isPG {
		q = q.ForUpdate()
	}
	current, err := q.Select(agent.FieldRunIntent, agent.FieldRunIntentAt).Only(ctx)
	if err != nil {
		return false, mapError(err)
	}
	// Compare in Go rather than with a SQL time equality, which depends on
	// each backend's timestamp encoding.
	row := store.Agent{RunIntentAt: current.RunIntentAt}
	if current.RunIntent != nil {
		row.RunIntent = store.RunIntent(*current.RunIntent)
	}
	if !row.RunIntentMatches(from, fromAt) {
		return false, nil
	}
	if _, err := ltx.client.Agent.UpdateOneID(uid).
		SetRunIntent(string(to)).
		Save(ctx); err != nil {
		return false, mapError(err)
	}
	if err := ltx.tx.Commit(); err != nil {
		return false, fmt.Errorf("run intent: commit RevertRunIntent: %w", err)
	}
	committed = true
	return true, nil
}

// BackfillRunIntent implements store.AgentStore.BackfillRunIntent.
func (s *AgentStore) BackfillRunIntent(ctx context.Context) (int, error) {
	ltx, err := s.beginLaunchTx(ctx)
	if err != nil {
		return 0, err
	}
	defer ltx.cleanup()
	isPG := s.dialect(ctx) == dialect.Postgres
	committed := false
	defer func() {
		if !committed {
			_ = ltx.tx.Rollback()
		}
	}()

	now, err := storeNow(ctx, ltx.tx, isPG)
	if err != nil {
		return 0, err
	}
	at := store.NormalizeRunIntentTime(now)

	running, err := ltx.client.Agent.Update().
		Where(agent.RunIntentIsNil(), agent.PhaseIn("running", "starting")).
		SetRunIntent(string(store.RunIntentRunning)).
		SetRunIntentAt(at).
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	stopped, err := ltx.client.Agent.Update().
		Where(agent.RunIntentIsNil()).
		SetRunIntent(string(store.RunIntentStopped)).
		SetRunIntentAt(at).
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	if err := ltx.tx.Commit(); err != nil {
		return 0, fmt.Errorf("run intent: commit BackfillRunIntent: %w", err)
	}
	committed = true
	return running + stopped, nil
}
