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
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/launchreaperstate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// launchReaperStateID is the single row's fixed ID (design §3.3).
const launchReaperStateID = "agent-launch-reaper"

// launchReaperTickTimeout bounds the whole tick, including connection
// checkout (design §3.7: "tickCtx starts before checkout, so the 5s checkout
// counts inside the 10s budget").
const launchReaperTickTimeout = 10 * time.Second

// launchReaperArmingMargin is added to ReaperParams.ReaperInterval to decide
// "no replica completed a tick within the interval" (design §3.7 step 1).
const launchReaperArmingMargin = 5 * time.Second

// launchReaperDisarmTimeout bounds the best-effort disarm write issued after
// a failed tick: a fresh, short context, not the expired tick context.
const launchReaperDisarmTimeout = 2 * time.Second

// inFlightPhaseList is store.InFlightPhases as an ordered slice, for ent's
// PhaseIn predicate.
var inFlightPhaseList = []string{
	string(state.PhaseCreated),
	string(state.PhaseProvisioning),
	string(state.PhaseCloning),
	string(state.PhaseStarting),
}

// windDownPhaseList is the phase set design §3.3/§3.7 calls the wind-down
// window: a create launch the Hub has already moved off the in-flight
// phases, whose broker has not reported back yet.
var windDownPhaseList = []string{
	string(state.PhaseSuspended),
	string(state.PhaseStopping),
	string(state.PhaseStopped),
	string(state.PhaseError),
}

// launchReaperFailureHook, when non-nil, is called at each named tick-level
// injection point; a non-nil return forces that point to fail exactly as a
// real driver error would. It exists so the reaper's tick-level failure
// cases — a tick-level statement failing disarms the cluster; the lock and
// SET LOCAL statements map to the neutral "unavailable" outcome while every
// other tick-level statement (including SAVEPOINT/ROLLBACK TO SAVEPOINT/
// RELEASE) maps to "failed" — can be exercised deterministically on SQLite,
// where there is no way to force a genuine lock, connection or savepoint
// failure from Go. Always nil in production. Injection points: "lock",
// "set_local_lock_timeout", "set_local_idle_timeout", "arm_check",
// "deadline_select", "stale_select", "wind_down_select", "ok_at_write",
// "commit", "savepoint", "rollback_to_savepoint", "release_savepoint",
// "row_apply:<agent id>".
var launchReaperFailureHook func(point string) error

// injectFailure calls launchReaperFailureHook if set, otherwise returns nil.
func injectFailure(point string) error {
	if launchReaperFailureHook == nil {
		return nil
	}
	return launchReaperFailureHook(point)
}

// reapReason classifies why a candidate row was selected, so the per-row
// savepoint applies the right write.
type reapReason int

const (
	reapDeadline reapReason = iota
	reapStaleness
	reapWindDown
)

