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
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

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
	s := entadapter.NewCompositeStore(client)
	if err := s.Migrate(context.Background()); err != nil {
		_ = s.Close()
		return nil, err
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, nil
}
