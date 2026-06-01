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

package storetest_test

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
	"github.com/GoogleCloudPlatform/scion/pkg/store/storetest"
	"github.com/stretchr/testify/require"
)

// compositeFactory returns a Factory that builds the production-shaped
// CompositeStore: a SQLite base store plus a separate Ent-managed database for
// the group and policy domains. This is exactly the dual-database layout used
// by the hub today (see cmd/server_foreground.go:initStore), so a green run
// proves the oracle works against the current backend.
//
// When Postgres lands (P3-2), an analogous postgresFactory can be passed to the
// same RunStoreSuite to assert identical observable behavior.
func compositeFactory(t *testing.T) store.Store {
	t.Helper()

	base, err := sqlite.New(":memory:")
	require.NoError(t, err)
	require.NoError(t, base.Migrate(context.Background()))

	entClient, err := entc.OpenSQLite("file:"+t.Name()+"?mode=memory&cache=shared", entc.PoolConfig{})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(context.Background(), entClient))

	cs := entadapter.NewCompositeStore(base, entClient)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestCompositeStore_CRUDParity runs the full CRUD-parity oracle against the
// current CompositeStore for the already-ported group and policy domains.
func TestCompositeStore_CRUDParity(t *testing.T) {
	storetest.RunStoreSuite(t, compositeFactory)
}
