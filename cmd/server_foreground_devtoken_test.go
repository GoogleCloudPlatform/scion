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
	"encoding/json"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureDevToken is a made-up dev token value used only by these tests.
const fixtureDevToken = "scion_dev_fixture0123456789abcdef"

// wantDevTokenExport is the line startup output must show for the dev token
// file at path (path must not contain a single quote).
func wantDevTokenExport(path string) string {
	return "export SCION_DEV_TOKEN=$(cat '" + path + "')"
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

	// The log says where the token is set, as a plain message: no command
	// (there is no file to cat) and no value.
	logged := out.String()
	assert.Contains(t, logged, "Developer token: set in the server settings (server.auth.dev_token")
	assert.NotContains(t, logged, "run:")
	assert.NotContains(t, logged, "SCION_DEV_TOKEN=")
	assert.NotContains(t, logged, "scion_dev_")
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

// TestInitDevAuth_JSONLogLine checks the dev token line as the server really
// writes it. Startup calls logging.SetupWithOTel (see initServerLogging),
// which installs a JSON handler on stdout as the slog default; that routes
// the standard log through slog. The line must hold the path form unescaped
// (so it can be copied from the log, and the e2e harness can match it after
// decoding the record) and no token value.
func TestInitDevAuth_JSONLogLine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		useGCP bool
		msgKey string
	}{
		{name: "json", useGCP: false, msgKey: "msg"},
		{name: "gcp", useGCP: true, msgKey: "message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_DEV_TOKEN", "")
			t.Setenv("SCION_AUTH_TOKEN", "")
			t.Setenv("K_SERVICE", "")
			globalDir := t.TempDir()
			tokenPath := filepath.Join(globalDir, "dev-token")
			require.NoError(t, os.WriteFile(tokenPath, []byte(fixtureDevToken+"\n"), 0o600))
			cfg := &config.GlobalConfig{}
			cfg.Auth.Enabled = true

			// The base handler writes to os.Stdout as it is when the handler
			// is built, so point it at a file first. Setting a non-default
			// slog handler also redirects the standard log; restore it all.
			stdoutFile, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
			require.NoError(t, err)
			prevStdout := os.Stdout
			prevSlog, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
			t.Cleanup(func() {
				os.Stdout = prevStdout
				slog.SetDefault(prevSlog)
				log.SetOutput(prevOut)
				log.SetFlags(prevFlags)
				_ = stdoutFile.Close()
			})
			os.Stdout = stdoutFile
			logging.SetupWithOTel("scion-hub", "", false, tc.useGCP, nil)

			_, err = initDevAuth(cfg, globalDir)
			os.Stdout = prevStdout
			require.NoError(t, err)

			out, err := os.ReadFile(stdoutFile.Name())
			require.NoError(t, err)
			want := wantDevTokenExport(tokenPath)
			var exportLine string
			for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
				var rec map[string]any
				require.NoError(t, json.Unmarshal(line, &rec), "log line is not JSON: %s", line)
				if msg, _ := rec[tc.msgKey].(string); msg == "  "+want {
					exportLine = string(line)
				}
			}
			require.NotEmpty(t, exportLine, "no log record with %s %q; stdout:\n%s", tc.msgKey, "  "+want, out)
			t.Logf("server stdout line: %s", exportLine)
			assert.Contains(t, exportLine, want, "the raw log line should hold the command unescaped")
			assert.NotContains(t, string(out), "scion_dev_")
		})
	}
}
