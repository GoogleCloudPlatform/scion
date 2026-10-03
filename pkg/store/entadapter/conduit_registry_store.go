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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/conduitprincipalepoch"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/conduitsession"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/relayinstance"
)

// ConduitRegistryStore is the Ent-backed registry.Store (design conduit
// v2.1 §3.4) over relay_instances, conduit_sessions and
// conduit_principal_epochs. The same code runs on Postgres and SQLite.
//
// Like the launch store (see launch_store.go's file comment), every
// multi-statement method builds its own database/sql transaction and binds
// an *ent.Client to it (newTxClient), because the repo's ent is generated
// without sql/execquery and the two atomic counters (relay generation and
// principal epoch) are raw INSERT … ON CONFLICT … DO UPDATE … RETURNING
// statements that must run in the same transaction as ent builders.
type ConduitRegistryStore struct {
	client *ent.Client
}

var _ registry.Store = (*ConduitRegistryStore)(nil)

// NewConduitRegistryStore returns a registry.Store backed by client.
func NewConduitRegistryStore(client *ent.Client) *ConduitRegistryStore {
	return &ConduitRegistryStore{client: client}
}

// ConduitRegistry returns the Conduit registry store sharing this
// composite store's client.
func (c *CompositeStore) ConduitRegistry() *ConduitRegistryStore {
	return NewConduitRegistryStore(c.client)
}

func (s *ConduitRegistryStore) isPG() bool {
	return s.client.Driver().Dialect() == dialect.Postgres
}

// ph returns the n-th (1-based) bind placeholder for the active dialect.
func (s *ConduitRegistryStore) ph(n int) string {
	if s.isPG() {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// conduitTx is a raw transaction plus an ent client bound to it.
type conduitTx struct {
	tx     *sql.Tx
	client *ent.Client
	done   bool
}

func (t *conduitTx) commit() error {
	t.done = true
	return t.tx.Commit()
}

func (t *conduitTx) rollback() {
	if t.done {
		return
	}
	t.done = true
	_ = t.tx.Rollback()
}

// begin opens a transaction (database/sql pins one pooled connection to it
// until commit or rollback; nothing here needs per-connection state). readOnly selects a
// read-only REPEATABLE READ snapshot on Postgres (so a multi-query read is
// one consistent view); SQLite transactions are already serializable.
func (s *ConduitRegistryStore) begin(ctx context.Context, readOnly bool) (*conduitTx, error) {
	drv, ok := s.client.Driver().(*entsql.Driver)
	if !ok || drv.DB() == nil {
		return nil, errors.New("conduit registry store: not backed by a *sql.DB")
	}
	dialectName := drv.Dialect()
	var opts *sql.TxOptions
	if readOnly && dialectName == dialect.Postgres {
		opts = &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	}
	tx, err := drv.DB().BeginTx(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("conduit registry store: begin transaction: %w", err)
	}
	return &conduitTx{tx: tx, client: newTxClient(dialectName, tx)}, nil
}

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// RegisterRelay implements registry.Store. The generation is assigned by a
// single atomic upsert (the registry's seed r.Generation on insert,
// stored+1 on conflict), so re-registration is derived from the database
// and strictly increasing per instance_id regardless of wall-clock
// behaviour.
func (s *ConduitRegistryStore) RegisterRelay(ctx context.Context, r registry.RelayInstance) (int64, error) {
	q := fmt.Sprintf(`INSERT INTO relay_instances
  (instance_id, generation, internal_endpoint, public_endpoint, started_at, last_seen, draining)
VALUES (%s, %s, %s, %s, %s, %s, %s)
ON CONFLICT (instance_id) DO UPDATE SET
  generation = relay_instances.generation + 1,
  internal_endpoint = excluded.internal_endpoint,
  public_endpoint = excluded.public_endpoint,
  started_at = excluded.started_at,
  last_seen = excluded.last_seen,
  draining = excluded.draining
RETURNING generation`, s.ph(1), s.ph(2), s.ph(3), s.ph(4), s.ph(5), s.ph(6), s.ph(7))
	seed := r.Generation
	if seed <= 0 {
		seed = 1
	}
	t, err := s.begin(ctx, false)
	if err != nil {
		return 0, err
	}
	defer t.rollback()
	var gen int64
	if err := t.tx.QueryRowContext(ctx, q,
		r.InstanceID, seed, r.InternalEndpoint, nullIfEmpty(r.PublicEndpoint),
		r.StartedAt.UTC(), r.LastSeen.UTC(), false,
	).Scan(&gen); err != nil {
		return 0, fmt.Errorf("conduit registry store: register relay %q: %w", r.InstanceID, err)
	}
	if err := t.commit(); err != nil {
		return 0, fmt.Errorf("conduit registry store: register relay %q: commit: %w", r.InstanceID, err)
	}
	return gen, nil
}

// HeartbeatRelay implements registry.Store (generation-CAS update).
func (s *ConduitRegistryStore) HeartbeatRelay(ctx context.Context, instanceID string, gen int64, now time.Time) error {
	n, err := s.client.RelayInstance.Update().
		Where(relayinstance.ID(instanceID), relayinstance.Generation(gen)).
		SetLastSeen(now.UTC()).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("conduit registry store: heartbeat relay %q: %w", instanceID, err)
	}
	if n == 0 {
		return registry.ErrRelaySuperseded
	}
	return nil
}

// SetRelayDraining implements registry.Store (generation-CAS update).
func (s *ConduitRegistryStore) SetRelayDraining(ctx context.Context, instanceID string, gen int64, draining bool) error {
	n, err := s.client.RelayInstance.Update().
		Where(relayinstance.ID(instanceID), relayinstance.Generation(gen)).
		SetDraining(draining).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("conduit registry store: set relay draining %q: %w", instanceID, err)
	}
	if n == 0 {
		return registry.ErrRelaySuperseded
	}
	return nil
}

