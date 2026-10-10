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
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubinstance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// hubInstanceMaxIDBytes caps a hub instance ID: POD_NAME (at most 253
// bytes) + "-" + a 36-character UUID, rounded up (F3 design §5.1).
const hubInstanceMaxIDBytes = 320

// HubInstanceStore implements store.HubInstanceStore using Ent ORM (table
// hub_instances). Each hub replica writes only its own row, so there is no
// contention between writers and no row locking.
type HubInstanceStore struct {
	client *ent.Client
}

// NewHubInstanceStore creates a new Ent-backed HubInstanceStore.
func NewHubInstanceStore(client *ent.Client) *HubInstanceStore {
	return &HubInstanceStore{client: client}
}

// Compile-time assertion that HubInstanceStore satisfies the
// store.HubInstanceStore sub-interface.
var _ store.HubInstanceStore = (*HubInstanceStore)(nil)

// now reads the store clock, the same rule as storeNow (launch_store.go):
// Postgres "SELECT now()" on the store's connection, so every replica uses
// the database's clock; SQLite, single-process, uses the Go wall clock. The
// result is bound as a parameter, so SQL never does time arithmetic.
func (s *HubInstanceStore) now(ctx context.Context) (time.Time, error) {
	drv := s.client.Driver()
	if drv.Dialect() != dialect.Postgres {
		return time.Now().UTC(), nil
	}
	var rows entsql.Rows
	if err := drv.Query(ctx, "SELECT now()", []any{}, &rows); err != nil {
		return time.Time{}, fmt.Errorf("hub instance store: SELECT now(): %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return time.Time{}, fmt.Errorf("hub instance store: SELECT now(): %w", err)
		}
		return time.Time{}, fmt.Errorf("hub instance store: SELECT now(): no row")
	}
	var now time.Time
	if err := rows.Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("hub instance store: SELECT now(): %w", err)
	}
	return now.UTC(), nil
}

