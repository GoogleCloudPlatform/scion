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

package hub

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/ciphertest"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/secretenvelope"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// The authenticating payload scan (encryption_scan.go) and verify, on
// SQLite and on Postgres (the Postgres leg runs under -tags integration
// with SCION_TEST_POSTGRES_URL set, and skips otherwise). Every key is a
// synthetic ciphertest fixture.

// scanBackends opens a fresh, migrated store per backend.
var scanBackends = map[string]func(t *testing.T) store.Store{
	"sqlite": encStore,
	"postgres": func(t *testing.T) store.Store {
		dsn := enttest.NewSchemaURL(t) // skips without the Postgres backend
		client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 4, MaxIdleConns: 2})
		require.NoError(t, err)
		cs := entadapter.NewCompositeStore(client)
		t.Cleanup(func() { _ = cs.Close() })
		return cs
	},
}

// scanRing is the ring under test: v2 writer k1, also holding k2 and the
// legacy key as the only v1 reader.
func scanRing(t *testing.T) *secret.ValueCipher {
	return ciphertest.Cipher(t, ciphertest.Ring{Active: "k1", IDs: []string{"k1", "k2", keyring.LegacyID},
		WriteFormat: keyring.WriteV2, V1Readers: []string{keyring.LegacyID}})
}

// sealV2With seals plaintext under keyID's fake key with the AAD of
// aadKID and labels the envelope labelKID.
func sealV2With(t *testing.T, keyID, aadKID, labelKID, plaintext string) string {
	t.Helper()
	block, err := aes.NewCipher(ciphertest.FakeKey(keyID))
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := []byte("FAKE-nonce12")
	return secretenvelope.FormatV2Value(labelKID, gcm.Seal(nonce, nonce, []byte(plaintext), secretenvelope.AADV2(aadKID)))
}

// scanFixtureRow is one planted row and what the scan and verify must
// conclude about it.
type scanFixtureRow struct {
	name          string
	key, scope    string
	value, ref    string
	format, kid   string // the authenticated classification; "" when unreadable
	failure       string // the scan failure kind; "" when readable
	verifyReason  string // "" when verify passes it
	referenceOnly bool
	emptyNoRef    bool
}

const scanTestScopeID = "scan-test-scope"

// scanFixture plants one row per case, in ascending ID order.
func scanFixture(t *testing.T) []scanFixtureRow {
	t.Helper()
	c := scanRing(t)
	enc := func(c *secret.ValueCipher, v string) string {
		out, err := c.Encrypt(v)
		require.NoError(t, err)
		return out
	}
	k2Writer := ciphertest.Cipher(t, ciphertest.Ring{Active: "k2", IDs: []string{"k2"}})
	legacyV1 := ciphertest.Legacy()
	foreignV1 := secret.NewLegacyValueCipher("test-only-scan-foreign-secret")
	goodK2 := enc(k2Writer, "scan-synthetic-k2")
	raw, err := base64.StdEncoding.DecodeString(strings.SplitN(goodK2, ":", 4)[3])
	require.NoError(t, err)
	tampered := append([]byte{}, raw...)
	tampered[len(tampered)-1] ^= 0x01
	signingKey, _ := randomB64Key(t)

	return []scanFixtureRow{
		{name: "v2 under an allowed kid", value: enc(c, "scan-synthetic-good"), format: "v2", kid: "k1"},
		{name: "v2 under a held kid outside the allowed set", value: goodK2, format: "v2", kid: "k2", verifyReason: verifyReasonKIDNotAllowed},
		{name: "kid-label lie (sealed under k2 with k1's AAD, labelled k1)", value: sealV2With(t, "k2", "k1", "k1", "scan-synthetic-lie"),
			failure: secret.FailureDecrypt, verifyReason: secret.FailureDecrypt},
		{name: "relabelled (sealed under k2, labelled k1)", value: strings.Replace(goodK2, "enc:v2:k2:", "enc:v2:k1:", 1),
			failure: secret.FailureDecrypt, verifyReason: secret.FailureDecrypt},
		{name: "tampered tag", value: secretenvelope.FormatV2Value("k2", tampered), failure: secret.FailureDecrypt, verifyReason: secret.FailureDecrypt},
		{name: "truncated envelope", value: "enc:v2:k1:" + base64.StdEncoding.EncodeToString(raw[:10]),
			failure: secret.FailureMalformed, verifyReason: secret.FailureMalformed},
		{name: "plaintext", value: "scan-synthetic-plaintext", failure: secret.FailurePlaintextNotAdmitted, verifyReason: secret.FailurePlaintextNotAdmitted},
		{name: "wrong-key v1", value: enc(foreignV1, "scan-synthetic-foreign"), failure: secret.FailureDecrypt, verifyReason: secret.FailureDecrypt},
		{name: "v1 under the legacy reader", value: enc(legacyV1, "scan-synthetic-v1"), format: "v1", kid: keyring.LegacyID, verifyReason: verifyReasonV1},
		{name: "unknown kid label", value: strings.Replace(goodK2, "enc:v2:k2:", "enc:v2:k9:", 1),
			failure: secret.FailureUnknownKeyID, verifyReason: secret.FailureUnknownKeyID},
		{name: "reference-only", ref: "projects/x/secrets/scan-synthetic/versions/1", referenceOnly: true},
		{name: "empty value and empty ref", emptyNoRef: true, failure: secret.FailurePlaintextNotAdmitted, verifyReason: secret.FailurePlaintextNotAdmitted},
		{name: "invalid infrastructure backup", key: SecretKeyAgentSigningKey, scope: store.ScopeHub, value: enc(c, "not base64!"),
			failure: auditKindInvalidPayload, verifyReason: auditKindInvalidPayload},
		{name: "valid infrastructure backup", key: SecretKeyUserSigningKey, scope: store.ScopeHub, value: enc(c, signingKey), format: "v2", kid: "k1"},
	}
}

