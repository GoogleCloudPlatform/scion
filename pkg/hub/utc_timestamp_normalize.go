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
	"io"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// UTCTimestampNormalizeExecutor runs the utc-timestamp-normalize maintenance
// migration: it rewrites stored timestamps to canonical UTC text so that
// SQL ordering and comparison are exact and rows with four-digit numeric
// zone abbreviations become readable. See entadapter.NormalizeUTCTimestamps.
// It honours the dryRun parameter.
type UTCTimestampNormalizeExecutor struct {
	DB *sql.DB
}

// Run implements MaintenanceExecutor.
func (e *UTCTimestampNormalizeExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	if e.DB == nil {
		return errors.New("the store has no SQL database handle; cannot normalize timestamps")
	}
	_, err := entadapter.NormalizeUTCTimestamps(ctx, e.DB, logger, entadapter.TimestampNormalizeOptions{
		DryRun: params["dryRun"] == "true",
	})
	return err
}

// storeDB returns the store's *sql.DB, or nil when it has none.
func (s *Server) storeDB() *sql.DB {
	if p, ok := s.store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

// checkNonCanonicalTimestamps logs one error at hub start when a SQLite
// store holds ent time values that are not canonical UTC text. Such rows
// misorder in SQL comparisons, and those with a four-digit numeric zone
// abbreviation (e.g. "+0545 +0545") make every ent read of their table fail
// until the utc-timestamp-normalize operation rewrites them. The check never
// blocks start.
func (s *Server) checkNonCanonicalTimestamps(ctx context.Context) {
	db := s.storeDB()
	if db == nil {
		return
	}
	tables, err := entadapter.NonCanonicalTimestampTables(ctx, db)
	if err != nil {
		slog.Warn("timestamp check: could not inspect stored timestamps", "error", err)
		return
	}
	if len(tables) == 0 {
		return
	}
	slog.Error("stored timestamps are not in canonical UTC form; back up the database and run the "+
		entadapter.UTCTimestampNormalizeKey+" maintenance operation (Admin -> Maintenance). "+
		"Until it runs, ordering and paging over these tables can be wrong, and tables with "+
		"numeric zone abbreviations cannot be read",
		"tables", tables)
}