// RunLaunchReaperTick implements store.AgentStore.RunLaunchReaperTick (design
// §3.7). See launch_store.go's file comment for why this — like every launch
// writer — builds its own hand-rolled transaction rather than using
// s.client.Tx(ctx).
func (s *AgentStore) RunLaunchReaperTick(ctx context.Context, p store.ReaperParams) (store.ReaperTickResult, error) {
	tickCtx, cancel := context.WithTimeout(ctx, launchReaperTickTimeout)
	defer cancel()

	// Prime dialect detection BEFORE checking out the connection below (same
	// gotcha as beginLaunchTx and AgentStore.UpdateAgentStatus): the
	// detection probe would otherwise contend with this tick's own
	// connection forever on single-connection SQLite.
	dialectName := s.dialect(ctx)

	db := s.sqlDB()
	if db == nil {
		return store.ReaperTickResult{Outcome: store.ReaperTickUnavailable}, nil
	}

	acquireCtx, cancelAcquire := context.WithTimeout(tickCtx, advisoryLockTimeout)
	conn, err := db.Conn(acquireCtx)
	cancelAcquire()
	if err != nil {
		return store.ReaperTickResult{Outcome: store.ReaperTickUnavailable}, nil
	}
	defer func() { _ = conn.Close() }()

	tx, err := conn.BeginTx(tickCtx, nil)
	if err != nil {
		return store.ReaperTickResult{Outcome: store.ReaperTickUnavailable}, nil
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	isPG := dialectName == dialect.Postgres

	if isPG {
		acquired, err := tryAdvisoryXactLock(tickCtx, tx)
		if err == nil {
			err = injectFailure("lock")
		}
		if err != nil {
			return store.ReaperTickResult{Outcome: store.ReaperTickUnavailable}, nil
		}
		if !acquired {
			_ = tx.Rollback()
			committed = true
			return store.ReaperTickResult{Outcome: store.ReaperTickNotAcquired}, nil
		}
		// Belt and braces: bound the tick's own lock waits so a
		// half-open session cannot hold the xact lock and every row lock it
		// took indefinitely. The injectFailure calls sit directly beside each
		// real statement's own error check, so a test exercises the same
		// `return ... Unavailable` branch the real driver error would take,
		// not a separate checkpoint.
		if _, err := tx.ExecContext(tickCtx, "SET LOCAL lock_timeout = '2s'"); err != nil {
			return store.ReaperTickResult{Outcome: store.ReaperTickUnavailable}, nil
		}
		if err := injectFailure("set_local_lock_timeout"); err != nil {
			return store.ReaperTickResult{Outcome: store.ReaperTickUnavailable}, nil
		}
		if _, err := tx.ExecContext(tickCtx, "SET LOCAL idle_in_transaction_session_timeout = '15s'"); err != nil {
			return store.ReaperTickResult{Outcome: store.ReaperTickUnavailable}, nil
		}
		if err := injectFailure("set_local_idle_timeout"); err != nil {
			return store.ReaperTickResult{Outcome: store.ReaperTickUnavailable}, nil
		}
	}

	txClient := newTxClient(dialectName, tx)

	result, err := s.runLaunchReaperTickBody(tickCtx, tx, txClient, isPG, p)
	if err != nil {
		// Tick-level failure: roll back (nothing from the tick persists),
		// then best-effort disarm on a fresh context/connection.
		_ = tx.Rollback()
		committed = true
		_ = conn.Close()
		s.bestEffortDisarm(ctx)
		slog.Warn("launch reaper: tick failed", "error", err)
		return store.ReaperTickResult{Outcome: store.ReaperTickFailed}, nil
	}

	err = injectFailure("commit")
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		committed = true
		_ = conn.Close()
		s.bestEffortDisarm(ctx)
		slog.Warn("launch reaper: tick commit failed", "error", err)
		return store.ReaperTickResult{Outcome: store.ReaperTickFailed}, nil
	}
	committed = true
	return result, nil
}

// tryAdvisoryXactLock issues `SELECT pg_try_advisory_xact_lock($1)` on tx:
// the lock is scoped to tx's transaction and released automatically at
// commit or rollback, on the same connection as every other statement in
// the tick.
func tryAdvisoryXactLock(ctx context.Context, tx *sql.Tx) (bool, error) {
	var acquired bool
	err := tx.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock($1)", int64(store.LockAgentLaunchDeadline)).Scan(&acquired)
	return acquired, err
}

