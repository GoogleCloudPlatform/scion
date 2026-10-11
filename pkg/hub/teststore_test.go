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

//go:build !no_sqlite

package hub

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	entgo "entgo.io/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hook"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/google/uuid"
	"modernc.org/sqlite"
)

// rootlessTestAgents holds the IDs of agents a test creates on purpose with
// no owner, creator or ancestry (markRootlessTestAgent).
var rootlessTestAgents sync.Map

// testAgentOwnerHookDisabled turns defaultTestAgentOwner off, for tests
// that prove a production create path writes a resolvable root on its own.
// Tests that set it must not run in parallel.
var testAgentOwnerHookDisabled atomic.Bool

// markRootlessTestAgent exempts the agent from defaultTestAgentOwner, for a
// test that needs an agent with no resolvable root.
func markRootlessTestAgent(id string) { rootlessTestAgents.Store(id, true) }

// defaultTestAgentOwner is an Agent create hook on the test store: an agent
// fixture created with no owner, no creator and no ancestry gets the dev user
// (a seeded platform admin) as its owner. Such a row has no resolvable root,
// and the hub refuses it at every agent standing check (ptone/scion#3433);
// fixtures that write bare agent rows stand for agents the test's admin
// created. Tests of the no-root refusal mark their agents with
// markRootlessTestAgent.
func defaultTestAgentOwner(next ent.Mutator) ent.Mutator {
	return hook.AgentFunc(func(ctx context.Context, m *ent.AgentMutation) (ent.Value, error) {
		if m.Op().Is(entgo.OpCreate) && !testAgentOwnerHookDisabled.Load() {
			_, hasOwner := m.OwnerID()
			_, hasCreator := m.CreatedBy()
			ancestry, _ := m.Ancestry()
			id, _ := m.ID()
			if _, rootless := rootlessTestAgents.Load(id.String()); !rootless && !hasOwner && !hasCreator && len(ancestry) == 0 {
				m.SetOwnerID(uuid.MustParse(DevUserID))
			}
		}
		// A full-row UpdateAgent from the fixture's own (ownerless) struct
		// keeps the owner this hook gave the row.
		if m.Op().Is(entgo.OpUpdate|entgo.OpUpdateOne) && m.OwnerIDCleared() && !testAgentOwnerHookDisabled.Load() {
			if ids, err := m.IDs(ctx); err == nil && len(ids) == 1 {
				if row, err := m.Client().Agent.Get(ctx, ids[0]); err == nil && row.OwnerID != nil &&
					row.OwnerID.String() == DevUserID && row.CreatedBy == nil && len(row.Ancestry) == 0 {
					m.ResetOwnerID()
					m.SetOwnerID(uuid.MustParse(DevUserID))
				}
			}
		}
		return next.Mutate(ctx, m)
	})
}

// testStoreSeq generates unique in-memory database names so each call to
// newTestStore(t, ":memory:") gets an isolated database.
var testStoreSeq atomic.Int64