func scanRowID(i int) string { return fmt.Sprintf("5ca10000-0000-4000-8000-%012d", i+1) }

// seedScanFixture writes the rows (before any policy, which would fence
// the non-v2 ones) and returns them.
func seedScanFixture(t *testing.T, st store.Store) []scanFixtureRow {
	t.Helper()
	rows := scanFixture(t)
	for i, r := range rows {
		key, scope := r.key, r.scope
		if key == "" {
			key, scope = fmt.Sprintf("SCAN_ROW_%02d", i), store.ScopeHub
		}
		secretType := store.SecretTypeEnvironment
		if r.key != "" {
			secretType = store.SecretTypeInternal
		}
		_, err := st.UpsertSecret(context.Background(), &store.Secret{ID: scanRowID(i), Key: key, EncryptedValue: r.value, SecretRef: r.ref,
			Scope: scope, ScopeID: scanTestScopeID, SecretType: secretType})
		require.NoError(t, err, r.name)
	}
	return rows
}

// countingDecrypter counts every value passed to DecryptAuthenticated.
type countingDecrypter struct {
	d     payloadDecrypter
	calls int
}

func (c *countingDecrypter) DecryptAuthenticated(v string) (string, secret.Authentication, error) {
	c.calls++
	return c.d.DecryptAuthenticated(v)
}

// Acceptance: every planted row gets the classification of the key that
// actually authenticated it; a kid-label lie, a tampered tag, a truncated
// envelope, plaintext and wrong-key v1 are failures; the scan sees exactly
// the rows inserted, and reference-only rows never reach a decrypt.
func TestEncryptionScan_ClassifiesByAuthenticatingKey(t *testing.T) {
	for backend, open := range scanBackends {
		t.Run(backend, func(t *testing.T) {
			st := open(t)
			rows := seedScanFixture(t, st)
			dec := &countingDecrypter{d: scanRing(t)}
			got := map[string]ScanRow{}
			var order []string
			n, err := scanSecretPayloads(context.Background(), st, dec, func(r ScanRow) error {
				got[r.ID] = r
				order = append(order, r.ID)
				return nil
			})
			require.NoError(t, err)
			assert.Equal(t, len(rows), n, "rows scanned == rows inserted")
			require.Len(t, got, len(rows), "every inserted row is visited once")

			refOnly := 0
			for i, want := range rows {
				r, ok := got[scanRowID(i)]
				require.True(t, ok, want.name)
				assert.Equal(t, scanRowID(i), order[i], "ascending ID order")
				cl := r.Class
				assert.Equal(t, want.referenceOnly, cl.ReferenceOnly, want.name)
				assert.Equal(t, want.emptyNoRef, cl.EmptyNoRef, want.name)
				assert.Equal(t, want.failure, cl.Failure, want.name)
				assert.Equal(t, want.format, cl.Format, want.name)
				assert.Equal(t, want.kid, cl.KID, want.name)
				assert.Equal(t, want.failure == "" && !want.referenceOnly, cl.Readable(), want.name)
				if want.referenceOnly {
					refOnly++
				}
			}
			require.Positive(t, refOnly)
			assert.Equal(t, len(rows)-refOnly, dec.calls, "reference-only rows are never passed to decrypt; every other row is")
		})
	}
}