// runLaunchReaperTickBody runs everything after the lock is held: arming,
// selection, and per-row reaping. A returned error means the tick failed
// (design §3.7 step 6: an error in a tick-level statement); per-row errors
// are handled internally via savepoints and never surface here.
func (s *AgentStore) runLaunchReaperTickBody(ctx context.Context, tx *sql.Tx, txClient *ent.Client, isPG bool, p store.ReaperParams) (store.ReaperTickResult, error) {
	now, err := storeNow(ctx, tx, isPG)
	if err != nil {
		return store.ReaperTickResult{}, err
	}

	if err := injectFailure("arm_check"); err != nil {
		return store.ReaperTickResult{}, err
	}
	armed, disarmedFor, err := armLaunchReaper(ctx, txClient, isPG, now, p)
	if err != nil {
		return store.ReaperTickResult{}, err
	}

	seen := map[uuid.UUID]reapReason{}

	if err := injectFailure("deadline_select"); err != nil {
		return store.ReaperTickResult{}, err
	}
	deadlineIDs, err := txClient.Agent.Query().
		Where(
			agent.LaunchStateEQ(store.LaunchStateActive),
			agent.PhaseIn(inFlightPhaseList...),
			agent.DeletedAtIsNil(),
			agent.LaunchDeadlineLT(now),
		).IDs(ctx)
	if err != nil {
		return store.ReaperTickResult{}, err
	}
	for _, id := range deadlineIDs {
		seen[id] = reapDeadline
	}

	if armed {
		staleBefore := now.Add(-8 * p.KeepaliveInterval)

		if err := injectFailure("stale_select"); err != nil {
			return store.ReaperTickResult{}, err
		}
		staleIDs, err := txClient.Agent.Query().
			Where(
				agent.LaunchStateEQ(store.LaunchStateActive),
				agent.PhaseIn(inFlightPhaseList...),
				agent.DeletedAtIsNil(),
				agent.LaunchLastReportAtLT(staleBefore),
			).IDs(ctx)
		if err != nil {
			return store.ReaperTickResult{}, err
		}
		for _, id := range staleIDs {
			if _, ok := seen[id]; !ok {
				seen[id] = reapStaleness
			}
		}

		if err := injectFailure("wind_down_select"); err != nil {
			return store.ReaperTickResult{}, err
		}
		windDownIDs, err := txClient.Agent.Query().
			Where(
				agent.LaunchStateEQ(store.LaunchStateActive),
				agent.PhaseIn(windDownPhaseList...),
				agent.DeletedAtIsNil(),
				agent.LaunchLastReportAtLT(staleBefore),
			).IDs(ctx)
		if err != nil {
			return store.ReaperTickResult{}, err
		}
		for _, id := range windDownIDs {
			if _, ok := seen[id]; !ok {
				seen[id] = reapWindDown
			}
		}
	}

	var reaped []store.Agent
	rowErrors := 0
	for id, reason := range seen {
		updated, rowErrored, err := s.reapOneRowInSavepoint(ctx, tx, txClient, isPG, id, reason, now, p)
		if err != nil {
			// SAVEPOINT/ROLLBACK TO SAVEPOINT/RELEASE failures are
			// tick-level: the transaction may already be gone.
			return store.ReaperTickResult{}, err
		}
		if rowErrored {
			rowErrors++
			continue
		}
		if updated == nil {
			continue // benign skip: locked elsewhere, or no longer due.
		}
		reaped = append(reaped, *updated)
	}

	if err := injectFailure("ok_at_write"); err != nil {
		return store.ReaperTickResult{}, err
	}
	if _, err := txClient.LaunchReaperState.UpdateOneID(launchReaperStateID).SetOkAt(now).Save(ctx); err != nil {
		return store.ReaperTickResult{}, err
	}

	return store.ReaperTickResult{
		Outcome:     store.ReaperTickCompleted,
		Armed:       armed,
		DisarmedFor: disarmedFor,
		Reaped:      reaped,
		RowErrors:   rowErrors,
	}, nil
}

// armLaunchReaper implements design §3.7's arming step. It creates the
// singleton row on demand, decides in Go (never SQL NULL semantics) whether
// to (re)arm, and returns whether staleness-based reaping should run on this
// tick plus how long the cluster has been disarmed (0 while armed).
func armLaunchReaper(ctx context.Context, txClient *ent.Client, isPG bool, now time.Time, p store.ReaperParams) (armed bool, disarmedFor time.Duration, err error) {
	// CreateBulk, not a plain Create: ent's single-row Create+OnConflict always
	// reads the row back via "INSERT ... RETURNING id" (creator.insertLastID),
	// which errors with sql.ErrNoRows when DoNothing() genuinely skips the
	// insert (the row already exists) — a real conflict is exactly the
	// common case here, since this runs on every tick. CreateBulk's read-back
	// (batchCreator.insertLastIDs) loops over however many rows RETURNING
	// produced, including zero, so a skipped insert is not an error. This
	// matches the allowlist_store.go:258-262 precedent design §3.3 cites,
	// which also uses CreateBulk for the same reason (there, with a real
	// multi-row batch; here, with a batch of one).
	if err := txClient.LaunchReaperState.CreateBulk(
		txClient.LaunchReaperState.Create().SetID(launchReaperStateID),
	).OnConflictColumns(launchreaperstate.FieldID).DoNothing().Exec(ctx); err != nil {
		return false, 0, err
	}

	q := txClient.LaunchReaperState.Query().Where(launchreaperstate.IDEQ(launchReaperStateID))
	if isPG {
		q = q.ForUpdate()
	}
	rs, err := q.Only(ctx)
	if err != nil {
		return false, 0, err
	}

	armedSince := rs.ArmedSince
	needsArm := rs.OkAt == nil || armedSince == nil || now.Sub(*rs.OkAt) > p.ReaperInterval+launchReaperArmingMargin
	if needsArm {
		if _, err := txClient.LaunchReaperState.UpdateOneID(launchReaperStateID).SetArmedSince(now).Save(ctx); err != nil {
			return false, 0, err
		}
		armedSince = &now
	}

	if armedSince == nil {
		// Unreachable: needsArm is true whenever armedSince starts nil, and
		// the block above always sets armedSince = &now in that case. Kept
		// as an explicit, documented branch rather than falling through to
		// the same "0" a real disarmed value could return, which would make
		// the two indistinguishable.
		return false, 0, fmt.Errorf("launch reaper: armed_since is nil after the arm check (unreachable)")
	}
	// DisarmedFor must report how long the cluster has been disarmed —
	// storeNow minus armed_since while not armed, 0 while armed — not time
	// remaining until it re-arms. A gauge/warning built on this in P1a-ii
	// must see it grow across ticks while disarmed.
	since := now.Sub(*armedSince)
	if since >= 8*p.KeepaliveInterval {
		return true, 0, nil
	}
	return false, since, nil
}

