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

package cmd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	scionplugin "github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// clearEnvWithPrefixForTest unsets every variable starting with prefix for
// the rest of the test (restored afterwards).
func clearEnvWithPrefixForTest(t *testing.T, prefix string) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, prefix) {
			t.Setenv(k, v) // registers the restore
			require.NoError(t, os.Unsetenv(k))
		}
	}
}

// PS-4: when hub initialization fails, runServerStart logs the same
// operator line, returns a wrapped error instead of calling log.Fatalf, and
// so runs its deferred cleanups (here the step-1 log cleanup). Nothing has
// started serving, the exit status is still 1, and no usage block is shown.
// This drives the real runServerStart on a temp HOME with SQLite; only the
// logging setup and the hub constructor are replaced.
func TestRunServerStart_HubInitFailureReturnsAndRunsDeferredCleanups(t *testing.T) {
	clearEnvWithPrefixForTest(t, "SCION_")
	clearEnvWithPrefixForTest(t, "K_SERVICE")
	home, _ := brokerTestHome(t)
	t.Chdir(home) // no project .scion above the working directory

	savedPin, savedLogging, savedHub := pinProcessUTC, initServerLoggingFn, initHubServerFn
	t.Cleanup(func() { pinProcessUTC, initServerLoggingFn, initHubServerFn = savedPin, savedLogging, savedHub })
	pinProcessUTC = func() {}
	// runServerStart's step 7 subscribes the process to SIGINT and SIGTERM
	// with the real signal.Notify and never unsubscribes (that is what makes
	// a second signal be ignored); restore default handling afterwards so
	// the subscription does not outlive this test. The step-7 signal
	// goroutine itself exits when runServerStart's deferred cancel runs
	// (see the N3 tests in server_shutdown_signals_test.go).
	t.Cleanup(func() { signal.Reset(os.Interrupt, syscall.SIGTERM) })
	resetServerShutdownForTest(t)

	resetServerFlags()
	savedGlobal, savedDebugEndpoints := globalMode, enableDebugEndpoints
	t.Cleanup(func() {
		resetServerFlags()
		globalMode, enableDebugEndpoints = savedGlobal, savedDebugEndpoints
	})
	globalMode, enableDebugEndpoints = false, false

	logs := captureStdLog(t)

	logCleanupRan := false
	initServerLoggingFn = func(*cobra.Command) ([]func(), *slog.Logger, *slog.Logger, error) {
		return []func(){func() { logCleanupRan = true }}, slog.Default(), slog.Default(), nil
	}
	initErr := errors.New("operational settings init failed after retries (driver=sqlite): injected")
	hubCalls := 0
	initHubServerFn = func(context.Context, *config.GlobalConfig, store.Store, *ent.Client, string, string, []string, bool, string, *slog.Logger, *slog.Logger, string, *scionplugin.Manager, secret.SecretBackend) (*hub.Server, error) {
		hubCalls++
		return nil, initErr
	}

	port := unusedPort(t)
	c := &cobra.Command{Use: "start"}
	fl := c.Flags()
	fl.BoolVar(&hostedMode, "hosted", false, "")
	fl.BoolVar(&enableHub, "enable-hub", false, "")
	fl.BoolVar(&enableRuntimeBroker, "enable-runtime-broker", false, "")
	fl.BoolVar(&enableWeb, "enable-web", false, "")
	fl.BoolVar(&enableDevAuth, "dev-auth", false, "")
	fl.BoolVar(&serverAutoProvide, "auto-provide", false, "")
	fl.StringVar(&hubHost, "host", "0.0.0.0", "")
	fl.IntVar(&hubPort, "port", 9810, "")
	fl.StringVar(&dbURL, "db", "", "")
	require.NoError(t, c.ParseFlags([]string{
		"--hosted=false",
		"--enable-hub=true",
		"--enable-runtime-broker=false",
		"--enable-web=false",
		"--dev-auth=false",
		"--auto-provide=false",
		"--host=127.0.0.1",
		"--port=" + strconv.Itoa(port),
		"--db=" + filepath.Join(home, "hub.db"),
	}))

	err := runServerStart(c, nil)

	require.Error(t, err)
	assert.ErrorIs(t, err, initErr, "the returned error wraps the hub init error")
	assert.Equal(t, "hub server failed to start: "+initErr.Error(), err.Error())
	assert.Equal(t, 1, hubCalls, "the hub constructor ran once")
	assert.True(t, logCleanupRan, "deferred log cleanups run (log.Fatalf would have skipped them)")
	assert.Contains(t, logs.String(), "Hub server failed to start: "+initErr.Error())

	// Start was never reached. The proof is structural: the injected
	// constructor returns (nil, err) and runServerStart returns at the
	// hub-init error site, before step 11's Start. The free port below is
	// supporting evidence only (a server that started and shut down would
	// also have freed it).
	l, lerr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, lerr, "the hub port must still be free")
	require.NoError(t, l.Close())

	// Exit status 1, as with log.Fatalf.
	assert.Equal(t, 1, exitCodeFor(err))
	// No usage block: the error is a plain runtime error, not a usageError
	// (asserted directly). The command tree below only models what root's
	// PersistentPreRunE does in Execute (it sets SilenceUsage on the
	// executing subcommand before RunE); it does not run that hook.
	assert.False(t, isUsageError(err), "a hub startup failure is not a usage error")
	parent := &cobra.Command{Use: "server"}
	sub := &cobra.Command{Use: "start", SilenceUsage: true}
	parent.AddCommand(sub)
	assert.False(t, showUsageForError(sub, err, true), "no usage block for a hub startup failure")
}
