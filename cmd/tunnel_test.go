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
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tunnelStdinProbe records whether anything read it.
type tunnelStdinProbe struct {
	mu   sync.Mutex
	read bool
}

func (p *tunnelStdinProbe) Read([]byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.read = true
	return 0, io.EOF
}

func (p *tunnelStdinProbe) wasRead() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.read
}

// tunnelResetFlags restores every flag of the tunnel commands and the root
// to its default, so one cobra run cannot leak into the next.
func tunnelResetFlags(t *testing.T) {
	t.Helper()
	reset := func(fs *pflag.FlagSet) {
		fs.VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	}
	reset(tunnelCmd.Flags())
	reset(rootCmd.PersistentFlags())
	for _, c := range tunnelCmd.Commands() {
		reset(c.Flags())
	}
}

// tunnelRunCLI runs the scion CLI with args through cobra and reports the
// failure the way Execute does. It returns the exit code and stderr.
func tunnelRunCLI(t *testing.T, stdin io.Reader, args ...string) (int, string) {
	t.Helper()
	return tunnelRunCLIWith(t, stdin, nil, args...)
}

// tunnelRunCLIWith is tunnelRunCLI with a setup step that runs after the
// home and working directories are isolated.
func tunnelRunCLIWith(t *testing.T, stdin io.Reader, setup func(), args ...string) (int, string) {
	t.Helper()
	code, stderr, _ := tunnelRunCLIOutput(t, stdin, setup, args...)
	return code, stderr
}

