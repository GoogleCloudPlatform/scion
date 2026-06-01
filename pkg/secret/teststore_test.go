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

package secret

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
// preserves test legibility and cross-reference consistency while satisfying the
// UUID requirement.
func tid(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

// testStoreSeq generates unique in-memory database names so each call to
// newTestStore(":memory:") gets an isolated database.
var testStoreSeq atomic.Int64

// newTestStore opens a fresh Ent-backed store for tests, mirroring the
// production single-database layout. It is a drop-in replacement for the former
// raw-SQL constructor: pass ":memory:" for an isolated in-memory database or a
// file path for a persistent one. The returned store is already migrated.
func newTestStore(url string) (store.Store, error) {
	var dsn string
	if url == ":memory:" {
		dsn = fmt.Sprintf("file:secrettest%d?mode=memory&cache=shared", testStoreSeq.Add(1))
	} else {
		dsn = "file:" + url + "?cache=shared"
	}

	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{})
	if err != nil {
		return nil, err
	}
	if err := entc.AutoMigrate(context.Background(), client); err != nil {
		_ = client.Close()
		return nil, err
	}
	return entadapter.NewCompositeStore(client), nil
}
