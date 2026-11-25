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
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entsecretrequest "github.com/GoogleCloudPlatform/scion/pkg/ent/secretrequest"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingVariableDeleter struct {
	deletedSecrets []string
	droppedPending []string
	onDelete       func(name, scope, scopeID string)
}

func (r *recordingVariableDeleter) Delete(_ context.Context, name, scope, scopeID string) error {
	r.deletedSecrets = append(r.deletedSecrets, scope+"/"+scopeID+"/"+name)
	if r.onDelete != nil {
		r.onDelete(name, scope, scopeID)
	}
	return nil
}

func (r *recordingVariableDeleter) DropPending(_ context.Context, ref string) error {
	r.droppedPending = append(r.droppedPending, ref)
	return nil
}

func TestMigrateRetireVariableSecretType_AllScopeClassesAndIdempotent(t *testing.T) {
	ctx := context.Background()
	client, err := OpenSQLite(":memory:", PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	require.NoError(t, AutoMigrate(ctx, client))

	db := SQLDB(client.Driver())
	require.NotNil(t, db)
	now := time.Now().UTC()

	insertSecret := func(key, secretType, target, scope, scopeID, injectionMode string) {
		t.Helper()
		_, err := db.ExecContext(ctx, `
			INSERT INTO secrets (
				id, key, encrypted_value, secret_ref, secret_type, target,
				scope, scope_id, description, injection_mode, allow_progeny,
				version, created_by, updated_by, created, updated
			) VALUES (?, ?, ?, '', ?, ?, ?, ?, '', ?, 0, 1, 'seed', 'seed', ?, ?)
		`, uuid.NewString(), key, "enc-val", secretType, target, scope, scopeID, injectionMode, now, now)
		require.NoError(t, err)
	}

	// Seed every scope class of legacy "variable" rows in secrets:
	// 1. Hub producer keys (chat, GitHub App, plugin) -> internal
	insertSecret(config.SecretTelegramBotToken, "variable", config.SecretTelegramBotToken, store.ScopeHub, "hub-1", "as_needed")
	insertSecret(config.SecretGitHubAppPrivateKey, "variable", config.SecretGitHubAppPrivateKey, store.ScopeHub, "hub-1", "as_needed")
	insertSecret(config.SecretA2AAPIKey, "variable", config.SecretA2AAPIKey, store.ScopeHub, "hub-1", "as_needed")
	// 2. Hub non-producer key -> leftover, deleted with WARN
	insertSecret("CUSTOM_HUB_VAR", "variable", "CUSTOM_HUB_VAR", store.ScopeHub, "hub-1", "always")
	// 3. User scope -> leftover, deleted with WARN
	insertSecret("USER_VAR", "variable", "USER_VAR", store.ScopeUser, "user-1", "always")
	// 4. Runtime broker scope -> leftover, deleted with WARN
	insertSecret("BROKER_VAR", "variable", "BROKER_VAR", store.ScopeRuntimeBroker, "broker-1", "always")
	// 5. Project scope -> on_demand, target=key, injection_mode=as_needed
	insertSecret("PROJECT_VAR", "variable", "legacy_proj_target", store.ScopeProject, "proj-1", "always")
	// 6. Agent scope -> on_demand, target=key, injection_mode=as_needed
	insertSecret("AGENT_VAR", "variable", "legacy_agent_target", store.ScopeAgent, "agent-1", "always")
	// Untouched non-variable row
	insertSecret("KEEP_ENV", store.SecretTypeEnvironment, "KEEP_ENV", store.ScopeProject, "proj-1", "always")

	// Seed pending_secrets: one variable (leftover -> deleted) and one environment (kept).
	pendingVarID := uuid.NewString()
	pendingEnvID := uuid.NewString()
	insertPending := func(id, key, secretType, valueRef string) {
		t.Helper()
		_, err := db.ExecContext(ctx, `
			INSERT INTO pending_secrets (
				id, key, secret_type, target, scope, scope_id, allow_progeny,
				injection_mode, description, value_ref, value_len,
				agent_id, agent_slug, project_id, project_name,
				created, updated, expires_at
			) VALUES (?, ?, ?, ?, ?, ?, 0, 'as_needed', '', ?, 5, 'a1', 'slug', 'p1', 'pname', ?, ?, ?)
		`, id, key, secretType, key, store.ScopeUser, "user-1", valueRef, now, now, now.Add(time.Hour))
		require.NoError(t, err)
	}
	insertPending(pendingVarID, "PENDING_VAR", "variable", "ref-pending-var")
	insertPending(pendingEnvID, "PENDING_ENV", store.SecretTypeEnvironment, "ref-pending-env")

	// Seed secret_requests: one variable (-> on_demand) and one environment (kept).
	reqVarID := uuid.NewString()
	reqEnvID := uuid.NewString()
	insertSecretReq := func(id, key, secretType string) {
		t.Helper()
		_, err := db.ExecContext(ctx, `
			INSERT INTO secret_requests (
				id, project_id, agent_id, agent_name, key, key_norm, version,
				description, secret_type, message_id, position, intro_length,
				status, created_at, expires_at
			) VALUES (?, ?, ?, 'agent-a', ?, ?, 1, '', ?, ?, 0, 0, 'pending', ?, ?)
		`, id, uuid.NewString(), uuid.NewString(), key, strings.ToLower(key), secretType, uuid.NewString(), now, now.Add(time.Hour))
		require.NoError(t, err)
	}
	insertSecretReq(reqVarID, "REQ_VAR", "variable")
	insertSecretReq(reqEnvID, "REQ_ENV", store.SecretTypeEnvironment)

	// Capture slog.Warn output during migration.
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	// Run AutoMigrate half (steps 1, 2, 5).
	require.NoError(t, MigrateRetireVariableSecretType(ctx, client))

	// Run startup-hook half (steps 3, 4).
	deleter := &recordingVariableDeleter{}
	require.NoError(t, MigrateRetireVariableSecretTypeLeftovers(ctx, client, deleter))

	type secretRow struct {
		secretType    string
		target        string
		injectionMode string
	}
	readSecrets := func() map[string]secretRow {
		rows, err := db.QueryContext(ctx, "SELECT scope, scope_id, key, secret_type, target, injection_mode FROM secrets")
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		out := make(map[string]secretRow)
		for rows.Next() {
			var sc, sid, k, st, tgt, im string
			require.NoError(t, rows.Scan(&sc, &sid, &k, &st, &tgt, &im))
			out[sc+"/"+sid+"/"+k] = secretRow{secretType: st, target: tgt, injectionMode: im}
		}
		require.NoError(t, rows.Err())
		return out
	}

	gotSecrets := readSecrets()
	assert.Equal(t, map[string]secretRow{
		"hub/hub-1/" + config.SecretTelegramBotToken:    {secretType: store.SecretTypeInternal, target: config.SecretTelegramBotToken, injectionMode: "as_needed"},
		"hub/hub-1/" + config.SecretGitHubAppPrivateKey: {secretType: store.SecretTypeInternal, target: config.SecretGitHubAppPrivateKey, injectionMode: "as_needed"},
		"hub/hub-1/" + config.SecretA2AAPIKey:           {secretType: store.SecretTypeInternal, target: config.SecretA2AAPIKey, injectionMode: "as_needed"},
		"project/proj-1/PROJECT_VAR":                    {secretType: store.SecretTypeOnDemand, target: "PROJECT_VAR", injectionMode: store.InjectionModeAsNeeded},
		"agent/agent-1/AGENT_VAR":                       {secretType: store.SecretTypeOnDemand, target: "AGENT_VAR", injectionMode: store.InjectionModeAsNeeded},
		"project/proj-1/KEEP_ENV":                       {secretType: store.SecretTypeEnvironment, target: "KEEP_ENV", injectionMode: "always"},
	}, gotSecrets)

	assert.ElementsMatch(t, []string{
		"hub/hub-1/CUSTOM_HUB_VAR",
		"user/user-1/USER_VAR",
		"runtime_broker/broker-1/BROKER_VAR",
	}, deleter.deletedSecrets)
	assert.Equal(t, []string{"ref-pending-var"}, deleter.droppedPending)

	// Verify one WARN line per deleted leftover row (3 secrets + 1 pending_secret = 4 lines).
	warnLines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	assert.Len(t, warnLines, 4, "expected one WARN per deleted leftover row; logs:\n%s", logBuf.String())
	assert.Contains(t, logBuf.String(), "CUSTOM_HUB_VAR")
	assert.Contains(t, logBuf.String(), "USER_VAR")
	assert.Contains(t, logBuf.String(), "BROKER_VAR")
	assert.Contains(t, logBuf.String(), "PENDING_VAR")

	// Verify pending_secrets state.
	var pendingCount int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_secrets").Scan(&pendingCount))
	assert.Equal(t, 1, pendingCount)
	var keptPendingKey string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT key FROM pending_secrets WHERE id = ?", pendingEnvID).Scan(&keptPendingKey))
	assert.Equal(t, "PENDING_ENV", keptPendingKey)

	// Verify secret_requests state.
	var reqVarType, reqEnvType string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT secret_type FROM secret_requests WHERE id = ?", reqVarID).Scan(&reqVarType))
	require.NoError(t, db.QueryRowContext(ctx, "SELECT secret_type FROM secret_requests WHERE id = ?", reqEnvID).Scan(&reqEnvType))
	assert.Equal(t, store.SecretTypeOnDemand, reqVarType)
	assert.Equal(t, store.SecretTypeEnvironment, reqEnvType)

	// Second run must be a complete no-op (idempotent).
	logBuf.Reset()
	deleter2 := &recordingVariableDeleter{}
	require.NoError(t, MigrateRetireVariableSecretType(ctx, client))
	require.NoError(t, MigrateRetireVariableSecretTypeLeftovers(ctx, client, deleter2))
	assert.Empty(t, deleter2.deletedSecrets)
	assert.Empty(t, deleter2.droppedPending)
	assert.Empty(t, logBuf.String())
	assert.Equal(t, gotSecrets, readSecrets())
}

func TestMigrateRetireVariableSecretTypeLeftovers_NilDeleter(t *testing.T) {
	ctx := context.Background()
	client, err := OpenSQLite(":memory:", PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	require.NoError(t, AutoMigrate(ctx, client))

	db := SQLDB(client.Driver())
	now := time.Now().UTC()
	_, err = db.ExecContext(ctx, `
		INSERT INTO secrets (
			id, key, encrypted_value, secret_ref, secret_type, target,
			scope, scope_id, description, injection_mode, allow_progeny,
			version, created_by, updated_by, created, updated
		) VALUES (?, 'ORPHAN_USER_VAR', 'enc', '', 'variable', 'ORPHAN_USER_VAR', 'user', 'u1', '', 'as_needed', 0, 1, '', '', ?, ?)
	`, uuid.NewString(), now, now)
	require.NoError(t, err)

	require.NoError(t, MigrateRetireVariableSecretTypeLeftovers(ctx, client, nil))

	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM secrets WHERE secret_type = 'variable'").Scan(&count))
	assert.Zero(t, count)
}

func TestMigrateRetireVariableSecretTypeLeftovers_SkipsConcurrentlyReplacedRow(t *testing.T) {
	ctx := context.Background()
	client, err := OpenSQLite(":memory:", PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	require.NoError(t, AutoMigrate(ctx, client))

	db := SQLDB(client.Driver())
	now := time.Now().UTC()
	for _, key := range []string{"A_VAR", "B_VAR"} {
		_, err = db.ExecContext(ctx, `
			INSERT INTO secrets (
				id, key, encrypted_value, secret_ref, secret_type, target,
				scope, scope_id, description, injection_mode, allow_progeny,
				version, created_by, updated_by, created, updated
			) VALUES (?, ?, 'enc', '', 'variable', ?, 'user', 'u1', '', 'as_needed', 0, 1, '', '', ?, ?)
		`, uuid.NewString(), key, key, now, now)
		require.NoError(t, err)
	}

	// Simulate a concurrent API writer replacing B_VAR with an environment
	// secret after the initial SELECT, while A_VAR is being deleted.
	deleter := &recordingVariableDeleter{
		onDelete: func(name, _, _ string) {
			if name == "A_VAR" {
				_, uErr := db.ExecContext(ctx,
					"UPDATE secrets SET secret_type = 'environment' WHERE key = 'B_VAR' AND scope = 'user' AND scope_id = 'u1'")
				require.NoError(t, uErr)
			}
		},
	}

	require.NoError(t, MigrateRetireVariableSecretTypeLeftovers(ctx, client, deleter))

	assert.Equal(t, []string{"user/u1/A_VAR"}, deleter.deletedSecrets,
		"re-check must prevent backend delete for concurrently replaced row B_VAR")

	var bType string
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT secret_type FROM secrets WHERE key = 'B_VAR' AND scope = 'user' AND scope_id = 'u1'").Scan(&bType))
	assert.Equal(t, store.SecretTypeEnvironment, bType)
}

func TestSecretRequest_LegacyVariableNormalizesToOnDemandInEnt(t *testing.T) {
	ctx := context.Background()
	client, err := OpenSQLite(":memory:", PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	require.NoError(t, AutoMigrate(ctx, client))

	reqs, err := messages.NormalizeSecretRequests("secret_requests", []messages.SecretRequestInput{
		{Key: "LEGACY_VAR_REQ", Type: messages.SecretRequestTypeVariable},
	})
	require.NoError(t, err)
	require.Len(t, reqs, 1)
	require.Equal(t, messages.SecretRequestTypeOnDemand, reqs[0].SecretType)

	row, err := client.SecretRequest.Create().
		SetProjectID(uuid.New()).
		SetAgentID(uuid.New()).
		SetAgentName("agent-r3").
		SetKey(reqs[0].Key).
		SetKeyNorm(reqs[0].KeyNorm).
		SetVersion(1).
		SetSecretType(entsecretrequest.SecretType(reqs[0].SecretType)).
		SetMessageID(uuid.New()).
		SetPosition(0).
		SetExpiresAt(time.Now().UTC().Add(time.Hour)).
		Save(ctx)
	require.NoError(t, err)
	assert.Equal(t, entsecretrequest.SecretTypeOnDemand, row.SecretType)
}

type execRecord struct {
	query string
	args  []any
}

type execRecordingDriver struct {
	dialect.Driver
	records *[]execRecord
}

func (d *execRecordingDriver) Exec(ctx context.Context, query string, args, v any) error {
	argv, _ := args.([]any)
	*d.records = append(*d.records, execRecord{query: query, args: argv})
	return d.Driver.Exec(ctx, query, args, v)
}

func (d *execRecordingDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	tx, err := d.Driver.Tx(ctx)
	if err != nil {
		return nil, err
	}
	return &execRecordingTx{Tx: tx, records: d.records}, nil
}

type execRecordingTx struct {
	dialect.Tx
	records *[]execRecord
}

func (t *execRecordingTx) Exec(ctx context.Context, query string, args, v any) error {
	argv, _ := args.([]any)
	*t.records = append(*t.records, execRecord{query: query, args: argv})
	return t.Tx.Exec(ctx, query, args, v)
}

// TestMigrateRetireVariableSecretType_NoParameterizedExecOnEmptySchema guards
// against emitting parameterized Exec statements during AutoMigrate on an empty
// schema, which breaks enttest's Postgres schema-snapshot recorder.
func TestMigrateRetireVariableSecretType_NoParameterizedExecOnEmptySchema(t *testing.T) {
	ctx := context.Background()
	baseClient, err := OpenSQLite(":memory:", PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	defer func() { _ = baseClient.Close() }()

	var records []execRecord
	wrapped := ent.NewClient(ent.Driver(&execRecordingDriver{
		Driver:  baseClient.Driver(),
		records: &records,
	}))

	require.NoError(t, AutoMigrate(ctx, wrapped))
	for _, r := range records {
		assert.Empty(t, r.args, "AutoMigrate on empty schema must not execute parameterized statement: %q", r.query)
	}

	records = nil
	require.NoError(t, MigrateRetireVariableSecretType(ctx, wrapped))
	assert.Empty(t, records, "MigrateRetireVariableSecretType on empty schema must execute zero Exec statements")
}