// DeleteSessionsOfOlderGenerations implements registry.Store.
func (s *ConduitRegistryStore) DeleteSessionsOfOlderGenerations(ctx context.Context, instanceID string, currentGen int64) (int, error) {
	n, err := s.client.ConduitSession.Delete().
		Where(conduitsession.RelayInstanceID(instanceID), conduitsession.RelayGenerationLT(currentGen)).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("conduit registry store: sweep relay %q generations < %d: %w", instanceID, currentGen, err)
	}
	return n, nil
}

// InsertSessionWithNextEpoch implements registry.Store. In one
// transaction it (1) bumps the principal's durable epoch with upsert …
// RETURNING (first, so SQLite takes its write lock up front and Postgres
// holds the epoch row lock until commit), (2) verifies the relay row is at
// the session's generation (FOR SHARE on Postgres, so a concurrent
// RegisterRelay of the same instance waits for this insert and its sweep
// then removes the row), and (3) inserts the session with that epoch.
func (s *ConduitRegistryStore) InsertSessionWithNextEpoch(ctx context.Context, rec registry.SessionRecord) (int64, error) {
	caps, err := json.Marshal(rec.Capabilities)
	if err != nil {
		return 0, fmt.Errorf("conduit registry store: marshal capabilities: %w", err)
	}
	epochQ := fmt.Sprintf(`INSERT INTO conduit_principal_epochs (principal_kind, principal_id, epoch)
VALUES (%s, %s, 1)
ON CONFLICT (principal_kind, principal_id) DO UPDATE SET epoch = conduit_principal_epochs.epoch + 1
RETURNING epoch`, s.ph(1), s.ph(2))
	relayQ := fmt.Sprintf(`SELECT generation FROM relay_instances WHERE instance_id = %s`, s.ph(1))
	if s.isPG() {
		relayQ += " FOR SHARE"
	}

	t, err := s.begin(ctx, false)
	if err != nil {
		return 0, err
	}
	defer t.rollback()

	var epoch int64
	if err := t.tx.QueryRowContext(ctx, epochQ, rec.PrincipalKind, rec.PrincipalID).Scan(&epoch); err != nil {
		return 0, fmt.Errorf("conduit registry store: bump epoch for %s/%s: %w", rec.PrincipalKind, rec.PrincipalID, err)
	}

	var relayGen int64
	switch err := t.tx.QueryRowContext(ctx, relayQ, rec.RelayInstanceID).Scan(&relayGen); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: relay %q is not registered", registry.ErrRelaySuperseded, rec.RelayInstanceID)
	case err != nil:
		return 0, fmt.Errorf("conduit registry store: read relay %q: %w", rec.RelayInstanceID, err)
	case relayGen != rec.RelayGeneration:
		return 0, fmt.Errorf("%w: relay %q is at generation %d, session claims %d",
			registry.ErrRelaySuperseded, rec.RelayInstanceID, relayGen, rec.RelayGeneration)
	}

	create := t.client.ConduitSession.Create().
		SetID(rec.SessionID).
		SetPrincipalKind(rec.PrincipalKind).
		SetPrincipalID(rec.PrincipalID).
		SetRelayInstanceID(rec.RelayInstanceID).
		SetRelayGeneration(rec.RelayGeneration).
		SetTransport(rec.Transport).
		SetEndpointIncarnation(rec.EndpointIncarnation).
		SetConnectionEpoch(epoch).
		SetDraining(rec.Draining).
		SetCapabilities(caps).
		SetConnectedAt(rec.ConnectedAt.UTC()).
		SetLastSeen(rec.LastSeen.UTC())
	if rec.ProjectID != "" {
		create.SetProjectID(rec.ProjectID)
	}
	if rec.ExecScope != "" {
		create.SetExecScope(rec.ExecScope)
	}
	if err := create.Exec(ctx); err != nil {
		if ent.IsConstraintError(err) {
			return 0, fmt.Errorf("%w: session %q: %v", registry.ErrInvalidInput, rec.SessionID, err)
		}
		return 0, fmt.Errorf("conduit registry store: insert session %q: %w", rec.SessionID, err)
	}
	if err := t.commit(); err != nil {
		return 0, fmt.Errorf("conduit registry store: insert session %q: commit: %w", rec.SessionID, err)
	}
	return epoch, nil
}

