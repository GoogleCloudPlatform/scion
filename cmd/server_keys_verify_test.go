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
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/ciphertest"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verifyEnc2Key is a synthetic encryption key for the "enc2" entry the
// verify fixtures add to the cmd test keyring.
var verifyEnc2Key = []byte("FAKE-cmd-verify-enc2-key-0000000")

// verifyKeyring is the cmd test keyring (legacy v1 writer) with a second
// encryption key, enc2, that the fixtures' policy allows.
func verifyKeyring(t *testing.T) string {
	t.Helper()
	return writeCmdKeyring(t, func(doc map[string]any) {
		e := doc["encryption"].(map[string]any)
		e["keys"] = append(e["keys"].([]any), map[string]any{"id": "enc2", "key_b64": base64.StdEncoding.EncodeToString(verifyEnc2Key)})
	})
}

// verifyRow is one row a fixture plants.
type verifyRow struct {
	id, key, value, ref string
}

const (
	verifyIDGood    = "7c1e3d2a-6b5c-4d4e-9f30-00000000c001"
	verifyIDRef     = "7c1e3d2a-6b5c-4d4e-9f30-00000000c002"
	verifyIDPlain   = "7c1e3d2a-6b5c-4d4e-9f30-00000000c003"
	verifyIDV1      = "7c1e3d2a-6b5c-4d4e-9f30-00000000c004"
	verifyIDLie     = "7c1e3d2a-6b5c-4d4e-9f30-00000000c005"
	verifyIDEmpty   = "7c1e3d2a-6b5c-4d4e-9f30-00000000c006"
	verifyKeyPrefix = "CMD_VERIFY_"
)

// verifyRows returns the passing rows and, with failing, one row per
// failure class.
func verifyRows(t *testing.T, keyringPath string, failing bool) []verifyRow {
	t.Helper()
	enc2 := ciphertest.Cipher(t, ciphertest.Ring{Active: "enc2", IDs: []string{"enc2"}, Keys: map[string][]byte{"enc2": verifyEnc2Key}})
	good, err := enc2.Encrypt("cmd-verify-synthetic-good")
	require.NoError(t, err)
	rows := []verifyRow{
		{id: verifyIDGood, key: verifyKeyPrefix + "GOOD", value: good},
		{id: verifyIDRef, key: verifyKeyPrefix + "REF", ref: "projects/x/secrets/cmd-verify/versions/1"},
	}
	if !failing {
		return rows
	}
	k, err := keyring.LoadFile(keyringPath)
	require.NoError(t, err)
	v1Writer, err := secret.NewValueCipher(k)
	require.NoError(t, err)
	v1, err := v1Writer.Encrypt("cmd-verify-synthetic-v1")
	require.NoError(t, err)
	// Sealed under the legacy key, labelled with the allowed enc2.
	other := ciphertest.Cipher(t, ciphertest.Ring{Active: "enc2", IDs: []string{"enc2"}, Keys: map[string][]byte{"enc2": keyring.DeriveLegacyEncryptionKey(cmdKRLegacySecret)}})
	lie, err := other.Encrypt("cmd-verify-synthetic-lie")
	require.NoError(t, err)
	return append(rows,
		verifyRow{id: verifyIDPlain, key: verifyKeyPrefix + "PLAIN", value: "cmd-verify-synthetic-plaintext"},
		verifyRow{id: verifyIDV1, key: verifyKeyPrefix + "V1", value: v1},
		verifyRow{id: verifyIDLie, key: verifyKeyPrefix + "LIE", value: lie},
		verifyRow{id: verifyIDEmpty, key: verifyKeyPrefix + "EMPTY"},
	)
}

