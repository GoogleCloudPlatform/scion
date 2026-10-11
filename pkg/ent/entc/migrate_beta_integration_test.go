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

package entc_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/accesspolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/group"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/groupmembership"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/policybinding"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
)

// TestMigrateBeta_SQLiteToPostgres exercises the full Migration β path against a
// real PostgreSQL instance. It seeds an Ent-on-SQLite database, copies it to
// Postgres with MigrateData, then asserts:
//   - every entity's source and destination counts match,
//   - the run is idempotent (a second run inserts nothing),
//   - FK relationships and a M2M edge survive the copy,
//   - representative field values round-trip intact.
//
// The destination DSN comes from SCION_TEST_POSTGRES_URL (the variable the CI
// Postgres job and `make test-launch-store-postgres` set), falling back to
// SCION_PG_TEST_DSN for older local setups; the test skips when both are
// unset. The migration runs in a fresh, uniquely named schema that is dropped
// when the test ends, so it never touches other tables in the same database.
// Run with:
//
//	SCION_TEST_POSTGRES_URL='postgres://user:pass@host:5432/db?sslmode=require' \
//	  go test -tags integration -run TestMigrateBeta ./pkg/ent/entc/...
func TestMigrateBeta_SQLiteToPostgres(t *testing.T) {
	baseDSN := os.Getenv("SCION_TEST_POSTGRES_URL")
	if baseDSN == "" {
		baseDSN = os.Getenv("SCION_PG_TEST_DSN")
	}
	if baseDSN == "" {
		t.Skip("SCION_TEST_POSTGRES_URL (or SCION_PG_TEST_DSN) not set; skipping Postgres integration test")
	}
	ctx := context.Background()

	// Start from an empty, isolated destination schema so row counts are
	// deterministic and nothing else in the database is affected.
	dstDSN := newIsolatedPostgresSchema(t, baseDSN)

	// --- Seed an Ent-on-SQLite source. ---
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "hub.db")
	seed := seedSQLiteSource(t, ctx, srcPath)

	// --- Open source read-only + destination, ensure schema, migrate. ---
	src, err := entc.OpenSQLiteReadOnly("file:" + srcPath + "?cache=shared")
	if err != nil {
		t.Fatalf("open source read-only: %v", err)
	}
	defer src.Close()

	dst, err := entc.OpenPostgres(dstDSN, entc.PoolConfig{MaxOpenConns: 10, MaxIdleConns: 5})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer dst.Close()

	if err := entc.AutoMigrate(ctx, dst); err != nil {
		t.Fatalf("auto-migrate destination: %v", err)
	}

	report, err := entc.MigrateData(ctx, src, dst, entc.MigrateOptions{
		Logf: func(format string, args ...any) { t.Logf(format, args...) },
	})
	if err != nil {
		t.Fatalf("first migration: %v", err)
	}

	// Every entity must have matching counts (less skipped duplicates); the
	// seeded entities must have actually inserted rows with nothing skipped on
	// the first pass. Session metrics hold one sub-microsecond duplicate.
	seenInserts := 0
	for _, e := range report.Entities {
		if e.Source-e.Duplicates != e.Dest {
			t.Errorf("%s: source=%d duplicates=%d dest=%d (mismatch)", e.Entity, e.Source, e.Duplicates, e.Dest)
		}
		wantDups := 0
		if e.Entity == "AgentSessionMetrics" {
			wantDups = 1
		}
		if e.Duplicates != wantDups {
			t.Errorf("%s: duplicates=%d, want %d", e.Entity, e.Duplicates, wantDups)
		}
		if e.Skipped != 0 {
			t.Errorf("%s: expected 0 skipped on first run, got %d", e.Entity, e.Skipped)
		}
		seenInserts += e.Inserted
	}
	if seenInserts == 0 {
		t.Fatal("first migration inserted nothing; seed did not take")
	}
	if report.ChildGroupEdgs != 1 {
		t.Errorf("expected 1 child-group edge, got %d", report.ChildGroupEdgs)
	}

	// --- Idempotency: a second run inserts nothing and skips everything. ---
	report2, err := entc.MigrateData(ctx, src, dst, entc.MigrateOptions{})
	if err != nil {
		t.Fatalf("second (idempotent) migration: %v", err)
	}
	for _, e := range report2.Entities {
		if e.Inserted != 0 {
			t.Errorf("%s: idempotent run inserted %d rows", e.Entity, e.Inserted)
		}
		if e.Source-e.Duplicates != e.Dest {
			t.Errorf("%s: idempotent run count mismatch source=%d duplicates=%d dest=%d", e.Entity, e.Source, e.Duplicates, e.Dest)
		}
	}
	if report2.ChildGroupEdgs != 0 {
		t.Errorf("idempotent run added %d child-group edges, want 0", report2.ChildGroupEdgs)
	}

	// --- Value round-trip + relationship checks on the destination. ---
	gotUser, err := dst.User.Get(ctx, seed.userID)
	if err != nil {
		t.Fatalf("fetch migrated user: %v", err)
	}
	if gotUser.Email != "alice@example.com" {
		t.Errorf("user email = %q, want alice@example.com", gotUser.Email)
	}
	if gotUser.Role != user.RoleAdmin {
		t.Errorf("user role = %q, want admin", gotUser.Role)
	}

	gotAgent, err := dst.Agent.Get(ctx, seed.agentID)
	if err != nil {
		t.Fatalf("fetch migrated agent: %v", err)
	}
	if gotAgent.ProjectID != seed.projectID {
		t.Errorf("agent project_id = %v, want %v", gotAgent.ProjectID, seed.projectID)
	}
	if gotAgent.OwnerID == nil || *gotAgent.OwnerID != seed.user2ID {
		t.Errorf("agent owner_id = %v, want %v", gotAgent.OwnerID, seed.user2ID)
	}

	// The parent group must still point at the child group.
	parent, err := dst.Group.Get(ctx, seed.parentGroupID)
	if err != nil {
		t.Fatalf("fetch parent group: %v", err)
	}
	childIDs, err := parent.QueryChildGroups().IDs(ctx)
	if err != nil {
		t.Fatalf("query child groups: %v", err)
	}
	if len(childIDs) != 1 || childIDs[0] != seed.childGroupID {
		t.Errorf("child group edges = %v, want [%v]", childIDs, seed.childGroupID)
	}

	// Both segments of the session survive the copy to Postgres, with their
	// sub-second started_at values intact. The resend whose start differs from
	// the first segment only below a microsecond is one key in Postgres; it
	// was stored later, so it is the duplicate that is not copied.
	for i, id := range seed.sessionMetricsIDs {
		m, err := dst.AgentSessionMetrics.Get(ctx, id)
		if err != nil {
			t.Fatalf("fetch migrated session metrics %d: %v", i, err)
		}
		if m.SessionID != "session-1" || m.AgentID != seed.agentID.String() || m.TurnCount != i+1 {
			t.Errorf("session metrics %d = (%q, %q, turns %d), want (session-1, %s, turns %d)",
				i, m.SessionID, m.AgentID, m.TurnCount, seed.agentID, i+1)
		}
		if !m.StartedAt.Equal(seed.segmentStarts[i]) {
			t.Errorf("session metrics %d started_at = %v, want %v", i, m.StartedAt, seed.segmentStarts[i])
		}
	}
	if _, err := dst.AgentSessionMetrics.Get(ctx, seed.subMicroResendID); !ent.IsNotFound(err) {
		t.Errorf("sub-microsecond resend: got err %v, want not found (it should not be copied)", err)
	}
}