// TouchSession implements registry.Store.
func (s *ConduitRegistryStore) TouchSession(ctx context.Context, sessionID string, now time.Time) error {
	n, err := s.client.ConduitSession.Update().
		Where(conduitsession.ID(sessionID)).
		SetLastSeen(now.UTC()).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("conduit registry store: touch session %q: %w", sessionID, err)
	}
	if n == 0 {
		return registry.ErrSessionNotFound
	}
	return nil
}

// SetSessionDraining implements registry.Store.
func (s *ConduitRegistryStore) SetSessionDraining(ctx context.Context, sessionID string) error {
	n, err := s.client.ConduitSession.Update().
		Where(conduitsession.ID(sessionID)).
		SetDraining(true).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("conduit registry store: drain session %q: %w", sessionID, err)
	}
	if n == 0 {
		return registry.ErrSessionNotFound
	}
	return nil
}

// DeleteSessionCAS implements registry.Store: a single DELETE whose WHERE
// clause matches all three of session_id, relay_instance_id and
// relay_generation.
func (s *ConduitRegistryStore) DeleteSessionCAS(ctx context.Context, sessionID, relayInstanceID string, relayGen int64) (bool, error) {
	n, err := s.client.ConduitSession.Delete().
		Where(
			conduitsession.ID(sessionID),
			conduitsession.RelayInstanceID(relayInstanceID),
			conduitsession.RelayGeneration(relayGen),
		).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("conduit registry store: CAS delete session %q: %w", sessionID, err)
	}
	return n == 1, nil
}

