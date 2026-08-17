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
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

// newBoolFlagCmd builds a throwaway command with a single bool flag and parses
// setArgs into it, so cmd.Flags().Changed reflects whether the flag was given.
func newBoolFlagCmd(t *testing.T, name string, setArgs ...string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
	var v bool
	c.Flags().BoolVar(&v, name, false, "")
	c.SetArgs(setArgs)
	if err := c.Execute(); err != nil {
		t.Fatalf("parse %v: %v", setArgs, err)
	}
	return c
}

// TestAppendDaemonBoolFlagForwardsExplicitDisable is the regression guard for
// the workstation-mode dev-auth bug: `scion server start --dev-auth=false`
// re-exec'd itself as a --foreground child WITHOUT --dev-auth, so the child's
// applyWorkstationDefaults silently re-enabled it. The daemon arg builder must
// forward an explicitly-set flag as --flag=<value> so the disable survives.
func TestAppendDaemonBoolFlagForwardsExplicitDisable(t *testing.T) {
	// Explicit --dev-auth=false → forwarded as --dev-auth=false (the fix).
	c := newBoolFlagCmd(t, "dev-auth", "--dev-auth=false")
	assert.Equal(t, []string{"--dev-auth=false"}, appendDaemonBoolFlag(c, nil, "dev-auth", false))

	// Explicit --dev-auth=true → forwarded as --dev-auth=true.
	c = newBoolFlagCmd(t, "dev-auth", "--dev-auth=true")
	assert.Equal(t, []string{"--dev-auth=true"}, appendDaemonBoolFlag(c, nil, "dev-auth", true))

	// Not set, value true (a workstation default) → historical bare form.
	c = newBoolFlagCmd(t, "dev-auth")
	assert.Equal(t, []string{"--dev-auth"}, appendDaemonBoolFlag(c, nil, "dev-auth", true))

	// Not set, value false → nothing appended (unchanged behavior).
	c = newBoolFlagCmd(t, "dev-auth")
	assert.Nil(t, appendDaemonBoolFlag(c, nil, "dev-auth", false))

	// Appends onto existing args rather than replacing them.
	c = newBoolFlagCmd(t, "enable-web", "--enable-web=false")
	assert.Equal(t, []string{"server", "start", "--enable-web=false"},
		appendDaemonBoolFlag(c, []string{"server", "start"}, "enable-web", false))
}

// TestBuildDaemonStartArgsForwardsExplicitFlags checks that buildDaemonStartArgs
// forwards every explicitly-set flag to the --foreground child — the workstation
// disable (--enable-web=false), the non-workstation bools, and the string/int
// flags that are otherwise lost across the re-exec — while omitting unset ones.
func TestBuildDaemonStartArgsForwardsExplicitFlags(t *testing.T) {
	resetServerFlags()
	// Globals resetServerFlags doesn't cover:
	noAutoMigrate, enableTestLogin, simulateRemoteBroker = false, false, false
	templateCacheDir, webAssetsDir, webBaseURL = "", "", ""
	adminEmails = nil
	templateCacheMax, globalMode = 0, false
	defer func() {
		resetServerFlags()
		noAutoMigrate, enableTestLogin, simulateRemoteBroker = false, false, false
		templateCacheDir, webAssetsDir, webBaseURL = "", "", ""
		adminEmails = nil
		templateCacheMax, globalMode = 0, false
	}()

	c := &cobra.Command{Use: "start", RunE: func(*cobra.Command, []string) error { return nil }}
	f := c.Flags()
	f.BoolVar(&hostedMode, "hosted", false, "")
	f.StringVar(&hubHost, "host", "0.0.0.0", "")
	f.BoolVar(&enableWeb, "enable-web", false, "")
	f.BoolVar(&enableDevAuth, "dev-auth", false, "")
	f.BoolVar(&noAutoMigrate, "no-auto-migrate", false, "")
	f.BoolVar(&enableTestLogin, "enable-test-login", false, "")
	f.BoolVar(&simulateRemoteBroker, "simulate-remote-broker", false, "")
	f.StringVar(&templateCacheDir, "template-cache-dir", "", "")
	f.Int64Var(&templateCacheMax, "template-cache-max", 100*1024*1024, "")
	f.StringVar(&webAssetsDir, "web-assets-dir", "", "")
	f.StringVar(&webSessionSecret, "session-secret", "", "")
	f.StringVar(&webBaseURL, "base-url", "", "")
	f.StringArrayVar(&adminEmails, "admin-emails", nil, "")

	c.SetArgs([]string{
		"--hosted=false",     // explicit mode disable must survive the re-exec
		"--enable-web=false", // explicit workstation-default disable (the core fix)
		"--no-auto-migrate",
		"--simulate-remote-broker=true",
		"--session-secret=topsecret", // set, but must NOT be forwarded (signing secret)
		"--base-url=https://scion.example.com",
		"--admin-emails=a@x.com,b@y.com",
		"--template-cache-max=42",
	})
	if err := c.Execute(); err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := buildDaemonStartArgs(c)

	assert.Equal(t, []string{"server", "start", "--foreground"}, got[:3])
	for _, want := range []string{
		"--hosted=false",
		"--enable-web=false",
		"--no-auto-migrate=true", // helper normalizes bare --no-auto-migrate to =true
		"--simulate-remote-broker=true",
		"--base-url=https://scion.example.com",
		"--admin-emails=a@x.com,b@y.com",
		"--template-cache-max=42",
	} {
		assert.Contains(t, got, want)
	}
	// Unset flags are not forwarded; --host is guarded by Changed; the session
	// secret must never reach the child argv / saved-args file.
	assert.NotContains(t, got, "--enable-test-login")
	for _, a := range got {
		assert.False(t, strings.HasPrefix(a, "--web-assets-dir="), "web-assets-dir omitted when unset")
		assert.False(t, strings.HasPrefix(a, "--template-cache-dir="), "template-cache-dir omitted when unset")
		assert.False(t, strings.HasPrefix(a, "--host="), "host omitted when not explicitly set")
		assert.False(t, strings.HasPrefix(a, "--session-secret"), "session-secret never forwarded")
	}
	assert.NotContains(t, strings.Join(got, " "), "topsecret", "session secret must not leak into args")
}

// TestBuildDaemonStartArgsForwardsExplicitHost guards the positive side of the
// --host fix: an explicitly-set host must still be forwarded (only the
// unconditional default forwarding was removed).
func TestBuildDaemonStartArgsForwardsExplicitHost(t *testing.T) {
	resetServerFlags()
	defer resetServerFlags()

	c := &cobra.Command{Use: "start", RunE: func(*cobra.Command, []string) error { return nil }}
	c.Flags().StringVar(&hubHost, "host", "0.0.0.0", "")
	c.SetArgs([]string{"--host=1.2.3.4"})
	if err := c.Execute(); err != nil {
		t.Fatalf("parse: %v", err)
	}
	assert.Contains(t, buildDaemonStartArgs(c), "--host=1.2.3.4")
}
