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

//go:build integration

// This file implements the Postgres test backend. It is compiled only with the
// `integration` build tag and is active only when SCION_TEST_POSTGRES_URL is
// set; otherwise NewClient transparently falls back to SQLite so the suite still
// runs under `go test -tags integration ./...`.
//
//	go test -tags integration -run TestCompositeStore_CRUDParity \
//	  ./pkg/store/... \
//	  with SCION_TEST_POSTGRES_URL=postgres://user:pass@host:5432/db?sslmode=require
//
// Isolation model:
//   - One ephemeral database is created per test package (MainSetup) and dropped
//     when the package finishes (MainTeardown) so concurrent runs never collide.
//   - Each NewClient call creates a uniquely-named schema inside that database
//     and points the Ent client's search_path at it, so every test gets a fresh,
//     empty set of tables and cannot observe rows created by other tests. The
//     schema is dropped (CASCADE) on test cleanup.
package enttest

import (
	"context"
	"database/sql"
	"log"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"

	// pgx stdlib driver registration ("pgx"). entc/driver_postgres.go also
	// imports it, but we keep it explicit so this file is self-describing.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// postgresURL is the operator-supplied connection string for the Postgres
// server. When empty, the backend is inactive and NewClient falls back to
// SQLite.
var postgresURL = os.Getenv("SCION_TEST_POSTGRES_URL")

var (
	// active reports whether a per-package ephemeral database was provisioned.
	active bool
	// pkgDBName is the name of the per-package ephemeral database.
	pkgDBName string
	// adminDB is connected to the per-package database and is used to create
	// and drop the per-test schemas.
	adminDB *sql.DB
)

// setup provisions the per-package ephemeral database. It is fatal on error so
// that a misconfigured integration run fails loudly rather than silently
// degrading to SQLite.
func setup() {
	if postgresURL == "" {
		return
	}

	server, err := sql.Open("pgx", postgresURL)
	if err != nil {
		log.Fatalf("enttest: opening postgres server connection: %v", err)
	}
	defer server.Close()
	if err := server.Ping(); err != nil {
		log.Fatalf("enttest: pinging postgres server: %v", err)
	}

	pkgDBName = "scion_test_" + hexID()
	if _, err := server.Exec("CREATE DATABASE " + pkgDBName); err != nil {
		log.Fatalf("enttest: creating ephemeral database %s: %v", pkgDBName, err)
	}

	dbURL, err := rewriteDatabase(postgresURL, pkgDBName)
	if err != nil {
		log.Fatalf("enttest: building ephemeral database URL: %v", err)
	}
	adminDB, err = sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("enttest: opening ephemeral database connection: %v", err)
	}
	if err := adminDB.Ping(); err != nil {
		log.Fatalf("enttest: pinging ephemeral database: %v", err)
	}

	active = true
	log.Printf("enttest: provisioned ephemeral postgres database %s", pkgDBName)
}

// teardown drops the per-package ephemeral database.
func teardown() {
	if !active {
		return
	}
	if adminDB != nil {
		_ = adminDB.Close()
		adminDB = nil
	}
	server, err := sql.Open("pgx", postgresURL)
	if err != nil {
		log.Printf("enttest: warning: reopening server to drop %s: %v", pkgDBName, err)
		return
	}
	defer server.Close()
	// FORCE terminates any lingering connections so the drop cannot hang.
	if _, err := server.Exec("DROP DATABASE IF EXISTS " + pkgDBName + " WITH (FORCE)"); err != nil {
		log.Printf("enttest: warning: dropping ephemeral database %s: %v", pkgDBName, err)
	}
	active = false
}

// newClient returns a Postgres-backed client isolated in its own schema, or a
// SQLite client when the Postgres backend is inactive.
func newClient(t *testing.T) *ent.Client {
	t.Helper()
	if !active {
		return newSQLiteClient(t)
	}

	schema := "t_" + hexID()
	if _, err := adminDB.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("enttest: creating schema %s: %v", schema, err)
	}

	clientURL, err := withSearchPath(postgresURL, pkgDBName, schema)
	if err != nil {
		t.Fatalf("enttest: building client URL: %v", err)
	}
	client, err := entc.OpenPostgres(clientURL, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	if err != nil {
		t.Fatalf("enttest: opening postgres ent client: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		if _, err := adminDB.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Logf("enttest: warning: dropping schema %s: %v", schema, err)
		}
	})

	if err := entc.AutoMigrate(context.Background(), client); err != nil {
		t.Fatalf("enttest: migrating postgres ent client: %v", err)
	}
	return client
}

// Active reports whether a per-package ephemeral Postgres database was
// provisioned (i.e. SCION_TEST_POSTGRES_URL was set and MainSetup succeeded).
// Integration tests that exercise Postgres-only behavior use it to skip cleanly
// when run without a live database.
func Active() bool { return active }

// NewSchemaURL creates and migrates a fresh, isolated schema inside the
// per-package ephemeral database and returns a connection URL whose search_path
// points at it. Cleanup drops the schema (CASCADE) on test completion.
//
// Unlike NewClient (which hands back a ready *ent.Client with a fixed pool), this
// returns the raw DSN so callers can open their own clients/pools — needed by the
// connection-pool stress tests (custom MaxOpenConns) and the multi-process tests
// (a stable DSN shared with a forked child process). The schema is migrated once
// here so every client opened against the returned URL sees the full table set.
//
// It skips the calling test when the Postgres backend is inactive.
func NewSchemaURL(t *testing.T) string {
	t.Helper()
	if !active {
		t.Skip("enttest: SCION_TEST_POSTGRES_URL not set; skipping Postgres-only integration test")
	}

	schema := "t_" + hexID()
	if _, err := adminDB.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("enttest: creating schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := adminDB.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Logf("enttest: warning: dropping schema %s: %v", schema, err)
		}
	})

	clientURL, err := withSearchPath(postgresURL, pkgDBName, schema)
	if err != nil {
		t.Fatalf("enttest: building schema URL: %v", err)
	}

	// Migrate once so the schema is fully provisioned; callers open their own
	// clients/pools against clientURL afterwards.
	client, err := entc.OpenPostgres(clientURL, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("enttest: opening migrate client for schema %s: %v", schema, err)
	}
	if err := entc.AutoMigrate(context.Background(), client); err != nil {
		_ = client.Close()
		t.Fatalf("enttest: migrating schema %s: %v", schema, err)
	}
	_ = client.Close()
	return clientURL
}

// hexID returns a 32-char lowercase hex identifier safe to embed in a Postgres
// database or schema name.
func hexID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// rewriteDatabase returns rawURL with its database path replaced by dbName.
func rewriteDatabase(rawURL, dbName string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.Path = "/" + dbName
	return u.String(), nil
}

// withSearchPath returns rawURL pointing at dbName with the connection
// search_path set to schema, so unqualified table creation/queries resolve to
// that schema.
func withSearchPath(rawURL, dbName, schema string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.Path = "/" + dbName
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