// ListPrincipalSessions implements registry.Store.
func (s *ConduitRegistryStore) ListPrincipalSessions(ctx context.Context, principalKind, principalID string) (registry.PrincipalSessions, error) {
	t, err := s.begin(ctx, true)
	if err != nil {
		return registry.PrincipalSessions{}, err
	}
	defer t.rollback()
	ps, err := loadPrincipal(ctx, t.client, principalKind, principalID)
	if err != nil {
		return registry.PrincipalSessions{}, err
	}
	if err := t.commit(); err != nil {
		return registry.PrincipalSessions{}, fmt.Errorf("conduit registry store: read commit: %w", err)
	}
	return ps, nil
}

// ListPrincipalSessionsBySession implements registry.Store.
func (s *ConduitRegistryStore) ListPrincipalSessionsBySession(ctx context.Context, sessionID string) (registry.PrincipalSessions, bool, error) {
	t, err := s.begin(ctx, true)
	if err != nil {
		return registry.PrincipalSessions{}, false, err
	}
	defer t.rollback()
	row, err := t.client.ConduitSession.Query().
		Where(conduitsession.ID(sessionID)).
		Select(conduitsession.FieldPrincipalKind, conduitsession.FieldPrincipalID).
		Only(ctx)
	if ent.IsNotFound(err) {
		return registry.PrincipalSessions{}, false, nil
	}
	if err != nil {
		return registry.PrincipalSessions{}, false, fmt.Errorf("conduit registry store: read session %q: %w", sessionID, err)
	}
	ps, err := loadPrincipal(ctx, t.client, row.PrincipalKind, row.PrincipalID)
	if err != nil {
		return registry.PrincipalSessions{}, false, err
	}
	if err := t.commit(); err != nil {
		return registry.PrincipalSessions{}, false, fmt.Errorf("conduit registry store: read commit: %w", err)
	}
	return ps, true, nil
}

func loadPrincipal(ctx context.Context, c *ent.Client, kind, id string) (registry.PrincipalSessions, error) {
	ps := registry.PrincipalSessions{PrincipalKind: kind, PrincipalID: id}
	ep, err := c.ConduitPrincipalEpoch.Query().
		Where(conduitprincipalepoch.PrincipalKind(kind), conduitprincipalepoch.PrincipalID(id)).
		Only(ctx)
	switch {
	case ent.IsNotFound(err):
	case err != nil:
		return ps, fmt.Errorf("conduit registry store: read epoch %s/%s: %w", kind, id, err)
	default:
		ps.CurrentEpoch = ep.Epoch
	}
	rows, err := c.ConduitSession.Query().
		Where(conduitsession.PrincipalKind(kind), conduitsession.PrincipalID(id)).
		WithRelay().
		Order(ent.Desc(conduitsession.FieldLastSeen), ent.Asc(conduitsession.FieldID)).
		All(ctx)
	if err != nil {
		return ps, fmt.Errorf("conduit registry store: read sessions %s/%s: %w", kind, id, err)
	}
	for _, r := range rows {
		v, err := toSessionView(r)
		if err != nil {
			return ps, err
		}
		ps.Sessions = append(ps.Sessions, v)
	}
	return ps, nil
}