// Decrypt-call counter: a store of reference-only rows only is scanned
// with no decrypt at all.
func TestEncryptionScan_ReferenceOnlyRowsNeverDecrypted(t *testing.T) {
	st := encStore(t)
	for i := range 3 {
		_, err := st.UpsertSecret(context.Background(), &store.Secret{ID: scanRowID(i), Key: fmt.Sprintf("SCAN_REF_%d", i),
			SecretRef: "projects/x/secrets/scan-ref/versions/1", Scope: store.ScopeHub, ScopeID: scanTestScopeID, SecretType: store.SecretTypeEnvironment})
		require.NoError(t, err)
	}
	dec := &countingDecrypter{d: scanRing(t)}
	n, err := scanSecretPayloads(context.Background(), st, dec, func(r ScanRow) error {
		assert.True(t, r.Class.ReferenceOnly)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Zero(t, dec.calls, "no reference-only row reaches a decrypt")
}

// scanPageCountingStore counts ScanSecretPayloads calls and checks each limit.
type scanPageCountingStore struct {
	store.SecretStore
	pages  int
	limits []int
}

func (s *scanPageCountingStore) ScanSecretPayloads(ctx context.Context, after string, limit int) ([]store.SecretPayload, error) {
	s.pages++
	s.limits = append(s.limits, limit)
	return s.SecretStore.ScanSecretPayloads(ctx, after, limit)
}

// Input count across page boundaries: pages of store.MaxSecretPayloadScan
// (500), every row visited exactly once, including an exact multiple of
// the page size.
func TestEncryptionScan_PagesCoverEveryRow(t *testing.T) {
	c := scanRing(t)
	sealed, err := c.Encrypt("scan-synthetic-paged")
	require.NoError(t, err)
	for backend, open := range scanBackends {
		for _, total := range []int{2*store.MaxSecretPayloadScan + 1, 2 * store.MaxSecretPayloadScan} {
			t.Run(fmt.Sprintf("%s/%d", backend, total), func(t *testing.T) {
				st := open(t)
				for i := range total {
					_, err := st.UpsertSecret(context.Background(), &store.Secret{ID: scanRowID(i), Key: fmt.Sprintf("SCAN_PAGE_%05d", i),
						EncryptedValue: sealed, Scope: store.ScopeHub, ScopeID: scanTestScopeID, SecretType: store.SecretTypeEnvironment})
					require.NoError(t, err)
				}
				ps := &scanPageCountingStore{SecretStore: st}
				dec := &countingDecrypter{d: c}
				seen := map[string]bool{}
				n, err := scanSecretPayloads(context.Background(), ps, dec, func(r ScanRow) error {
					assert.False(t, seen[r.ID], "row visited twice")
					seen[r.ID] = true
					assert.True(t, r.Class.Readable())
					return nil
				})
				require.NoError(t, err)
				assert.Equal(t, total, n, "rows scanned == rows inserted")
				assert.Len(t, seen, total)
				assert.Equal(t, total, dec.calls)
				assert.Equal(t, total/store.MaxSecretPayloadScan+1, ps.pages)
				for _, l := range ps.limits {
					assert.Equal(t, store.MaxSecretPayloadScan, l)
				}
			})
		}
	}
}

// fakeScanStore returns fixed pages.
type fakeScanStore struct {
	store.SecretStore
	pages [][]store.SecretPayload
	err   error
}

func (s *fakeScanStore) ScanSecretPayloads(context.Context, string, int) ([]store.SecretPayload, error) {
	if s.err != nil {
		return nil, s.err
	}
	if len(s.pages) == 0 {
		return nil, nil
	}
	p := s.pages[0]
	s.pages = s.pages[1:]
	return p, nil
}

// A store error, a page out of ID order and a visitor error stop the scan
// and are returned.
func TestEncryptionScan_Errors(t *testing.T) {
	c := scanRing(t)
	ctx := context.Background()
	visitNone := func(ScanRow) error { return nil }

	_, err := scanSecretPayloads(ctx, &fakeScanStore{err: errors.New("db down")}, c, visitNone)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db down")

	outOfOrder := &fakeScanStore{pages: [][]store.SecretPayload{{{ID: "b"}, {ID: "a"}}}}
	_, err = scanSecretPayloads(ctx, outOfOrder, c, visitNone)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of ID order")

	stop := errors.New("visitor stop")
	_, err = scanSecretPayloads(ctx, &fakeScanStore{pages: [][]store.SecretPayload{{{ID: "a", SecretRef: "r"}}}}, c,
		func(ScanRow) error { return stop })
	require.ErrorIs(t, err, stop)
}

// The startup audit aggregates the scan: formats and key IDs of readable
// rows by the authenticating key only, so a label lie never counts under
// its label.
func TestEncryptionAudit_CountsAuthenticatedKeysOnly(t *testing.T) {
	for backend, open := range scanBackends {
		t.Run(backend, func(t *testing.T) {
			st := open(t)
			rows := seedScanFixture(t, st)
			a, err := runEncryptionAudit(context.Background(), st, scanRing(t))
			require.NoError(t, err)
			assert.Equal(t, len(rows), a.Scanned)
			assert.Equal(t, 1, a.ReferenceOnly)
			assert.Equal(t, 1, a.EmptyNoRef)
			assert.Equal(t, 4, a.Readable)
			assert.Equal(t, map[string]int{"v2": 3, "v1": 1}, a.ByFormat)
			assert.Equal(t, map[string]int{"k1": 2, "k2": 1}, a.ByKID)
			kinds := map[string]int{}
			for _, r := range a.Unreadable {
				kinds[r.Kind]++
			}
			assert.Equal(t, map[string]int{secret.FailureDecrypt: 4, secret.FailureMalformed: 1, secret.FailurePlaintextNotAdmitted: 2,
				secret.FailureUnknownKeyID: 1, auditKindInvalidPayload: 1}, kinds)
			require.Len(t, a.BackupUnreadable, 1)
			assert.Equal(t, SecretKeyAgentSigningKey, a.BackupUnreadable[0].Key)
		})
	}
}

// Verify fails every row that is not a readable enc:v2 value under an
// allowed (policy) kid, the planted kid-label lie included, and lists
// only identities.
func TestVerifyEncryption_PolicyAllowedSet(t *testing.T) {
	for backend, open := range scanBackends {
		t.Run(backend, func(t *testing.T) {
			st := open(t)
			rows := seedScanFixture(t, st)
			ksSeedPolicy(t, st, "k1")
			r, err := VerifyEncryption(context.Background(), st, scanRing(t))
			require.NoError(t, err)
			assert.False(t, r.OK)
			assert.Equal(t, VerifyAllowedFromPolicy, r.AllowedSource)
			assert.Equal(t, []string{"k1"}, r.AllowedWriteKIDs)
			assert.Equal(t, len(rows), r.Scanned)
			assert.Equal(t, 1, r.ReferenceOnly)
			assert.Equal(t, 1, r.EmptyNoRef)

			wantReasons := map[string]int{}
			wantFailed := map[string]string{}
			for i, row := range rows {
				if row.verifyReason != "" {
					wantReasons[row.verifyReason]++
					wantFailed[scanRowID(i)] = row.verifyReason
				}
			}
			assert.Equal(t, len(rows)-len(wantFailed), r.Passed)
			assert.Equal(t, len(wantFailed), r.Failed)
			assert.Equal(t, wantReasons, r.FailedByReason)
			gotFailed := map[string]string{}
			for _, f := range r.Failures {
				gotFailed[f.ID] = f.Reason
				assert.Equal(t, scanTestScopeID, f.ScopeID)
			}
			assert.Equal(t, wantFailed, gotFailed)
			// The label lie is a failure even though its label is allowed.
			assert.Equal(t, secret.FailureDecrypt, gotFailed[scanRowID(2)])
		})
	}
}

// Verify passes only when every payload row is enc:v2 under an allowed
// kid; reference-only rows pass without a decrypt.
func TestVerifyEncryption_AllV2Passes(t *testing.T) {
	for backend, open := range scanBackends {
		t.Run(backend, func(t *testing.T) {
			st := open(t)
			c := scanRing(t)
			sealed, err := c.Encrypt("scan-synthetic-ok")
			require.NoError(t, err)
			for i, s := range []*store.Secret{{EncryptedValue: sealed}, {SecretRef: "projects/x/secrets/ok/versions/1"}} {
				s.ID, s.Key, s.Scope, s.ScopeID, s.SecretType = scanRowID(i), fmt.Sprintf("SCAN_OK_%d", i), store.ScopeHub, scanTestScopeID, store.SecretTypeEnvironment
				_, err := st.UpsertSecret(context.Background(), s)
				require.NoError(t, err)
			}
			ksSeedPolicy(t, st, "k1")
			r, err := VerifyEncryption(context.Background(), st, c)
			require.NoError(t, err)
			assert.True(t, r.OK)
			assert.Equal(t, 2, r.Scanned)
			assert.Equal(t, 2, r.Passed)
			assert.Equal(t, 1, r.ReferenceOnly)
			assert.Zero(t, r.Failed)
			assert.Empty(t, r.Failures)
		})
	}
}

// Without a policy the allowed set is the keyring file's v2 writer; a v1
// writer or a legacy (no keyring file) cipher allows nothing.
func TestVerifyEncryption_NoPolicyAllowedSet(t *testing.T) {
	ctx := context.Background()
	st := encStore(t)
	writer := scanRing(t)
	sealed, err := writer.Encrypt("scan-synthetic-nopolicy")
	require.NoError(t, err)
	_, err = st.UpsertSecret(ctx, &store.Secret{ID: scanRowID(0), Key: "SCAN_NOPOLICY", EncryptedValue: sealed,
		Scope: store.ScopeHub, ScopeID: scanTestScopeID, SecretType: store.SecretTypeEnvironment})
	require.NoError(t, err)

	r, err := VerifyEncryption(ctx, st, writer)
	require.NoError(t, err)
	assert.True(t, r.OK)
	assert.Equal(t, VerifyAllowedFromKeyring, r.AllowedSource)
	assert.Equal(t, []string{"k1"}, r.AllowedWriteKIDs)

	v1Writer := ciphertest.Cipher(t, ciphertest.Ring{Active: keyring.LegacyID, IDs: []string{keyring.LegacyID, "k1"},
		WriteFormat: keyring.WriteV1, V1Readers: []string{keyring.LegacyID}})
	r, err = VerifyEncryption(ctx, st, v1Writer)
	require.NoError(t, err)
	assert.False(t, r.OK, "a readable v2 row fails when nothing is allowed")
	assert.Equal(t, VerifyAllowedNone, r.AllowedSource)
	assert.Empty(t, r.AllowedWriteKIDs)
	assert.Equal(t, map[string]int{verifyReasonKIDNotAllowed: 1}, r.FailedByReason)
}

// Legacy mode reads plaintext, but verify still fails it, an empty
// value without a reference included.
func TestVerifyEncryption_LegacyModePlaintextFails(t *testing.T) {
	ctx := context.Background()
	st := encStore(t)
	for i, v := range []string{"scan-synthetic-legacy-plaintext", ""} {
		_, err := st.UpsertSecret(ctx, &store.Secret{ID: scanRowID(i), Key: fmt.Sprintf("SCAN_LEGACY_%d", i), EncryptedValue: v,
			Scope: store.ScopeHub, ScopeID: scanTestScopeID, SecretType: store.SecretTypeEnvironment})
		require.NoError(t, err)
	}
	r, err := VerifyEncryption(ctx, st, ciphertest.Legacy())
	require.NoError(t, err)
	assert.False(t, r.OK)
	assert.Equal(t, VerifyAllowedNone, r.AllowedSource)
	assert.Equal(t, 1, r.EmptyNoRef)
	assert.Equal(t, map[string]int{verifyReasonPlaintext: 2}, r.FailedByReason)

	a, err := runEncryptionAudit(ctx, st, ciphertest.Legacy())
	require.NoError(t, err)
	assert.Equal(t, 2, a.Readable, "legacy mode reads plaintext")
	assert.Equal(t, map[string]int{"plaintext": 2}, a.ByFormat)
	assert.Equal(t, 1, a.EmptyNoRef)
}
