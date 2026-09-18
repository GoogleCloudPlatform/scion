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

package state

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	migrationHelperDSNEnv = "SCION_STATE_MIGRATION_HELPER_DSN"
	migrationHelperStart  = "SCION_STATE_MIGRATION_HELPER_START_UNIX_NANO"
	migrationLockSQL      = `SELECT pg_advisory_xact_lock(hashtext(current_database()), hashtext(current_schema()))`
)

func TestPostgresMigrationHelperProcess(t *testing.T) {
	dsn := os.Getenv(migrationHelperDSNEnv)
	if dsn == "" {
		t.Skip("migration helper only")
	}
	if _, err := io.CopyN(io.Discard, os.Stdin, 1); err != nil {
		t.Fatalf("wait for start signal: %v", err)
	}
	if rawStart := os.Getenv(migrationHelperStart); rawStart != "" {
		var startUnixNano int64
		if _, err := fmt.Sscan(rawStart, &startUnixNano); err != nil {
			t.Fatalf("parse synchronized start: %v", err)
		}
		if wait := time.Until(time.Unix(0, startUnixNano)); wait > 0 {
			time.Sleep(wait)
		}
	}
	store, err := NewPostgres(dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestConcurrentPostgresStateMigrations(t *testing.T) {
	dsn := newMigrationSchemaDSN(t, "same_schema", "state-migrate-goroutines")

	const replicas = 20
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	ready.Add(replicas)
	start := make(chan struct{})
	errs := make(chan error, replicas)
	for range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			store, err := NewPostgres(dsn)
			if err != nil {
				errs <- err
				return
			}
			if err := store.Close(); err != nil {
				errs <- err
			}
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent migration: %v", err)
	}
}

func TestPostgresStateMigrationTwoProductionProcesses(t *testing.T) {
	dsn := newMigrationSchemaDSN(t, "two_process", "state-migrate-process")
	type helper struct {
		cmd    *exec.Cmd
		stdin  io.WriteCloser
		output bytes.Buffer
	}
	helpers := make([]*helper, 2)
	startUnixNano := time.Now().Add(500 * time.Millisecond).UnixNano()
	for index := range helpers {
		h := &helper{}
		h.cmd = exec.Command(os.Args[0], "-test.run=^TestPostgresMigrationHelperProcess$", "-test.v")
		h.cmd.Env = append(os.Environ(),
			migrationHelperDSNEnv+"="+dsn,
			fmt.Sprintf("%s=%d", migrationHelperStart, startUnixNano),
		)
		h.cmd.Stdout = &h.output
		h.cmd.Stderr = &h.output
		stdin, err := h.cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		h.stdin = stdin
		if err := h.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		helpers[index] = h
	}
	for _, h := range helpers {
		if _, err := h.stdin.Write([]byte{'x'}); err != nil {
			t.Fatal(err)
		}
		if err := h.stdin.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for index, h := range helpers {
		if err := h.cmd.Wait(); err != nil {
			t.Errorf("helper %d (pid %d): %v\n%s", index, h.cmd.Process.Pid, err, h.output.String())
		}
	}
}

func TestPostgresStateMigrationLockScopesByDatabaseAndSchema(t *testing.T) {
	dsnA := newMigrationSchemaDSN(t, "scope_a", "state-migrate-scope-a")
	dsnB := newMigrationSchemaDSN(t, "scope_b", "state-migrate-scope-b")
	ctx := context.Background()

	connA, err := pgx.Connect(ctx, dsnA)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Close(ctx)
	tx, err := connA.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, migrationLockSQL); err != nil {
		t.Fatal(err)
	}

	sameDone := make(chan error, 1)
	go func() {
		store, err := NewPostgres(dsnA)
		if store != nil {
			_ = store.Close()
		}
		sameDone <- err
	}()
	select {
	case err := <-sameDone:
		t.Fatalf("same-schema migration bypassed held lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	distinctDone := make(chan error, 1)
	go func() {
		store, err := NewPostgres(dsnB)
		if store != nil {
			_ = store.Close()
		}
		distinctDone <- err
	}()
	select {
	case err := <-distinctDone:
		if err != nil {
			t.Fatalf("distinct-schema migration: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("distinct-schema migration blocked on unrelated schema lock")
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-sameDone:
		if err != nil {
			t.Fatalf("same-schema migration after unlock: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("same-schema migration did not resume after unlock")
	}
}

func TestPostgresStateMigrationFailureAndProcessCancellationReleaseLock(t *testing.T) {
	dsn := newMigrationSchemaDSN(t, "cleanup", "state-migrate-cleanup")
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, `CREATE VIEW a2a_tasks AS SELECT 1 AS incompatible`); err != nil {
		t.Fatal(err)
	}
	if store, err := NewPostgres(dsn); err == nil {
		_ = store.Close()
		t.Fatal("migration unexpectedly succeeded with incompatible existing object")
	}
	if _, err := conn.Exec(ctx, `DROP VIEW a2a_tasks`); err != nil {
		t.Fatal(err)
	}
	store, err := NewPostgres(dsn)
	if err != nil {
		t.Fatalf("migration after failed transaction: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	blocker, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, migrationLockSQL); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPostgresMigrationHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), migrationHelperDSNEnv+"="+dsn)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	time.Sleep(150 * time.Millisecond)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	store, err = NewPostgres(dsn)
	if err != nil {
		t.Fatalf("migration after canceled helper: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresStateMigrationExistingDataIdempotent(t *testing.T) {
	dsn := newMigrationSchemaDSN(t, "idempotent", "state-migrate-idempotent")
	first, err := NewPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.DB().Exec(`INSERT INTO a2a_contexts (context_id, project_id, agent_slug) VALUES ('migration-canary', 'project', 'agent')`); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := NewPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var count int
	if err := second.DB().QueryRow(`SELECT COUNT(*) FROM a2a_contexts WHERE context_id = 'migration-canary'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("canary count=%d, want 1", count)
	}
}

func newMigrationSchemaDSN(t *testing.T, label, applicationName string) string {
	t.Helper()
	baseDSN := os.Getenv("TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	schema := fmt.Sprintf("test_%s_%d", label, time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+identifier+" CASCADE")
	})
	cfg, err := pgx.ParseConfig(baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	dsnURL, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	query := dsnURL.Query()
	query.Set("search_path", schema)
	query.Set("application_name", applicationName)
	dsnURL.RawQuery = query.Encode()
	t.Logf("database=%s schema=%s search_path=%s application_name=%s", cfg.Database, schema, schema, applicationName)
	return dsnURL.String()
}
