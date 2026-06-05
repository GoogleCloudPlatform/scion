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

package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/spf13/cobra"
)

var (
	migrateFrom       string
	migrateTo         string
	migrateDropSource bool
	migrateKeepSource bool
	migrateBatchSize  int
)

// serverMigrateCmd implements Migration β from the PostgreSQL strategy: an
// entity-by-entity copy from an Ent-on-SQLite database to an Ent-on-Postgres
// database. Both endpoints share the same Ent schema, so the copy is a plain
// dependency-ordered transfer through the Ent client.
var serverMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate Hub data from SQLite to PostgreSQL",
	Long: `Copy all Hub state from an Ent-backed SQLite database into an
Ent-backed PostgreSQL database.

The migration is:
  - Dependency-ordered: parents are inserted before children so every foreign
    key resolves at insert time.
  - Idempotent: rows whose primary key already exists in the destination are
    skipped, so a failed run can be safely restarted.
  - Read-only on the source: the SQLite file is opened with PRAGMA query_only,
    so the running SQLite hub can stay up until you cut over.
  - Verified: source and destination row counts are compared after every
    entity, aborting on any mismatch.

By default the source SQLite file is left untouched (--keep-source). Pass
--drop-source for an explicit cutover that deletes the SQLite file after a
successful, verified migration.

Examples:
  # Dry-safe copy; leaves SQLite in place.
  scion server migrate \
    --from sqlite:///var/lib/scion/hub.db \
    --to "postgres://scion:secret@db.example.com:5432/scion?sslmode=require"

  # Explicit cutover: delete the SQLite file once migration succeeds.
  scion server migrate \
    --from sqlite:///var/lib/scion/hub.db \
    --to "postgres://scion:secret@db:5432/scion?sslmode=require" \
    --drop-source`,
	RunE: runServerMigrate,
}

func init() {
	serverCmd.AddCommand(serverMigrateCmd)

	serverMigrateCmd.Flags().StringVar(&migrateFrom, "from", "", "Source SQLite DSN (e.g. sqlite:///path/to/hub.db) [required]")
	serverMigrateCmd.Flags().StringVar(&migrateTo, "to", "", "Destination PostgreSQL DSN (e.g. postgres://user:pass@host:5432/db?sslmode=require) [required]")
	serverMigrateCmd.Flags().BoolVar(&migrateKeepSource, "keep-source", true, "Leave the source SQLite file untouched (default)")
	serverMigrateCmd.Flags().BoolVar(&migrateDropSource, "drop-source", false, "Delete the source SQLite file after a successful migration (explicit cutover)")
	serverMigrateCmd.Flags().IntVar(&migrateBatchSize, "batch-size", 0, "Max rows per bulk insert statement (0 = default)")

	_ = serverMigrateCmd.MarkFlagRequired("from")
	_ = serverMigrateCmd.MarkFlagRequired("to")
}

func runServerMigrate(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	if migrateBatchSize < 0 {
		return fmt.Errorf("batch size must be non-negative, got %d", migrateBatchSize)
	}

	srcDSN, srcPath, err := parseSQLiteSourceDSN(migrateFrom)
	if err != nil {
		return err
	}
	dstDSN, err := parsePostgresDestDSN(migrateTo)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Opening source (read-only): %s\n", srcPath)
	src, err := entc.OpenSQLiteReadOnly(srcDSN)
	if err != nil {
		return fmt.Errorf("opening source sqlite: %w", err)
	}
	defer func() {
		if src != nil {
			_ = src.Close()
		}
	}()

	fmt.Fprintln(out, "Opening destination PostgreSQL")
	dst, err := entc.OpenPostgres(dstDSN, entc.PoolConfig{MaxOpenConns: 10, MaxIdleConns: 5})
	if err != nil {
		return fmt.Errorf("opening destination postgres: %w", err)
	}
	defer dst.Close()

	fmt.Fprintln(out, "Ensuring destination schema (auto-migrate)")
	if err := entc.AutoMigrate(ctx, dst); err != nil {
		return fmt.Errorf("destination auto-migrate: %w", err)
	}

	fmt.Fprintln(out, "Migrating entities...")
	report, err := entc.MigrateData(ctx, src, dst, entc.MigrateOptions{
		BatchSize: migrateBatchSize,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(out, "  "+format+"\n", args...)
		},
	})
	if err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}

	total := 0
	for _, e := range report.Entities {
		total += e.Dest
	}
	fmt.Fprintf(out, "Migration complete: %d entities, %d rows total, %d child-group edges\n",
		len(report.Entities), total, report.ChildGroupEdgs)

	if migrateDropSource {
		_ = src.Close()
		src = nil
		fmt.Fprintf(out, "Dropping source SQLite file: %s\n", srcPath)
		if err := dropSQLiteFile(srcPath); err != nil {
			return fmt.Errorf("dropping source: %w", err)
		}
		fmt.Fprintln(out, "Source dropped.")
	} else {
		fmt.Fprintf(out, "Source left in place: %s\n", srcPath)
	}

	return nil
}

// parseSQLiteSourceDSN normalizes the --from value into a modernc.org/sqlite DSN
// and returns the bare filesystem path (for --drop-source). It accepts:
//
//	sqlite:///abs/path/hub.db   -> /abs/path/hub.db
//	sqlite://rel/path/hub.db    -> rel/path/hub.db
//	file:/abs/path/hub.db       -> passed through, path extracted
//	/abs/path/hub.db            -> bare path
func parseSQLiteSourceDSN(raw string) (dsn, path string, err error) {
	if raw == "" {
		return "", "", fmt.Errorf("--from is required")
	}
	switch {
	case strings.HasPrefix(raw, "sqlite://"):
		path = strings.TrimPrefix(raw, "sqlite://")
		// sqlite:///abs -> "/abs"; an extra leading slash denotes an absolute path.
	case strings.HasPrefix(raw, "sqlite:"):
		path = strings.TrimPrefix(raw, "sqlite:")
	case strings.HasPrefix(raw, "file://"):
		path = strings.TrimPrefix(raw, "file://")
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
	case strings.HasPrefix(raw, "file:"):
		path = strings.TrimPrefix(raw, "file:")
		// Strip any query parameters from the extracted path.
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
	default:
		path = raw
	}
	if path == "" {
		return "", "", fmt.Errorf("could not determine sqlite file path from %q", raw)
	}
	// cache=shared matches how the hub opens its SQLite database elsewhere.
	dsn = "file:" + path + "?cache=shared"
	return dsn, path, nil
}

// parsePostgresDestDSN validates the --to value and returns a DSN the pgx
// stdlib driver accepts. Both URL-style ("postgres://...") and keyword/value
// ("host=... port=...") DSNs are passed through unchanged.
func parsePostgresDestDSN(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("--to is required")
	}
	if strings.HasPrefix(raw, "postgres://") ||
		strings.HasPrefix(raw, "postgresql://") ||
		strings.Contains(raw, "host=") {
		return raw, nil
	}
	return "", fmt.Errorf("--to must be a PostgreSQL DSN (postgres://... or host=...), got %q", raw)
}

// dropSQLiteFile removes the SQLite database file and any WAL/SHM/journal
// sidecar files left next to it.
func dropSQLiteFile(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