func toSessionView(r *ent.ConduitSession) (registry.SessionView, error) {
	var caps registry.Capabilities
	if len(r.Capabilities) > 0 && !bytes.Equal(bytes.TrimSpace(r.Capabilities), []byte("null")) {
		if err := json.Unmarshal(r.Capabilities, &caps); err != nil {
			return registry.SessionView{}, fmt.Errorf("conduit registry store: session %q capabilities: %w", r.ID, err)
		}
	}
	rec := registry.SessionRecord{
		SessionID:           r.ID,
		PrincipalKind:       r.PrincipalKind,
		PrincipalID:         r.PrincipalID,
		RelayInstanceID:     r.RelayInstanceID,
		RelayGeneration:     r.RelayGeneration,
		Transport:           r.Transport,
		EndpointIncarnation: r.EndpointIncarnation,
		ConnectionEpoch:     r.ConnectionEpoch,
		Draining:            r.Draining,
		Capabilities:        caps,
		ConnectedAt:         r.ConnectedAt.UTC(),
		LastSeen:            r.LastSeen.UTC(),
	}
	if r.ProjectID != nil {
		rec.ProjectID = *r.ProjectID
	}
	if r.ExecScope != nil {
		rec.ExecScope = *r.ExecScope
	}
	v := registry.SessionView{Session: rec}
	if ri := r.Edges.Relay; ri != nil {
		rel := registry.RelayInstance{
			InstanceID:       ri.ID,
			Generation:       ri.Generation,
			InternalEndpoint: ri.InternalEndpoint,
			StartedAt:        ri.StartedAt.UTC(),
			LastSeen:         ri.LastSeen.UTC(),
			Draining:         ri.Draining,
		}
		if ri.PublicEndpoint != nil {
			rel.PublicEndpoint = *ri.PublicEndpoint
		}
		v.Relay = &rel
	}
	return v, nil
}

// DeleteSessionsOfStaleRelays implements registry.Store. It is a single
// DELETE whose relay-staleness condition is a subquery, so a relay cannot be
// judged stale and then have sessions it re-touched deleted by a separate
// later statement, and the operation is write-first (no read-then-upgrade
// on SQLite). Relay rows are kept (generation monotonicity).
func (s *ConduitRegistryStore) DeleteSessionsOfStaleRelays(ctx context.Context, staleBefore time.Time) (int, error) {
	n, err := s.client.ConduitSession.Delete().
		Where(conduitsession.HasRelayWith(relayinstance.LastSeenLT(staleBefore.UTC()))).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("conduit registry store: reap sessions of stale relays: %w", err)
	}
	return n, nil
}

// DeleteStaleSessions implements registry.Store: one write-first DELETE on
// the session's own last_seen. Epoch rows are untouched.
func (s *ConduitRegistryStore) DeleteStaleSessions(ctx context.Context, staleBefore time.Time) (int, error) {
	n, err := s.client.ConduitSession.Delete().
		Where(conduitsession.LastSeenLT(staleBefore.UTC())).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("conduit registry store: reap stale sessions: %w", err)
	}
	return n, nil
}

// DeleteIdleRelays implements registry.Store: one DELETE of relay rows that
// are stale and have no sessions. On SQLite the writers are serialised, so
// the ON DELETE CASCADE never fires from here. On Postgres (READ COMMITTED)
// it can: if InsertSessionWithNextEpoch holds the relay row FOR SHARE and
// commits a session while this DELETE waits on that lock, the DELETE is not
// re-evaluated and the FK cascade removes the just-inserted session. That
// fails closed: the session is already relay_stale (never eligible or
// admissible), the relay gets ErrRelaySuperseded on its next heartbeat and
// ErrSessionNotFound on TouchSession, and closes it. The registry's
// MinRelayPruneAfter floor (1h) limits this to relays idle for an hour or
// more that still insert.
func (s *ConduitRegistryStore) DeleteIdleRelays(ctx context.Context, staleBefore time.Time) (int, error) {
	n, err := s.client.RelayInstance.Delete().
		Where(relayinstance.LastSeenLT(staleBefore.UTC()), relayinstance.Not(relayinstance.HasSessions())).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("conduit registry store: prune idle relays: %w", err)
	}
	return n, nil
}

// DeletePrincipalEpoch implements registry.Store.
func (s *ConduitRegistryStore) DeletePrincipalEpoch(ctx context.Context, principalKind, principalID string) error {
	_, err := s.client.ConduitPrincipalEpoch.Delete().
		Where(conduitprincipalepoch.PrincipalKind(principalKind), conduitprincipalepoch.PrincipalID(principalID)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("conduit registry store: delete epoch %s/%s: %w", principalKind, principalID, err)
	}
	return nil
}
