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

//go:build !no_sqlite

package entc

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeLegacyDB creates a representative legacy raw-SQL hub.db at path. It
// intentionally exercises every mechanical difference migration α handles:
// the schema_migrations sentinel, created_at/updated_at and agent_id renames,
// the policies/group_members table renames, composite-keyed tables, the
// polymorphic member/principal split, bool-as-int, JSON-as-TEXT, BLOBs, a
// dangling agents.created_by reference, and non-UUID primary keys for both
// secrets and runtime_brokers (with a foreign-key reference to the latter).
func writeLegacyDB(t *testing.T, path string) legacyFixture {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	ddl := []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMP)`,
		`CREATE TABLE users (id TEXT PRIMARY KEY, email TEXT, display_name TEXT, role TEXT, status TEXT, preferences TEXT, created_at TIMESTAMP, last_login TIMESTAMP, last_seen TIMESTAMP)`,
		`CREATE TABLE projects (id TEXT PRIMARY KEY, name TEXT, slug TEXT, labels TEXT, annotations TEXT, visibility TEXT, created_at TIMESTAMP, updated_at TIMESTAMP, created_by TEXT, owner_id TEXT, default_runtime_broker_id TEXT)`,
		`CREATE TABLE runtime_brokers (id TEXT PRIMARY KEY, name TEXT, slug TEXT, type TEXT, mode TEXT, status TEXT, connection_state TEXT, auto_provide INTEGER, capabilities TEXT, created_at TIMESTAMP, updated_at TIMESTAMP)`,
		`CREATE TABLE agents (id TEXT PRIMARY KEY, agent_id TEXT, name TEXT, project_id TEXT, labels TEXT, detached INTEGER, web_pty_enabled INTEGER, visibility TEXT, runtime_broker_id TEXT, created_by TEXT, owner_id TEXT, created_at TIMESTAMP, updated_at TIMESTAMP)`,
		`CREATE TABLE broker_secrets (broker_id TEXT PRIMARY KEY, secret_key BLOB, algorithm TEXT, status TEXT, created_at TIMESTAMP)`,
		`CREATE TABLE secrets (id TEXT PRIMARY KEY, key TEXT, encrypted_value TEXT, scope TEXT, scope_id TEXT, version INTEGER, secret_type TEXT, injection_mode TEXT, allow_progeny INTEGER, created_at TIMESTAMP, updated_at TIMESTAMP)`,
		`CREATE TABLE groups (id TEXT PRIMARY KEY, name TEXT, slug TEXT, group_type TEXT, parent_id TEXT, created_at TIMESTAMP, updated_at TIMESTAMP)`,
		`CREATE TABLE group_members (group_id TEXT, member_type TEXT, member_id TEXT, role TEXT, added_at TIMESTAMP, added_by TEXT, PRIMARY KEY (group_id, member_type, member_id))`,
		`CREATE TABLE policies (id TEXT PRIMARY KEY, name TEXT, scope_type TEXT, resource_type TEXT, effect TEXT, priority INTEGER, created_at TIMESTAMP, updated_at TIMESTAMP)`,
		`CREATE TABLE policy_bindings (policy_id TEXT, principal_type TEXT, principal_id TEXT, PRIMARY KEY (policy_id, principal_type, principal_id))`,
		`CREATE TABLE allow_list (id TEXT PRIMARY KEY, email TEXT, note TEXT, added_by TEXT, created DATETIME)`,
		`CREATE TABLE messages (id TEXT PRIMARY KEY, project_id TEXT, sender TEXT, recipient TEXT, msg TEXT, type TEXT, urgent INTEGER, read INTEGER, created_at TIMESTAMP)`,
	}
	for _, s := range ddl {
		_, err := db.Exec(s)
		require.NoError(t, err, s)
	}

	const ts = "2026-05-01 12:30:00"
	fx := legacyFixture{
		userID:    uuid.NewString(),
		projectID: uuid.NewString(),
		agentID:   uuid.NewString(),
		groupID:   uuid.NewString(),
		policyID:  uuid.NewString(),
		// Non-UUID ids that must be remapped to deterministic UUIDs.
		brokerLegacyID: "plugin-broker-telegram",
		secretLegacyID: "hub-abc123-agent_signing_key",
		// A created_by that references no user row (dangling, must survive).
		danglingPrincipal: uuid.NewString(),
	}

	exec := func(q string, args ...any) {
		_, err := db.Exec(q, args...)
		require.NoError(t, err, q)
	}

	exec(`INSERT INTO users (id,email,display_name,role,status,preferences,created_at) VALUES (?,?,?,?,?,?,?)`,
		fx.userID, "a@example.com", "Alice", "admin", "active", `{"theme":"dark"}`, ts)
	exec(`INSERT INTO projects (id,name,slug,labels,annotations,visibility,created_at,updated_at,owner_id,default_runtime_broker_id) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		fx.projectID, "Proj", "proj", `{"team":"x"}`, "", "private", ts, ts, fx.userID, fx.brokerLegacyID)
	exec(`INSERT INTO runtime_brokers (id,name,slug,type,mode,status,connection_state,auto_provide,capabilities,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		fx.brokerLegacyID, "telegram", "telegram", "plugin", "connected", "online", "connected", 1, `{"x":true}`, ts, ts)
	// Agent with agent_id (->slug), int bools, JSON labels, dangling created_by,
	// and a runtime_broker_id pointing at the non-UUID broker.
	exec(`INSERT INTO agents (id,agent_id,name,project_id,labels,detached,web_pty_enabled,visibility,runtime_broker_id,created_by,owner_id,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		fx.agentID, "deploy-bot", "Deploy Bot", fx.projectID, `{"k":"v"}`, 1, 0, "private", fx.brokerLegacyID, fx.danglingPrincipal, fx.danglingPrincipal, ts, ts)
	exec(`INSERT INTO broker_secrets (broker_id,secret_key,algorithm,status,created_at) VALUES (?,?,?,?,?)`,
		fx.brokerLegacyID, []byte{0x01, 0x02, 0x03, 0xff}, "hmac-sha256", "active", ts)
	exec(`INSERT INTO secrets (id,key,encrypted_value,scope,scope_id,version,secret_type,injection_mode,allow_progeny,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		fx.secretLegacyID, "agent_signing_key", "ZW5j", "hub", "abc123", 1, "internal", "as_needed", 0, ts, ts)
	exec(`INSERT INTO groups (id,name,slug,group_type,created_at,updated_at) VALUES (?,?,?,?,?,?)`,
		fx.groupID, "G", "g", "custom", ts, ts)
	exec(`INSERT INTO group_members (group_id,member_type,member_id,role,added_at,added_by) VALUES (?,?,?,?,?,?)`,
		fx.groupID, "user", fx.userID, "member", ts, fx.userID)
	exec(`INSERT INTO policies (id,name,scope_type,resource_type,effect,priority,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		fx.policyID, "P", "hub", "project", "allow", 0, ts, ts)
	exec(`INSERT INTO policy_bindings (policy_id,principal_type,principal_id) VALUES (?,?,?)`,
		fx.policyID, "user", fx.userID)
	exec(`INSERT INTO allow_list (id,email,note,added_by,created) VALUES (?,?,?,?,?)`,
		uuid.NewString(), "b@example.com", "", fx.userID, ts)
	exec(`INSERT INTO messages (id,project_id,sender,recipient,msg,type,urgent,read,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		uuid.NewString(), fx.projectID, "alice", "deploy-bot", "hi", "instruction", 0, 1, ts)
	// Empty-string JSON / nullable cases on a second project (must not break read-back).
	exec(`INSERT INTO projects (id,name,slug,labels,annotations,visibility,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		uuid.NewString(), "Proj2", "proj2", "", "", "private", ts, ts)

	return fx
}

type legacyFixture struct {
	userID, projectID, agentID, groupID, policyID string
	brokerLegacyID, secretLegacyID                string
	danglingPrincipal                             string
}

// openMigrated opens the migrated file through the Ent client.
func openMigrated(t *testing.T, path string) *ent.Client {
	t.Helper()
	client, err := OpenSQLite("file:"+path+"?cache=shared", PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestIsLegacyRawSQLSchema(t *testing.T) {
	dir := t.TempDir()

	// Non-existent file.
	ok, err := IsLegacyRawSQLSchema(filepath.Join(dir, "nope.db"))
	require.NoError(t, err)
	assert.False(t, ok)

	// Legacy file.
	legacyPath := filepath.Join(dir, "legacy.db")
	writeLegacyDB(t, legacyPath)
	ok, err = IsLegacyRawSQLSchema(legacyPath)
	require.NoError(t, err)
	assert.True(t, ok, "legacy raw-SQL schema should be detected")

	// Fresh Ent file.
	entPath := filepath.Join(dir, "ent.db")
	require.NoError(t, buildEntSchema(context.Background(), entPath))
	ok, err = IsLegacyRawSQLSchema(entPath)
	require.NoError(t, err)
	assert.False(t, ok, "Ent schema must not be flagged as legacy")
}

func TestMigrateAlphaSQLite_EndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	fx := writeLegacyDB(t, path)

	report, err := MigrateAlphaSQLite(ctx, path, AlphaOptions{BackupSuffix: "unit"})
	require.NoError(t, err)
	require.False(t, report.Skipped)

	// Backup exists and is itself the legacy schema.
	assert.Equal(t, path+".bak.unit", report.BackupPath)
	bakLegacy, err := IsLegacyRawSQLSchema(report.BackupPath)
	require.NoError(t, err)
	assert.True(t, bakLegacy, "backup should retain the legacy schema")

	// The migrated file is no longer legacy.
	nowLegacy, err := IsLegacyRawSQLSchema(path)
	require.NoError(t, err)
	assert.False(t, nowLegacy)

	// Every table reported equal source/dest counts.
	for _, tr := range report.Tables {
		assert.Equalf(t, tr.Source, tr.Dest, "row count mismatch for %s", tr.EntTable)
	}

	// Read everything back through the Ent client.
	client := openMigrated(t, path)

	users, err := client.User.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, users, 1)
	assert.Equal(t, "a@example.com", users[0].Email)
	require.NotNil(t, users[0].Preferences)
	assert.Equal(t, "dark", users[0].Preferences.Theme, "typed JSON preferences must deserialize")

	projects, err := client.Project.Query().All(ctx)
	require.NoError(t, err)
	assert.Len(t, projects, 2)

	// Agent: agent_id -> slug, int bools -> real bools, JSON labels.
	agents, err := client.Agent.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, agents, 1)
	assert.Equal(t, "deploy-bot", agents[0].Slug)
	assert.True(t, agents[0].Detached)
	assert.False(t, agents[0].WebPtyEnabled)
	assert.Equal(t, map[string]string{"k": "v"}, agents[0].Labels)

	// Non-UUID broker id remapped deterministically, and the broker_secret +
	// agent + project references resolve to the SAME new id.
	wantBroker := uuid.NewSHA1(migrationNamespace, []byte(fx.brokerLegacyID))
	brokers, err := client.RuntimeBroker.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, brokers, 1)
	assert.Equal(t, wantBroker, brokers[0].ID)
	assert.True(t, brokers[0].AutoProvide)

	bs, err := client.BrokerSecret.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, bs, 1)
	assert.Equal(t, wantBroker, bs[0].ID, "broker_secret PK (broker_id) must follow the remap")
	assert.Equal(t, []byte{0x01, 0x02, 0x03, 0xff}, bs[0].SecretKey, "BLOB must survive")
	assert.Equal(t, wantBroker.String(), agents[0].RuntimeBrokerID, "agent broker ref must follow the remap")
	var brokerProj *ent.Project
	for _, p := range projects {
		if p.DefaultRuntimeBrokerID != nil && *p.DefaultRuntimeBrokerID != "" {
			brokerProj = p
		}
	}
	require.NotNil(t, brokerProj, "expected a project with a default runtime broker")
	assert.Equal(t, wantBroker.String(), *brokerProj.DefaultRuntimeBrokerID, "project broker ref must follow the remap")

	// Non-UUID secret id remapped (no inbound FK, just the PK).
	wantSecret := uuid.NewSHA1(migrationNamespace, []byte(fx.secretLegacyID))
	secrets, err := client.Secret.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, secrets, 1)
	assert.Equal(t, wantSecret, secrets[0].ID)
	assert.Equal(t, "agent_signing_key", secrets[0].Key)

	// Restructured tables: polymorphic split.
	gms, err := client.GroupMembership.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, gms, 1)
	require.NotNil(t, gms[0].UserID)
	assert.Equal(t, fx.userID, gms[0].UserID.String())

	pbs, err := client.PolicyBinding.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, pbs, 1)
	require.NotNil(t, pbs[0].UserID)
	assert.Equal(t, fx.userID, pbs[0].UserID.String())
	assert.False(t, pbs[0].Created.IsZero(), "policy binding created must be stamped")

	// Dangling created_by survives as a (parseable) UUID even though no user row exists.
	require.NotNil(t, agents[0].CreatedBy)
	assert.Equal(t, fx.danglingPrincipal, agents[0].CreatedBy.String())
}