// seededIDs records the primary keys created by seedSQLiteSource for later
// assertions against the destination.
type seededIDs struct {
	userID        uuid.UUID
	user2ID       uuid.UUID
	projectID     uuid.UUID
	agentID       uuid.UUID
	parentGroupID uuid.UUID
	childGroupID  uuid.UUID
	// Two segments of one session, with their started_at values.
	sessionMetricsIDs [2]uuid.UUID
	segmentStarts     [2]time.Time
	// A resend of the first segment whose started_at differs only below a
	// microsecond, stored after it.
	subMicroResendID uuid.UUID
}

// seedSQLiteSource creates an Ent-on-SQLite database at path and populates it
// with a representative graph: two users, a project, a policy, two groups (in a
// parent/child relationship), an agent, a group membership, a policy binding,
// an API key (an independent entity with a plain FK-style column), and two
// segments of one agent session plus a resend of the first whose started_at
// differs only below a microsecond (session metrics, copied through a row
// filter).
func seedSQLiteSource(t *testing.T, ctx context.Context, path string) seededIDs {
	t.Helper()
	c, err := entc.OpenSQLite("file:"+path+"?cache=shared", entc.PoolConfig{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open sqlite for seeding: %v", err)
	}
	defer c.Close()
	if err := entc.AutoMigrate(ctx, c); err != nil {
		t.Fatalf("auto-migrate source: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	ids := seededIDs{
		userID:        uuid.New(),
		user2ID:       uuid.New(),
		projectID:     uuid.New(),
		agentID:       uuid.New(),
		parentGroupID: uuid.New(),
		childGroupID:  uuid.New(),
	}

	if err := c.User.Create().
		SetID(ids.userID).SetEmail("alice@example.com").SetDisplayName("Alice").
		SetRole(user.RoleAdmin).SetStatus(user.StatusActive).SetCreated(now).
		Exec(ctx); err != nil {
		t.Fatalf("seed user1: %v", err)
	}
	if err := c.User.Create().
		SetID(ids.user2ID).SetEmail("bob@example.com").SetDisplayName("Bob").
		SetRole(user.RoleMember).SetStatus(user.StatusActive).SetCreated(now).
		Exec(ctx); err != nil {
		t.Fatalf("seed user2: %v", err)
	}

	if err := c.Project.Create().
		SetID(ids.projectID).SetName("Demo").SetSlug("demo").
		SetOwnerID(ids.userID.String()).SetCreated(now).SetUpdated(now).
		Exec(ctx); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	policyID := uuid.New()
	if err := c.AccessPolicy.Create().
		SetID(policyID).SetName("allow-all").SetScopeType(accesspolicy.ScopeTypeHub).
		SetResourceType("agent").SetEffect(accesspolicy.EffectAllow).SetActions([]string{"read"}).
		SetPriority(0).SetCreated(now).SetUpdated(now).
		Exec(ctx); err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	if err := c.Group.Create().
		SetID(ids.parentGroupID).SetName("Parent").SetSlug("parent").
		SetGroupType(group.GroupTypeExplicit).SetOwnerID(ids.userID).SetCreated(now).SetUpdated(now).
		Exec(ctx); err != nil {
		t.Fatalf("seed parent group: %v", err)
	}
	if err := c.Group.Create().
		SetID(ids.childGroupID).SetName("Child").SetSlug("child").
		SetGroupType(group.GroupTypeExplicit).SetOwnerID(ids.userID).SetCreated(now).SetUpdated(now).
		Exec(ctx); err != nil {
		t.Fatalf("seed child group: %v", err)
	}
	if err := c.Group.UpdateOneID(ids.parentGroupID).AddChildGroupIDs(ids.childGroupID).Exec(ctx); err != nil {
		t.Fatalf("link child group: %v", err)
	}

	if err := c.Agent.Create().
		SetID(ids.agentID).SetSlug("agent-1").SetName("Agent One").
		SetProjectID(ids.projectID).SetStatus(agent.StatusRunning).
		SetCreatedBy(ids.userID).SetOwnerID(ids.user2ID).SetCreated(now).SetUpdated(now).
		Exec(ctx); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	if err := c.GroupMembership.Create().
		SetID(uuid.New()).SetRole(groupmembership.RoleMember).SetAddedAt(now).
		SetGroupID(ids.parentGroupID).SetUserID(ids.user2ID).
		Exec(ctx); err != nil {
		t.Fatalf("seed group membership: %v", err)
	}

	if err := c.PolicyBinding.Create().
		SetID(uuid.New()).SetPrincipalType(policybinding.PrincipalTypeUser).
		SetPolicyID(policyID).SetUserID(ids.userID).SetCreated(now).
		Exec(ctx); err != nil {
		t.Fatalf("seed policy binding: %v", err)
	}

	if err := c.ApiKey.Create().
		SetID(uuid.New()).SetUserID(ids.userID).SetKeyHash("hash-abc").SetCreated(now).
		Exec(ctx); err != nil {
		t.Fatalf("seed api key: %v", err)
	}

	// A session resumed after a restart keeps its ID and starts a new segment.
	ids.sessionMetricsIDs = [2]uuid.UUID{uuid.New(), uuid.New()}
	segStart := now.Add(-time.Hour).Add(123456 * time.Microsecond)
	ids.segmentStarts = [2]time.Time{segStart, segStart.Add(30 * time.Minute)}
	for i, id := range ids.sessionMetricsIDs {
		if err := c.AgentSessionMetrics.Create().
			SetID(id).SetAgentID(ids.agentID.String()).SetProjectID(ids.projectID.String()).
			SetSessionID("session-1").SetStartedAt(ids.segmentStarts[i]).SetTurnCount(i + 1).
			SetCreatedAt(now).
			Exec(ctx); err != nil {
			t.Fatalf("seed session metrics %d: %v", i, err)
		}
	}
	// SQLite keeps nanoseconds, so its unique index accepts this row; Postgres
	// keeps microseconds, so the copy must treat it as a duplicate.
	ids.subMicroResendID = uuid.New()
	if err := c.AgentSessionMetrics.Create().
		SetID(ids.subMicroResendID).SetAgentID(ids.agentID.String()).SetProjectID(ids.projectID.String()).
		SetSessionID("session-1").SetStartedAt(segStart.Add(400 * time.Nanosecond)).SetTurnCount(9).
		SetCreatedAt(now.Add(time.Second)).
		Exec(ctx); err != nil {
		t.Fatalf("seed sub-microsecond resend: %v", err)
	}

	return ids
}

// newIsolatedPostgresSchema creates a uniquely named, empty schema in the
// database dsn points at, registers a cleanup that drops it, and returns dsn
// with its search_path set to that schema. Ent creates and queries tables
// unqualified, so the migration lands entirely in the new schema and leaves
// the rest of the database (which other test packages may share) untouched.
func newIsolatedPostgresSchema(t *testing.T, dsn string) string {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres for schema setup: %v", err)
	}
	schema := "entc_it_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := db.Exec("CREATE SCHEMA " + schema); err != nil {
		_ = db.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE"); err != nil {
			t.Logf("warning: drop schema %s: %v", schema, err)
		}
		_ = db.Close()
	})
	scoped, err := withSearchPath(dsn, schema)
	if err != nil {
		t.Fatalf("build schema-scoped dsn: %v", err)
	}
	return scoped
}

// withSearchPath returns dsn with the search_path connection parameter set to
// schema. It accepts both URL-style ("postgres://...") and libpq keyword/value
// ("host=... dbname=...") DSNs; pgx sends the parameter as a startup setting
// on every pooled connection.
func withSearchPath(dsn, schema string) (string, error) {
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return dsn + " search_path=" + schema, nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ensure ent is referenced even if future edits drop direct uses.
var _ = ent.Client{}
