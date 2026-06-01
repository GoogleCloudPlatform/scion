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

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/google/uuid"
)

// tid deterministically maps a human-readable test identifier (e.g. "user-1")
// to a stable UUID string. The Ent-backed store uses UUID primary keys, so test
// fixtures cannot use arbitrary strings as IDs; wrapping a readable name in tid
// preserves test legibility and cross-reference consistency (tid("user-1")
// always returns the same UUID) while satisfying the UUID requirement.
func tid(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

// testStoreSeq generates unique in-memory database names so each call to
// newTestStore(":memory:") gets an isolated database.
var testStoreSeq atomic.Int64

// newTestStore opens a fresh Ent-backed store for tests, mirroring the
// production single-database layout (see cmd/server_foreground.go:initStore).
// It is a drop-in replacement for the former sqlite.New: pass ":memory:" for an
// isolated in-memory database or a file path for a persistent one. The returned
// store is already migrated; callers may still invoke Migrate (it is
// idempotent).
func newTestStore(url string) (store.Store, error) {
	var dsn string
	if url == ":memory:" {
		dsn = fmt.Sprintf("file:hubtest%d?mode=memory&cache=shared", testStoreSeq.Add(1))
	} else {
		dsn = "file:" + url + "?cache=shared"
	}

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
	return s, nil
}
