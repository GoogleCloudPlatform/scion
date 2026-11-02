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
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/limitdefinition"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagereservation"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file implements the T1 async-agent-create launch store methods
// (design t1-async-create-v11.md §3.3, §3.7): BeginLaunch, MarkLaunchAccepted
// and EndLaunch here; ApplyLaunchReport in launch_report.go;
// RunLaunchReaperTick in launch_reaper.go.
//
// Every method here builds its own hand-rolled database/sql transaction
// (beginLaunchTx) rather than using s.client.Tx(ctx) (a regular *ent.Tx).
// That is because storeNow — the store clock every launch write is defined
// against (§3.3) — is read with a raw "SELECT now()" on Postgres, and
// the repo's ent is generated with sql/upsert,sql/lock only (no
// sql/execquery), so a plain *ent.Tx cannot issue raw SQL (the same
// constraint that makes RunLaunchReaperTick build its transaction by hand).
// Building every launch writer the same way keeps one
// construction to review instead of two.

// sqlDB returns the *sql.DB backing s.client, or nil if the client is not
// backed by a database/sql driver (mirrors CompositeStore.DB).
func (s *AgentStore) sqlDB() *sql.DB {
	if drv, ok := entc.SQLDriver(s.client.Driver()); ok {
		return drv.DB()
	}
	return nil
}

// launchTx bundles the raw *sql.Tx (for storeNow and any other raw
// statement) with an *ent.Client bound to the same transaction and
// connection, plus a cleanup func that returns the connection to the pool.
// Callers must Commit or Rollback tx themselves, then always call cleanup.
type launchTx struct {
	tx      *sql.Tx
	client  *ent.Client
	cleanup func()
}

// txOnlyDriver adapts a *sql.Tx-backed entsql.Driver for ent's generated
// single-entity update/delete paths (UpdateOneID, DeleteOneID), which always
// call dialect.Driver.Tx once via sqlgraph.UpdateNode/DeleteNode — even for a
// field-only update with no edges. entsql.Driver's own Tx/BeginTx requires a
// genuine *sql.DB (it type-asserts the ExecQuerier to *sql.DB to call its
// BeginTx), which a driver wrapping an already-open *sql.Tx does not have.
// Overriding Tx to return a Commit/Rollback no-op wrapper around the same
// driver (dialect.NopTx) lets that internal call execute directly against our
// already-open transaction instead of erroring; the caller of
// BeginLaunch/EndLaunch/etc. commits or rolls back the real *sql.Tx itself,
// once, when the whole method's work is done.
type txOnlyDriver struct {
	*entsql.Driver
}

func (d txOnlyDriver) Tx(context.Context) (dialect.Tx, error) {
	return dialect.NopTx(d.Driver), nil
}

// newTxClient builds the *ent.Client every launch writer uses: ent builders
// issued through it run on tx's connection and participate in tx, including
// through UpdateOneID/DeleteOneID's internal nested-Tx call (see txOnlyDriver).
// It registers entc.ClearLaunchResumePhaseHook, so the launch writers' phase
// writes follow the same resume-memory rule as every other agent write
// (ptone/scion#4669).
func newTxClient(dialectName string, tx *sql.Tx) *ent.Client {
	drv := entsql.NewDriver(dialectName, entsql.Conn{ExecQuerier: tx})
	client := ent.NewClient(ent.Driver(txOnlyDriver{drv}))
	client.Use(entc.ClearLaunchResumePhaseHook)
	return client
}

// beginLaunchTx opens a fresh connection and transaction and wraps it in an
// *ent.Client so raw statements and ent builders share one connection,
// reused here for every launch writer — see the file comment above.
func (s *AgentStore) beginLaunchTx(ctx context.Context) (*launchTx, error) {
	dialectName := s.dialect()

	db := s.sqlDB()
	if db == nil {
		return nil, fmt.Errorf("launch store: not backed by a *sql.DB")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("launch store: acquiring connection: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("launch store: begin transaction: %w", err)
	}
	txClient := newTxClient(dialectName, tx)
	return &launchTx{
		tx:     tx,
		client: txClient,
		cleanup: func() {
			_ = conn.Close()
		},
	}, nil
}

// dialect returns the store's dialect name, for use as entsql.NewDriver's
// dialect argument. It is derived from usesRowLocks so the launch store and
// AgentStore's row-lock gating agree on one source: the driver's dialect.
func (s *AgentStore) dialect() string {
	if s.usesRowLocks() {
		return dialect.Postgres
	}
	return dialect.SQLite
}

// storeNow reads the store clock (design §3.3): Postgres reads "SELECT
// now()" on tx, so it is that transaction's start time and stays consistent
// with any FOR UPDATE reads in it; SQLite, single-process, uses the Go wall
// clock. Every launch store method calls this exactly once per transaction
// and passes the result as a parameter to every write and comparison in that
// transaction — never SQL time arithmetic.
func storeNow(ctx context.Context, tx *sql.Tx, isPostgres bool) (time.Time, error) {
	if !isPostgres {
		return time.Now(), nil
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, "SELECT now()").Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("launch store: SELECT now(): %w", err)
	}
	return now, nil
}