// tunnelRunCLIOutput is tunnelRunCLIWith that also returns what the command
// wrote to its own stdout and stderr (for example the host-key hint).
func tunnelRunCLIOutput(t *testing.T, stdin io.Reader, setup func(), args ...string) (int, string, string) {
	t.Helper()
	isolateRootHook(t)
	for _, k := range []string{"SCION_HUB_TOKEN", "SCION_DEV_TOKEN", "SCION_DEV_TOKEN_FILE", "SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT", "SCION_PROJECT_ID", "XDG_RUNTIME_DIR"} {
		t.Setenv(k, "")
	}
	restoreCreds := credentials.ExportCredentialsPath()
	credPath := filepath.Join(t.TempDir(), "credentials.json")
	credentials.SetCredentialsPath(func() string { return credPath })
	tunnelResetFlags(t)
	if setup != nil {
		setup()
	}
	t.Cleanup(func() {
		credentials.SetCredentialsPath(restoreCreds)
		tunnelResetFlags(t)
		rootCmd.SetArgs(nil)
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	var out bytes.Buffer
	rootCmd.SetIn(stdin)
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	cmd, err := rootCmd.ExecuteC()
	if err == nil {
		return 0, "", out.String()
	}
	require.True(t, plainErrorCommands[topLevelName(cmd)], "%s must report plain errors", cmd.CommandPath())
	var stderr bytes.Buffer
	code := reportPlainError(&stderr, err)
	return code, stderr.String(), out.String()
}

func tunnelAssertOneLine(t *testing.T, stderr string) {
	t.Helper()
	assert.Equal(t, 1, strings.Count(stderr, "\n"), "want exactly one line, got %q", stderr)
	assert.True(t, strings.HasSuffix(stderr, "\n"), "line must end with a newline: %q", stderr)
	assert.NotContains(t, stderr, "\x1b", "no ANSI sequences")
	assert.NotContains(t, stderr, "Usage:", "no usage block")
}

func TestTunnelCLIUnknownFlagExits2(t *testing.T) {
	code, stderr := tunnelRunCLI(t, nil, "tunnel", "--bogus", "a1")
	assert.Equal(t, tunnelclient.ExitUsage, code)
	tunnelAssertOneLine(t, stderr)
}

func TestTunnelCLIMissingAgentExits2(t *testing.T) {
	code, stderr := tunnelRunCLI(t, nil, "tunnel", "--port", "80", "--hub", "http://127.0.0.1:1", "--project", "p1")
	assert.Equal(t, tunnelclient.ExitUsage, code)
	tunnelAssertOneLine(t, stderr)
}

func TestTunnelCLIInvalidPortExits2(t *testing.T) {
	code, stderr := tunnelRunCLI(t, nil, "tunnel", "--port", "70000", "--hub", "http://127.0.0.1:1", "--project", "p1", "a1")
	assert.Equal(t, tunnelclient.ExitUsage, code)
	tunnelAssertOneLine(t, stderr)
}

func TestTunnelCLIRootHookErrorExits1(t *testing.T) {
	code, stderr := tunnelRunCLI(t, nil, "tunnel", "--tz", "Not/AZone", "--port", "80", "--hub", "http://127.0.0.1:1", "--project", "p1", "a1")
	assert.Equal(t, tunnelclient.ExitError, code, "a root hook failure is not a usage error: %q", stderr)
	tunnelAssertOneLine(t, stderr)
}

// Criterion 25(e): with no credential, tunnel --stdio exits 3 with one
// plain line, without reading stdin, from a directory with no project and
// a minimal PATH.
func TestTunnelCLINoCredentialStdioExits3(t *testing.T) {
	stdin := &tunnelStdinProbe{}
	t.Setenv("PATH", "/nonexistent")
	code, stderr := tunnelRunCLI(t, stdin, "tunnel", "--stdio", "--ssh", "--hub", "http://127.0.0.1:1", "--project", "p1", "a1")
	assert.Equal(t, tunnelclient.ExitCredential, code, "stderr %q", stderr)
	tunnelAssertOneLine(t, stderr)
	assert.Contains(t, stderr, "scion hub auth login")
	assert.Contains(t, stderr, "http://127.0.0.1:1")
	assert.False(t, stdin.wasRead(), "stdin must not be read")
}

func TestTunnelCLINoEndpointExits2(t *testing.T) {
	code, stderr := tunnelRunCLI(t, nil, "tunnel", "--stdio", "--port", "22", "--project", "p1", "a1")
	assert.Equal(t, tunnelclient.ExitUsage, code, "stderr %q", stderr)
	tunnelAssertOneLine(t, stderr)
}

func TestTunnelReportPlainErrorFoldsAndStrips(t *testing.T) {
	var buf bytes.Buffer
	code := reportPlainError(&buf, errors.New("\x1b[31mfirst\x1b[0m\n\nsecond   line\n"))
	assert.Equal(t, tunnelclient.ExitError, code)
	assert.Equal(t, "Error: first second line\n", buf.String())

	buf.Reset()
	code = reportPlainError(&buf, tunnelclient.Errorf(tunnelclient.ExitAgentNotFound, "agent not found"))
	assert.Equal(t, tunnelclient.ExitAgentNotFound, code)
	assert.Equal(t, "Error: agent not found\n", buf.String())
}

func TestTunnelCommandsSkipProjectGate(t *testing.T) {
	assert.False(t, commandRequiresProject(tunnelCmd), "tunnel must not require a project")

	root := &cobra.Command{Use: "scion"}
	tunnel := &cobra.Command{Use: "tunnel"}
	status := &cobra.Command{Use: "status"}
	tunnel.AddCommand(status)
	root.AddCommand(tunnel)
	other := &cobra.Command{Use: "other"}
	nested := &cobra.Command{Use: "tunnel"}
	other.AddCommand(nested)
	root.AddCommand(other)

	assert.False(t, commandRequiresProject(tunnel))
	assert.False(t, commandRequiresProject(status), "tunnel status must not require a project")
	assert.True(t, commandRequiresProject(nested), "a nested command named tunnel under another parent still requires a project")
	assert.Equal(t, "tunnel", topLevelName(status))
	assert.Equal(t, "", topLevelName(root))
}

func TestTunnelPlainErrorCommands(t *testing.T) {
	assert.True(t, plainErrorCommands["tunnel"])
	assert.True(t, plainErrorCommands[topLevelName(tunnelCmd)])
	assert.False(t, plainErrorCommands["attach"])
}

// A stored login is found under any spelling of the same hub URL (letter
// case in the scheme or host, a trailing slash, an explicit default port),
// but is never offered to a different origin or path: another scheme, host,
// non-default port or path prefix does not find it, and SCION_HUB_TOKEN
// applies there instead.
func TestTunnelCredentialLookupIsNormalised(t *testing.T) {
	isolateRootHook(t)
	for _, k := range []string{"SCION_HUB_TOKEN", "SCION_DEV_TOKEN", "SCION_DEV_TOKEN_FILE"} {
		t.Setenv(k, "")
	}
	restore := credentials.ExportCredentialsPath()
	credPath := filepath.Join(t.TempDir(), "credentials.json")
	credentials.SetCredentialsPath(func() string { return credPath })
	t.Cleanup(func() { credentials.SetCredentialsPath(restore) })

	require.NoError(t, credentials.Store("https://Hub.example.com/hub/", &credentials.TokenResponse{AccessToken: "stored-token", ExpiresIn: time.Hour}))
	require.NoError(t, credentials.Store("http://plain.example.com", &credentials.TokenResponse{AccessToken: "plain-token", ExpiresIn: time.Hour}))
	stored := tunnelclient.Credential{Token: "stored-token", Kind: tunnelclient.CredentialOAuth}

	for _, spelled := range []string{
		"https://Hub.example.com/hub/",
		"https://hub.example.com/hub",
		"HTTPS://HUB.EXAMPLE.COM/hub",
		"https://hub.example.com:443/hub/",
	} {
		t.Run("same hub "+spelled, func(t *testing.T) {
			assert.Equal(t, stored, tunnelCredential(spelled))
		})
	}
	t.Run("same hub http default port", func(t *testing.T) {
		assert.Equal(t, tunnelclient.Credential{Token: "plain-token", Kind: tunnelclient.CredentialOAuth}, tunnelCredential("http://plain.example.com:80/"))
	})

	for _, other := range []struct{ name, url string }{
		{"different scheme", "http://hub.example.com/hub"},
		{"different scheme, https to http login", "https://plain.example.com"},
		{"different host", "https://hub2.example.com/hub"},
		{"different port", "https://hub.example.com:8443/hub"},
		{"other scheme's default port", "https://hub.example.com:80/hub"},
		{"different path", "https://hub.example.com/other"},
		{"path case differs", "https://hub.example.com/HUB"},
		{"no path", "https://hub.example.com"},
	} {
		t.Run(other.name, func(t *testing.T) {
			t.Setenv("SCION_HUB_TOKEN", "")
			got := tunnelCredential(other.url)
			assert.NotEqual(t, "stored-token", got.Token, "a stored login must not be offered to %s", other.url)
			assert.NotEqual(t, "plain-token", got.Token, "a stored login must not be offered to %s", other.url)

			t.Setenv("SCION_HUB_TOKEN", "env-token")
			assert.Equal(t, tunnelclient.Credential{Token: "env-token", Kind: tunnelclient.CredentialToken}, tunnelCredential(other.url))
		})
	}
}

// A settings load that fails must not reach GetHubEndpoint as a typed nil.
func TestTunnelEndpointNilSettings(t *testing.T) {
	isolateRootHook(t)
	tunnelResetFlags(t)
	t.Cleanup(func() { tunnelResetFlags(t) })
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	assert.NotPanics(t, func() { assert.Equal(t, "", tunnelEndpoint(nil)) })
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com")
	assert.Equal(t, "https://hub.example.com", tunnelEndpoint(nil))
}

func TestTunnelCLIUnreadableSettingsNoHubExits2(t *testing.T) {
	code, stderr := tunnelRunCLIWith(t, nil, func() {
		dir := filepath.Join(os.Getenv("HOME"), ".scion")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		path := filepath.Join(dir, "settings.yaml")
		require.NoError(t, os.WriteFile(path, []byte("hub: [unterminated\n\t: :\n"), 0o600))
		if os.Getuid() != 0 {
			require.NoError(t, os.Chmod(path, 0))
			t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		}
	}, "tunnel", "--stdio", "--port", "22", "--project", "p1", "a1")
	assert.Equal(t, tunnelclient.ExitUsage, code, "stderr %q", stderr)
	tunnelAssertOneLine(t, stderr)
	assert.Contains(t, stderr, "no hub endpoint")
}

func TestTunnelSignalError(t *testing.T) {
	err := tunnelSignalError(os.Interrupt)
	assert.Equal(t, tunnelclient.ExitInterrupted, tunnelclient.CodeOf(err))
	assert.Equal(t, "interrupted", err.Error())
	err = tunnelSignalError(syscall.SIGTERM)
	assert.Equal(t, tunnelclient.ExitTerminated, tunnelclient.CodeOf(err))
	assert.Equal(t, "terminated", err.Error())
}

// For tunnel, --project names a hub project: the root hook does not read
// settings from a directory of that name.
func TestTunnelCLIDoesNotLoadProjectFlagAsPath(t *testing.T) {
	settingsYAML := []byte("cli:\n  autohelp: false\n  interactive_disabled: true\n")
	var files []string
	code, stderr := tunnelRunCLIWith(t, nil, func() {
		autoHelp, nonInteractive, autoConfirm = true, false, false
		for _, dir := range []string{"p1", filepath.Join("p1", ".scion")} {
			require.NoError(t, os.MkdirAll(dir, 0o700))
			f := filepath.Join(dir, "settings.yaml")
			require.NoError(t, os.WriteFile(f, settingsYAML, 0o600))
			files = append(files, f)
		}
	}, "tunnel", "--stdio", "--ssh", "--hub", "http://127.0.0.1:1", "--project", "p1", "a1")
	assert.Equal(t, tunnelclient.ExitCredential, code, "stderr %q", stderr)
	assert.True(t, autoHelp, "cli.autohelp was read from ./p1")
	assert.False(t, nonInteractive, "cli.interactive_disabled was read from ./p1")
	for _, f := range files {
		data, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.Equal(t, settingsYAML, data, "%s was rewritten", f)
	}
}

// With --project, the settings come from the global layer only: a
// ./global directory in the working directory is neither read nor changed.
func TestTunnelCLIProjectFlagIgnoresCwdGlobalDir(t *testing.T) {
	cwdSettings := []byte("hub:\n  endpoint: https://from-cwd-global.example.com\n")
	var path string
	code, stderr := tunnelRunCLIWith(t, nil, func() {
		require.NoError(t, os.MkdirAll("global", 0o700))
		path = filepath.Join("global", "settings.yaml")
		require.NoError(t, os.WriteFile(path, cwdSettings, 0o600))
	}, "tunnel", "--stdio", "--port", "22", "--project", "p1", "a1")
	// Had ./global been read, its endpoint would be used and the run
	// would fail later, on the missing credential (exit 3).
	assert.Equal(t, tunnelclient.ExitUsage, code, "stderr %q", stderr)
	assert.Contains(t, stderr, "no hub endpoint")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, cwdSettings, data, "./global/settings.yaml was rewritten")
}
