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
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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

// TestRunServerStart_ScrubsSecretEnvBeforeFirstChild pins the main server
// path's ordering constraint: scrubServerSecretEnv (and the keyring load)
// run before the first step that can start a child process, the plugin
// manager. It drives the real runServerStart and observes the process env
// at that step, iterating serverSecretEnvNames so every future addition is
// covered, in legacy mode (session and peer secrets in the env) and in
// keyring mode (the keyring file path in the env). The relay role has the
// equivalent observation point at database open
// (TestRunRelayRoleUnsetsServerSecretEnv).
func TestRunServerStart_ScrubsSecretEnvBeforeFirstChild(t *testing.T) {
	for _, mode := range []string{"legacy", "keyring"} {
		t.Run(mode, func(t *testing.T) {
			clearEnvWithPrefixForTest(t, "SCION_")
			clearEnvWithPrefixForTest(t, "K_SERVICE")
			isolateServerSecretEnv(t)
			isolateKeyring(t)
			home, _ := brokerTestHome(t)
			t.Chdir(home)

			keyringNames := map[string]bool{keyringFileEnv: true, keyringFileAliasEnv: true}
			set := 0
			for _, name := range serverSecretEnvNames {
				switch {
				case keyringNames[name] && mode == "keyring":
					// A keyring file plus a legacy secret refuses startup,
					// so keyring mode carries only the keyring names (and
					// the peer secret below).
					t.Setenv(name, writeCmdKeyring(t, nil))
				case keyringNames[name]:
					continue
				case mode == "keyring" && (name == sessionSecretEnv || name == legacySessionSecretEnv):
					continue
				case name == peerSecretEnv || name == underscoredPeerSecretEnv:
					// Both spellings of one setting carry one value.
					t.Setenv(name, "test-only-peer-secret-value-0123456789abcdefghij")
				default:
					t.Setenv(name, "test-only-value-0123456789abcdefghijklmnop-"+name)
				}
				set++
			}
			require.NotZero(t, set)

			savedPin, savedLogging, savedHub, savedPlugins := pinProcessUTC, initServerLoggingFn, initHubServerFn, initPluginManagerFn
			t.Cleanup(func() {
				pinProcessUTC, initServerLoggingFn, initHubServerFn, initPluginManagerFn = savedPin, savedLogging, savedHub, savedPlugins
			})
			pinProcessUTC = func() {}
			t.Cleanup(func() { signal.Reset(os.Interrupt, syscall.SIGTERM) })
			resetServerShutdownForTest(t)
			resetServerFlags()
			savedGlobal, savedDebugEndpoints := globalMode, enableDebugEndpoints
			t.Cleanup(func() {
				resetServerFlags()
				globalMode, enableDebugEndpoints = savedGlobal, savedDebugEndpoints
			})
			globalMode, enableDebugEndpoints = false, false

			initServerLoggingFn = func(*cobra.Command) ([]func(), *slog.Logger, *slog.Logger, *slog.Logger, error) {
				return nil, slog.Default(), slog.Default(), slog.Default(), nil
			}
			observed := false
			var present []string
			initPluginManagerFn = func(ctx context.Context, sb secret.SecretBackend, s store.Store) *scionplugin.Manager {
				observed = true
				for _, name := range serverSecretEnvNames {
					if _, ok := os.LookupEnv(name); ok {
						present = append(present, name)
					}
				}
				return savedPlugins(ctx, sb, s)
			}
			errStop := errors.New("stop after the first child-capable step")
			initHubServerFn = func(context.Context, *config.GlobalConfig, store.Store, *ent.Client, hub.HubEndpoints, string, []string, bool, string, *slog.Logger, *slog.Logger, *slog.Logger, string, *scionplugin.Manager, secret.SecretBackend) (*hub.Server, error) {
				return nil, errStop
			}

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
				"--port=" + strconv.Itoa(unusedPort(t)),
				"--db=" + filepath.Join(home, "hub.db"),
			}))

			err := runServerStart(c, nil)
			require.ErrorIs(t, err, errStop, "runServerStart must reach the hub constructor")
			require.True(t, observed, "the plugin manager step was not reached")
			assert.Empty(t, present, "server secret env names still set when the first child can start")
			if mode == "keyring" {
				assert.NotNil(t, serverKeyringFromFile(), "the keyring was loaded before the first child-capable step")
			}
		})
	}
}
