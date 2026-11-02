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
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// relaxKeyringReleaseCheck swaps the release check for a no-op for the
// test, as lifting the release limits will. Tests using it do not run in
// parallel.
func relaxKeyringReleaseCheck(t *testing.T) {
	t.Helper()
	prev := keyringReleaseCheck
	keyringReleaseCheck = func(*keyring.Keyring, *ServerConfig) error { return nil }
	t.Cleanup(func() { keyringReleaseCheck = prev })
}

// seamNoLegacyKeyring is a keyring file with no "legacy" signing entry:
// gen1 active (a legacy reader, as emit "legacy" requires), gen2 staged.
func seamNoLegacyKeyring(t *testing.T) *keyring.Keyring {
	t.Helper()
	k := ksFile(t, ksPilotDoc("rev-nl", "gen1",
		[]any{ksEntry("gen1", krGen1Secret, nil, nil), ksEntry("gen2", krGen2Secret, nil, nil)},
		[]any{"gen1"}))
	_, ok := k.Signing().LegacySecret()
	require.False(t, ok)
	return k
}

func seamConfig(k *keyring.Keyring, shared string) ServerConfig {
	cfg := DefaultServerConfig()
	cfg.KeyringConfigured = true
	cfg.Keyring = k
	cfg.SharedSigningSecret = shared
	return cfg
}

// The "legacy" entry requirement lives only behind the release check:
// production (the default seam) refuses a ring without it, at startup and
// in `scion server keys check`; with the seam relaxed both admit it, with
// or without a session secret. A second, seam-independent legacy check
// would make the relaxed half fail; dropping the requirement from the
// production seam would make the default half fail.
func TestKeyringReleaseSeam_NoLegacyEntryBehindSeam(t *testing.T) {
	k := seamNoLegacyKeyring(t)

	err := validateKeyringConfig(seamConfig(k, krLegacySecret))
	require.Error(t, err, "production refuses a ring without the legacy entry")
	assert.Contains(t, err.Error(), `signing key with id "legacy"`)
	assert.NotContains(t, err.Error(), krGen1Secret)
	err = CheckKeyringFile(k)
	require.Error(t, err, "keys check refuses it too")
	assert.Contains(t, err.Error(), `signing key with id "legacy"`)

	relaxKeyringReleaseCheck(t)
	assert.NoError(t, validateKeyringConfig(seamConfig(k, "")))
	assert.NoError(t, validateKeyringConfig(seamConfig(k, krLegacySecret)))
	assert.NoError(t, CheckKeyringFile(k))
}

// SharedSigningSecret == the "legacy" entry is a release requirement too:
// production refuses a mismatch (every byte compared, see
// TestNew_KeyringModeRefusesExplicitSigningKeys); keys check, which has no
// session secret, does not check it; the relaxed seam drops it.
func TestKeyringReleaseSeam_SharedSecretBehindSeam(t *testing.T) {
	k := ksKeyringA(t)
	require.NoError(t, validateKeyringConfig(seamConfig(k, krLegacySecret)))
	for _, shared := range []string{"", "some-other-secret", krGen1Secret} {
		err := validateKeyringConfig(seamConfig(k, shared))
		require.Error(t, err, "production refuses SharedSigningSecret %q", shared)
		assert.Contains(t, err.Error(), "SharedSigningSecret")
		assert.NotContains(t, err.Error(), krLegacySecret)
		if shared != "" {
			assert.NotContains(t, err.Error(), shared)
		}
	}
	require.NoError(t, CheckKeyringFile(k))

	relaxKeyringReleaseCheck(t)
	assert.NoError(t, validateKeyringConfig(seamConfig(k, "some-other-secret")))
	assert.NoError(t, validateKeyringConfig(seamConfig(k, "")))
}

// Relaxing the release check relaxes nothing else: a missing keyring, a
// keyring with no active generation, a legacy adapter in place of the
// file, an emit "legacy" active generation that is not a legacy reader and
// explicit JWT signing keys all still refuse, at startup and (where it
// applies) in keys check.
func TestKeyringReleaseSeam_RelaxedKeepsOtherChecks(t *testing.T) {
	relaxKeyringReleaseCheck(t)
	notReader := ksFile(t, ksPilotDoc("rev-r", "gen1",
		[]any{ksEntry("gen1", krGen1Secret, nil, nil), ksEntry("gen2", krGen2Secret, nil, nil)}, []any{"gen2"}))
	for name, tc := range map[string]struct {
		cfg   ServerConfig
		check bool // also refused by CheckKeyringFile
		want  string
	}{
		"no keyring loaded":          {cfg: seamConfig(nil, ""), want: "no keyring file was loaded"},
		"no active generation":       {cfg: seamConfig(&keyring.Keyring{}, ""), check: true, want: "no active generation"},
		"legacy adapter":             {cfg: seamConfig(keyring.Legacy(krLegacySecret), krLegacySecret), check: true, want: "did not come from it"},
		"active not a legacy reader": {cfg: seamConfig(notReader, ""), check: true, want: "legacy_read_ids"},
		"explicit agent key": {cfg: func() ServerConfig {
			c := seamConfig(seamNoLegacyKeyring(t), "")
			c.AgentTokenConfig.SigningKey = []byte("explicit-agent-key-0123456789abcdef")
			return c
		}(), want: "AgentTokenConfig.SigningKey"},
		"explicit user key": {cfg: func() ServerConfig {
			c := seamConfig(seamNoLegacyKeyring(t), "")
			c.UserTokenConfig.SigningKey = []byte("explicit-user-key-0123456789abcdef")
			return c
		}(), want: "UserTokenConfig.SigningKey"},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateKeyringConfig(tc.cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			if tc.check {
				require.Error(t, CheckKeyringFile(tc.cfg.Keyring))
			}
		})
	}
}

// The exported test hook refuses to relax the release check outside a test
// binary. The guard's own branch is driven here through the injected
// predicate: inside any test binary testing.Testing() is true, so only a
// non-test binary could exercise the real predicate's false value.
func TestRelaxKeyringReleaseForTest_RefusesOutsideTests(t *testing.T) {
	assert.PanicsWithValue(t, "hub.RelaxKeyringReleaseForTest called outside a test binary", func() {
		relaxKeyringRelease(func() bool { return false })
	})
	assert.Equal(t, reflect.ValueOf(releaseKeyringCheck).Pointer(), reflect.ValueOf(keyringReleaseCheck).Pointer(), "a refused call leaves the production check in place")

	restore := RelaxKeyringReleaseForTest()
	// Restore even if an assertion below stops the test, so the relaxed
	// check never leaks into later tests (restoring twice is harmless).
	t.Cleanup(restore)
	require.NoError(t, keyringReleaseCheck(seamNoLegacyKeyring(t), nil), "inside a test binary the hook relaxes the check")
	restore()
	assert.Equal(t, reflect.ValueOf(releaseKeyringCheck).Pointer(), reflect.ValueOf(keyringReleaseCheck).Pointer(), "restore reinstates the production check")
}