// reapOneRowInSavepoint re-checks and reaps a single candidate row inside its
// own savepoint (design §3.7 step 4). rowErrored reports whether a per-row
// store error was rolled back and should be counted toward a row-error
// metric — distinct from a benign skip (locked elsewhere, or no longer due),
// which returns (nil, false, nil). The returned error is non-nil only for a
// SAVEPOINT/ROLLBACK TO SAVEPOINT/RELEASE failure, which is tick-level: the
// transaction may already be gone.
func (s *AgentStore) reapOneRowInSavepoint(ctx context.Context, tx *sql.Tx, txClient *ent.Client, isPG bool, id uuid.UUID, reason reapReason, now time.Time, p store.ReaperParams) (updated *store.Agent, rowErrored bool, err error) {
	const savepoint = "launch_reap_row"
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		return nil, false, fmt.Errorf("launch reaper: SAVEPOINT: %w", err)
	}
	if err := injectFailure("savepoint"); err != nil {
		return nil, false, fmt.Errorf("launch reaper: SAVEPOINT: %w", err)
	}

	updated, rowErr := reapRow(ctx, txClient, isPG, id, reason, now, p)
	if rowErr != nil {
		if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); err != nil {
			return nil, false, fmt.Errorf("launch reaper: ROLLBACK TO SAVEPOINT: %w", err)
		}
		if err := injectFailure("rollback_to_savepoint"); err != nil {
			return nil, false, fmt.Errorf("launch reaper: ROLLBACK TO SAVEPOINT: %w", err)
		}
		slog.Warn("launch reaper: row reap failed; row left for a later tick", "agent_id", id.String(), "error", rowErr)
		return nil, true, nil
	}
	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint); err != nil {
		return nil, false, fmt.Errorf("launch reaper: RELEASE SAVEPOINT: %w", err)
	}
	if err := injectFailure("release_savepoint"); err != nil {
		return nil, false, fmt.Errorf("launch reaper: RELEASE SAVEPOINT: %w", err)
	}
	return updated, false, nil
}

// reapRow re-verifies the candidate under a Postgres FOR UPDATE SKIP LOCKED
// (a no-op lock on SQLite, which has one writer) and applies the reap write
// if the predicate still holds. Returns (nil, nil) on a benign skip (locked
// elsewhere, or a racing write already resolved the row).
func reapRow(ctx context.Context, txClient *ent.Client, isPG bool, id uuid.UUID, reason reapReason, now time.Time, p store.ReaperParams) (*store.Agent, error) {
	q := txClient.Agent.Query().Where(agent.IDEQ(id))
	if isPG {
		q = q.ForUpdate(entsql.WithLockAction(entsql.SkipLocked))
	}
	row, err := q.Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil // locked elsewhere, or the row is gone.
		}
		return nil, err
	}

	if row.LaunchState != store.LaunchStateActive || row.DeletedAt != nil {
		return nil, nil // a racing write already ended or deleted the launch.
	}

	if err := injectFailure("row_apply:" + id.String()); err != nil {
		return nil, err
	}

	switch reason {
	case reapDeadline:
		if row.LaunchDeadline == nil || !row.LaunchDeadline.Before(now) || !isInFlightPhase(row.Phase) {
			return nil, nil
		}
		upd := txClient.Agent.UpdateOneID(id).
			SetPhase(string(state.PhaseError)).
			SetLaunchError(store.LaunchErrorLaunchTimeout).
			SetLaunchState(store.LaunchStateEnded).
			SetLaunchEndReason(store.LaunchEndReasonTimedOut).
			SetStateVersion(row.StateVersion + 1)
		updated, err := withLaunchEndSettlement(upd, row, store.LaunchEndReasonTimedOut, now).Save(ctx)
		if err != nil {
			return nil, err
		}
		if err := releaseBrokerQuotaTx(ctx, txClient, id, now); err != nil {
			return nil, err
		}
		return entAgentToStore(updated), nil

	case reapStaleness:
		if row.LaunchLastReportAt == nil || !row.LaunchLastReportAt.Before(now.Add(-8*p.KeepaliveInterval)) || !isInFlightPhase(row.Phase) {
			return nil, nil
		}
		upd := txClient.Agent.UpdateOneID(id).
			SetPhase(string(state.PhaseError)).
			SetLaunchError(store.LaunchErrorBrokerLost).
			SetLaunchState(store.LaunchStateEnded).
			SetLaunchEndReason(store.LaunchEndReasonLost).
			SetStateVersion(row.StateVersion + 1)
		updated, err := withLaunchEndSettlement(upd, row, store.LaunchEndReasonLost, now).Save(ctx)
		if err != nil {
			return nil, err
		}
		if err := releaseBrokerQuotaTx(ctx, txClient, id, now); err != nil {
			return nil, err
		}
		return entAgentToStore(updated), nil

	case reapWindDown:
		if row.LaunchLastReportAt == nil || !row.LaunchLastReportAt.Before(now.Add(-8*p.KeepaliveInterval)) || !isWindDownPhase(row.Phase) {
			return nil, nil
		}
		newErr := row.LaunchError
		switch state.Phase(row.Phase) {
		case state.PhaseSuspended, state.PhaseStopping, state.PhaseStopped:
			newErr = store.LaunchErrorLaunchStopped
		case state.PhaseError:
			if newErr == "" {
				newErr = store.LaunchErrorBrokerLost
			}
		}
		upd := txClient.Agent.UpdateOneID(id).
			SetLaunchState(store.LaunchStateEnded).
			SetLaunchEndReason(store.LaunchEndReasonLost).
			SetLaunchError(newErr).
			SetStateVersion(row.StateVersion + 1)
		updated, err := withLaunchEndSettlement(upd, row, store.LaunchEndReasonLost, now).Save(ctx)
		if err != nil {
			return nil, err
		}
		return entAgentToStore(updated), nil
	}
	return nil, fmt.Errorf("launch reaper: unknown reap reason %d", reason)
}

