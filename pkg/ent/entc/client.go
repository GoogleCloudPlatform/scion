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

// Package entc provides factory functions for creating Ent clients with
// SQLite or PostgreSQL backends.
package entc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	atlasmigrate "ariga.io/atlas/sql/migrate"
	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	entschema "entgo.io/ent/dialect/sql/schema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/migrate"
)

// PoolConfig holds connection pool settings applied to the underlying
// *sql.DB after it is opened. A zero value leaves the corresponding pool
// setting at the database/sql default (i.e. the field is only applied when
// it is greater than zero).
//
// NOTE: for SQLite, MaxOpenConns must be 1 to serialize writes and avoid
// "database is locked" errors; callers are responsible for supplying that.
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	// ConnMaxIdleTime bounds how long a connection may sit idle in the pool
	// before being closed. Set it shorter than the server/proxy idle timeout
	// (CloudSQL drops idle connections after ~10m) so the pool recycles a
	// connection before the remote silently closes it; otherwise the first
	// request after an idle period stalls waiting for a dead connection to time
	// out. A zero value leaves the database/sql default (no idle limit).
	ConnMaxIdleTime time.Duration
}

// apply sets the pool parameters on db, skipping any unset (non-positive) field.
func (p PoolConfig) apply(db *sql.DB) {
	if p.MaxOpenConns > 0 {
		db.SetMaxOpenConns(p.MaxOpenConns)
	}
	if p.MaxIdleConns > 0 {
		db.SetMaxIdleConns(p.MaxIdleConns)
	}
	if p.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(p.ConnMaxLifetime)
	}
	if p.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(p.ConnMaxIdleTime)
	}
}

// withUTCTimezone returns dsn with the modernc.org/sqlite "_timezone" DSN
// option forced to "UTC", preserving any other query options already
// present. modernc parses every bound and scanned time.Time through the
// connection's configured location (sqlite.go, applyQueryParams), so this
// makes every SQLite time.Time bind and read-back canonical UTC regardless
// of the process's time.Local — including legacy rows written with a
// non-UTC zone suffix (tz-refactor design §2.1.2).
//
// dsn may be a bare path, an in-memory name (":memory:"), a "file:" URI with
// or without an existing query, or a DSN that already sets "_timezone": the
// option is force-replaced because modernc honours only the first value for
// a repeated key, so a naive append would leave the operator's value in
// effect.
//
// If the caller's query string fails to parse, dsn is returned unchanged
// rather than rewritten with every other option dropped: modernc's own
// sql.Open/applyQueryParams calls url.ParseQuery on the same string and will
// surface the same error at open time, which is the caller's error to see,
// not something this rewrite should mask by silently opening a different
// database (e.g. dropping "mode=memory" and landing on disk instead).
func withUTCTimezone(dsn string) string {
	base, rawQuery, hasQuery := strings.Cut(dsn, "?")
	if !hasQuery {
		return dsn + "?_timezone=UTC"
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return dsn
	}
	values.Set("_timezone", "UTC")
	return base + "?" + values.Encode()
}

