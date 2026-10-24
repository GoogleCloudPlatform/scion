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

package secret_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/ciphertest"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/secretenvelope"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Golden fixtures (synthetic keys from ciphertest, fixed fake nonce).
const (
	goldenPlaintext = "fake-plaintext-value"
	goldenNonce     = "FAKE-nonce12"
	// goldenV1 is Encrypt by the legacy cipher over
	// ciphertest.FakeSigningSecret.
	goldenV1 = "enc:v1:RkFLRS1ub25jZTEyJQrCUqxblXw8+RsT+Z3YN5TKtyz4oIWliAdBBAK336IUkkjk"
	// goldenV2 is Encrypt by a v2 ring whose active key is
	// ciphertest.FakeKey("k2").
	goldenV2 = "enc:v2:k2:RkFLRS1ub25jZTEya1yTxtfmjOZFexEKI/+SL/7TIQ9JYbNjs6vDWrZ2lIvHNUei"
)

func v2Ring(active string, ids ...string) ciphertest.Ring {
	return ciphertest.Ring{Active: active, IDs: ids, WriteFormat: keyring.WriteV2}
}

func TestValueCipher_GoldenV1(t *testing.T) {
	c := secret.WithFixedNonceForTest(ciphertest.Legacy(), []byte(goldenNonce))
	got, err := c.Encrypt(goldenPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenV1 {
		t.Fatalf("v1 golden mismatch:\n got %s\nwant %s", got, goldenV1)
	}
	// Byte compatibility with the legacy helpers, both directions.
	legacyKey := secret.DeriveLocalEncryptionKey(ciphertest.FakeSigningSecret)
	pt, isLegacy, err := secret.DecryptValue(goldenV1, legacyKey)
	if err != nil || isLegacy || pt != goldenPlaintext {
		t.Fatalf("DecryptValue(golden v1) = %q, %v, %v", pt, isLegacy, err)
	}
	old, err := secret.EncryptValue(goldenPlaintext, legacyKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{goldenV1, old} {
		if pt, err := ciphertest.Legacy().Decrypt(v); err != nil || pt != goldenPlaintext {
			t.Fatalf("legacy cipher Decrypt = %q, %v", pt, err)
		}
	}
	// A managed ring that imports the legacy key as a v1 reader reads it.
	m := ciphertest.Cipher(t, ciphertest.Ring{Active: "k2", IDs: []string{keyring.LegacyID, "k2"}, V1Readers: []string{keyring.LegacyID}})
	if pt, err := m.Decrypt(goldenV1); err != nil || pt != goldenPlaintext {
		t.Fatalf("managed v1 reader Decrypt = %q, %v", pt, err)
	}
}

func TestValueCipher_GoldenV2(t *testing.T) {
	c := secret.WithFixedNonceForTest(ciphertest.Cipher(t, v2Ring("k2", "k2")), []byte(goldenNonce))
	got, err := c.Encrypt(goldenPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenV2 {
		t.Fatalf("v2 golden mismatch:\n got %s\nwant %s", got, goldenV2)
	}
	// Independent check of the wire format: AES-256-GCM, 12-byte nonce
	// prefix, AAD "scion-secret-v2:" + kid, standard padded base64.
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(goldenV2, "enc:v2:k2:"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(ciphertest.FakeKey("k2"))
	gcm, _ := cipher.NewGCM(block)
	pt, err := gcm.Open(nil, raw[:12], raw[12:], []byte("scion-secret-v2:k2"))
	if err != nil || string(pt) != goldenPlaintext {
		t.Fatalf("independent open = %q, %v", pt, err)
	}
	if !bytes.Equal(raw[:12], []byte(goldenNonce)) {
		t.Fatal("nonce is not the prefix")
	}
	if pt, err := ciphertest.Cipher(t, v2Ring("k3", "k3", "k2")).Decrypt(goldenV2); err != nil || pt != goldenPlaintext {
		t.Fatalf("retained reader Decrypt = %q, %v", pt, err)
	}
}

func TestValueCipher_RoundTripFreshNonces(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *secret.ValueCipher
	}{
		{"legacy v1", ciphertest.Legacy()},
		{"managed v1", ciphertest.Cipher(t, ciphertest.Ring{Active: "k1", IDs: []string{"k1"}, WriteFormat: keyring.WriteV1, V1Readers: []string{"k1"}})},
		{"managed v2", ciphertest.Cipher(t, v2Ring("k1", "k1"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, pt := range []string{"", "x", "fake multi\nline value", strings.Repeat("z", 5000)} {
				a, err := tc.c.Encrypt(pt)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := tc.c.Encrypt(pt)
				if a == b {
					t.Fatal("two encryptions share a nonce")
				}
				if got, err := tc.c.Decrypt(a); err != nil || got != pt {
					t.Fatalf("Decrypt = %q, %v", got, err)
				}
			}
		})
	}
}

func TestValueCipher_UnknownKeyIDNeverFallsBack(t *testing.T) {
	c := ciphertest.Cipher(t, v2Ring("k1", "k1", "k2"))
	// A value sealed under k2 but labelled with an ID the ring lacks.
	relabelled := strings.Replace(goldenV2, "enc:v2:k2:", "enc:v2:k9:", 1)
	if _, err := c.Decrypt(relabelled); !errors.Is(err, secret.ErrUnknownKeyID) {
		t.Fatalf("unknown kid: err = %v, want ErrUnknownKeyID", err)
	}
	// Labelled with another ID the ring holds: only that key (and its AAD)
	// is tried, so k2 is never used and the value does not open.
	relabelled = strings.Replace(goldenV2, "enc:v2:k2:", "enc:v2:k1:", 1)
	if _, err := c.Decrypt(relabelled); !errors.Is(err, secret.ErrDecrypt) {
		t.Fatalf("relabelled kid: err = %v, want ErrDecrypt", err)
	}
	// Even when k1's key bytes equal k2's, the AAD binds the label.
	same := ciphertest.Cipher(t, ciphertest.Ring{Active: "k1", IDs: []string{"k1"}, Keys: map[string][]byte{"k1": ciphertest.FakeKey("k2")}})
	if _, err := same.Decrypt(relabelled); !errors.Is(err, secret.ErrDecrypt) {
		t.Fatalf("relabelled with equal key bytes: err = %v, want ErrDecrypt", err)
	}
}

func TestValueCipher_V1BoundedCandidates(t *testing.T) {
	// The ring holds the legacy key, but not as a v1 reader: v1 is
	// unreadable (bounded legacy readers, no fallback to other entries).
	notReader := ciphertest.Cipher(t, ciphertest.Ring{Active: "k2", IDs: []string{"k2", keyring.LegacyID}, V1Readers: []string{}})
	if _, err := notReader.Decrypt(goldenV1); !errors.Is(err, secret.ErrNoDecryptionKey) {
		t.Fatalf("no v1 readers: err = %v, want ErrNoDecryptionKey", err)
	}
	wrongReader := ciphertest.Cipher(t, ciphertest.Ring{Active: "k2", IDs: []string{"k2", "old", keyring.LegacyID}, V1Readers: []string{"old"}})
	if _, err := wrongReader.Decrypt(goldenV1); !errors.Is(err, secret.ErrDecrypt) {
		t.Fatalf("wrong v1 reader: err = %v, want ErrDecrypt", err)
	}
	second := ciphertest.Cipher(t, ciphertest.Ring{Active: "k2", IDs: []string{"k2", "old", keyring.LegacyID}, V1Readers: []string{"old", keyring.LegacyID}})
	if pt, err := second.Decrypt(goldenV1); err != nil || pt != goldenPlaintext {
		t.Fatalf("second v1 reader: %q, %v", pt, err)
	}
}

func TestValueCipher_MalformedAndTampered(t *testing.T) {
	c := ciphertest.Cipher(t, ciphertest.Ring{Active: "k2", IDs: []string{"k2", keyring.LegacyID}, V1Readers: []string{keyring.LegacyID}})
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(goldenV2, "enc:v2:k2:"))
	tampered := append([]byte{}, raw...)
	tampered[len(tampered)-1] ^= 0x01
	tamperedBody := append([]byte{}, raw...)
	tamperedBody[13] ^= 0x01
	v1raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(goldenV1, "enc:v1:"))
	v1tampered := append([]byte{}, v1raw...)
	v1tampered[len(v1tampered)-1] ^= 0x01
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"unknown version", "enc:v9:" + base64.StdEncoding.EncodeToString(raw), secretenvelope.ErrUnknownVersion},
		{"bare enc", "enc:", secretenvelope.ErrUnknownVersion},
		{"bare v1", "enc:v1:", secretenvelope.ErrMalformed},
		{"bad base64", "enc:v2:k2:@@@@", secretenvelope.ErrMalformed},
		{"short nonce", "enc:v2:k2:" + base64.StdEncoding.EncodeToString(raw[:10]), secretenvelope.ErrMalformed},
		{"missing kid", "enc:v2::" + base64.StdEncoding.EncodeToString(raw), secretenvelope.ErrMissingKeyID},
		{"invalid kid", "enc:v2:k/2:" + base64.StdEncoding.EncodeToString(raw), secretenvelope.ErrMalformed},
		{"tampered tag", secretenvelope.FormatV2Value("k2", tampered), secret.ErrDecrypt},
		{"tampered ciphertext", secretenvelope.FormatV2Value("k2", tamperedBody), secret.ErrDecrypt},
		{"tampered v1", secretenvelope.FormatV1Value(v1tampered), secret.ErrDecrypt},
		{"v2 sealed without AAD", v2WithoutAAD(t), secret.ErrDecrypt},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pt, err := c.Decrypt(tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if pt != "" {
				t.Fatal("a failure returned a value")
			}
			assertNoLeak(t, err.Error(), tc.in)
			if errors.Is(err, store.ErrNotFound) {
				t.Fatal("a decrypt failure must never read as not found")
			}
		})
	}
}