// verifyDBFixture creates a migrated SQLite hub database at a file path
// holding rows and, with policy, an encryption policy allowing enc2, and
// closes it. It returns the path.
func verifyDBFixture(t *testing.T, home string, rows []verifyRow, policy bool) string {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(home, ".scion", "keys-verify.db")
	client, err := entc.OpenSQLite("file:"+dbPath, entc.PoolConfig{})
	require.NoError(t, err)
	cs := entadapter.NewCompositeStore(client)
	require.NoError(t, migrateHubStore(ctx, cs, false))
	for _, r := range rows {
		_, err := cs.UpsertSecret(ctx, &store.Secret{ID: r.id, Key: r.key, EncryptedValue: r.value, SecretRef: r.ref,
			Scope: store.ScopeUser, ScopeID: "cmd-verify-user", SecretType: store.SecretTypeEnvironment})
		require.NoError(t, err)
	}
	if policy {
		applied, err := cs.UpdateEncryptionPolicy(ctx, 0, store.EncryptionPolicy{
			Generation: 1, MinReaderVersion: store.EncryptionPolicyMinReaderVersion,
			AllowedWriteKIDs: []string{"enc2"}, RequiredReadKIDs: []string{"enc2"}, State: store.EncryptionPolicyPrepared,
		})
		require.NoError(t, err)
		require.True(t, applied)
	}
	require.NoError(t, cs.Close())
	return dbPath
}

// assertNoVerifyIdentities checks that s names no record of the fixtures.
func assertNoVerifyIdentities(t *testing.T, s string) {
	t.Helper()
	for _, id := range []string{verifyIDGood, verifyIDRef, verifyIDPlain, verifyIDV1, verifyIDLie, verifyIDEmpty, verifyKeyPrefix, "cmd-verify-synthetic", "cmd-verify-user"} {
		assert.NotContains(t, s, id)
	}
	assertNoCmdKRSecret(t, s)
}

// Every payload row authenticates in v2 under the policy's allowed kid:
// exit 0, counts only.
func TestServerKeysVerify_AllAllowedIsExit0(t *testing.T) {
	home := keysTestEnv(t)
	path := verifyKeyring(t)
	dbPath := verifyDBFixture(t, home, verifyRows(t, path, false), true)

	out, code, err := runKeys(t, "verify", "--keyring-file", path, "--db", dbPath)
	require.NoError(t, err, out)
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "Allowed writer key IDs (policy): [enc2]")
	assert.Contains(t, out, "Scanned 2: 2 passed (1 reference-only, not decrypted), 0 failed")
	assert.Contains(t, out, "Verify: OK")
	assertNoVerifyIdentities(t, out)
}

// Plaintext, v1, a kid-label lie and an empty row without a reference all
// fail: exit 1, aggregate counts on stdout, identities only in the
// private report file.
func TestServerKeysVerify_FailuresAreExit1WithPrivateReport(t *testing.T) {
	home := keysTestEnv(t)
	path := verifyKeyring(t)
	dbPath := verifyDBFixture(t, home, verifyRows(t, path, true), true)
	reportPath := filepath.Join(t.TempDir(), "verify-report.jsonl")
	outputFormat = "json"

	out, code, err := runKeys(t, "verify", "--keyring-file", path, "--db", dbPath, "--report-file", reportPath)
	require.Error(t, err)
	assert.Equal(t, keysExitFailure, code)
	assertNoVerifyIdentities(t, out+err.Error())

	var got hub.EncryptionVerifyReport
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.False(t, got.OK)
	assert.Equal(t, 6, got.Scanned)
	assert.Equal(t, 2, got.Passed)
	assert.Equal(t, 4, got.Failed)
	assert.Equal(t, 1, got.EmptyNoRef)
	assert.Equal(t, map[string]int{secret.FailurePlaintextNotAdmitted: 2, "v1": 1, secret.FailureDecrypt: 1}, got.FailedByReason)

	info, err := os.Stat(reportPath)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	f, err := os.Open(reportPath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	reasons := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec hub.VerifyFailure
		require.NoError(t, json.Unmarshal(sc.Bytes(), &rec))
		reasons[rec.ID] = rec.Reason
		assert.True(t, strings.HasPrefix(rec.Key, verifyKeyPrefix))
		assert.NotContains(t, sc.Text(), "cmd-verify-synthetic", "the report never holds a payload")
	}
	require.NoError(t, sc.Err())
	assert.Equal(t, map[string]string{
		verifyIDPlain: secret.FailurePlaintextNotAdmitted,
		verifyIDV1:    "v1",
		verifyIDLie:   secret.FailureDecrypt,
		verifyIDEmpty: secret.FailurePlaintextNotAdmitted,
	}, reasons)
}