// UTCTimeHook is an Ent mutation hook that converts every time.Time field
// value set in a mutation (explicit or defaulted — ent fills in defaults
// before hooks run) to UTC before it is persisted. On SQLite this makes a
// value canonical even with no "_timezone" DSN option, because modernc only
// adjusts a bound value's Location when one is configured and otherwise
// formats it as-is — so a value this hook has already converted still comes
// out as canonical "... +0000 UTC" text. On Postgres (field.Time maps to
// timestamptz, which is already instant-correct regardless of Location) the
// hook's only effect is that a create/update no longer echoes a non-UTC
// Location back to the caller. See tz-refactor design §2.1.2.
//
// Registered by OpenSQLite, OpenSQLiteReadOnly and openPostgres. Exported so
// a caller that builds an *ent.Client around some other driver — for example
// a test harness that must keep working under the "no_sqlite" build tag,
// where modernc (and so OpenSQLite's "_timezone" option) is unavailable —
// can still register it directly with client.Use(entc.UTCTimeHook).
//
// What it does not cover, because a mutation hook never sees these:
//   - predicate arguments, e.g. a bare time.Now() passed to a generated
//     XxxLT/XxxGTE predicate. On SQLite this binds as local-zone text under
//     a non-UTC time.Local (a numeric-abbreviation zone such as Kathmandu's
//     "+0545 +0545" compares wrong, and may not even Scan back); callers on
//     modernc should also set the DSN "_timezone=UTC" option (OpenSQLite
//     does this) or convert the predicate argument themselves. On Postgres,
//     timestamptz comparisons are correct regardless;
//   - values set via OnConflict(...).Update(func(u *XUpsert){...}), which
//     bypasses mutation hooks entirely — same SQLite/Postgres split as above;
//   - raw SQL;
//   - time.Time values embedded inside a JSON field (e.g.
//     PolicyConditions.ValidFrom/ValidUntil, ExposedPort.ExposedAt) — out of
//     reach of a field-level hook; each writer converts them instead, and
//     TestJSONEmbeddedTimesAreAllowlisted fails on any new embedded
//     time.Time that is not on its allowlist of normalised paths.
func UTCTimeHook(next ent.Mutator) ent.Mutator {
	return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
		for _, name := range m.Fields() {
			v, ok := m.Field(name)
			if !ok {
				continue
			}
			t, ok := v.(time.Time)
			if !ok {
				continue
			}
			// Always convert, even when Location() is already time.UTC: a
			// time.Time can carry a monotonic clock reading (e.g. a bare
			// time.Now() under a process pinned to UTC by util.PinProcessUTC)
			// whose String() form appends " m=...". Only .UTC()/.In() strip
			// it, and doing so unconditionally is cheap and idempotent.
			if err := m.SetField(name, t.UTC()); err != nil {
				return nil, fmt.Errorf("UTCTimeHook: setting field %q to UTC: %w", name, err)
			}
		}
		return next.Mutate(ctx, m)
	})
}

// OpenSQLite creates an Ent client backed by SQLite.
// The dsn should be a SQLite connection string (e.g. "file:ent?mode=memory&cache=shared").
// Foreign keys and WAL journal mode are enabled automatically.
// This uses the modernc.org/sqlite pure-Go driver which registers as "sqlite".
func OpenSQLite(dsn string, pool PoolConfig, opts ...ent.Option) (*ent.Client, error) {
	db, err := sql.Open("sqlite", withUTCTimezone(dsn))
	if err != nil {
		return nil, fmt.Errorf("opening sqlite connection: %w", err)
	}
	// Enable foreign keys and WAL mode, matching existing store pattern.
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enabling WAL mode: %w", err)
	}
	pool.apply(db)
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := ent.NewClient(append(opts, ent.Driver(drv))...)
	client.Use(UTCTimeHook)
	return client, nil
}

// OpenSQLiteReadOnly creates an Ent client backed by a read-only SQLite
// database. It is used by the migration tool to read from a source SQLite file
// without mutating it: the connection is opened with `PRAGMA query_only = ON`
// so any accidental write fails loudly, and—unlike OpenSQLite—it does NOT
// switch the journal to WAL mode (doing so would write to the database header
// and fail on a query-only connection).
//
// MaxOpenConns is forced to 1 because the query_only and foreign_keys pragmas
// are connection-scoped; with a larger pool, unprimed connections would not
// inherit them.
func OpenSQLiteReadOnly(dsn string, opts ...ent.Option) (*ent.Client, error) {
	db, err := sql.Open("sqlite", withUTCTimezone(dsn))
	if err != nil {
		return nil, fmt.Errorf("opening sqlite connection: %w", err)
	}
	// Pin to a single connection so the pragmas below apply to every query.
	db.SetMaxOpenConns(1)
	// Foreign keys on for read consistency; query_only to guarantee the source
	// is never modified during migration.
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
	}
	if _, err := db.Exec("PRAGMA query_only = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enabling query_only mode: %w", err)
	}
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := ent.NewClient(append(opts, ent.Driver(drv))...)
	client.Use(UTCTimeHook)
	return client, nil
}