// v2WithoutAAD seals under k2 with a v2 label but no AAD.
func v2WithoutAAD(t *testing.T) string {
	t.Helper()
	block, _ := aes.NewCipher(ciphertest.FakeKey("k2"))
	gcm, _ := cipher.NewGCM(block)
	nonce := []byte(goldenNonce)
	return secretenvelope.FormatV2Value("k2", gcm.Seal(nonce, nonce, []byte(goldenPlaintext), nil))
}

func TestValueCipher_PlaintextModes(t *testing.T) {
	const plain = "fake-legacy-plaintext"
	if got, err := ciphertest.Legacy().Decrypt(plain); err != nil || got != plain {
		t.Fatalf("legacy passthrough = %q, %v", got, err)
	}
	disabled := secret.NewLegacyValueCipher("")
	if disabled.Enabled() || disabled.Managed() {
		t.Fatal("empty secret must give the disabled cipher")
	}
	if got, err := disabled.Decrypt(plain); err != nil || got != plain {
		t.Fatalf("disabled passthrough = %q, %v", got, err)
	}
	if got, err := disabled.Encrypt(plain); err != nil || got != plain {
		t.Fatalf("disabled Encrypt = %q, %v", got, err)
	}
	if _, err := disabled.Decrypt(goldenV1); !errors.Is(err, secret.ErrNoDecryptionKey) {
		t.Fatalf("disabled reading ciphertext: err = %v", err)
	}
	managed := ciphertest.Cipher(t, v2Ring("k2", "k2"))
	for _, v := range []string{plain, ""} {
		if got, err := managed.Decrypt(v); !errors.Is(err, secret.ErrPlaintextNotAdmitted) || got != "" {
			t.Fatalf("managed plaintext %q: %q, %v", v, got, err)
		}
	}
}

