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

// Package ciphertest builds synthetic keyrings and value ciphers for
// tests (ptone/scion#1530). Every key it produces is an obviously fake,
// deterministic fixture; never use it outside tests.
package ciphertest

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
)

// FakeSigningSecret is the synthetic legacy signing secret of every ring
// built here.
const FakeSigningSecret = "FAKE-test-only-signing-secret-0000000000"

// FakeKey returns a deterministic 32-byte fake encryption key for id:
// "FAKE-KEY-<id>-" padded with '0'.
func FakeKey(id string) []byte {
	k := "FAKE-KEY-" + id + "-"
	if len(k) < 32 {
		k += strings.Repeat("0", 32-len(k))
	}
	return []byte(k[:32])
}

// Ring describes an encryption ring. Keys maps IDs to raw keys; a nil
// key uses FakeKey(id). The special ID keyring.LegacyID with a nil key
// uses the legacy derivation of FakeSigningSecret.
type Ring struct {
	Active      string
	WriteFormat keyring.WriteFormat // default v2
	IDs         []string            // file order; must include Active
	Keys        map[string][]byte
	V1Readers   []string
}

// Keyring parses a synthetic keyring file (source "file") for r.
func Keyring(t testing.TB, r Ring) *keyring.Keyring {
	t.Helper()
	wf := r.WriteFormat
	if wf == "" {
		wf = keyring.WriteV2
	}
	keys := make([]any, 0, len(r.IDs))
	for _, id := range r.IDs {
		k := r.Keys[id]
		if k == nil {
			if id == keyring.LegacyID {
				k = keyring.DeriveLegacyEncryptionKey(FakeSigningSecret)
			} else {
				k = FakeKey(id)
			}
		}
		keys = append(keys, map[string]any{"id": id, "key_b64": base64.StdEncoding.EncodeToString(k)})
	}
	readers := r.V1Readers
	if readers == nil {
		readers = []string{}
	}
	doc := map[string]any{
		"schema":   1,
		"revision": "ciphertest",
		"signing": map[string]any{
			"active": keyring.LegacyID,
			"emit":   "legacy",
			"keys": []any{map[string]any{"id": keyring.LegacyID, "secret": FakeSigningSecret,
				"sign_until": nil, "verify_until": nil}},
			"legacy_read_ids": []any{keyring.LegacyID},
		},
		"encryption": map[string]any{
			"active":             r.Active,
			"write_format":       string(wf),
			"keys":               keys,
			"legacy_v1_read_ids": readers,
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("ciphertest: marshal: %v", err)
	}
	k, err := keyring.Parse(raw)
	if err != nil {
		t.Fatalf("ciphertest: parse synthetic keyring: %v", err)
	}
	return k
}

// Cipher returns a managed ValueCipher for r.
func Cipher(t testing.TB, r Ring) *secret.ValueCipher {
	t.Helper()
	c, err := secret.NewValueCipher(Keyring(t, r))
	if err != nil {
		t.Fatalf("ciphertest: cipher: %v", err)
	}
	return c
}

// Legacy returns the legacy-mode cipher over FakeSigningSecret.
func Legacy() *secret.ValueCipher { return secret.NewLegacyValueCipher(FakeSigningSecret) }
