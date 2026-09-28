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
	"fmt"
	"strings"
	"testing"

	atlasmigrate "ariga.io/atlas/sql/migrate"
	"entgo.io/ent/dialect"
	entschema "entgo.io/ent/dialect/sql/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubExecQuerier is a no-op ExecQuerier used in tests — the UPDATE
// statements inside normalizeBrokerLabels are expected to fail when
// there is no real runtime_brokers table, but the hook swallows those
// errors with slog.Debug.
type stubExecQuerier struct{}

func (stubExecQuerier) Exec(_ context.Context, _ string, _, _ any) error {
	return nil // swallow; no real DB
}

func (stubExecQuerier) Query(_ context.Context, _ string, _, _ any) error {
	return nil
}

// abortableExecQuerier models the one piece of real Postgres transaction
// behavior that stubExecQuerier (always-nil) cannot: once any statement
// fails, the surrounding transaction is "aborted" and every subsequent
// statement fails too — except a ROLLBACK (TO SAVEPOINT), which clears the
// abort state established after that savepoint. A plain stub that never
// errors can never expose the bug this hook had (a discarded error left
// the transaction aborted for whatever ran next), because it never
// models Postgres refusing to run anything after an unhandled failure.
//
// failOn matches statements (by substring) that should fail as if the
// target table doesn't exist yet, e.g. a fresh database.
//
// openSavepoints tracks live savepoint names, so ROLLBACK TO / RELEASE on a
// name that was never SAVEPOINTed fails like real Postgres (SQLSTATE
// 3B001) instead of silently clearing the abort state. Without this, a
// broken fix that rolled back to the wrong (or a nonexistent) savepoint
// name would pass this mock.
type abortableExecQuerier struct {
	failOn         []string
	aborted        bool
	openSavepoints map[string]bool
}

func (q *abortableExecQuerier) Exec(_ context.Context, stmt string, _, _ any) error {
	if q.openSavepoints == nil {
		q.openSavepoints = map[string]bool{}
	}

	switch {
	case strings.HasPrefix(stmt, "SAVEPOINT "):
		if q.aborted {
			return fmt.Errorf("current transaction is aborted, commands ignored until end of transaction block (SQLSTATE 25P02)")
		}
		q.openSavepoints[strings.TrimPrefix(stmt, "SAVEPOINT ")] = true
		return nil
	case strings.HasPrefix(stmt, "ROLLBACK TO SAVEPOINT "):
		name := strings.TrimPrefix(stmt, "ROLLBACK TO SAVEPOINT ")
		if !q.openSavepoints[name] {
			return fmt.Errorf("savepoint %q does not exist (SQLSTATE 3B001)", name)
		}
		// Rolling back to a savepoint established before the failing
		// statement clears the abort state, exactly like real Postgres.
		// The savepoint itself stays valid (Postgres keeps it open until
		// RELEASE or the enclosing transaction ends).
		q.aborted = false
		return nil
	case strings.HasPrefix(stmt, "RELEASE SAVEPOINT "):
		name := strings.TrimPrefix(stmt, "RELEASE SAVEPOINT ")
		if !q.openSavepoints[name] {
			return fmt.Errorf("savepoint %q does not exist (SQLSTATE 3B001)", name)
		}
		delete(q.openSavepoints, name)
		return nil
	case q.aborted:
		// Real Postgres: "current transaction is aborted, commands
		// ignored until end of transaction block" (SQLSTATE 25P02).
		return fmt.Errorf("current transaction is aborted, commands ignored until end of transaction block (SQLSTATE 25P02)")
	}
	for _, sub := range q.failOn {
		if strings.Contains(stmt, sub) {
			q.aborted = true
			return fmt.Errorf(`relation "runtime_brokers" does not exist (SQLSTATE 42P01)`)
		}
	}
	return nil
}

func (q *abortableExecQuerier) Query(_ context.Context, _ string, _, _ any) error {
	return nil
}