// BeginLaunch implements store.AgentStore.BeginLaunch.
func (s *AgentStore) BeginLaunch(ctx context.Context, agentID, kind string, timeout time.Duration) (string, error) {
	// Start and restart launches go through BeginStartLaunch, which carries
	// their options (ptone/scion#4669).
	if kind != store.LaunchKindCreate {
		return "", fmt.Errorf("%w: BeginLaunch kind must be %q, got %q", store.ErrInvalidInput, store.LaunchKindCreate, kind)
	}
	return s.beginLaunch(ctx, agentID, kind, timeout, store.BeginLaunchOptions{})
}

// BeginStartLaunch implements store.AgentStore.BeginStartLaunch.
func (s *AgentStore) BeginStartLaunch(ctx context.Context, agentID, kind string, timeout time.Duration, opts store.BeginLaunchOptions) (string, error) {
	if kind != store.LaunchKindStart && kind != store.LaunchKindRestart {
		return "", fmt.Errorf("%w: BeginStartLaunch kind must be %q or %q, got %q", store.ErrInvalidInput, store.LaunchKindStart, store.LaunchKindRestart, kind)
	}
	return s.beginLaunch(ctx, agentID, kind, timeout, opts)
}

// beginLaunchPhaseAllowed reports whether a launch of kind may begin on a
// row in phase: a create on created or provisioning; a start only on
// starting, the phase the start dispatch wrote for an uncounted prior
// phase; a restart on starting, running or stopped, which its synchronous
// stop leg can leave (ptone/scion#4669).
func beginLaunchPhaseAllowed(kind string, phase state.Phase) bool {
	switch kind {
	case store.LaunchKindCreate:
		return phase == state.PhaseCreated || phase == state.PhaseProvisioning
	case store.LaunchKindStart:
		return phase == state.PhaseStarting
	case store.LaunchKindRestart:
		return phase == state.PhaseStarting || phase == state.PhaseRunning || phase == state.PhaseStopped
	}
	return false
}

// beginLaunch is BeginLaunch and BeginStartLaunch: kind has been checked by
// the caller.
func (s *AgentStore) beginLaunch(ctx context.Context, agentID, kind string, timeout time.Duration, opts store.BeginLaunchOptions) (string, error) {
	uid, err := parseUUID(agentID)
	if err != nil {
		return "", err
	}

	ltx, err := s.beginLaunchTx(ctx)
	if err != nil {
		return "", err
	}
	defer ltx.cleanup()
	isPG := s.dialect() == dialect.Postgres
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
	current, err := q.Only(ctx)
	if err != nil {
		return "", mapError(err)
	}

	if !beginLaunchPhaseAllowed(kind, state.Phase(current.Phase)) {
		return "", store.ErrInvalidPhase
	}

	now, err := storeNow(ctx, ltx.tx, isPG)
	if err != nil {
		return "", err
	}

	newID := uuid.NewString()
	deadline := now.Add(timeout)

	// A previous active launch on this row becomes implicitly superseded:
	// its ID no longer matches the new launch_id (design §3.3). No explicit
	// write of its end state is needed or made here. The resume memory
	// (launch_resume_phase) is left alone: EffectiveRestingPhase ignores it
	// while this launch is active, and this launch's end rewrites it.
	upd := ltx.client.Agent.UpdateOneID(uid).
		SetLaunchID(newID).
		SetLaunchState(store.LaunchStateActive).
		SetLaunchKind(kind).
		SetLaunchDeadline(deadline).
		SetLaunchLastReportAt(now).
		SetLaunchOwner("").
		SetLaunchSeq(0).
		SetLaunchStep("").
		SetLaunchError("").
		SetLaunchCause("").
		SetLaunchPriorPhase(opts.PriorPhase).
		SetLaunchRevokeOnFail(opts.RevokeOnFail)
	if kind == store.LaunchKindRestart && current.Phase != string(state.PhaseStarting) {
		// A restart's start leg moves the row to starting, as a phase
		// change, so it bumps state_version (v11 §3.13).
		upd.SetPhase(string(state.PhaseStarting)).
			SetStateVersion(current.StateVersion + 1)
	}
	// Link the start claim of the matching kind to this launch, so only
	// this launch's end settles it.
	if _, err := linkLaunchClaim(upd, current, kind, newID, now).Save(ctx); err != nil {
		return "", mapError(err)
	}

	if err := ltx.tx.Commit(); err != nil {
		return "", fmt.Errorf("launch store: commit BeginLaunch: %w", err)
	}
	committed = true
	return newID, nil
}

