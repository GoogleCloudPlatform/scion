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
	"context"
	"database/sql"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
)

// rawStoredText returns CAST(col AS TEXT) of the row of table whose idCol is
// uid, read through drv so that inside WithTx it runs on the transaction's
// connection. found is false when no row has that ID or the value is NULL.
// For SQLite only.
//
// It and storedTextEQ make a compare-and-swap on a time column (typically
// updated) compare the exact stored value on SQLite, where an ent time
// column is TEXT. Rows written through ent are canonical ("... +0000 UTC";
// entc.OpenSQLite sets _timezone=UTC and registers UTCTimeHook), and a bound
// time.Time read back from them matches. But a legacy row written before
// that, or a write that bypasses the hook (raw SQL, an OnConflict upsert),
// can hold another form of the same instant (an RFC 3339 or non-UTC offset
// form, or a monotonic-clock suffix), which a bound time.Time never equals.
// So the store reads the raw text before its typed read and puts
// storedTextEQ in the UPDATE's WHERE. On Postgres the column is timestamptz
// and the generated <Col>EQ predicate compares instants. Used by
// DeactivateDelegationEdgeGuarded and TemplateStore.SetTemplateAgentConfig.
func rawStoredText(ctx context.Context, drv dialect.Driver, table, idCol, col string, uid uuid.UUID) (text string, found bool, err error) {
	rows := &entsql.Rows{}
	query := "SELECT CAST(" + col + " AS TEXT) FROM " + table + " WHERE " + idCol + " = ?"
	if err := drv.Query(ctx, query, []any{uid}, rows); err != nil {
		return "", false, mapError(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return "", false, mapError(rows.Err())
	}
	var raw sql.NullString
	if err := rows.Scan(&raw); err != nil {
		return "", false, mapError(err)
	}
	if err := rows.Err(); err != nil {
		return "", false, mapError(err)
	}
	if !raw.Valid {
		return "", false, nil
	}
	return raw.String, true, nil
}

// storedTextEQ is a WHERE predicate matching rows whose col holds exactly
// text, the value rawStoredText returned. Wrap it in the entity's predicate
// type (for example predicate.Template(storedTextEQ(...))).
func storedTextEQ(col, text string) func(*entsql.Selector) {
	return func(sel *entsql.Selector) {
		sel.Where(entsql.EQ(sel.C(col), text))
	}
}