// validHubInstanceID reports whether id is 1-320 bytes of printable ASCII.
func validHubInstanceID(id string) bool {
	if id == "" || len(id) > hubInstanceMaxIDBytes {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// UpsertHubInstance implements store.HubInstanceStore.
func (s *HubInstanceStore) UpsertHubInstance(ctx context.Context, in store.HubInstance) error {
	if !validHubInstanceID(in.ID) {
		return fmt.Errorf("%w: hub instance id must be 1-%d bytes of printable ASCII", store.ErrInvalidInput, hubInstanceMaxIDBytes)
	}
	now, err := s.now(ctx)
	if err != nil {
		return err
	}
	checks := in.Checks
	if checks == nil {
		checks = map[string]string{}
	}
	stats := in.Stats
	if len(stats) == 0 {
		stats = json.RawMessage("{}")
	}
	// The conflict branch reuses the INSERT values (UpdateX emits
	// "col = excluded.col") rather than setting them again, so the JSON
	// columns are encoded once (see PutUserTerminalWorkspace). started_at
	// is not in the update list, so it keeps its first-insert value.
	err = s.client.HubInstance.Create().
		SetID(in.ID).
		SetLabel(in.Label).
		SetVersion(in.Version).
		SetStatus(in.Status).
		SetChecks(checks).
		SetStats(stats).
		SetStartedAt(now).
		SetLastSeen(now).
		OnConflictColumns(hubinstance.FieldID).
		Update(func(u *ent.HubInstanceUpsert) {
			u.UpdateLabel()
			u.UpdateVersion()
			u.UpdateStatus()
			u.UpdateChecks()
			u.UpdateStats()
			u.UpdateLastSeen()
			u.ClearStoppedAt()
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert hub instance: %w", err)
	}
	return nil
}

// TouchHubInstance implements store.HubInstanceStore. It reads the row's
// stats, replaces only its "db" key and writes it back with last_seen. The
// read and the write need no transaction: each hub instance writes only its
// own row, from one goroutine. A row deleted in between (pruned) updates
// nothing and reports found=false, so the caller upserts.
func (s *HubInstanceStore) TouchHubInstance(ctx context.Context, id string, db *api.HubInstanceDBStats) (bool, error) {
	if !validHubInstanceID(id) {
		return false, fmt.Errorf("%w: hub instance id must be 1-%d bytes of printable ASCII", store.ErrInvalidInput, hubInstanceMaxIDBytes)
	}
	now, err := s.now(ctx)
	if err != nil {
		return false, err
	}
	row, err := s.client.HubInstance.Query().
		Where(hubinstance.IDEQ(id)).
		Select(hubinstance.FieldStats).
		Only(ctx)
	if ent.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("touch hub instance: read stats: %w", err)
	}
	stats, err := replaceHubInstanceStatsDB(row.Stats, db)
	if err != nil {
		return false, fmt.Errorf("touch hub instance: %w", err)
	}
	n, err := s.client.HubInstance.Update().
		Where(hubinstance.IDEQ(id)).
		SetLastSeen(now).
		SetStats(stats).
		Save(ctx)
	if err != nil {
		return false, fmt.Errorf("touch hub instance: %w", err)
	}
	return n > 0, nil
}

// replaceHubInstanceStatsDB returns stored with its "db" key set to db, or
// removed when db is nil. Every other key is kept byte for byte, so Touch
// never rewrites fields it does not own. Empty or null stats count as {}.
func replaceHubInstanceStatsDB(stored json.RawMessage, db *api.HubInstanceDBStats) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if len(stored) > 0 && string(stored) != "null" {
		if err := json.Unmarshal(stored, &fields); err != nil {
			return nil, fmt.Errorf("decode stats: %w", err)
		}
		if fields == nil {
			fields = map[string]json.RawMessage{}
		}
	}
	if db == nil {
		delete(fields, "db")
	} else {
		b, err := json.Marshal(db)
		if err != nil {
			return nil, fmt.Errorf("encode stats.db: %w", err)
		}
		fields["db"] = b
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode stats: %w", err)
	}
	return out, nil
}

// ListHubInstances implements store.HubInstanceStore. The cut is
// coalesce(stopped_at, last_seen) >= now - window, with now the store
// clock, bound as a parameter and written as two predicates so it needs no
// dialect-specific SQL.
func (s *HubInstanceStore) ListHubInstances(ctx context.Context, window time.Duration) ([]store.HubInstance, time.Time, error) {
	now, err := s.now(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	seenSince := now.Add(-window)
	rows, err := s.client.HubInstance.Query().
		Where(hubinstance.Or(
			hubinstance.And(hubinstance.StoppedAtIsNil(), hubinstance.LastSeenGTE(seenSince)),
			hubinstance.StoppedAtGTE(seenSince),
		)).
		Order(ent.Asc(hubinstance.FieldID)).
		All(ctx)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("list hub instances: %w", err)
	}
	out := make([]store.HubInstance, 0, len(rows))
	for _, r := range rows {
		out = append(out, entHubInstanceToStore(r))
	}
	return out, now, nil
}

// entHubInstanceToStore converts an Ent HubInstance to the store model,
// with every timestamp in UTC.
func entHubInstanceToStore(r *ent.HubInstance) store.HubInstance {
	h := store.HubInstance{
		ID:        r.ID,
		Label:     r.Label,
		Version:   r.Version,
		StartedAt: r.StartedAt.UTC(),
		LastSeen:  r.LastSeen.UTC(),
		Status:    r.Status,
		Checks:    r.Checks,
	}
	if r.StoppedAt != nil {
		t := r.StoppedAt.UTC()
		h.StoppedAt = &t
	}
	if h.Checks == nil {
		h.Checks = map[string]string{}
	}
	if len(r.Stats) > 0 && string(r.Stats) != "{}" && string(r.Stats) != "null" {
		h.Stats = r.Stats
	}
	return h
}
