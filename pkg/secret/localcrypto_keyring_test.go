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

package secret

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// TestDeriveLocalEncryptionKeyMatchesKeyringLegacy pins the keyring's
// legacy adapter (encryption id "legacy") to this package's derivation, so
// a keyring built from the legacy secret encrypts and decrypts exactly
// what the local backend does (ptone/scion#1530 Phase 1). The golden value
// is for a synthetic fixture secret.
func TestDeriveLocalEncryptionKeyMatchesKeyringLegacy(t *testing.T) {
	const fixture = "test-only-legacy-secret"
	const golden = "216b3dfda93c482541529e7bd3362249aac73f3b32c745298ed1e08ae45482aa"
	for _, s := range []string{fixture, "", "x", "a much longer synthetic secret value for the test only"} {
		if !bytes.Equal(DeriveLocalEncryptionKey(s), keyring.DeriveLegacyEncryptionKey(s)) {
			t.Fatalf("derivations differ for a %d-byte secret", len(s))
		}
		if !bytes.Equal(DeriveLocalEncryptionKey(s), keyring.Legacy(s).Encryption().Active().Bytes()) {
			t.Fatalf("legacy keyring encryption key differs for a %d-byte secret", len(s))
		}
	}
	if got := hex.EncodeToString(DeriveLocalEncryptionKey(fixture)); got != golden {
		t.Fatalf("DeriveLocalEncryptionKey golden = %s", got)
	}
}