// OpenPostgres creates an Ent client backed by PostgreSQL.
// The dsn should be a PostgreSQL connection string
// (e.g. "host=localhost port=5432 user=scion dbname=scion sslmode=disable").
func OpenPostgres(dsn string, pool PoolConfig, opts ...ent.Option) (*ent.Client, error) {
	return openPostgres(dsn, pool, false, opts...)
}

// OpenPostgresReadOnly creates an Ent client backed by PostgreSQL with the
// session-level default_transaction_read_only GUC set to "on" for every
// connection in the pool (ptone/scion#2152 round-3 review finding 10): every
// transaction starts read-only, so a write attempted by a tool that should
// never perform one (e.g. `hub secret migrate-names --dry-run`) fails
// loudly at the database itself instead of relying solely on the caller
// never issuing one. This is defense in depth on top of that caller
// discipline — an application-level read-only call (e.g. PlanRefRepair)
// choosing to write due to a bug elsewhere still hits this and fails,
// rather than silently succeeding.
//
// Note (ptone/scion#2152 round-4 review FYI): default_transaction_read_only
// is sent as a connection startup parameter, which PgBouncer in transaction
// or statement pooling mode can reject unless explicitly listed in
// ignore_startup_parameters. Session pooling mode is unaffected. If a
// deployment fronts Postgres with PgBouncer in transaction-pooling mode,
// confirm that setting is allow-listed before relying on this for --dry-run.
func OpenPostgresReadOnly(dsn string, pool PoolConfig, opts ...ent.Option) (*ent.Client, error) {
	return openPostgres(dsn, pool, true, opts...)
}

// buildPostgresConnConfig parses dsn and applies the keepalive and (when
// readOnly) default_transaction_read_only RuntimeParams, without opening any
// connection. Factored out of openPostgres so the resulting config is
// directly assertable in tests (ptone/scion#2152 round-4 review Consider 4)
// — in particular, that OpenPostgresReadOnly actually sets
// default_transaction_read_only=on, without needing a real Postgres server.
func buildPostgresConnConfig(dsn string, readOnly bool) (*pgx.ConnConfig, error) {
	// Parse the DSN with pgx (accepts both keyword/value DSNs "host=... port=..."
	// and URL-style "postgres://..." connection strings) so we can attach TCP
	// keepalive settings to the connection before handing it to database/sql via
	// stdlib.OpenDB. Keepalives let the OS detect a connection silently dropped by
	// a peer (e.g. CloudSQL recycling idle backends or a NAT timeout) instead of
	// the first query after idle hanging on a dead socket.
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing postgres dsn: %w", err)
	}
	applyKeepalives(connConfig.RuntimeParams)
	if readOnly {
		connConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}
	if connConfig.ConnectTimeout == 0 {
		connConfig.ConnectTimeout = connectTimeout
	}
	return connConfig, nil
}

// openPostgres is the shared implementation behind OpenPostgres and
// OpenPostgresReadOnly.
func openPostgres(dsn string, pool PoolConfig, readOnly bool, opts ...ent.Option) (*ent.Client, error) {
	connConfig, err := buildPostgresConnConfig(dsn, readOnly)
	if err != nil {
		return nil, err
	}

	// Register google/uuid.UUID with pgx's type system so that UUID values are
	// encoded with OID 2950 (uuid) instead of the fragile DriverValuer fallback
	// that sends OID 25 (text). Without this, raw queries comparing uuid columns
	// against Go uuid.UUID parameters fail with SQLSTATE 42883 ("operator does
	// not exist: uuid = text"). See https://github.com/ptone/scion/issues/1634.
	db := stdlib.OpenDB(*connConfig, stdlib.OptionAfterConnect(
		func(ctx context.Context, conn *pgx.Conn) error {
			conn.TypeMap().RegisterDefaultPgType(uuid.UUID{}, "uuid")
			conn.TypeMap().RegisterDefaultPgType([]uuid.UUID{}, "_uuid")
			return nil
		},
	))
	pool.apply(db)
	drv := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(append(opts, ent.Driver(drv))...)
	client.Use(UTCTimeHook)
	return client, nil
}