// --report-file never overwrites: an existing file and a symlink (even a
// dangling one) are refused with exit 2, before the database is opened,
// and left as they were.
func TestServerKeysVerify_ReportFileRefusesExistingAndSymlink(t *testing.T) {
	home := keysTestEnv(t)
	path := verifyKeyring(t)
	dbPath := verifyDBFixture(t, home, verifyRows(t, path, true), true)
	dir := t.TempDir()

	existing := filepath.Join(dir, "existing.jsonl")
	require.NoError(t, os.WriteFile(existing, []byte("keep"), 0o600))
	_, code, err := runKeys(t, "verify", "--keyring-file", path, "--db", dbPath, "--report-file", existing)
	require.Error(t, err)
	assert.Equal(t, keysExitInvalid, code)
	assert.Contains(t, err.Error(), "already exists")
	b, err := os.ReadFile(existing)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(b))

	if runtime.GOOS == "windows" {
		return
	}
	target := filepath.Join(dir, "target.jsonl")
	link := filepath.Join(dir, "link.jsonl")
	require.NoError(t, os.Symlink(target, link))
	_, code, err = runKeys(t, "verify", "--keyring-file", path, "--db", dbPath, "--report-file", link)
	require.Error(t, err)
	assert.Equal(t, keysExitInvalid, code)
	assert.Contains(t, err.Error(), "symlink")
	_, statErr := os.Lstat(target)
	assert.True(t, os.IsNotExist(statErr), "the symlink target must not be created")

	// The create itself refuses a link (O_EXCL, no follow), not only the
	// pre-check: a link planted after the check is refused too.
	race := filepath.Join(dir, "race.jsonl")
	require.NoError(t, os.Symlink(target, race))
	_, err = createVerifyReportFileUnchecked(race)
	require.Error(t, err)
	_, statErr = os.Lstat(target)
	assert.True(t, os.IsNotExist(statErr), "the symlink target must not be created")
}

// createVerifyReportFileUnchecked is createVerifyReportFile's create step
// without its Lstat pre-check.
func createVerifyReportFileUnchecked(path string) (*verifyReportFile, error) {
	return createVerifyReportFileWith(path, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })
}

// A failed scan leaves no report file behind.
func TestServerKeysVerify_ReportFileRemovedOnFailure(t *testing.T) {
	home := keysTestEnv(t)
	reportPath := filepath.Join(t.TempDir(), "verify-report.jsonl")
	dbPath := filepath.Join(home, ".scion", "absent.db")
	_, code, err := runKeys(t, "verify", "--keyring-file", verifyKeyring(t), "--db", dbPath, "--report-file", reportPath)
	require.Error(t, err)
	assert.Equal(t, keysExitFailure, code)
	_, statErr := os.Lstat(reportPath)
	assert.True(t, os.IsNotExist(statErr), "an unfinished report file is removed")
	_, statErr = os.Stat(dbPath)
	assert.True(t, os.IsNotExist(statErr), "the read-only open must not create the database")
}

// Without a policy the keyring file (write_format v1) allows nothing, so
// a payload row fails; legacy mode (no keyring file) likewise.
func TestServerKeysVerify_NoPolicyAllowedSet(t *testing.T) {
	t.Run("keyring file writing v1", func(t *testing.T) {
		home := keysTestEnv(t)
		path := verifyKeyring(t)
		dbPath := verifyDBFixture(t, home, verifyRows(t, path, false), false)
		out, code, err := runKeys(t, "verify", "--keyring-file", path, "--db", dbPath)
		require.Error(t, err)
		assert.Equal(t, keysExitFailure, code)
		assert.Contains(t, out, "Allowed writer key IDs (none): []")
		assert.Contains(t, out, "v2_kid_not_allowed=1")
		assert.Contains(t, out, "Verify: FAILED")
	})
	t.Run("legacy mode", func(t *testing.T) {
		home := keysTestEnv(t)
		dbPath := verifyDBFixture(t, home, verifyRows(t, verifyKeyring(t), false), false)
		out, code, err := runKeys(t, "verify", "--db", dbPath)
		require.Error(t, err)
		assert.Equal(t, keysExitFailure, code)
		assert.Contains(t, out, "Allowed writer key IDs (none): []")
		assert.Contains(t, out, "legacy mode (no keyring file)")
	})
}

