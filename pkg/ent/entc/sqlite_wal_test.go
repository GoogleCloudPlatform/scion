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

package entc

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/observability/txdiag"
)

// TestOpenSQLiteFileBackedEnablesWAL checks that opening a file-backed SQLite
// database puts it in WAL journal mode.
func TestOpenSQLiteFileBackedEnablesWAL(t *testing.T) {
	dsns := map[string]string{
		"bare-path":   filepath.Join(t.TempDir(), "bare.db"),
		"file-uri":    "file:" + filepath.Join(t.TempDir(), "uri.db"),
		"shared-file": "file:" + filepath.Join(t.TempDir(), "shared.db") + "?cache=shared",
	}
	for name, dsn := range dsns {
		t.Run(name, func(t *testing.T) {
			client, err := OpenSQLite(dsn, PoolConfig{MaxOpenConns: 1})
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })

			var mode string
			require.NoError(t, sqliteDB(t, client).QueryRow("PRAGMA journal_mode").Scan(&mode))
			require.Equal(t, "wal", mode)
		})
	}
}

// TestOpenSQLiteRejectsSimulatedNonWALMode checks that OpenSQLite fails closed
// when PRAGMA journal_mode = WAL returns a non-WAL mode without an error on a
// file-backed database (including when diagnostics wrap the connector).
func TestOpenSQLiteRejectsSimulatedNonWALMode(t *testing.T) {
	orig := setSQLiteJournalModeWAL
	t.Cleanup(func() { setSQLiteJournalModeWAL = orig })

	for _, reportedMode := range []string{"delete", "memory", "off"} {
		t.Run(reportedMode, func(t *testing.T) {
			setSQLiteJournalModeWAL = func(_ *sql.DB) (string, error) {
				return reportedMode, nil
			}
			path := filepath.Join(t.TempDir(), "hub.db")
			client, err := OpenSQLite("file:"+path, PoolConfig{MaxOpenConns: 1, Diag: txdiag.QuietConfig()})
			require.Error(t, err)
			require.Nil(t, client)
			require.EqualError(t, err,
				`enabling WAL mode: journal_mode is "`+reportedMode+`" after PRAGMA journal_mode = WAL, want "wal"`)
		})
	}
}

// TestOpenSQLiteRejectsFileBackedNonWALMode reproduces ptone/scion#7727 against
// real SQLite without stubs: when SQLite cannot switch a file-backed database
// to WAL (here immutable=1 on a rollback-journal file, just like a VFS without
// WAL support), PRAGMA journal_mode = WAL returns "delete" as a row without
// raising an error. OpenSQLite must fail closed rather than silently succeed.
func TestOpenSQLiteRejectsFileBackedNonWALMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	seed, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	seed.SetMaxOpenConns(1)
	_, err = seed.Exec(`CREATE TABLE seed (id INTEGER PRIMARY KEY)`)
	require.NoError(t, err)
	var initialMode string
	require.NoError(t, seed.QueryRow("PRAGMA journal_mode").Scan(&initialMode))
	require.Equal(t, "delete", initialMode)
	require.NoError(t, seed.Close())

	client, err := OpenSQLite("file:"+path+"?immutable=1", PoolConfig{MaxOpenConns: 1})
	if client != nil {
		_ = client.Close()
	}
	require.Error(t, err)
	require.Nil(t, client)
	require.EqualError(t, err,
		`enabling WAL mode: journal_mode is "delete" after PRAGMA journal_mode = WAL, want "wal"`)

	// When PRAGMA journal_mode = WAL itself fails (mode=ro refuses the header
	// write), OpenSQLite still surfaces that query error under the same prefix.
	client, err = OpenSQLite("file:"+path+"?mode=ro", PoolConfig{MaxOpenConns: 1})
	if client != nil {
		_ = client.Close()
	}
	require.Error(t, err)
	require.Nil(t, client)
	require.Contains(t, err.Error(), "enabling WAL mode:")
}

// TestOpenSQLiteAllowsInMemoryModes checks that in-memory SQLite databases,
// where PRAGMA journal_mode = WAL legitimately returns "memory", keep opening.
func TestOpenSQLiteAllowsInMemoryModes(t *testing.T) {
	dsns := map[string]string{
		"colon-memory":     ":memory:",
		"uri-colon-memory": "file::memory:?cache=shared",
		"uri-mode-memory":  "file:" + t.Name() + "?mode=memory&cache=shared",
		"vfs-memdb":        "file:/" + t.Name() + "?vfs=memdb",
	}
	for name, dsn := range dsns {
		t.Run(name, func(t *testing.T) {
			client, err := OpenSQLite(dsn, PoolConfig{MaxOpenConns: 1})
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })

			var mode string
			require.NoError(t, sqliteDB(t, client).QueryRow("PRAGMA journal_mode").Scan(&mode))
			require.Equal(t, "memory", mode)
		})
	}
}
