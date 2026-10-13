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

// This file implements the Postgres schema snapshot that NewClient and
// NewSchemaURL use to provision each test schema.
//
// Why: entc.AutoMigrate on an empty schema costs about 0.4 s per call (Atlas
// inspects the catalog, diffs and plans 84 tables and about 260 indexes, and
// skipExistingRelations wraps every planned statement in its own SAVEPOINT and
// RELEASE). The store suites call NewClient about 1,300 times per run, so most
// of their Postgres time went to provisioning, not to the tests.
//
// How: the first time a package needs a migrated schema, captureSchemaSnapshot
// runs the real entc.AutoMigrate once, into a throwaway schema, through a
// driver wrapper that records every statement AutoMigrate executes. The
// statements that took effect (savepoint control, statements that failed and
// statements undone by a rollback are dropped) are joined into one script.
// Every later schema is provisioned by sending that script as a single
// multi-statement Exec on a connection whose search_path is the new schema:
// one round trip, no catalog inspection, no savepoints. The script is
// regenerated from AutoMigrate in every test process, so it cannot go stale
// against pkg/ent/migrate, and TestSchemaSnapshot_MatchesAutoMigrate compares
// the catalog of a snapshot-provisioned schema with an AutoMigrated one.
//
// Atlas plans for Ent omit the schema qualifier (ent's noQualifierOpt), so the
// recorded statements are unqualified and resolve through search_path, exactly
// as they did when AutoMigrate ran them.
//
// Set SCION_TEST_POSTGRES_AUTOMIGRATE=1 to skip the snapshot and run
// entc.AutoMigrate for every schema, as before (for debugging the harness or a
// migration).
package enttest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
)

// forceAutoMigrateEnv names the environment variable that, when set to a
// non-empty value other than "0" or "false", makes every schema run
// entc.AutoMigrate instead of replaying the snapshot.
const forceAutoMigrateEnv = "SCION_TEST_POSTGRES_AUTOMIGRATE"

var (
	snapshotOnce   sync.Once
	snapshotScript string
	snapshotErr    error
)

// forceAutoMigrate reports whether forceAutoMigrateEnv asks for the old
// AutoMigrate-per-schema path.
func forceAutoMigrate() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(forceAutoMigrateEnv))) {
	case "", "0", "false":
		return false
	}
	return true
}

// migrateSchema brings the empty schema that client's search_path points at
// to the full Ent schema: by replaying the snapshot, or by running
// entc.AutoMigrate when forceAutoMigrateEnv is set.
func migrateSchema(ctx context.Context, client *ent.Client) error {
	if forceAutoMigrate() {
		return entc.AutoMigrate(ctx, client)
	}
	script, err := schemaSnapshot()
	if err != nil {
		return fmt.Errorf("capturing the schema snapshot (set %s=1 to bypass it): %w", forceAutoMigrateEnv, err)
	}
	return applySchemaSnapshot(ctx, client, script)
}

// applySchemaSnapshot runs script on client in one Exec. With no bind
// arguments pgx sends it over the simple query protocol, so Postgres runs the
// statements as one implicit transaction: the schema ends up either fully
// provisioned or untouched.
func applySchemaSnapshot(ctx context.Context, client *ent.Client, script string) error {
	if err := client.Driver().Exec(ctx, script, []any{}, nil); err != nil {
		return fmt.Errorf("replaying the schema snapshot: %w", err)
	}
	return nil
}

// schemaSnapshot returns the package's snapshot script, capturing it on the
// first call.
func schemaSnapshot() (string, error) {
	snapshotOnce.Do(func() {
		snapshotScript, snapshotErr = captureSchemaSnapshot(context.Background())
		if snapshotErr == nil {
			log.Printf("enttest: captured the AutoMigrate schema snapshot (%d bytes); set %s=1 to migrate each schema with AutoMigrate instead",
				len(snapshotScript), forceAutoMigrateEnv)
		}
	})
	return snapshotScript, snapshotErr
}