func TestValueCipher_DecryptForMigration(t *testing.T) {
	const plain = "fake-legacy-plaintext"
	managed := ciphertest.Cipher(t, v2Ring("k2", "k2"))
	mig := func(state store.EncryptionMigrationState, admitted bool) *store.EncryptionMigration {
		return &store.EncryptionMigration{ID: "00000000-0000-4000-8000-000000000001", TargetKID: "k2", State: state, PlaintextReadAdmitted: admitted}
	}
	if got, err := managed.DecryptForMigration(plain, mig(store.EncryptionMigrationRunning, true)); err != nil || got != plain {
		t.Fatalf("running+admitted = %q, %v", got, err)
	}
	for name, m := range map[string]*store.EncryptionMigration{
		"nil":                  nil,
		"running not admitted": mig(store.EncryptionMigrationRunning, false),
		"interrupted admitted": mig(store.EncryptionMigrationInterrupted, true),
		"verified admitted":    mig(store.EncryptionMigrationVerified, true),
	} {
		if _, err := managed.DecryptForMigration(plain, m); !errors.Is(err, secret.ErrPlaintextNotAdmitted) {
			t.Fatalf("%s: err = %v, want ErrPlaintextNotAdmitted", name, err)
		}
	}
	// Encrypted values read the same as Decrypt.
	if got, err := managed.DecryptForMigration(goldenV2, nil); err != nil || got != goldenPlaintext {
		t.Fatalf("migration v2 read = %q, %v", got, err)
	}
	if _, err := managed.DecryptForMigration("enc:v2:k9:"+strings.TrimPrefix(goldenV2, "enc:v2:k2:"), mig(store.EncryptionMigrationRunning, true)); !errors.Is(err, secret.ErrUnknownKeyID) {
		t.Fatalf("migration unknown kid: %v", err)
	}
}

func TestValueCipher_Accessors(t *testing.T) {
	if _, err := secret.NewValueCipher(nil); err == nil {
		t.Fatal("NewValueCipher(nil) must fail")
	}
	k := ciphertest.Keyring(t, ciphertest.Ring{Active: "k2", IDs: []string{"k1", "k2"}})
	c, err := secret.NewValueCipher(k)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Enabled() || !c.Managed() || c.WriterID() != "k2" || c.WriteFormat() != keyring.WriteV2 {
		t.Fatalf("accessors: %v", c)
	}
	if ids := c.ReaderIDs(); len(ids) != 2 || ids[0] != "k2" || ids[1] != "k1" {
		t.Fatalf("ReaderIDs = %v", ids)
	}
	if !c.UsesRing(k.Encryption()) || c.UsesRing(ciphertest.Keyring(t, ciphertest.Ring{Active: "k2", IDs: []string{"k2"}}).Encryption()) {
		t.Fatal("UsesRing")
	}
	l := ciphertest.Legacy()
	if !l.Enabled() || l.Managed() || l.WriterID() != keyring.LegacyID || l.WriteFormat() != keyring.WriteV1 {
		t.Fatalf("legacy accessors: %v", l)
	}
}

