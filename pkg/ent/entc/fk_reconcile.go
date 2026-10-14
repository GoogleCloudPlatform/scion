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

package entc

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// SQLite enforces foreign keys only on connections that set
// PRAGMA foreign_keys = ON, so a long-lived database can hold child rows
// whose parent was deleted without the declared ON DELETE action running.
// Atlas's SQLite migrator compares PRAGMA foreign_key_check before and
// after its migration transaction, keyed by rowid and foreign-key index;
// rebuilding a table renumbers both, so such pre-existing orphans look new
// and the migration aborts with "foreign key mismatch".
//
// reconcileSQLiteForeignKeys runs before the schema migration and applies
// each orphaned key's declared ON DELETE action, read from the database's
// own PRAGMA foreign_key_list: CASCADE deletes the orphaned rows, SET NULL
// clears the key columns. Keys whose action cannot be applied automatically
// (NO ACTION, RESTRICT, SET DEFAULT, or SET NULL on a NOT NULL column) are
// left untouched and reported, with repair SQL, in a diagnostic: logged at
// ERROR, and attached by AutoMigrate to the error if the schema migration
// then fails. A database that does not need such a rebuild keeps booting.

// fkReconcileMaxPasses bounds the repair loop. Each pass can only expose new
// orphans by deleting rows (CASCADE), so the depth is bounded by the length
// of the longest CASCADE chain in the schema.
const fkReconcileMaxPasses = 16

// fkKey identifies one foreign key of one child table.
type fkKey struct {
	table string
	id    int
}

// fkInfo is one foreign key as declared in the database.
type fkInfo struct {
	table    string
	id       int
	parent   string
	from     []string
	to       []string
	onDelete string
}

// fkOrphanGroup describes orphaned rows of one foreign key that were not
// repaired automatically.
type fkOrphanGroup struct {
	info  fkInfo
	count int
	// reason explains why the declared action was not applied.
	reason string
}

// fkReconcileResult reports what reconcileSQLiteForeignKeys did.
type fkReconcileResult struct {
	// Unrepaired lists orphans left in place, one entry per foreign key.
	Unrepaired []fkOrphanGroup
}

// diagnostic renders the unrepaired orphans with suggested repair SQL. It
// returns "" when there is nothing to report.
func (r fkReconcileResult) diagnostic() string {
	if len(r.Unrepaired) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("orphaned rows whose declared ON DELETE action cannot be applied automatically; " +
		"repair them by hand (back up the database first) and restart:")
	for _, g := range r.Unrepaired {
		if g.reason == reasonNoParent {
			fmt.Fprintf(&b, "\n  - %s(%s) -> %s ON DELETE %s: %d row(s) reference a table that does not exist",
				g.info.table, strings.Join(g.info.from, ", "), g.info.parent, g.info.onDelete, g.count)
			fmt.Fprintf(&b, "\n      inspect: SELECT * FROM %s WHERE %s;", quoteIdent(g.info.table), keyNotNull(g.info))
			continue
		}
		fmt.Fprintf(&b, "\n  - %s(%s) -> %s(%s) ON DELETE %s: %d orphaned row(s) (%s)",
			g.info.table, strings.Join(g.info.from, ", "), g.info.parent, strings.Join(g.info.to, ", "),
			g.info.onDelete, g.count, g.reason)
		pred := orphanPredicate(g.info)
		fmt.Fprintf(&b, "\n      inspect: SELECT * FROM %s WHERE %s;", quoteIdent(g.info.table), pred)
		fmt.Fprintf(&b, "\n      delete:  DELETE FROM %s WHERE %s;", quoteIdent(g.info.table), pred)
		if g.reason != reasonNotNull {
			fmt.Fprintf(&b, "\n      or null: UPDATE %s SET %s WHERE %s;", quoteIdent(g.info.table), setNullList(g.info.from), pred)
		}
	}
	return b.String()
}

const (
	reasonNotAuto = "action is not applied automatically"
	reasonNotNull = "SET NULL on a NOT NULL column"
	// reasonNoParent: the referenced table does not exist, so every
	// non-NULL reference is reported; no repair is suggested beyond
	// inspecting those rows.
	reasonNoParent = "parent table does not exist"
)

// fkViolations is the result of PRAGMA foreign_key_check: the violating
// rows of each foreign key, identified by rowid. Rows of WITHOUT ROWID
// tables have no rowid; they are only counted.
type fkViolations map[fkKey]*fkViolatingRows