// captureSchemaSnapshot runs entc.AutoMigrate once against a fresh, empty
// throwaway schema while recording the statements it executes, then drops the
// schema and returns the recorded statements as one script.
func captureSchemaSnapshot(ctx context.Context) (string, error) {
	if !active {
		return "", errors.New("the Postgres backend is not active")
	}
	schema := "t_snapshot_" + hexID()
	if _, err := adminDB.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		return "", fmt.Errorf("creating snapshot schema %s: %w", schema, err)
	}
	defer func() {
		if _, err := adminDB.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			log.Printf("enttest: warning: dropping snapshot schema %s: %v", schema, err)
		}
	}()

	clientURL, err := withSearchPath(postgresURL, pkgDBName, schema)
	if err != nil {
		return "", fmt.Errorf("building snapshot schema URL: %w", err)
	}
	db, err := sql.Open("pgx", clientURL)
	if err != nil {
		return "", fmt.Errorf("opening snapshot schema connection: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)

	rec := &statementRecorder{}
	client := ent.NewClient(ent.Driver(&recordingDriver{Driver: entsql.OpenDB(dialect.Postgres, db), rec: rec}))
	if err := entc.AutoMigrate(ctx, client); err != nil {
		return "", fmt.Errorf("running AutoMigrate into snapshot schema %s: %w", schema, err)
	}
	return rec.script()
}

// recordedStatement is one statement that took effect during the recorded
// AutoMigrate run.
type recordedStatement struct {
	query string
	args  []any
}

// savepointMark remembers how many statements were recorded when a savepoint
// was set, so ROLLBACK TO SAVEPOINT can discard the ones after it.
type savepointMark struct {
	name string
	n    int
}

// statementRecorder collects the statements that took effect, in order. It
// models SAVEPOINT / ROLLBACK TO SAVEPOINT / RELEASE SAVEPOINT and transaction
// rollback, so only effective statements end up in the script. It is not safe
// for concurrent transactions; AutoMigrate runs its transactions one at a time.
type statementRecorder struct {
	mu         sync.Mutex
	stmts      []recordedStatement
	savepoints []savepointMark
	err        error
}

// exec records the outcome of one Exec call.
func (r *statementRecorder) exec(query string, args any, execErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if execErr != nil {
		// A failed statement had no effect. AutoMigrate either rolls back to
		// a savepoint (recorded separately) or fails as a whole.
		return
	}
	q := strings.TrimSpace(query)
	fields := strings.Fields(strings.ToUpper(q))
	switch {
	case len(fields) == 2 && fields[0] == "SAVEPOINT":
		r.savepoints = append(r.savepoints, savepointMark{name: fields[1], n: len(r.stmts)})
		return
	case len(fields) == 3 && fields[0] == "RELEASE" && fields[1] == "SAVEPOINT":
		if i := r.findSavepoint(fields[2]); i >= 0 {
			r.savepoints = r.savepoints[:i]
		}
		return
	case len(fields) == 4 && fields[0] == "ROLLBACK" && fields[1] == "TO" && fields[2] == "SAVEPOINT":
		i := r.findSavepoint(fields[3])
		if i < 0 {
			r.setErr(fmt.Errorf("ROLLBACK TO unknown savepoint %s", fields[3]))
			return
		}
		r.stmts = r.stmts[:r.savepoints[i].n]
		r.savepoints = r.savepoints[:i+1] // the savepoint survives a rollback to it
		return
	}
	argv, ok := args.([]any)
	if !ok && args != nil {
		r.setErr(fmt.Errorf("statement %q has arguments of type %T, want []any", q, args))
		return
	}
	r.stmts = append(r.stmts, recordedStatement{query: q, args: argv})
}

// begin returns the recorder position at the start of a transaction.
func (r *statementRecorder) begin() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stmts)
}

// rollback discards the statements recorded since the transaction started at
// position start.
func (r *statementRecorder) rollback(start int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if start < len(r.stmts) {
		r.stmts = r.stmts[:start]
	}
	r.savepoints = nil
}

// commit forgets the transaction's savepoints.
func (r *statementRecorder) commit() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.savepoints = nil
}

func (r *statementRecorder) findSavepoint(name string) int {
	for i := len(r.savepoints) - 1; i >= 0; i-- {
		if r.savepoints[i].name == name {
			return i
		}
	}
	return -1
}

func (r *statementRecorder) setErr(err error) {
	if r.err == nil {
		r.err = err
	}
}

// script joins the recorded statements into one multi-statement script. It
// fails if any statement carries bind arguments: those cannot be sent in a
// simple-protocol batch.
func (r *statementRecorder) script() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return "", r.err
	}
	if len(r.stmts) == 0 {
		return "", errors.New("AutoMigrate executed no statements")
	}
	var b strings.Builder
	for _, s := range r.stmts {
		if len(s.args) > 0 {
			return "", fmt.Errorf("statement %q has %d bind arguments and cannot be replayed as a script", s.query, len(s.args))
		}
		b.WriteString(strings.TrimRight(s.query, "; \t\n"))
		b.WriteString(";\n")
	}
	return b.String(), nil
}

// recordingDriver is a dialect.Driver that records every Exec, including those
// inside transactions, into rec.
type recordingDriver struct {
	dialect.Driver
	rec *statementRecorder
}

func (d *recordingDriver) Exec(ctx context.Context, query string, args, v any) error {
	err := d.Driver.Exec(ctx, query, args, v)
	d.rec.exec(query, args, err)
	return err
}

func (d *recordingDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	tx, err := d.Driver.Tx(ctx)
	if err != nil {
		return nil, err
	}
	return &recordingTx{Tx: tx, rec: d.rec, start: d.rec.begin()}, nil
}

// recordingTx is the transaction counterpart of recordingDriver.
type recordingTx struct {
	dialect.Tx
	rec   *statementRecorder
	start int
}

func (t *recordingTx) Exec(ctx context.Context, query string, args, v any) error {
	err := t.Tx.Exec(ctx, query, args, v)
	t.rec.exec(query, args, err)
	return err
}

func (t *recordingTx) Commit() error {
	err := t.Tx.Commit()
	if err != nil {
		t.rec.rollback(t.start)
		return err
	}
	t.rec.commit()
	return nil
}

func (t *recordingTx) Rollback() error {
	err := t.Tx.Rollback()
	t.rec.rollback(t.start)
	return err
}
