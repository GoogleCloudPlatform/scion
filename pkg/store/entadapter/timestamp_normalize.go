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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"entgo.io/ent/schema/field"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/migrate"
	"github.com/GoogleCloudPlatform/scion/pkg/store/storedtime"
)

// UTCTimestampNormalizeKey is the maintenance operation key of the
// timestamp normalizer.
const UTCTimestampNormalizeKey = "utc-timestamp-normalize"

// defaultTimestampNormalizeBatch is the number of rows read per batch.
const defaultTimestampNormalizeBatch = 500

// Canonical text forms, as SQLite GLOB patterns.
//
// An ent time column on SQLite holds Go's time.Time.String() text; its
// canonical form is t.UTC().String(): "YYYY-MM-DD HH:MM:SS[.fraction] +0000 UTC",
// with no monotonic-clock suffix. A raw webchat_* column holds RFC 3339 text;
// its canonical form is t.UTC().Format(time.RFC3339Nano), ending in "Z".
//
// The patterns are the single definition of "canonical" for both the
// normalizer (which selects only non-canonical rows) and the hub startup
// check (which reports tables holding any), so the two cannot disagree.
const (
	globDateTime         = "[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]"
	globDateTimeT        = "[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]"
	globEntCanonical     = globDateTime + " +0000 UTC"
	globEntCanonicalFrac = globDateTime + ".*[1-9] +0000 UTC"
	globEntFracNonDigit  = globDateTime + ".*[^0-9]* +0000 UTC"
	globWebchatCanonical = globDateTimeT + "Z"
	globWebchatFrac      = globDateTimeT + ".*[1-9]Z"
	globWebchatNonDigit  = globDateTimeT + ".*[^0-9]*Z"
)

// columnFamily is the storage form of a time column.
type columnFamily int

const (
	familyEnt     columnFamily = iota // time.Time.String() text
	familyWebchat                     // RFC 3339 text
)

// nonCanonicalSQL returns a SQLite predicate that is true when col holds a
// non-empty value that is not in its family's canonical form.
func nonCanonicalSQL(col string, family columnFamily) string {
	c := "CAST(" + quoteIdent(col) + " AS TEXT)"
	whole, frac, nonDigit := globEntCanonical, globEntCanonicalFrac, globEntFracNonDigit
	if family == familyWebchat {
		whole, frac, nonDigit = globWebchatCanonical, globWebchatFrac, globWebchatNonDigit
	}
	// A fraction is canonical when it is one or more digits ending in a
	// non-zero digit (String() and RFC3339Nano both trim trailing zeros).
	// GLOB's "*" matches any text, so the fraction form also requires that
	// nothing but digits sits between the "." and the zone.
	return fmt.Sprintf("(%s IS NOT NULL AND %s <> '' AND NOT (%s GLOB '%s' OR (%s GLOB '%s' AND %s NOT GLOB '%s')))",
		c, c, c, whole, c, frac, c, nonDigit)
}

