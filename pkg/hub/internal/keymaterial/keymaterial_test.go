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

package keymaterial

import (
	"encoding/base64"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/ciphertest"
)

func b64(v string) ([]byte, bool) {
	b, err := base64.StdEncoding.DecodeString(v)
	return b, err == nil
}

func seal(t *testing.T, c *secret.ValueCipher, plaintext string) string {
	t.Helper()
	v, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// BackupIsCurrent is true only for the writer's own sealing of the same key
// bytes; every other case is false, so the caller re-saves.
func TestBackupIsCurrent(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("FAKE-KEY-BYTES-0000000000000000A"))
	other := base64.StdEncoding.EncodeToString([]byte("FAKE-KEY-BYTES-0000000000000000B"))
	k2 := ciphertest.Cipher(t, ciphertest.Ring{Active: "k2", IDs: []string{"k2"}, WriteFormat: keyring.WriteV2})
	k2OverK1 := ciphertest.Cipher(t, ciphertest.Ring{Active: "k2", IDs: []string{"k1", "k2"}, WriteFormat: keyring.WriteV2})
	k1 := ciphertest.Cipher(t, ciphertest.Ring{Active: "k1", IDs: []string{"k1"}, WriteFormat: keyring.WriteV2})
	v1Writer := ciphertest.Cipher(t, ciphertest.Ring{Active: "k2", IDs: []string{"k2"}, WriteFormat: keyring.WriteV1, V1Readers: []string{"k2"}})
	never := func(string) ([]byte, bool) { return nil, false }
	empty := func(string) ([]byte, bool) { return []byte{}, true }

	cases := []struct {
		name   string
		c      *secret.ValueCipher
		stored string
		value  string
		decode func(string) ([]byte, bool)
		want   bool
	}{
		{"writer, same key", k2, seal(t, k2, key), key, b64, true},
		{"writer, other key bytes", k2, seal(t, k2, other), key, b64, false},
		{"readable but not the writer key (k1 value, k2 writer)", k2OverK1, seal(t, k1, key), key, b64, false},
		{"the writer key in the other format (v1 value, v2 writer)", k2, seal(t, v1Writer, key), key, b64, false},
		{"stored plaintext does not decode", k2, seal(t, k2, "fake-not-base64!"), key, b64, false},
		{"new value does not decode", k2, seal(t, k2, key), "fake-not-base64!", b64, false},
		{"decoder refuses", k2, seal(t, k2, key), key, never, false},
		{"empty key", k2, seal(t, k2, key), key, empty, false},
		{"disabled cipher", secret.NewLegacyValueCipher(""), key, key, b64, false},
		{"plaintext row", k2, key, key, b64, false},
	}
	for _, tc := range cases {
		if got := BackupIsCurrent(tc.c, tc.stored, tc.value, tc.decode); got != tc.want {
			t.Errorf("%s: BackupIsCurrent = %v, want %v", tc.name, got, tc.want)
		}
	}
	// The control for the non-writer case: the ring does read the value,
	// so only the writer restriction makes it false.
	if pt, err := k2OverK1.Decrypt(seal(t, k1, key)); err != nil || pt != key {
		t.Fatalf("control: Decrypt = %q, %v", pt, err)
	}
}
