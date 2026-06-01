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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/store/storetest"
)

// compositeFactory returns a Factory that builds the production-shaped
// CompositeStore: a single Ent-managed database serving every domain. This is
// exactly the single-database layout used by the hub today (see
// cmd/server_foreground.go:initStore), so a green run proves the oracle works
// against the current backend.
//
// The backend (SQLite by default, Postgres under -tags integration with
// SCION_TEST_POSTGRES_URL set) is selected by enttest.NewClient, so the same
// oracle asserts identical observable behavior across both backends.
func compositeFactory(t *testing.T) store.Store {
	t.Helper()

	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	return cs
}

// TestCompositeStore_CRUDParity runs the full CRUD-parity oracle against the
// current CompositeStore across all ported domains.
func TestCompositeStore_CRUDParity(t *testing.T) {
	storetest.RunStoreSuite(t, compositeFactory)
}