// canonicalText returns the canonical text for t in the given family.
func canonicalText(t time.Time, family columnFamily) string {
	if family == familyWebchat {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return t.UTC().String()
}

// TimestampNormalizeOptions configures NormalizeUTCTimestamps.
type TimestampNormalizeOptions struct {
	// DryRun reports what would change without writing.
	DryRun bool
	// BatchSize is the number of rows read per batch (default 500).
	BatchSize int
}

// TimestampNormalizeReport summarises a normalizer run.
type TimestampNormalizeReport struct {
	// Rewritten is the number of values rewritten (or, on a dry run, that
	// would be rewritten).
	Rewritten int
	// Unparseable is the number of values left alone because no supported
	// layout parses them.
	Unparseable int
}

// scalarTarget is one SQLite table and its time columns of one family.
type scalarTarget struct {
	table   string
	columns []string
	family  columnFamily
}

// jsonTarget is a JSON column holding embedded RFC 3339 times.
type jsonTarget struct {
	table  string
	column string
	// keys are the time-valued object keys. When array is true the column
	// holds an array of objects and the keys are looked up in each element;
	// otherwise the column holds one object.
	keys  []string
	array bool
}

// jsonTimeTargets lists every time.Time embedded in an ent JSON field (see
// jsonEmbeddedTimeAllowlist in pkg/ent/entc, which fails when a new one is
// added): access_policies.conditions (PolicyConditions.ValidFrom/ValidUntil)
// and agents.exposed_ports ([]ExposedPort.ExposedAt).
var jsonTimeTargets = []jsonTarget{
	{table: "access_policies", column: "conditions", keys: []string{"validFrom", "validUntil"}},
	{table: "agents", column: "exposed_ports", keys: []string{"exposedAt"}, array: true},
}

// NormalizeUTCTimestamps rewrites stored timestamps to their canonical UTC
// form. It implements the utc-timestamp-normalize maintenance operation.
//
// On SQLite it rewrites every ent time column (enumerated from
// migrate.Tables) to t.UTC().String(), every *_at column of every webchat_*
// table to RFC 3339 UTC, and the JSON-embedded times listed in
// jsonTimeTargets to RFC 3339 UTC. On Postgres, whose scalar time columns are
// timestamptz, it rewrites only the JSON-embedded times.
//
// Values are read as text (never scanned into time.Time, which fails for
// four-digit numeric zone abbreviations) and parsed with storedtime. Work is
// per table, in batches keyed by rowid (SQLite) or id (Postgres). Canonical
// values are skipped, so a second run changes nothing and an interrupted run
// can simply be run again. Each write is a compare-and-set against the text
// that was read, so a concurrent write is never overwritten. A value that
// does not parse is reported by table, column and row and left alone; values
// are never written to the log.
func NormalizeUTCTimestamps(ctx context.Context, db *sql.DB, log io.Writer, opts TimestampNormalizeOptions) (TimestampNormalizeReport, error) {
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultTimestampNormalizeBatch
	}
	n := &normalizer{db: db, log: log, opts: opts}
	sqlite, err := isSQLiteDB(ctx, db)
	if err != nil {
		return n.report, err
	}
	n.sqlite = sqlite
	if opts.DryRun {
		n.logf("dry run: no rows are written")
	}

	if sqlite {
		tables, err := sqliteTables(ctx, db)
		if err != nil {
			return n.report, err
		}
		targets, err := sqliteScalarTargets(ctx, db, tables)
		if err != nil {
			return n.report, err
		}
		for _, tg := range targets {
			if err := n.scalarTable(ctx, tg); err != nil {
				return n.report, fmt.Errorf("table %s: %w", tg.table, err)
			}
		}
		for _, jt := range jsonTimeTargets {
			if !tables[jt.table] {
				continue
			}
			if err := n.jsonColumn(ctx, jt); err != nil {
				return n.report, fmt.Errorf("table %s: %w", jt.table, err)
			}
		}
	} else {
		for _, jt := range jsonTimeTargets {
			if err := n.jsonColumn(ctx, jt); err != nil {
				return n.report, fmt.Errorf("table %s: %w", jt.table, err)
			}
		}
	}

	verb := "rewrote"
	if opts.DryRun {
		verb = "would rewrite"
	}
	n.logf("done: %s %d values, %d unparseable values left unchanged", verb, n.report.Rewritten, n.report.Unparseable)
	return n.report, nil
}

// NonCanonicalTimestampTables reports the ent tables that hold at least one
// time value not in canonical UTC String() form. It is a cheap startup probe
// (one EXISTS per column, stopping at the first match) for SQLite hubs that
// need the utc-timestamp-normalize operation; it uses the normalizer's own
// canonical predicate. On Postgres it returns nil.
func NonCanonicalTimestampTables(ctx context.Context, db *sql.DB) ([]string, error) {
	sqlite, err := isSQLiteDB(ctx, db)
	if err != nil || !sqlite {
		return nil, err
	}
	tables, err := sqliteTables(ctx, db)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, tg := range entTimeTargets(tables) {
		for _, col := range tg.columns {
			var found bool
			q := fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s WHERE %s)", quoteIdent(tg.table), nonCanonicalSQL(col, familyEnt))
			if err := db.QueryRowContext(ctx, q).Scan(&found); err != nil {
				return out, fmt.Errorf("probe %s.%s: %w", tg.table, col, err)
			}
			if found {
				out = append(out, tg.table)
				break
			}
		}
	}
	return out, nil
}

type normalizer struct {
	db     *sql.DB
	log    io.Writer
	opts   TimestampNormalizeOptions
	sqlite bool
	report TimestampNormalizeReport
}

func (n *normalizer) logf(format string, args ...any) {
	if n.log != nil {
		_, _ = fmt.Fprintf(n.log, format+"\n", args...)
	}
}

// cellUpdate is one compare-and-set write.
type cellUpdate struct {
	key    any // rowid (SQLite) or id text (Postgres)
	column string
	old    string
	new    string
}

