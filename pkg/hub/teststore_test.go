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
// store is already migrated; callers may still invoke Migrate (it is
// idempotent).
//
// The store is closed in t.Cleanup. A migrated in-memory database holds
// several MiB of SQLite memory until its last connection closes, so a store
// a test forgets to close stays resident for the rest of the package run;
// enough of them tripped the pkg/hub memory guard (mem_guard_helpers_test.go).
// Closing twice is harmless, so callers that close the store themselves (for
// example to reopen a file-backed one) keep working.
func newTestStore(t testing.TB, url string) (store.Store, error) {
	t.Helper()
	var dsn string
	if url == ":memory:" {
		dsn = fmt.Sprintf("file:hubtest%d?mode=memory&cache=shared", testStoreSeq.Add(1))
	} else {
		dsn = "file:" + url + "?cache=shared"
	}
	return newTestStoreAt(t, dsn)
}

// newTestStoreAt opens a fresh, migrated Ent-backed store on the given SQLite
// DSN. Tests that need a second raw connection to the same database (for
// example to write legacy column text) pick the DSN themselves. Like
// newTestStore, it closes the store in t.Cleanup.
func newTestStoreAt(t testing.TB, dsn string) (store.Store, error) {
	t.Helper()
	// MaxOpenConns must be 1 for SQLite to serialize writes and avoid
	// "database is locked" errors under concurrent access (e.g. the parallel
	// per-agent writes in stop-all). This mirrors the production pool config in
	// cmd/server_foreground.go / pkg/config.
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	if err != nil {
		return nil, err
	}
	client.Agent.Use(defaultTestAgentOwner)
	s := entadapter.NewCompositeStore(client)
	if err := s.Migrate(context.Background()); err != nil {
		_ = s.Close()
		return nil, err
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, nil
}

// newTestHubServer builds a Server with New and registers srv.Shutdown in
// t.Cleanup, so the background goroutines New starts (decision audit
// worker, link-service and preview cleanup loops, broker-auth nonce cache,
// OIDC key loops, ...) stop when the test ends instead of keeping the whole
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
