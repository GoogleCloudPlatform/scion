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

package hubtestutil

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/txdiag"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// migratedTestStoreSeq names the private in-memory database of each
// OpenMigratedTestStore call.
var migratedTestStoreSeq atomic.Int64

// OpenMigratedTestStore opens a fresh Ent-backed store on a private
// shared-cache in-memory SQLite database, runs the full Migrate on it, and
// closes it in t.Cleanup (closing twice is harmless).
//
// It has the body of pkg/hub's newTestStoreAt
// (teststore_sqlite_helpers_test.go) on a fresh in-memory database, minus two
// things:
//   - no migrate-once template: every call runs the full Migrate (schema,
//     backfills and seeds), which is slower than pkg/hub's newTestStore;
//   - no defaultTestAgentOwner hook: that Agent create hook gives ownerless
//     agent fixtures pkg/hub's dev user as owner, and needs pkg/hub's
//     DevUserID, so it cannot live here. Tests that create agents should not
//     use this store.
//
// The pool is pinned to one connection, as in pkg/hub's openTestClient and
// the production SQLite pool config.
func OpenMigratedTestStore(t testing.TB) (store.Store, error) {
	t.Helper()
	dsn := fmt.Sprintf("file:hubtestmigrated%d?mode=memory&cache=shared", migratedTestStoreSeq.Add(1))
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1, Diag: txdiag.QuietConfig()})
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