func TestMigrateAlphaSQLite_Idempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	writeLegacyDB(t, path)

	r1, err := MigrateAlphaSQLite(ctx, path, AlphaOptions{BackupSuffix: "one"})
	require.NoError(t, err)
	require.False(t, r1.Skipped)

	// Second run is a no-op: already migrated.
	r2, err := MigrateAlphaSQLite(ctx, path, AlphaOptions{BackupSuffix: "two"})
	require.NoError(t, err)
	assert.True(t, r2.Skipped)

	// No duplicate backup from the skipped run.
	_, err = os.Stat(path + ".bak.two")
	assert.True(t, os.IsNotExist(err), "skipped run must not create a backup")

	// Data is unchanged: still exactly one agent.
	client := openMigrated(t, path)
	n, err := client.Agent.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestMigrateAlphaSQLite_SkipsNonLegacy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// A fresh Ent file.
	entPath := filepath.Join(dir, "ent.db")
	require.NoError(t, buildEntSchema(ctx, entPath))
	r, err := MigrateAlphaSQLite(ctx, entPath, AlphaOptions{})
	require.NoError(t, err)
	assert.True(t, r.Skipped)

	// A missing file.
	r, err = MigrateAlphaSQLite(ctx, filepath.Join(dir, "absent.db"), AlphaOptions{})
	require.NoError(t, err)
	assert.True(t, r.Skipped)
}