// writeCountingDriver counts every statement that could write: Exec
// calls (outside or inside a transaction) and commits.
type writeCountingDriver struct {
	dialect.Driver
	mu             sync.Mutex
	execs, commits int
}

func (d *writeCountingDriver) Exec(ctx context.Context, q string, args, v any) error {
	d.mu.Lock()
	d.execs++
	d.mu.Unlock()
	return d.Driver.Exec(ctx, q, args, v)
}

func (d *writeCountingDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	tx, err := d.Driver.Tx(ctx)
	if err != nil {
		return nil, err
	}
	return &writeCountingTx{Tx: tx, d: d}, nil
}

type writeCountingTx struct {
	dialect.Tx
	d *writeCountingDriver
}

func (t *writeCountingTx) Exec(ctx context.Context, q string, args, v any) error {
	t.d.mu.Lock()
	t.d.execs++
	t.d.mu.Unlock()
	return t.Tx.Exec(ctx, q, args, v)
}

func (t *writeCountingTx) Commit() error {
	t.d.mu.Lock()
	t.d.commits++
	t.d.mu.Unlock()
	return t.Tx.Commit()
}

// Verify opens the database strictly read only and never writes: through
// the client verify itself opens, no statement that could write is issued
// and nothing is committed; the database file is byte for byte unchanged;
// and a write through a second client opened the same read-only way fails.
func TestServerKeysVerify_NeverWrites(t *testing.T) {
	home := keysTestEnv(t)
	path := verifyKeyring(t)
	dbPath := verifyDBFixture(t, home, verifyRows(t, path, true), true)
	before := fileState(t, dbPath)

	counter := &writeCountingDriver{}
	var opened *ent.Client
	orig := openPreflightClient
	t.Cleanup(func() { openPreflightClient = orig })
	openPreflightClient = func(driver, dsn string) (*ent.Client, error) {
		c, err := orig(driver, dsn)
		if err != nil {
			return nil, err
		}
		opened = c
		counter.Driver = c.Driver()
		return ent.NewClient(ent.Driver(counter)), nil
	}

	_, code, err := runKeys(t, "verify", "--keyring-file", path, "--db", dbPath)
	require.Error(t, err)
	assert.Equal(t, keysExitFailure, code)
	require.NotNil(t, opened)
	assert.Zero(t, counter.execs, "verify issues no statement that could write")
	assert.Zero(t, counter.commits, "verify commits nothing")
	assert.Equal(t, before, fileState(t, dbPath), "the database file is unchanged")

	ro, err := orig("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = ro.Close() }()
	_, err = entadapter.NewCompositeStore(ro).UpsertSecret(context.Background(), &store.Secret{
		ID: "7c1e3d2a-6b5c-4d4e-9f30-00000000c0ff", Key: verifyKeyPrefix + "WRITE", Scope: store.ScopeUser, ScopeID: "cmd-verify-user",
		SecretType: store.SecretTypeEnvironment, SecretRef: "projects/x/secrets/w/versions/1"})
	require.Error(t, err, "a write through the read-only client must fail")
}

// The text summary names no record identifier even when it lists the
// report path.
func TestPrintKeysVerify_CountsOnly(t *testing.T) {
	var b strings.Builder
	printKeysVerify(&b, hub.EncryptionVerifyReport{
		AllowedSource: hub.VerifyAllowedFromPolicy, AllowedWriteKIDs: []string{"enc2"}, Scanned: 3, Passed: 1, Failed: 2,
		FailedByReason: map[string]int{"v1": 2},
		Failures:       []hub.VerifyFailure{{ID: verifyIDV1, Key: verifyKeyPrefix + "V1", Reason: "v1"}},
	}, "/tmp/report.jsonl")
	out := b.String()
	assert.Contains(t, out, "failed by reason: v1=2")
	assert.Contains(t, out, "Failing record identifiers: 1 written to /tmp/report.jsonl")
	assert.Contains(t, out, "Verify: FAILED")
	assertNoVerifyIdentities(t, out)
}