type fkViolatingRows struct {
	rowids  map[int64]struct{}
	noRowid int
}

func (r *fkViolatingRows) count() int { return len(r.rowids) + r.noRowid }

// newSince returns how many rows of k violate in v but did not in before.
func (v fkViolations) newSince(before fkViolations, k fkKey) int {
	cur := v[k]
	if cur == nil {
		return 0
	}
	prev := before[k]
	if prev == nil {
		return cur.count()
	}
	n := 0
	for id := range cur.rowids {
		if _, ok := prev.rowids[id]; !ok {
			n++
		}
	}
	if cur.noRowid > prev.noRowid {
		n += cur.noRowid - prev.noRowid
	}
	return n
}

// checkForeignKeys runs PRAGMA foreign_key_check.
func checkForeignKeys(ctx context.Context, q sqlQuerier) (fkViolations, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return nil, fmt.Errorf("PRAGMA foreign_key_check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(fkViolations)
	for rows.Next() {
		var (
			table, parent string
			rowid         sql.NullInt64
			id            int
		)
		if err := rows.Scan(&table, &rowid, &parent, &id); err != nil {
			return nil, fmt.Errorf("PRAGMA foreign_key_check: scanning: %w", err)
		}
		k := fkKey{table: table, id: id}
		r := out[k]
		if r == nil {
			r = &fkViolatingRows{rowids: make(map[int64]struct{})}
			out[k] = r
		}
		if rowid.Valid {
			r.rowids[rowid.Int64] = struct{}{}
		} else {
			r.noRowid++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PRAGMA foreign_key_check: %w", err)
	}
	return out, nil
}

// sqlQuerier is the subset of *sql.Conn and *sql.Tx used here.
type sqlQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// foreignKeyList reads PRAGMA foreign_key_list(table), keyed by foreign key id.
func foreignKeyList(ctx context.Context, q sqlQuerier, table string) (map[int]fkInfo, error) {
	rows, err := q.QueryContext(ctx, "SELECT id, seq, \"table\", \"from\", \"to\", on_delete FROM pragma_foreign_key_list(?) ORDER BY id, seq", table)
	if err != nil {
		return nil, fmt.Errorf("PRAGMA foreign_key_list(%s): %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int]fkInfo)
	for rows.Next() {
		var (
			id, seq      int
			parent, from string
			to           sql.NullString
			onDelete     string
		)
		if err := rows.Scan(&id, &seq, &parent, &from, &to, &onDelete); err != nil {
			return nil, fmt.Errorf("PRAGMA foreign_key_list(%s): scanning: %w", table, err)
		}
		fk := out[id]
		fk.table, fk.id, fk.parent, fk.onDelete = table, id, parent, strings.ToUpper(onDelete)
		fk.from = append(fk.from, from)
		fk.to = append(fk.to, to.String)
		out[id] = fk
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PRAGMA foreign_key_list(%s): %w", table, err)
	}
	return out, nil
}

// fkTableColumns returns the table's columns (name -> NOT NULL) and its
// primary-key columns in key order.
func fkTableColumns(ctx context.Context, q sqlQuerier, table string) (map[string]bool, []string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name, \"notnull\", pk FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, nil, fmt.Errorf("PRAGMA table_info(%s): %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	notNull := make(map[string]bool)
	pkByPos := make(map[int]string)
	for rows.Next() {
		var (
			name   string
			nn, pk int
		)
		if err := rows.Scan(&name, &nn, &pk); err != nil {
			return nil, nil, fmt.Errorf("PRAGMA table_info(%s): scanning: %w", table, err)
		}
		notNull[name] = nn != 0
		if pk > 0 {
			pkByPos[pk] = name
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("PRAGMA table_info(%s): %w", table, err)
	}
	pk := make([]string, 0, len(pkByPos))
	for i := 1; i <= len(pkByPos); i++ {
		pk = append(pk, pkByPos[i])
	}
	return notNull, pk, nil
}

// resolveFK fills in implicit parent columns (a REFERENCES clause without a
// column list refers to the parent's primary key). ok is false when the
// parent table does not exist.
func resolveFK(ctx context.Context, q sqlQuerier, fk fkInfo) (fkInfo, bool, error) {
	parentCols, parentPK, err := fkTableColumns(ctx, q, fk.parent)
	if err != nil {
		return fk, false, err
	}
	if len(parentCols) == 0 {
		return fk, false, nil
	}
	implicit := true
	for _, c := range fk.to {
		if c != "" {
			implicit = false
		}
	}
	if implicit {
		if len(parentPK) != len(fk.from) {
			return fk, false, fmt.Errorf("foreign key %s(%s) -> %s: parent primary key has %d column(s)",
				fk.table, strings.Join(fk.from, ", "), fk.parent, len(parentPK))
		}
		fk.to = append([]string(nil), parentPK...)
	}
	return fk, true, nil
}

// orphanPredicate returns a WHERE clause, over the unaliased child table,
// matching rows that violate fk: every key column is non-NULL (SQLite skips
// the check when any is NULL) and no parent row matches.
func orphanPredicate(fk fkInfo) string {
	child := quoteIdent(fk.table)
	parts := make([]string, 0, len(fk.from)+1)
	match := make([]string, 0, len(fk.from))
	for i, c := range fk.from {
		parts = append(parts, fmt.Sprintf("%s.%s IS NOT NULL", child, quoteIdent(c)))
		match = append(match, fmt.Sprintf("p.%s = %s.%s", quoteIdent(fk.to[i]), child, quoteIdent(c)))
	}
	parts = append(parts, fmt.Sprintf("NOT EXISTS (SELECT 1 FROM %s AS p WHERE %s)",
		quoteIdent(fk.parent), strings.Join(match, " AND ")))
	return strings.Join(parts, " AND ")
}

// keyNotNull returns a WHERE clause matching rows whose key columns are all
// non-NULL, i.e. the rows SQLite checks against the parent.
func keyNotNull(fk fkInfo) string {
	parts := make([]string, len(fk.from))
	for i, c := range fk.from {
		parts[i] = quoteIdent(c) + " IS NOT NULL"
	}
	return strings.Join(parts, " AND ")
}

func setNullList(cols []string) string {
	sets := make([]string, len(cols))
	for i, c := range cols {
		sets[i] = quoteIdent(c) + " = NULL"
	}
	return strings.Join(sets, ", ")
}

// quoteIdent double-quotes a SQLite identifier. It is shared with the
// legacy-data migration, so it lives in this untagged file.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// errFKRepairIntroducedOrphans is returned when applying the declared
// actions would orphan a row that was not orphaned before, which can happen
// when a CASCADE delete removes a row that another table references through
// a key whose action is not applied automatically.
var errFKRepairIntroducedOrphans = errors.New("applying the declared ON DELETE actions would orphan further rows")

// reconcileSQLiteForeignKeys applies the declared ON DELETE action to the
// orphaned rows of every foreign key in db. It works in one transaction on
// one connection with foreign-key enforcement off, so the outcome does not
// depend on that connection's pragma state; cascades are followed by
// re-running the check until nothing more changes.
//
// The declared actions are applied even when other keys have orphans that
// are left in place: they are exactly what the schema says should have
// happened when the parent row was deleted. The repairs never orphan a row
// that was not orphaned before; if they would, nothing is changed and an
// error is returned. A database without orphans costs one
// PRAGMA foreign_key_check and is not written to.
func reconcileSQLiteForeignKeys(ctx context.Context, db *sql.DB) (fkReconcileResult, error) {
	var res fkReconcileResult
	conn, err := db.Conn(ctx)
	if err != nil {
		return res, fmt.Errorf("foreign key reconcile: acquiring connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	initial, err := checkForeignKeys(ctx, conn)
	if err != nil {
		return res, fmt.Errorf("foreign key reconcile: %w", err)
	}
	if len(initial) == 0 {
		return res, nil
	}

	var fkOn bool
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkOn); err != nil {
		return res, fmt.Errorf("foreign key reconcile: reading foreign_keys pragma: %w", err)
	}
	// The pragma is a no-op inside a transaction, so toggle it around one.
	if fkOn {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
			return res, fmt.Errorf("foreign key reconcile: disabling foreign_keys: %w", err)
		}
		defer func() {
			if _, err := conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON"); err != nil {
				// Never hand a connection with enforcement off back to the
				// pool: returning ErrBadConn from Raw makes database/sql
				// discard it.
				slog.Error("foreign key reconcile: re-enabling foreign_keys failed; discarding the connection", "error", err)
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}()
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("foreign key reconcile: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	fkCache := make(map[string]map[int]fkInfo)
	lookup := func(k fkKey) (fkInfo, bool, error) {
		fks, ok := fkCache[k.table]
		if !ok {
			var err error
			if fks, err = foreignKeyList(ctx, tx, k.table); err != nil {
				return fkInfo{}, false, err
			}
			fkCache[k.table] = fks
		}
		info, ok := fks[k.id]
		if !ok {
			return fkInfo{}, false, fmt.Errorf("%s has no foreign key with id %d", k.table, k.id)
		}
		return resolveFK(ctx, tx, info)
	}

	type applied struct {
		info fkInfo
		rows int64
	}
	var repairs []applied
	// skipped holds keys whose declared action is not applied, with why.
	skipped := make(map[fkKey]string)
	current := initial
	for pass := 0; pass < fkReconcileMaxPasses; pass++ {
		progressed := false
		for _, k := range sortedFKKeys(current) {
			if _, ok := skipped[k]; ok {
				continue
			}
			info, parentExists, err := lookup(k)
			if err != nil {
				return res, fmt.Errorf("foreign key reconcile: %w", err)
			}
			if !parentExists {
				// The parent table itself is missing: that is not an orphan
				// the declared action describes, so leave the rows alone.
				skipped[k] = reasonNoParent
				continue
			}
			var stmt string
			switch info.onDelete {
			case "CASCADE":
				stmt = fmt.Sprintf("DELETE FROM %s WHERE %s", quoteIdent(info.table), orphanPredicate(info))
			case "SET NULL":
				notNull, _, err := fkTableColumns(ctx, tx, info.table)
				if err != nil {
					return res, fmt.Errorf("foreign key reconcile: %w", err)
				}
				for _, c := range info.from {
					if notNull[c] {
						skipped[k] = reasonNotNull
					}
				}
				if _, ok := skipped[k]; ok {
					continue
				}
				stmt = fmt.Sprintf("UPDATE %s SET %s WHERE %s", quoteIdent(info.table), setNullList(info.from), orphanPredicate(info))
			default:
				skipped[k] = reasonNotAuto
				continue
			}
			r, err := tx.ExecContext(ctx, stmt)
			if err != nil {
				return res, fmt.Errorf("foreign key reconcile: repairing %s(%s): %w", info.table, strings.Join(info.from, ", "), err)
			}
			if n, _ := r.RowsAffected(); n > 0 {
				progressed = true
				repairs = append(repairs, applied{info: info, rows: n})
			}
		}
		if current, err = checkForeignKeys(ctx, tx); err != nil {
			return res, fmt.Errorf("foreign key reconcile: %w", err)
		}
		if !progressed {
			break
		}
	}

	// Every row still orphaned must have been orphaned at the start; a row
	// that is newly orphaned was orphaned by the repairs above.
	var introduced []fkOrphanGroup
	for _, k := range sortedFKKeys(current) {
		info, _, err := lookup(k)
		if err != nil {
			return res, fmt.Errorf("foreign key reconcile: %w", err)
		}
		if n := current.newSince(initial, k); n > 0 {
			introduced = append(introduced, fkOrphanGroup{info: info, count: n, reason: "would be orphaned by the automatic repair"})
			continue
		}
		reason, ok := skipped[k]
		if !ok {
			reason = "still orphaned after repair"
		}
		res.Unrepaired = append(res.Unrepaired, fkOrphanGroup{info: info, count: current[k].count(), reason: reason})
	}
	if len(introduced) > 0 {
		res = fkReconcileResult{Unrepaired: introduced}
		return res, fmt.Errorf("foreign key reconcile: %w; no changes were made\n%s", errFKRepairIntroducedOrphans, res.diagnostic())
	}

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("foreign key reconcile: commit: %w", err)
	}
	committed = true
	for _, a := range repairs {
		slog.Warn("repaired orphaned rows by applying the declared ON DELETE action before schema migration",
			"table", a.info.table, "column", strings.Join(a.info.from, ","), "parent", a.info.parent,
			"action", a.info.onDelete, "rows", a.rows)
	}
	if d := res.diagnostic(); d != "" {
		slog.Error("foreign key reconcile: " + d)
	}
	return res, nil
}

func sortedFKKeys(m fkViolations) []fkKey {
	keys := make([]fkKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].table != keys[j].table {
			return keys[i].table < keys[j].table
		}
		return keys[i].id < keys[j].id
	})
	return keys
}