const connectTimeout = 10 * time.Second

// applyKeepalives sets server-side TCP keepalive GUCs as pgx RuntimeParams so the
// kernel probes idle connections and tears down dead ones promptly. Values mirror
// the pgx event pool (events_postgres.go): probe after 60s idle, every 15s, give
// up after 4 missed probes (~2 min to detect a dead peer). Existing keys are not
// overwritten so an explicit DSN setting wins.
func applyKeepalives(params map[string]string) {
	defaults := map[string]string{
		"tcp_keepalives_idle":     "60",
		"tcp_keepalives_interval": "15",
		"tcp_keepalives_count":    "4",
	}
	for k, v := range defaults {
		if _, ok := params[k]; !ok {
			params[k] = v
		}
	}
}

// AutoMigrate runs automatic schema migration on the given client.
// It is idempotent: running it against a database that already has some or
// all Ent-managed tables succeeds without dropping existing data. On
// Postgres, any DDL statement that fails with SQLSTATE 42P07 ("relation
// already exists") is silently skipped so that new tables are created
// alongside pre-existing ones.
func AutoMigrate(ctx context.Context, client *ent.Client) error {
	migrateOpts := []entschema.MigrateOption{
		migrate.WithDropColumn(false),
		migrate.WithDropIndex(false),
	}
	if client.Driver().Dialect() == dialect.Postgres {
		migrateOpts = append(migrateOpts,
			entschema.WithApplyHook(normalizeBrokerLabels),
			entschema.WithApplyHook(skipExistingRelations),
		)
	}
	return client.Schema.Create(ctx, migrateOpts...)
}

// skipExistingRelations is an Ent schema ApplyHook that makes DDL
// idempotent on Postgres by skipping any statement that fails with
// SQLSTATE 42P07 ("duplicate_table" / "relation already exists").
//
// Each statement is wrapped in a SAVEPOINT so that a 42P07 failure can
// be rolled back without aborting the surrounding transaction — in
// Postgres, any statement error marks the transaction as aborted and
// all subsequent commands are rejected until a ROLLBACK (TO SAVEPOINT).
func skipExistingRelations(next entschema.Applier) entschema.Applier {
	return entschema.ApplyFunc(func(ctx context.Context, conn dialect.ExecQuerier, plan *atlasmigrate.Plan) error {
		for i, c := range plan.Changes {
			sp := fmt.Sprintf("migrate_change_%d", i)
			if err := conn.Exec(ctx, fmt.Sprintf("SAVEPOINT %s", sp), []any{}, nil); err != nil {
				return fmt.Errorf("creating savepoint: %w", err)
			}
			if err := conn.Exec(ctx, c.Cmd, c.Args, nil); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "42P07" {
					slog.Debug("AutoMigrate: skipping existing relation", "stmt", c.Cmd)
					if rbErr := conn.Exec(ctx, fmt.Sprintf("ROLLBACK TO SAVEPOINT %s", sp), []any{}, nil); rbErr != nil {
						return fmt.Errorf("rolling back savepoint after 42P07: %w", rbErr)
					}
					continue
				}
				if c.Comment != "" {
					err = fmt.Errorf("%s: %w", c.Comment, err)
				}
				return err
			}
			if err := conn.Exec(ctx, fmt.Sprintf("RELEASE SAVEPOINT %s", sp), []any{}, nil); err != nil {
				return fmt.Errorf("releasing savepoint: %w", err)
			}
		}
		return nil
	})
}

