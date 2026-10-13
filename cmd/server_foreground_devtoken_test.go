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
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureDevToken is a made-up dev token value used only by these tests.
const fixtureDevToken = "scion_dev_fixture0123456789abcdef"

// wantDevTokenExport is the line startup output must show for the dev token
// file at path (path must not contain a single quote).
func wantDevTokenExport(path string) string {
	return `export SCION_DEV_TOKEN="$(cat '` + path + `')"`
}

// captureDevTokenLog redirects the standard logger to a buffer for the test.
func captureDevTokenLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// TestInitDevAuth_LogsTokenFilePath checks that server startup logs the dev
// token file path and the command that loads it, never the token value.
func TestInitDevAuth_LogsTokenFilePath(t *testing.T) {
	cases := []struct {
		name string
		// customFile, when true, configures auth.dev_token_file.
		customFile bool
		// envFile, when true, sets SCION_DEV_TOKEN_FILE (a client-side
		// variable the server does not read) to another file.
		envFile bool
	}{
		{name: "default path"},
		{name: "configured token file", customFile: true},
		{name: "SCION_DEV_TOKEN_FILE set", envFile: true},
		{name: "configured file and SCION_DEV_TOKEN_FILE set", customFile: true, envFile: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// initDevAuth exports these; t.Setenv restores them afterwards.
			t.Setenv("SCION_DEV_TOKEN", "")
			t.Setenv("SCION_AUTH_TOKEN", "")

			globalDir := t.TempDir()
			wantPath := filepath.Join(globalDir, "dev-token")
			cfg := &config.GlobalConfig{}
			cfg.Auth.Enabled = true
			if tc.customFile {
				wantPath = filepath.Join(t.TempDir(), "custom", "token")
				cfg.Auth.TokenFile = wantPath
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(wantPath), 0o755))
			require.NoError(t, os.WriteFile(wantPath, []byte(fixtureDevToken+"\n"), 0o600))
			if tc.envFile {
				envPath := filepath.Join(t.TempDir(), "env-token")
				require.NoError(t, os.WriteFile(envPath, []byte(fixtureDevToken+"\n"), 0o600))
				t.Setenv("SCION_DEV_TOKEN_FILE", envPath)
			}

			out := captureDevTokenLog(t)
			token, err := initDevAuth(cfg, globalDir)
			require.NoError(t, err)
			require.Equal(t, fixtureDevToken, token)

			logged := out.String()
			assert.Contains(t, logged, wantDevTokenExport(wantPath))
			assert.NotContains(t, logged, "scion_dev_")
			assert.Contains(t, devTokenStartupHint(cfg, globalDir), wantDevTokenExport(wantPath))
		})
	}
}

// TestInitDevAuth_GeneratedTokenNotLogged covers first start, when the
// server generates the token and writes the file itself.
func TestInitDevAuth_GeneratedTokenNotLogged(t *testing.T) {
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_AUTH_TOKEN", "")
	globalDir := t.TempDir()
	cfg := &config.GlobalConfig{}
	cfg.Auth.Enabled = true

	out := captureDevTokenLog(t)
	token, err := initDevAuth(cfg, globalDir)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	logged := out.String()
	assert.Contains(t, logged, wantDevTokenExport(filepath.Join(globalDir, "dev-token")))
	assert.NotContains(t, logged, token)
	assert.NotContains(t, logged, "scion_dev_")
}

// TestDevTokenStartupHint_ConfiguredToken checks that a token set directly
// in the server settings, which has no file, is not printed either.
func TestDevTokenStartupHint_ConfiguredToken(t *testing.T) {
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_AUTH_TOKEN", "")
	cfg := &config.GlobalConfig{}
	cfg.Auth.Enabled = true
	cfg.Auth.Token = fixtureDevToken

	out := captureDevTokenLog(t)
	token, err := initDevAuth(cfg, t.TempDir())
	require.NoError(t, err)
	require.Equal(t, fixtureDevToken, token)

	hint := devTokenStartupHint(cfg, t.TempDir())
	assert.NotContains(t, hint, "scion_dev_")
	assert.Contains(t, hint, "server.auth.dev_token")
	assert.NotContains(t, out.String(), "scion_dev_")
}

// TestLogWorkstationReadyBanner_DevTokenPath checks the workstation startup
// banner: it shows the dev token file path, not the token value.
func TestLogWorkstationReadyBanner_DevTokenPath(t *testing.T) {
	globalDir := t.TempDir()
	cfg := &config.GlobalConfig{}
	cfg.Auth.Enabled = true

	out := captureDevTokenLog(t)
	logWorkstationReadyBanner(cfg, globalDir, true, 8080, fixtureDevToken)

	logged := out.String()
	assert.Contains(t, logged, "Developer token: "+wantDevTokenExport(filepath.Join(globalDir, "dev-token")))
	assert.NotContains(t, logged, "scion_dev_")
}

// TestDevTokenExportHint_ShellSafe checks that the printed command works in
// a POSIX shell for a path with a space and a single quote in it.
func TestDevTokenExportHint_ShellSafe(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "it's a dir")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "dev-token")
	require.NoError(t, os.WriteFile(path, []byte(fixtureDevToken+"\n"), 0o600))

	hint := devTokenExportHint(path)
	assert.NotContains(t, hint, "scion_dev_")

	// Evaluate the printed command in sh and print the exported value.
	got, err := exec.Command("sh", "-c", hint+` && printf '%s' "$SCION_DEV_TOKEN"`).Output()
	require.NoError(t, err)
	assert.Equal(t, fixtureDevToken, string(got))
}
