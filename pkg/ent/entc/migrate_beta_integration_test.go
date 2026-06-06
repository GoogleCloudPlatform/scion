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
	"os"
	"path/filepath"
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
// The destination DSN comes from SCION_PG_TEST_DSN; the test skips when it is
// unset. Run with:
//
//	SCION_PG_TEST_DSN='postgres://user:pass@host:5432/db?sslmode=require' \
//	  go test -tags integration -run TestMigrateBeta ./pkg/ent/entc/...
func TestMigrateBeta_SQLiteToPostgres(t *testing.T) {
	dstDSN := os.Getenv("SCION_PG_TEST_DSN")
	if dstDSN == "" {
		t.Skip("SCION_PG_TEST_DSN not set; skipping Postgres integration test")
	}
	ctx := context.Background()

	// Start from a clean destination schema so row counts are deterministic.
	resetPostgresSchema(t, dstDSN)

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

	// Every entity must have matching counts; the seeded entities must have
	// actually inserted rows with nothing skipped on the first pass.
	seenInserts := 0
	for _, e := range report.Entities {
		if e.Source != e.Dest {
			t.Errorf("%s: source=%d dest=%d (mismatch)", e.Entity, e.Source, e.Dest)
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
		if e.Source != e.Dest {
			t.Errorf("%s: idempotent run count mismatch source=%d dest=%d", e.Entity, e.Source, e.Dest)
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
}

// seedSQLiteSource creates an Ent-on-SQLite database at path and populates it
// with a representative graph: two users, a project, a policy, two groups (in a
// parent/child relationship), an agent, a group membership, a policy binding,
// and an API key (an independent entity with a plain FK-style column).
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
		SetID(ids.projectID).SetName("Demo").SetSlug("demo").SetVisibility("private").
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
		SetProjectID(ids.projectID).SetStatus(agent.StatusRunning).SetVisibility("private").
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

	return ids
}

// resetPostgresSchema drops and recreates the public schema so the test starts
// from an empty database, making row-count assertions deterministic.
func resetPostgresSchema(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres for reset: %v", err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"DROP SCHEMA public CASCADE",
		"CREATE SCHEMA public",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("reset schema (%s): %v", stmt, err)
		}
	}
}

// ensure ent is referenced even if future edits drop direct uses.
var _ = ent.Client{}