// TestValueCipher_UnconfiguredFailsClosed: a nil or zero cipher (a missed
// wiring) never stores or returns plaintext; only NewLegacyValueCipher("")
// gives the disabled plaintext mode.
func TestValueCipher_UnconfiguredFailsClosed(t *testing.T) {
	var nilCipher *secret.ValueCipher
	for name, c := range map[string]*secret.ValueCipher{"nil": nilCipher, "zero": {}} {
		if c.Enabled() || c.Managed() || c.WriterID() != "" || c.ReaderIDs() != nil {
			t.Fatalf("%s cipher reports a configuration", name)
		}
		if got, err := c.Encrypt("fake-plain"); !errors.Is(err, secret.ErrCipherNotConfigured) || got != "" {
			t.Fatalf("%s Encrypt = %q, %v; want ErrCipherNotConfigured", name, got, err)
		}
		for _, v := range []string{"fake-plain", "", goldenV1, goldenV2} {
			if got, err := c.Decrypt(v); !errors.Is(err, secret.ErrCipherNotConfigured) || got != "" {
				t.Fatalf("%s Decrypt(%.10q) = %q, %v; want ErrCipherNotConfigured", name, v, got, err)
			}
		}
		admitted := &store.EncryptionMigration{State: store.EncryptionMigrationRunning, PlaintextReadAdmitted: true}
		if _, err := c.DecryptForMigration("fake-plain", admitted); !errors.Is(err, secret.ErrCipherNotConfigured) {
			t.Fatalf("%s DecryptForMigration: %v", name, err)
		}
	}
}

// TestValueCipher_V1WriterMustReadItself: a v1-writing ring whose active
// key is not a legacy v1 reader would write values it cannot read back.
func TestValueCipher_V1WriterMustReadItself(t *testing.T) {
	for name, readers := range map[string][]string{"no readers": {}, "other reader": {"k0"}} {
		k := ciphertest.Keyring(t, ciphertest.Ring{Active: "k1", IDs: []string{"k0", "k1"}, WriteFormat: keyring.WriteV1, V1Readers: readers})
		if _, err := secret.NewValueCipher(k); !errors.Is(err, secret.ErrWriterNotReadable) {
			t.Fatalf("%s: err = %v, want ErrWriterNotReadable", name, err)
		}
	}
	// v2 writers carry their key ID, so readers are irrelevant.
	if _, err := secret.NewValueCipher(ciphertest.Keyring(t, ciphertest.Ring{Active: "k1", IDs: []string{"k1"}, WriteFormat: keyring.WriteV2})); err != nil {
		t.Fatal(err)
	}
	ok := ciphertest.Cipher(t, ciphertest.Ring{Active: "k1", IDs: []string{"k0", "k1"}, WriteFormat: keyring.WriteV1, V1Readers: []string{"k0", "k1"}})
	v, err := ok.Encrypt("fake")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ok.Decrypt(v); err != nil || got != "fake" {
		t.Fatalf("v1 writer reads itself: %q, %v", got, err)
	}
}

func TestValueCipher_NeverPrintsKeys(t *testing.T) {
	k1 := ciphertest.FakeKey("k1")
	c := ciphertest.Cipher(t, v2Ring("k1", "k1"))
	var outputs []string
	outputs = append(outputs, fmt.Sprintf("%v %+v %#v %s %q %x", c, c, c, c, c, c), c.String(), c.GoString())
	j, _ := json.Marshal(c)
	outputs = append(outputs, string(j))
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cipher", "c", c)
	outputs = append(outputs, buf.String())
	for _, out := range outputs {
		for _, needle := range []string{string(k1), hex.EncodeToString(k1), base64.StdEncoding.EncodeToString(k1), "FAKE-KEY"} {
			if strings.Contains(out, needle) {
				t.Fatalf("key material in output %q", out)
			}
		}
		if !strings.Contains(out, "k1") {
			t.Fatalf("output lacks the key ID: %q", out)
		}
	}
}

func assertNoLeak(t *testing.T, msg, stored string) {
	t.Helper()
	for _, needle := range []string{goldenPlaintext, "FAKE-KEY", ciphertest.FakeSigningSecret} {
		if strings.Contains(msg, needle) {
			t.Fatalf("error text leaks %q", needle)
		}
	}
	if len(stored) > 12 && strings.Contains(msg, stored[7:]) {
		t.Fatal("error text echoes the stored value")
	}
}
