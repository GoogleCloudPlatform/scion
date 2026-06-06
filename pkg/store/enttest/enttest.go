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

// Package enttest provides a single backend-selecting factory for Ent clients
// used by the store test suites. By default it returns an in-memory SQLite
// client; built with the `integration` tag and with SCION_TEST_POSTGRES_URL
// set, it returns a Postgres-backed client isolated in its own schema inside a
// per-package ephemeral database.
//
// This lets the same store tests (pkg/store/storetest, pkg/store/entadapter)
// run unchanged against both backends, proving CRUD parity between SQLite and
// Postgres.
//
// Lifecycle:
//   - Each test package wires MainSetup/MainTeardown into its TestMain so the
//     per-package ephemeral Postgres database is created once and dropped once.
//     Both are no-ops in the default (SQLite) build.
//   - NewClient(t) returns a fresh, migrated *ent.Client per call with cleanup
//     registered via t.Cleanup. Under Postgres each call gets its own schema so
//     tests never observe each other's rows.
package enttest

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
)

// NewClient returns a fresh, migrated Ent client for the active backend with
// cleanup registered on t. See the package doc for backend selection.
func NewClient(t *testing.T) *ent.Client {
	t.Helper()
	return newClient(t)
}

// MainSetup prepares package-level backend resources. Call from TestMain before
// m.Run(). No-op for the SQLite backend.
func MainSetup() { setup() }

// MainTeardown releases package-level backend resources. Call from TestMain
// after m.Run(). No-op for the SQLite backend.
func MainTeardown() { teardown() }

// newSQLiteClient opens an in-memory SQLite-backed Ent client, migrates it, and
// registers cleanup. It is the default backend and the fallback used by the
// integration build when SCION_TEST_POSTGRES_URL is unset. MaxOpenConns is
// pinned to 1 so the shared-cache in-memory database serializes writes, matching
// production SQLite behavior.
func newSQLiteClient(t *testing.T) *ent.Client {
	t.Helper()
	client, err := entc.OpenSQLite("file:"+t.Name()+"?mode=memory&cache=shared", entc.PoolConfig{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open sqlite ent client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := entc.AutoMigrate(context.Background(), client); err != nil {
		t.Fatalf("migrate sqlite ent client: %v", err)
	}
	return client
}
