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

package cmd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
)

// Keyring mode without the legacy session secret: the startup paths that
// used to read the keyring's "legacy" entry through the session secret now
// decide from the keyring itself, and never read the secret.

// noLegacyCmdKeyring writes a keyring file with no "legacy" signing entry
// (gen1 active, a legacy reader as emit "legacy" requires).
func noLegacyCmdKeyring(t *testing.T) string {
	t.Helper()
	return writeCmdKeyring(t, func(doc map[string]any) {
		s := doc["signing"].(map[string]any)
		s["keys"] = []any{map[string]any{"id": "gen1", "secret": cmdKRGen1Secret, "sign_until": nil, "verify_until": nil}}
		s["legacy_read_ids"] = []any{"gen1"}
	})
}

// forbidHostedHALegacySecret makes any read of the legacy session secret by
// the hosted HA present-key check fail the test.
func forbidHostedHALegacySecret(t *testing.T) *int {
	t.Helper()
	calls := 0
	prev := hostedHALegacySecret
	hostedHALegacySecret = func() string { calls++; return "" }
	t.Cleanup(func() { hostedHALegacySecret = prev })
	return &calls
}

// The hosted HA preflight keeps a present-key assertion. With a keyring
// file it passes on the ring's active generation, with no session secret
// configured and without reading one (for a ring with the "legacy" entry,
// and, with the release check relaxed, one without it). In legacy mode an
// empty session secret still refuses: never a keyless start.
func TestHostedHAPreflight_PresentKeyAssertion(t *testing.T) {
	t.Run("keyring mode", func(t *testing.T) {
		withHostedHAGuards(t)
		isolateKeyring(t)
		hostedMode = true
		t.Setenv("SCION_SERVER_SESSION_SECRET", "")
		require.NoError(t, loadServerKeyring(&config.GlobalConfig{KeyringFile: writeCmdKeyring(t, nil)}))
		calls := forbidHostedHALegacySecret(t)
		require.NoError(t, validateHostedHAPreflight(validHostedHAConfig()))
		assert.Zero(t, *calls, "keyring mode must not read the legacy session secret")
	})
	t.Run("keyring mode without the legacy entry (release check relaxed)", func(t *testing.T) {
		withHostedHAGuards(t)
		isolateKeyring(t)
		hostedMode = true
		t.Setenv("SCION_SERVER_SESSION_SECRET", "")
		t.Cleanup(hub.RelaxKeyringReleaseForTest())
		require.NoError(t, loadServerKeyring(&config.GlobalConfig{KeyringFile: noLegacyCmdKeyring(t)}))
		calls := forbidHostedHALegacySecret(t)
		require.NoError(t, validateHostedHAPreflight(validHostedHAConfig()))
		assert.Zero(t, *calls)
	})
	t.Run("legacy mode without a secret refuses", func(t *testing.T) {
		withHostedHAGuards(t)
		isolateKeyring(t)
		hostedMode = true
		t.Setenv("SCION_SERVER_SESSION_SECRET", "")
		require.NoError(t, loadServerKeyring(&config.GlobalConfig{}))
		err := validateHostedHAPreflight(validHostedHAConfig())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "durable session/signing secret")
	})
	t.Run("legacy mode with a secret passes", func(t *testing.T) {
		withHostedHAGuards(t)
		require.NoError(t, validateHostedHAPreflight(validHostedHAConfig()))
	})
}

// With a keyring file the session-secret resolvers return "" for every cmd
// consumer (hub SharedSigningSecret, relay-peer options, the preflight's
// peer-secret check), also for a ring without the "legacy" entry.
func TestKeyringMode_ResolversNeverReturnTheLegacyEntry(t *testing.T) {
	for name, path := range map[string]func(*testing.T) string{
		"with the legacy entry":    func(t *testing.T) string { return writeCmdKeyring(t, nil) },
		"without the legacy entry": noLegacyCmdKeyring,
	} {
		t.Run(name, func(t *testing.T) {
			isolateKeyring(t)
			t.Cleanup(hub.RelaxKeyringReleaseForTest())
			require.NoError(t, loadServerKeyring(&config.GlobalConfig{KeyringFile: path(t)}))
			assert.Empty(t, resolveSessionSecret())
			assert.Empty(t, sessionSecretValue())
			o := conduitPeerAuthOptions(context.Background(), &config.GlobalConfig{}, "hub-a", false)
			assert.Empty(t, o.SharedSecret)
			assert.NotNil(t, o.SigningRing)
		})
	}
}

// G7 in keyring mode: testModeInputsFor marks keyring signing and never
// reads SharedSigningSecret (the reader is instrumented), and the admin
// tier then refuses with keyring_signing.
func TestTestAdminG7_KeyringModeRefusesWithoutReadingTheSharedSecret(t *testing.T) {
	calls := 0
	prev := testModeSharedSecretSet
	testModeSharedSecretSet = func(hub.ServerConfig) bool { calls++; return true }
	t.Cleanup(func() { testModeSharedSecretSet = prev })

	isolateKeyring(t)
	require.NoError(t, loadServerKeyring(&config.GlobalConfig{KeyringFile: writeCmdKeyring(t, nil)}))
	in := testModeInputsFor(context.Background(), &config.GlobalConfig{}, hub.ServerConfig{Keyring: serverKeyringFromFile(), KeyringConfigured: true}, true, nil)
	assert.True(t, in.KeyringSigning)
	assert.False(t, in.SharedSigningSecret)
	assert.Zero(t, calls, "keyring mode must not read SharedSigningSecret")

	g := tiaValidInputs()
	g.KeyringSigning = in.KeyringSigning
	_, err := resolveTestMode(g)
	require.Error(t, err)
	assert.True(t, errors.Is(err, hub.ErrTestAdminKeyringSigning), "got %v", err)

	// Legacy mode reads it (through the same reader) and G7 refuses on it.
	isolateKeyring(t)
	in = testModeInputsFor(context.Background(), &config.GlobalConfig{}, hub.ServerConfig{SharedSigningSecret: "synthetic-test-secret"}, true, nil)
	assert.False(t, in.KeyringSigning)
	assert.True(t, in.SharedSigningSecret)
	assert.Equal(t, 1, calls)
}

// The keyring-mode decisions of the hosted checks read the loaded keyring
// (serverKeyringFromFile), so runServerStart must load it first: a check
// that ran before loadServerKeyring would see legacy mode and refuse (or
// warn) on the absent session secret.
func TestRunServerStart_LoadsKeyringBeforeHostedChecks(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "server_foreground.go", nil, 0)
	require.NoError(t, err)
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "runServerStart" {
			fn = fd
		}
	}
	require.NotNil(t, fn, "runServerStart not found")
	first := map[string]token.Pos{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok {
				if _, seen := first[id.Name]; !seen {
					first[id.Name] = call.Pos()
				}
			}
		}
		return true
	})
	load, ok := first["loadServerKeyring"]
	require.True(t, ok, "runServerStart does not call loadServerKeyring")
	for _, check := range []string{"validateHostedBasic", "validateHostedHAPreflight", "validateServerPreflight"} {
		pos, ok := first[check]
		require.True(t, ok, "runServerStart does not call %s", check)
		assert.Less(t, load, pos, "%s must run after loadServerKeyring", check)
	}
}
