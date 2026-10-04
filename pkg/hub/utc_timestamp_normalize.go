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

package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// UTCTimestampNormalizeExecutor runs the utc-timestamp-normalize maintenance
// migration: it rewrites stored timestamps to canonical UTC text so that
// SQL ordering and comparison are exact and rows with four-digit numeric
// zone abbreviations become readable. See entadapter.NormalizeUTCTimestamps.
// It honours the dryRun parameter and is safe to run again after it has
// completed: canonical values are skipped.
type UTCTimestampNormalizeExecutor struct {
	DB *sql.DB
	// Dialect is the ent dialect of DB.
	Dialect string
}

// Run implements MaintenanceExecutor.
func (e *UTCTimestampNormalizeExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	if e.DB == nil {
		return errors.New("the store has no SQL database handle; cannot normalize timestamps")
	}
	_, err := entadapter.NormalizeUTCTimestamps(ctx, e.DB, e.Dialect, logger, entadapter.TimestampNormalizeOptions{
		DryRun: params["dryRun"] == "true",
	})
	return err
}

// rerunnableMigrations lists migration-category operations that may run
// again after completing. utc-timestamp-normalize is idempotent, and the
// startup check keeps reporting rows it has not yet rewritten (rows written
// by an older binary, restored from a backup, or written by a later
// migration), so the remedy it names must stay available.
var rerunnableMigrations = map[string]bool{
	entadapter.UTCTimestampNormalizeKey: true,
}

// storeDB returns the store's *sql.DB and ent dialect, or nil and "" when
// the store has no SQL handle.
func (s *Server) storeDB() (*sql.DB, string) {
	if p, ok := s.store.(interface {
		DB() *sql.DB
		Dialect() string
	}); ok {
		return p.DB(), p.Dialect()
	}
	return nil, ""
}

// startupTimestampCheckTimeout bounds the startup timestamp check.
const startupTimestampCheckTimeout = 5 * time.Minute

// Startup log messages of checkStoredTimestamps. The release note and docs
// use the same wording.
const (
	msgTimestampsNeedNormalize = "stored timestamps are not in canonical UTC form: run the " +
		entadapter.UTCTimestampNormalizeKey + " maintenance operation (Admin -> Maintenance). " +
		"Tables whose rows cannot be read are repaired automatically at hub start, after a snapshot of the database; " +
		"if tables_unreadable lists tables here, run the operation immediately: every read of those tables fails. " +
		"Until it runs, ordering and paging over the listed tables can be wrong"
	msgTimestampsUnparseable = "stored timestamps that the " + entadapter.UTCTimestampNormalizeKey +
		" operation cannot parse remain; its run log (Admin -> Maintenance) lists them by table, column and rowid; " +
		"correct or clear those values. Every read of a table listed in tables_unreadable fails until then"
)

// startStoredTimestampCheck runs checkStoredTimestamps in the background with
// a bounded context. On a canonical store every ent time column is one full
// scan, so running it inline would add a cost that grows with the database
// to every start. It only logs, and nothing at start depends on its result.
// Each column is its own query, so on SQLite's single connection other work
// waits at most for one column's scan. A panic in the check is logged and
// does not take the hub down.
func (s *Server) startStoredTimestampCheck(ctx context.Context) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("timestamp check: recovered from panic", "panic", fmt.Sprint(r))
			}
		}()
		ctx, cancel := context.WithTimeout(ctx, startupTimestampCheckTimeout)
		defer cancel()
		storedTimestampCheck(s, ctx)
	}()
}

// storedTimestampCheck is the check startStoredTimestampCheck runs. A
// variable so tests can observe that it runs in the background.
var storedTimestampCheck = (*Server).checkStoredTimestamps

// checkStoredTimestamps logs at most one error and one warning when a SQLite
// store holds ent time values that are not canonical UTC text. Such rows
// misorder in SQL comparisons, and those with a four-digit numeric zone
// abbreviation (e.g. "+0545 +0545") make every ent read of their table fail
// until the utc-timestamp-normalize operation rewrites them. It logs table
// names only, never values.
func (s *Server) checkStoredTimestamps(ctx context.Context) {
	db, dbDialect := s.storeDB()
	if db == nil {
		return
	}
	chk, err := entadapter.CheckStoredTimestamps(ctx, db, dbDialect)
	if err != nil {
		slog.Warn("timestamp check: could not inspect stored timestamps", "error", err)
		return
	}
	if len(chk.NeedsRun) > 0 {
		slog.Error(msgTimestampsNeedNormalize, "tables", chk.NeedsRun,
			"tables_unreadable", intersectTables(chk.Unreadable, chk.NeedsRun))
	}
	if len(chk.UnparseableOnly) > 0 {
		// A table whose only leftovers do not parse can still be unreadable;
		// then no read of it succeeds, which is an error, not a warning.
		unreadable := intersectTables(chk.Unreadable, chk.UnparseableOnly)
		level := slog.LevelWarn
		if len(unreadable) > 0 {
			level = slog.LevelError
		}
		slog.Log(ctx, level, msgTimestampsUnparseable, "tables", chk.UnparseableOnly, "tables_unreadable", unreadable)
	}
}

// intersectTables returns the names in a that are also in b, in a's order.
func intersectTables(a, b []string) []string {
	out := []string{}
	for _, x := range a {
		if slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}