// newTestStore opens a fresh Ent-backed store for tests, mirroring the
// production single-database layout (see cmd/server_foreground.go:initStore).
// It is a drop-in replacement for the former sqlite.New: pass ":memory:" for an
// isolated in-memory database or a file path for a persistent one. The returned
// store is already migrated. Do not Migrate it again unless data written
// since needs the migration's backfills: Migrate is idempotent, but a re-run
// on a migrated store costs several times a fresh one.
//
// A ":memory:" store is not migrated in place: it gets a private copy of the
// migrate-once template (testStoreTemplate), which holds the result of one
// full Migrate, seeds and backfills included, run once per test binary. A
// file path is migrated in place, because the file may already hold data (a
// test that reopens a store to simulate a restart).
//
// The store is closed in t.Cleanup. A migrated in-memory database holds
// several MiB of SQLite memory until its last connection closes, so a store
// a test forgets to close stays resident for the rest of the package run;
// enough of them tripped the pkg/hub memory guard (mem_guard_helpers_test.go).
// Closing twice is harmless, so callers that close the store themselves (for
// example to reopen a file-backed one) keep working.
func newTestStore(t testing.TB, url string) (store.Store, error) {
	t.Helper()
	if url != ":memory:" {
		return newTestStoreAt(t, "file:"+url+"?cache=shared")
	}
	dsn := fmt.Sprintf("file:hubtest%d?mode=memory&cache=shared", testStoreSeq.Add(1))
	client, err := openTestClient(dsn)
	if err != nil {
		return nil, err
	}
	s := entadapter.NewCompositeStore(client)
	if err := restoreTestStoreTemplate(context.Background(), s); err != nil {
		_ = s.Close()
		return nil, err
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, nil
}

// newTestStoreAt opens a fresh, migrated Ent-backed store on the given SQLite
// DSN. Tests that need a second raw connection to the same database (for
// example to write legacy column text) pick the DSN themselves. It always
// runs the full Migrate on the database, since a caller-chosen DSN may name
// a database that already holds data. Like newTestStore, it closes the store
// in t.Cleanup.
func newTestStoreAt(t testing.TB, dsn string) (store.Store, error) {
	t.Helper()
	client, err := openTestClient(dsn)
	if err != nil {
		return nil, err
	}
	s := entadapter.NewCompositeStore(client)
	if err := migrateTestStore(context.Background(), s); err != nil {
		_ = s.Close()
		return nil, err
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, nil
}

// openTestClient opens the Ent client behind every test store, with the
// test agent-owner hook installed.
func openTestClient(dsn string) (*ent.Client, error) {
	// MaxOpenConns must be 1 for SQLite to serialize writes and avoid
	// "database is locked" errors under concurrent access (e.g. the parallel
	// per-agent writes in stop-all). This mirrors the production pool config in
	// cmd/server_foreground.go / pkg/config.
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	if err != nil {
		return nil, err
	}
	client.Agent.Use(defaultTestAgentOwner)
	return client, nil
}

// testStoreTemplateURI names the shared-cache in-memory database that holds
// the migrate-once template. testStoreTemplateHolder keeps one raw driver
// connection to it open for the life of the test binary: an in-memory
// database is dropped when its last connection closes. A raw driver
// connection starts no goroutines, so the package-exit leak guard
// (leak_guard_helpers_test.go), which counts database/sql connection
// openers, does not see it.
const testStoreTemplateURI = "file:hubtesttemplate?mode=memory&cache=shared"

var (
	testStoreTemplateOnce   sync.Once
	testStoreTemplateErr    error
	testStoreTemplateHolder driver.Conn
)

// testStoreTemplate builds the migrate-once template the first time it is
// called: it runs one full CompositeStore.Migrate (schema, backfills and
// seeds) on the template database, under testMigrateMu like every other
// test migration. Later calls return the first call's error.
func testStoreTemplate(ctx context.Context) error {
	testStoreTemplateOnce.Do(func() {
		holder, err := (&sqlite.Driver{}).Open(testStoreTemplateURI)
		if err != nil {
			testStoreTemplateErr = fmt.Errorf("open test store template: %w", err)
			return
		}
		client, err := openTestClient(testStoreTemplateURI)
		if err != nil {
			_ = holder.Close()
			testStoreTemplateErr = fmt.Errorf("open test store template client: %w", err)
			return
		}
		s := entadapter.NewCompositeStore(client)
		err = migrateTestStore(ctx, s)
		_ = s.Close()
		if err != nil {
			_ = holder.Close()
			testStoreTemplateErr = fmt.Errorf("migrate test store template: %w", err)
			return
		}
		testStoreTemplateHolder = holder
	})
	return testStoreTemplateErr
}

// sqliteRestorer is the modernc.org/sqlite connection method that copies a
// whole database into the connection's main database (the SQLite online
// backup API, run in the restore direction).
type sqliteRestorer interface {
	NewRestore(srcURI string) (*sqlite.Backup, error)
}

// restoreTestStoreTemplate copies the migrate-once template into s's
// (fresh, empty) database. The copy is private: s shares no database or
// connection with the template or with any other test store.
func restoreTestStoreTemplate(ctx context.Context, s *entadapter.CompositeStore) error {
	if err := testStoreTemplate(ctx); err != nil {
		return err
	}
	db := s.DB()
	if db == nil {
		return fmt.Errorf("restore test store template: store exposes no *sql.DB")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("restore test store template: %w", err)
	}
	defer func() { _ = conn.Close() }()
	return conn.Raw(func(dc any) error {
		r, ok := dc.(sqliteRestorer)
		if !ok {
			return fmt.Errorf("restore test store template: driver connection %T has no NewRestore", dc)
		}
		b, err := r.NewRestore(testStoreTemplateURI)
		if err != nil {
			return fmt.Errorf("restore test store template: %w", err)
		}
		for {
			more, err := b.Step(-1)
			if err != nil {
				_ = b.Finish()
				return fmt.Errorf("restore test store template: %w", err)
			}
			if !more {
				break
			}
		}
		return b.Finish()
	})
}

// newTestHubServer builds a Server with New and registers srv.Shutdown in
// t.Cleanup, so the background goroutines New starts (link-service and preview
// cleanup loops, broker-auth nonce cache, OIDC key loops, ...) stop when the
// test ends instead of keeping the whole
// server graph reachable for the rest of the package run (ptone/scion#3641;
// the package-exit leak guard in leak_guard_helpers_test.go enforces it).
// Use it instead of calling New directly in tests.
//
// Ordering: t.Cleanup runs last-registered first, so the server shuts down
// before any store whose cleanup was registered earlier (newTestStore
// registers the store's Close when the store is created). A caller that
// registers its own store Close must do so before calling this helper.
// Shutdown is idempotent, so a test that shuts the server down itself (for
// example to simulate a restart) keeps working. On error nothing is
// registered: New tears down whatever it started before failing.
func newTestHubServer(t testing.TB, cfg ServerConfig, s store.Store) (*Server, error) {
	t.Helper()
	srv, err := New(cfg, s)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv, nil
}
