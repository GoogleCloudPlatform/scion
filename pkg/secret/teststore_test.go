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
)

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