// scalarTable normalizes one table's scalar time columns (SQLite only).
func (n *normalizer) scalarTable(ctx context.Context, tg scalarTarget) error {
	preds := make([]string, len(tg.columns))
	sel := make([]string, len(tg.columns))
	for i, c := range tg.columns {
		preds[i] = nonCanonicalSQL(c, tg.family)
		sel[i] = "CAST(" + quoteIdent(c) + " AS TEXT)"
	}
	q := fmt.Sprintf("SELECT rowid, %s FROM %s WHERE rowid > ? AND (%s) ORDER BY rowid LIMIT ?",
		strings.Join(sel, ", "), quoteIdent(tg.table), strings.Join(preds, " OR "))

	rewritten, unparseable := 0, 0
	last := int64(math.MinInt64)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var updates []cellUpdate
		count := 0
		err := func() error {
			rows, err := n.db.QueryContext(ctx, q, last, n.opts.BatchSize)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			vals := make([]sql.NullString, len(tg.columns))
			dest := make([]any, len(tg.columns)+1)
			var rowid int64
			dest[0] = &rowid
			for i := range vals {
				dest[i+1] = &vals[i]
			}
			for rows.Next() {
				if err := rows.Scan(dest...); err != nil {
					return err
				}
				count++
				last = rowid
				for i, v := range vals {
					if !v.Valid || v.String == "" {
						continue
					}
					t, err := storedtime.Parse(v.String)
					if err != nil {
						unparseable++
						n.logf("unparseable value left unchanged: table=%s column=%s rowid=%d", tg.table, tg.columns[i], rowid)
						continue
					}
					want := canonicalText(t, tg.family)
					if want == v.String {
						continue
					}
					updates = append(updates, cellUpdate{key: rowid, column: tg.columns[i], old: v.String, new: want})
				}
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		applied, err := n.apply(ctx, tg.table, updates, false)
		if err != nil {
			return err
		}
		rewritten += applied
		if count < n.opts.BatchSize {
			break
		}
	}
	n.report.Rewritten += rewritten
	n.report.Unparseable += unparseable
	if rewritten > 0 || unparseable > 0 {
		n.logf("table %s: %d values %s, %d unparseable", tg.table, rewritten, n.rewroteWord(), unparseable)
	}
	return nil
}

func (n *normalizer) rewroteWord() string {
	if n.opts.DryRun {
		return "to rewrite"
	}
	return "rewritten"
}

// jsonColumn normalizes the embedded times of one JSON column.
func (n *normalizer) jsonColumn(ctx context.Context, jt jsonTarget) error {
	rewritten, unparseable := 0, 0
	var lastRowid = int64(math.MinInt64)
	var lastID string
	first := true
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var q string
		var args []any
		col := quoteIdent(jt.column)
		tbl := quoteIdent(jt.table)
		if n.sqlite {
			q = fmt.Sprintf("SELECT rowid, CAST(%s AS TEXT) FROM %s WHERE rowid > ? AND %s IS NOT NULL ORDER BY rowid LIMIT ?", col, tbl, col)
			args = []any{lastRowid, n.opts.BatchSize}
		} else if first {
			q = fmt.Sprintf("SELECT id::text, %s::text FROM %s WHERE %s IS NOT NULL ORDER BY id LIMIT $1", col, tbl, col)
			args = []any{n.opts.BatchSize}
		} else {
			q = fmt.Sprintf("SELECT id::text, %s::text FROM %s WHERE %s IS NOT NULL AND id > $1 ORDER BY id LIMIT $2", col, tbl, col)
			args = []any{lastID, n.opts.BatchSize}
		}
		first = false

		var updates []cellUpdate
		count := 0
		err := func() error {
			rows, err := n.db.QueryContext(ctx, q, args...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var raw sql.NullString
				var key any
				var rowLabel string
				if n.sqlite {
					var rowid int64
					if err := rows.Scan(&rowid, &raw); err != nil {
						return err
					}
					lastRowid, key, rowLabel = rowid, rowid, fmt.Sprintf("rowid=%d", rowid)
				} else {
					var id string
					if err := rows.Scan(&id, &raw); err != nil {
						return err
					}
					lastID, key, rowLabel = id, id, "id="+id
				}
				count++
				if !raw.Valid || raw.String == "" {
					continue
				}
				out, changed, err := rewriteJSONTimes(raw.String, jt)
				if err != nil {
					unparseable++
					n.logf("unparseable value left unchanged: table=%s column=%s %s", jt.table, jt.column, rowLabel)
					continue
				}
				if changed {
					updates = append(updates, cellUpdate{key: key, column: jt.column, old: raw.String, new: out})
				}
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		applied, err := n.apply(ctx, jt.table, updates, true)
		if err != nil {
			return err
		}
		rewritten += applied
		if count < n.opts.BatchSize {
			break
		}
	}
	n.report.Rewritten += rewritten
	n.report.Unparseable += unparseable
	if rewritten > 0 || unparseable > 0 {
		n.logf("table %s column %s: %d values %s, %d unparseable", jt.table, jt.column, rewritten, n.rewroteWord(), unparseable)
	}
	return nil
}

// apply writes one batch of compare-and-set updates in a transaction and
// returns how many took effect. A row whose text changed since it was read
// (a concurrent write) is skipped. On a dry run it writes nothing and
// returns len(updates).
func (n *normalizer) apply(ctx context.Context, table string, updates []cellUpdate, isJSON bool) (int, error) {
	if len(updates) == 0 {
		return 0, nil
	}
	if n.opts.DryRun {
		return len(updates), nil
	}
	tx, err := n.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	applied := 0
	for _, u := range updates {
		col := quoteIdent(u.column)
		var q string
		switch {
		case n.sqlite:
			q = fmt.Sprintf("UPDATE %s SET %s = ? WHERE rowid = ? AND CAST(%s AS TEXT) = ?", quoteIdent(table), col, col)
		case isJSON:
			q = fmt.Sprintf("UPDATE %s SET %s = $1::jsonb WHERE id = $2 AND %s::text = $3", quoteIdent(table), col, col)
		default:
			return 0, errors.New("scalar rewrite is SQLite-only")
		}
		res, err := tx.ExecContext(ctx, q, u.new, u.key, u.old)
		if err != nil {
			return 0, err
		}
		if k, err := res.RowsAffected(); err == nil {
			applied += int(k)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return applied, nil
}

// rewriteJSONTimes returns raw with the target's time keys in canonical
// RFC 3339 UTC form, and whether anything changed. Only those keys are
// touched; numbers keep their text (json.Number). A time key holding
// anything but null or a parseable RFC 3339 string is an error.
func rewriteJSONTimes(raw string, jt jsonTarget) (string, bool, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", false, err
	}
	changed := false
	fix := func(obj map[string]any) error {
		for _, k := range jt.keys {
			val, ok := obj[k]
			if !ok || val == nil {
				continue
			}
			s, ok := val.(string)
			if !ok {
				return fmt.Errorf("key %s is not a string", k)
			}
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return storedtime.ErrUnparseable
			}
			if want := t.UTC().Format(time.RFC3339Nano); want != s {
				obj[k] = want
				changed = true
			}
		}
		return nil
	}
	switch x := v.(type) {
	case nil:
		return raw, false, nil
	case map[string]any:
		if jt.array {
			return "", false, errors.New("want an array")
		}
		if err := fix(x); err != nil {
			return "", false, err
		}
	case []any:
		if !jt.array {
			return "", false, errors.New("want an object")
		}
		for _, el := range x {
			if el == nil {
				continue
			}
			obj, ok := el.(map[string]any)
			if !ok {
				return "", false, errors.New("want an array of objects")
			}
			if err := fix(obj); err != nil {
				return "", false, err
			}
		}
	default:
		return "", false, errors.New("unexpected JSON value")
	}
	if !changed {
		return raw, false, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", false, err
	}
	return string(bytes.TrimSpace(out)), true, nil
}

// isSQLiteDB reports whether db is a SQLite database.
func isSQLiteDB(ctx context.Context, db *sql.DB) (bool, error) {
	if db == nil {
		return false, errors.New("no database handle")
	}
	var v string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&v); err != nil {
		// Postgres has no sqlite_version(); confirm the connection works.
		if pingErr := db.PingContext(ctx); pingErr != nil {
			return false, pingErr
		}
		return false, nil
	}
	return true, nil
}

// sqliteTables returns the names of the ordinary tables in a SQLite DB.
func sqliteTables(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// entTimeTargets returns every field.TypeTime column of every ent table
// (from migrate.Tables) present in tables.
func entTimeTargets(tables map[string]bool) []scalarTarget {
	var out []scalarTarget
	for _, t := range migrate.Tables {
		if !tables[t.Name] {
			continue
		}
		var cols []string
		for _, c := range t.Columns {
			if c.Type == field.TypeTime {
				cols = append(cols, c.Name)
			}
		}
		if len(cols) > 0 {
			out = append(out, scalarTarget{table: t.Name, columns: cols, family: familyEnt})
		}
	}
	return out
}

// sqliteScalarTargets returns the ent time columns and the *_at columns of
// every webchat_* table (discovered with PRAGMA table_info).
func sqliteScalarTargets(ctx context.Context, db *sql.DB, tables map[string]bool) ([]scalarTarget, error) {
	out := entTimeTargets(tables)
	var webchat []string
	for name := range tables {
		if strings.HasPrefix(name, "webchat_") {
			webchat = append(webchat, name)
		}
	}
	sort.Strings(webchat)
	for _, name := range webchat {
		cols, err := sqliteColumns(ctx, db, name)
		if err != nil {
			return nil, err
		}
		var at []string
		for _, c := range cols {
			if strings.HasSuffix(c, "_at") {
				at = append(at, c)
			}
		}
		if len(at) > 0 {
			out = append(out, scalarTarget{table: name, columns: at, family: familyWebchat})
		}
	}
	return out, nil
}

func sqliteColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// quoteIdent quotes a SQL identifier (valid on SQLite and Postgres).
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