// TestNormalizeBrokerLabels_USINGClause verifies that the
// normalizeBrokerLabels hook injects "USING <col>::jsonb" into ALTER
// COLUMN statements that cast varchar to jsonb without an existing
// USING clause. This prevents SQLSTATE 42804 on fresh deployments.
func TestNormalizeBrokerLabels_USINGClause(t *testing.T) {
	tests := []struct {
		name   string
		cmd    string
		expect string
	}{
		{
			name:   "labels gets USING",
			cmd:    `ALTER TABLE "runtime_brokers" ALTER COLUMN "labels" TYPE jsonb`,
			expect: `ALTER TABLE "runtime_brokers" ALTER COLUMN "labels" TYPE jsonb USING "labels"::jsonb`,
		},
		{
			name:   "annotations gets USING",
			cmd:    `ALTER TABLE "runtime_brokers" ALTER COLUMN "annotations" TYPE jsonb`,
			expect: `ALTER TABLE "runtime_brokers" ALTER COLUMN "annotations" TYPE jsonb USING "annotations"::jsonb`,
		},
		{
			name:   "both columns in single statement",
			cmd:    `ALTER TABLE "runtime_brokers" ALTER COLUMN "labels" TYPE jsonb, ALTER COLUMN "annotations" TYPE jsonb`,
			expect: `ALTER TABLE "runtime_brokers" ALTER COLUMN "labels" TYPE jsonb USING "labels"::jsonb, ALTER COLUMN "annotations" TYPE jsonb USING "annotations"::jsonb`,
		},
		{
			name:   "already has USING — no double injection",
			cmd:    `ALTER TABLE "runtime_brokers" ALTER COLUMN "labels" TYPE jsonb USING "labels"::jsonb`,
			expect: `ALTER TABLE "runtime_brokers" ALTER COLUMN "labels" TYPE jsonb USING "labels"::jsonb`,
		},
		{
			name:   "unrelated statement unchanged",
			cmd:    `ALTER TABLE "runtime_brokers" ADD COLUMN "foo" text`,
			expect: `ALTER TABLE "runtime_brokers" ADD COLUMN "foo" text`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := &atlasmigrate.Plan{
				Changes: []*atlasmigrate.Change{{Cmd: tt.cmd}},
			}

			// terminal is a no-op applier — we only care about what the
			// hook does to the plan before calling next.
			var captured *atlasmigrate.Plan
			terminal := entschema.ApplyFunc(func(_ context.Context, _ dialect.ExecQuerier, p *atlasmigrate.Plan) error {
				captured = p
				return nil
			})

			hook := normalizeBrokerLabels(terminal)
			err := hook.Apply(context.Background(), stubExecQuerier{}, plan)
			require.NoError(t, err)
			require.NotNil(t, captured)
			assert.Equal(t, tt.expect, captured.Changes[0].Cmd)
		})
	}
}

// TestNormalizeBrokerLabels_FreshDatabase_DoesNotPoisonTransaction is a
// regression test for the fresh-database migration bug: normalizeBrokerLabels
// ran two UPDATE runtime_brokers statements that fail with 42P01 ("relation
// does not exist") on a database where that table hasn't been created yet.
// The Go error was logged and discarded, but Postgres had already aborted
// the surrounding transaction — so the next statement anywhere in the
// migration (in production, skipExistingRelations' own SAVEPOINT) failed
// with an unrelated-looking SQLSTATE 25P02, and the real cause never
// surfaced.
//
// stubExecQuerier (always nil) cannot model this, because it never fails at
// all. abortableExecQuerier models the one behavior that matters: once a
// statement fails, every later statement fails too, except a
// ROLLBACK TO SAVEPOINT.
//
// This test FAILS against the pre-fix hook (the old code never issues that
// rollback) and passes once each UPDATE is wrapped in its own
// SAVEPOINT / ROLLBACK TO SAVEPOINT / RELEASE SAVEPOINT, matching
// skipExistingRelations' existing pattern in this file.
func TestNormalizeBrokerLabels_FreshDatabase_DoesNotPoisonTransaction(t *testing.T) {
	conn := &abortableExecQuerier{failOn: []string{"runtime_brokers"}}

	plan := &atlasmigrate.Plan{}

	// terminal models what actually runs next in production
	// (skipExistingRelations): it issues its own SAVEPOINT before the real
	// migration statement. If normalizeBrokerLabels left the transaction
	// aborted, this call fails exactly like the "creating savepoint" error
	// seen in the real crash.
	nextCalled := false
	terminal := entschema.ApplyFunc(func(ctx context.Context, conn dialect.ExecQuerier, _ *atlasmigrate.Plan) error {
		nextCalled = true
		return conn.Exec(ctx, "SAVEPOINT migrate_change_0", []any{}, nil)
	})

	hook := normalizeBrokerLabels(terminal)
	err := hook.Apply(context.Background(), conn, plan)

	require.NoError(t, err, "normalizeBrokerLabels must not leave the transaction aborted for the next migration step on a fresh database")
	assert.True(t, nextCalled, "next.Apply must still run even when the UPDATE statements fail")
}