// MarkLaunchAccepted implements store.AgentStore.MarkLaunchAccepted.
func (s *AgentStore) MarkLaunchAccepted(ctx context.Context, agentID, launchID, owner string) (store.Agent, error) {
	uid, err := parseUUID(agentID)
	if err != nil {
		return store.Agent{}, err
	}

	ltx, err := s.beginLaunchTx(ctx)
	if err != nil {
		return store.Agent{}, err
	}
	defer ltx.cleanup()
	isPG := s.dialect() == dialect.Postgres
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
	current, err := q.Only(ctx)
	if err != nil {
		return store.Agent{}, mapError(err)
	}

	if current.LaunchID != launchID || current.LaunchState != store.LaunchStateActive {
		// Not (or no longer) this launch: no-op — nothing was written, so
		// roll back rather than commit, consistent with every other
		// read-only exit in this file. The caller distinguishes
		// superseded/ended by comparing the returned Agent's LaunchID /
		// LaunchState against launchID.
		_ = ltx.tx.Rollback()
		committed = true
		return *entAgentToStore(current), nil
	}

	upd := ltx.client.Agent.UpdateOneID(uid)
	if owner != "" && current.LaunchOwner == "" {
		upd.SetLaunchOwner(owner)
	}
	if state.Phase(current.Phase) == state.PhaseCreated {
		upd.SetPhase(string(state.PhaseProvisioning))
	}
	updated, err := upd.Save(ctx)
	if err != nil {
		return store.Agent{}, mapError(err)
	}

	if err := ltx.tx.Commit(); err != nil {
		return store.Agent{}, fmt.Errorf("launch store: commit MarkLaunchAccepted: %w", err)
	}
	committed = true
	return *entAgentToStore(updated), nil
}

// EndLaunch implements store.AgentStore.EndLaunch.
func (s *AgentStore) EndLaunch(ctx context.Context, agentID, launchID, reason string) error {
	return s.endLaunch(ctx, agentID, launchID, reason, "")
}

// EndLaunchWithCode implements store.AgentStore.EndLaunchWithCode.
func (s *AgentStore) EndLaunchWithCode(ctx context.Context, agentID, launchID, reason, code string) error {
	return s.endLaunch(ctx, agentID, launchID, reason, code)
}

// endLaunch ends launchID with reason, and records code as the launch
// error when it is not empty.
func (s *AgentStore) endLaunch(ctx context.Context, agentID, launchID, reason, code string) error {
	uid, err := parseUUID(agentID)
	if err != nil {
		return err
	}

	ltx, err := s.beginLaunchTx(ctx)
	if err != nil {
		return err
	}
	defer ltx.cleanup()
	isPG := s.dialect() == dialect.Postgres
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
	current, err := q.Only(ctx)
	if err != nil {
		return mapError(err)
	}

	if current.LaunchID != launchID || current.LaunchState != store.LaunchStateActive {
		// id mismatch or already ended: no-op success (design §3.3). Nothing
		// was written, so roll back rather than commit — consistent with
		// every other read-only exit among the launch writers.
		_ = ltx.tx.Rollback()
		committed = true
		return nil
	}

	now, err := storeNow(ctx, ltx.tx, isPG)
	if err != nil {
		return err
	}
	upd := ltx.client.Agent.UpdateOneID(uid).
		SetLaunchState(store.LaunchStateEnded).
		SetLaunchEndReason(reason)
	if code != "" {
		upd.SetLaunchError(code)
	}
	if _, err := withLaunchEndSettlement(upd, current, reason, now).
		Save(ctx); err != nil {
		return mapError(err)
	}

	if err := ltx.tx.Commit(); err != nil {
		return fmt.Errorf("launch store: commit EndLaunch: %w", err)
	}
	committed = true
	return nil
}

// releaseBrokerQuotaTx releases row's max_agents_per_broker reservation,
// if any, through txClient, only when it is scoped to row's Runtime Broker
// (ptone/scion#5137). row is the agent row the caller read under its launch
// transaction's lock: its runtime_broker_id is the broker the launch was
// claimed for (a launch report is accepted only from that broker). A
// reservation on another broker is left as it is (the sweep owns it), and
// an agent with no broker releases nothing. The unique active
// (limit, resource) index means the one conditional write names at most one
// row. This must run through the tick's/report's own transaction client,
// never QuotaService.Release (pkg/hub/quota.go) or the ambient QuotaStore
// (quota_store.go) — on Postgres those take a second connection outside
// this transaction/savepoint, and on SQLite (pool forced to
// MaxOpenConns=1) that self-deadlocks until the caller's timeout. Zero rows
// matched, or no such limit definition at all (e.g. an unseeded test
// store), is success — not store.ErrNotFound, unlike
// QuotaStore.ReleaseReservation.
func releaseBrokerQuotaTx(ctx context.Context, txClient *ent.Client, row *ent.Agent, now time.Time) error {
	ld, err := txClient.LimitDefinition.Query().
		Where(limitdefinition.NameEQ(store.LimitMaxAgentsPerBroker)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil
		}
		return err
	}
	_, err = txClient.UsageReservation.Update().
		Where(
			usagereservation.LimitDefinitionIDEQ(ld.ID),
			usagereservation.ResourceIDEQ(row.ID.String()),
			usagereservation.ReleasedAtIsNil(),
			usagereservation.ScopeTypeEQ(usagereservation.ScopeType(store.QuotaScopeBroker)), usagereservation.ScopeIDEQ(row.RuntimeBrokerID),
		).
		SetReleasedAt(now).
		Save(ctx)
	return err
}