// normalizeBrokerLabels is an Ent schema ApplyHook that runs before the
// migration plan to normalize empty-string labels/annotations to NULL on
// runtime_brokers and inject USING clauses for varchar→jsonb casts.
//
// PostgreSQL cannot automatically cast varchar to jsonb (SQLSTATE 42804),
// so any ALTER COLUMN … TYPE jsonb must include a USING col::jsonb clause.
// The ent framework does not emit USING, so we patch the plan here.
//
// The empty-string→NULL normalization is still needed because even with
// USING, a bare empty string is not valid JSON and would fail the cast.
// The WHERE clause uses ::text so the comparison remains valid on
// subsequent runs when the column is already jsonb.
func normalizeBrokerLabels(next entschema.Applier) entschema.Applier {
	return entschema.ApplyFunc(func(ctx context.Context, conn dialect.ExecQuerier, plan *atlasmigrate.Plan) error {
		for i, stmt := range []string{
			`UPDATE runtime_brokers SET labels = NULL WHERE labels::text = ''`,
			`UPDATE runtime_brokers SET annotations = NULL WHERE annotations::text = ''`,
		} {
			// Wrap in a SAVEPOINT so a failure (e.g. 42P01 on a fresh DB,
			// where runtime_brokers doesn't exist yet) doesn't abort the
			// surrounding migration transaction; see skipExistingRelations.
			sp := fmt.Sprintf("normalize_broker_labels_%d", i)
			if err := conn.Exec(ctx, fmt.Sprintf("SAVEPOINT %s", sp), []any{}, nil); err != nil {
				return fmt.Errorf("creating savepoint: %w", err)
			}
			if err := conn.Exec(ctx, stmt, []any{}, nil); err != nil {
				// Table may not exist yet on a fresh database — that is
				// fine, but the transaction is now aborted and must be
				// rolled back to the savepoint before continuing.
				slog.Debug("normalizeBrokerLabels: skipping", "stmt", stmt, "err", err)
				if rbErr := conn.Exec(ctx, fmt.Sprintf("ROLLBACK TO SAVEPOINT %s", sp), []any{}, nil); rbErr != nil {
					return fmt.Errorf("rolling back savepoint after normalizeBrokerLabels statement failure: %w", rbErr)
				}
				continue
			}
			if err := conn.Exec(ctx, fmt.Sprintf("RELEASE SAVEPOINT %s", sp), []any{}, nil); err != nil {
				return fmt.Errorf("releasing savepoint: %w", err)
			}
		}
		// Patch the plan to add USING clauses for varchar→jsonb column casts
		// on runtime_brokers. Without this, Postgres rejects the ALTER with
		// SQLSTATE 42804 ("column cannot be cast automatically to type jsonb").
		for _, c := range plan.Changes {
			for _, col := range []string{"labels", "annotations"} {
				// Match "ALTER COLUMN "<col>" TYPE jsonb" that is NOT already
				// followed by " USING". We check for the bare target (without
				// USING) and the extended target (with USING) separately, so
				// that adding USING for one column doesn't block the other
				// when both appear in the same statement.
				bare := fmt.Sprintf("ALTER COLUMN \"%s\" TYPE jsonb", col)
				withUsing := fmt.Sprintf("ALTER COLUMN \"%s\" TYPE jsonb USING", col)
				if strings.Contains(c.Cmd, bare) && !strings.Contains(c.Cmd, withUsing) {
					replacement := fmt.Sprintf("ALTER COLUMN \"%s\" TYPE jsonb USING \"%s\"::jsonb", col, col)
					c.Cmd = strings.Replace(c.Cmd, bare, replacement, 1)
					slog.Debug("normalizeBrokerLabels: added USING clause", "col", col, "cmd", c.Cmd)
				}
			}
		}
		return next.Apply(ctx, conn, plan)
	})
}