func isInFlightPhase(phase string) bool {
	for _, p := range inFlightPhaseList {
		if p == phase {
			return true
		}
	}
	return false
}

func isWindDownPhase(phase string) bool {
	for _, p := range windDownPhaseList {
		if p == phase {
			return true
		}
	}
	return false
}

// bestEffortDisarm writes armed_since = a freshly read storeNow on a fresh,
// short-lived connection and context, after the failed tick's connection has
// been returned to the pool. Errors are logged,
// not returned: this is deliberately best-effort, and its failure just means
// the next completed tick (on any replica) disarms in its own arm check
// instead, per the design's "if no replica completes a tick within 20s"
// argument.
func (s *AgentStore) bestEffortDisarm(ctx context.Context) {
	disarmCtx, cancel := context.WithTimeout(context.Background(), launchReaperDisarmTimeout)
	defer cancel()

	// Prime dialect detection before checking out the connection below (same
	// gotcha as beginLaunchTx / RunLaunchReaperTick). This is always already
	// cached by the RunLaunchReaperTick call that led here, but priming
	// defensively costs nothing and does not depend on call order. Using ctx
	// here rather than disarmCtx is deliberate and safe even if ctx is
	// already cancelled or expired: s.dialect's detection query captures the
	// dialect via a predicate callback applied while the SQL is being built
	// (ent's sqlQuery, before the driver's Query(ctx, ...) call), not during
	// the actual round-trip, so a cancelled ctx cannot prevent the dialect
	// from being cached, on the first call or any other.
	dialectName := s.dialect(ctx)

	db := s.sqlDB()
	if db == nil {
		return
	}
	conn, err := db.Conn(disarmCtx)
	if err != nil {
		slog.Warn("launch reaper: best-effort disarm: acquiring connection failed", "error", err)
		return
	}
	defer func() { _ = conn.Close() }()

	tx, err := conn.BeginTx(disarmCtx, nil)
	if err != nil {
		slog.Warn("launch reaper: best-effort disarm: begin transaction failed", "error", err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	isPG := dialectName == dialect.Postgres
	now, err := storeNow(disarmCtx, tx, isPG)
	if err != nil {
		slog.Warn("launch reaper: best-effort disarm: storeNow failed", "error", err)
		return
	}

	txClient := newTxClient(dialectName, tx)

	err = txClient.LaunchReaperState.Create().
		SetID(launchReaperStateID).
		SetArmedSince(now).
		OnConflictColumns(launchreaperstate.FieldID).
		Update(func(u *ent.LaunchReaperStateUpsert) {
			u.SetArmedSince(now)
		}).
		Exec(disarmCtx)
	if err != nil {
		slog.Warn("launch reaper: best-effort disarm: write failed", "error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		slog.Warn("launch reaper: best-effort disarm: commit failed", "error", err)
		return
	}
	committed = true
}
