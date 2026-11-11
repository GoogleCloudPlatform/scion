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

package entadapter

import (
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// timeColumnExpr returns the SQL text to use wherever a time column (for
// example created, updated or last_activity_event) is compared or ordered
// on.
//
// On SQLite these columns hold Go's default time.Time text. The store
// boundary (entc.OpenSQLite's UTC mutation hook and "_timezone=UTC" DSN
// option) writes every value as canonical UTC text, e.g.
// "2026-10-01 20:52:18.944713015 +0000 UTC". A row written before that
// boundary existed may still carry a monotonic-clock suffix
// (" m=+0.033607249") from a bare time.Now(); a cursor-decoded or
// read-back time never does, so the expression strips " m=" onward before
// comparing, and a row without a suffix is returned unchanged.
//
// The result compares in time order only if every row is UTC text. The
// normalization assumes it: the store boundary writes it, and for rows
// written before that boundary the utc-timestamp-normalize maintenance
// operation establishes it (the hub startup check reports tables that still
// need the operation). A NULL column stays NULL (instr(NULL, ...) is NULL,
// so the ELSE branch returns the column).
//
// On Postgres the columns are native timestamptz, so the bare column is used.
func timeColumnExpr(s *entsql.Selector, col string) string {
	c := s.C(col)
	if s.Dialect() == dialect.Postgres {
		return c
	}
	return fmt.Sprintf("(CASE WHEN instr(%s, ' m=') > 0 THEN substr(%s, 1, instr(%s, ' m=') - 1) ELSE %s END)", c, c, c, c)
}

// timeArg converts a time t into the parameter form that compares equal to
// timeColumnExpr's output for the same instant. On SQLite that is the
// default String() text of t in UTC, bound as TEXT so both sides of the
// comparison are text. The conversion is done here, not left to the driver,
// because a string argument bypasses the driver's "_timezone" handling:
// without it a time that time.Parse placed in a non-UTC Location (an offset
// matching time.Local) would bind as local-offset text and compare against
// the wrong rows. UTC() also drops any monotonic reading. On Postgres t is
// bound as a time.Time.
func timeArg(dialectName string, t time.Time) any {
	if dialectName == dialect.Postgres {
		return t
	}
	return t.UTC().String()
}
