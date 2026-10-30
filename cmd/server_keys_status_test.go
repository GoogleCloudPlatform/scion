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

package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var keysStatusTestNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// keysStatusDBFixture creates a migrated SQLite hub database holding one
// secret the keyring at path reads and one it cannot, and returns its path
// and store.
func keysStatusDBFixture(t *testing.T, home, keyringPath string) (string, store.Store) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(home, ".scion", "keys-status.db")
	client, err := entc.OpenSQLite("file:"+dbPath+"?cache=shared", entc.PoolConfig{})
	require.NoError(t, err)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	require.NoError(t, migrateHubStore(ctx, cs, false))

	k, err := keyring.LoadFile(keyringPath)
	require.NoError(t, err)
	c, err := secret.NewValueCipher(k)
	require.NoError(t, err)
	readable, err := c.Encrypt("cmd-keys-synthetic-payload")
	require.NoError(t, err)
	foreign, err := secret.NewLegacyValueCipher("test-only-some-other-secret").Encrypt("cmd-keys-synthetic-foreign")
	require.NoError(t, err)
	for _, row := range []*store.Secret{
		{ID: "cmd-keys-record-readable", Key: "CMD_KEYS_READABLE", EncryptedValue: readable},
		{ID: "cmd-keys-record-foreign", Key: "CMD_KEYS_FOREIGN", EncryptedValue: foreign},
	} {
		row.Scope, row.ScopeID, row.SecretType = store.ScopeUser, "cmd-keys-user", store.SecretTypeEnvironment
		_, err := cs.UpsertSecret(ctx, row)
		require.NoError(t, err)
	}
	return dbPath, cs
}

// status --format json prints the versioned report LocalKeyStatus builds
// for the same file and database, field for field, from a read-only
// database, with no key material or record identifier.
func TestServerKeysStatus_JSONMatchesBuilder(t *testing.T) {
	home := keysTestEnv(t)
	path := writeCmdKeyring(t, nil)
	dbPath, st := keysStatusDBFixture(t, home, path)
	old := keysStatusNow
	keysStatusNow = func() time.Time { return keysStatusTestNow }
	t.Cleanup(func() { keysStatusNow = old })
	outputFormat = "json"

	out, err, code := runKeys(t, "status", "--keyring-file", path, "--db", dbPath)
	require.NoError(t, err, out)
	require.Equal(t, 0, code)

	var got hub.KeyStatusReport
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, hub.KeyStatusReportVersion, got.ReportVersion)
	assert.Equal(t, hub.KeyStatusSourceLocal, got.Instance.Source)
	assert.Equal(t, "gen1", got.Signing.ActiveID)
	assert.True(t, got.Audit.Ran)
	assert.Equal(t, 2, got.Audit.Scanned)
	assert.Equal(t, 1, got.Audit.Unreadable)
	assert.Nil(t, got.MaterialDigest)
	assert.Equal(t, hub.KeyStatusAuditFresh, got.Audit.Scope)
	require.NotNil(t, got.Audit.CompletedAt)

	k, err := keyring.LoadFile(path)
	require.NoError(t, err)
	c, err := secret.NewValueCipher(k)
	require.NoError(t, err)
	want, err := hub.LocalKeyStatus(context.Background(), k, c, st, keysStatusTestNow)
	require.NoError(t, err)
	// Each run's audit has its own completion time.
	want.Audit.CompletedAt, got.Audit.CompletedAt = nil, nil
	wantJSON, err := json.Marshal(want)
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, string(wantJSON), string(gotJSON))

	assertNoCmdKRSecret(t, out)
	for _, s := range []string{"cmd-keys-record-readable", "cmd-keys-record-foreign", "CMD_KEYS_READABLE", "CMD_KEYS_FOREIGN", "cmd-keys-synthetic"} {
		assert.NotContains(t, out, s)
	}
}

// The text output names the omitted digest; --no-store reports the
// keyring without opening any database.
func TestServerKeysStatus_TextAndNoStore(t *testing.T) {
	keysTestEnv(t)
	path := writeCmdKeyring(t, nil)

	out, err, code := runKeys(t, "status", "--keyring-file", path, "--no-store")
	require.NoError(t, err, out)
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "Keyring: source file, revision cmd-test-rev")
	assert.Contains(t, out, "Audit: not run (no database)")
	assert.Contains(t, out, "Encryption policy: none")
	assert.Contains(t, out, "Material digest: omitted (pending_legacy_independence)")
	assertNoCmdKRSecret(t, out)
}

// A missing database is a failure (exit 1) and is never created: the
// database is opened read only.
func TestServerKeysStatus_MissingDatabaseNotCreated(t *testing.T) {
	home := keysTestEnv(t)
	dbPath := filepath.Join(home, ".scion", "absent.db")
	_, err, code := runKeys(t, "status", "--keyring-file", writeCmdKeyring(t, nil), "--db", dbPath)
	require.Error(t, err)
	assert.Equal(t, keysExitFailure, code)
	_, statErr := os.Stat(dbPath)
	assert.True(t, os.IsNotExist(statErr), "the read-only open must not create the database")
}

// An invalid keyring file is exit 2 before any database is opened.
func TestServerKeysStatus_InvalidKeyringIsExit2(t *testing.T) {
	keysTestEnv(t)
	path := writeCmdKeyring(t, func(doc map[string]any) {
		doc["encryption"].(map[string]any)["write_format"] = "v2"
	})
	_, err, code := runKeys(t, "status", "--keyring-file", path, "--no-store")
	require.Error(t, err)
	assert.Equal(t, keysExitInvalid, code)
}
