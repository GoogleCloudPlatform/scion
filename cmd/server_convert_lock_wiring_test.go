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
	"syscall"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	scionplugin "github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The server-start wiring of the convert host lock (AC10): runServerStart,
// when it hosts the Runtime Broker, takes the lock before its port check
// and returns the lock's refusal as is. The lock call is replaced by a stub
// that returns the refusal and counts its calls; the hub port is held, so a
// start that skipped the lock would fail at the port check instead (and
// nothing can start).
func TestRunServerStart_ConvertLockRefusesBeforePortCheck(t *testing.T) {
	for _, refusal := range []error{brokerconvert.ErrHostLockHeld, brokerconvert.ErrConvertInterrupted} {
		t.Run(refusal.Error(), func(t *testing.T) {
			clearEnvWithPrefixForTest(t, "SCION_")
			clearEnvWithPrefixForTest(t, "K_SERVICE")
			isolateServerSecretEnv(t)
			home, _ := brokerTestHome(t)
			t.Chdir(home)

			savedPin, savedLogging, savedHub, savedLock := pinProcessUTC, initServerLoggingFn, initHubServerFn, acquireConvertStartLock
			t.Cleanup(func() {
				pinProcessUTC, initServerLoggingFn, initHubServerFn, acquireConvertStartLock = savedPin, savedLogging, savedHub, savedLock
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
			initHubServerFn = func(context.Context, *config.GlobalConfig, store.Store, *ent.Client, hub.HubEndpoints, string, []string, bool, string, *slog.Logger, *slog.Logger, *slog.Logger, string, *scionplugin.Manager, secret.SecretBackend) (*hub.Server, error) {
				return nil, errors.New("the hub must not be reached")
			}
			lockCalls := 0
			acquireConvertStartLock = func(string) (*brokerconvert.HostLock, error) {
				lockCalls++
				return nil, refusal
			}

			// The hub port is held for the whole test.
			busy, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = busy.Close() })
			port := busy.Addr().(*net.TCPAddr).Port

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
				"--enable-runtime-broker=true",
				"--enable-web=false",
				"--dev-auth=false",
				"--auto-provide=false",
				"--host=127.0.0.1",
				"--port=" + strconv.Itoa(port),
				"--db=" + filepath.Join(home, "hub.db"),
			}))

			err = runServerStart(c, nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, refusal, "the lock's refusal is returned, not a port error")
			assert.Equal(t, 1, lockCalls, "the lock is taken once, before the port check")
		})
	}
}

// The daemon path (runServerStartOrDaemon without --foreground) runs the
// same lock check before it re-executes itself, so a refusal is returned
// directly instead of as a failed daemon start. The stub counts the call;
// the ports are free, so the refusal is the only thing that stops the run
// before the executable lookup and the spawn.
func TestRunServerStartOrDaemon_ConvertLockRefusesBeforeSpawn(t *testing.T) {
	for _, refusal := range []error{brokerconvert.ErrHostLockHeld, brokerconvert.ErrConvertInterrupted} {
		t.Run(refusal.Error(), func(t *testing.T) {
			clearEnvWithPrefixForTest(t, "SCION_")
			clearEnvWithPrefixForTest(t, "K_SERVICE")
			isolateServerSecretEnv(t)
			home, _ := brokerTestHome(t)
			t.Chdir(home)

			savedLock := acquireConvertStartLock
			t.Cleanup(func() { acquireConvertStartLock = savedLock })
			resetServerFlags()
			savedGlobal, savedDebugEndpoints := globalMode, enableDebugEndpoints
			t.Cleanup(func() {
				resetServerFlags()
				globalMode, enableDebugEndpoints = savedGlobal, savedDebugEndpoints
			})
			globalMode, enableDebugEndpoints = false, false
			lockCalls := 0
			acquireConvertStartLock = func(string) (*brokerconvert.HostLock, error) {
				lockCalls++
				return nil, refusal
			}

			// Three ports that were free a moment ago, so the phantom-port
			// check passes.
			var ports [3]int
			for i := range ports {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				ports[i] = l.Addr().(*net.TCPAddr).Port
				require.NoError(t, l.Close())
			}
			webPort, hubPort, runtimeBrokerPort = ports[0], ports[1], ports[2]
			serverStartForeground = false

			c := &cobra.Command{Use: "start"}
			fl := c.Flags()
			fl.BoolVar(&hostedMode, "hosted", false, "")
			fl.BoolVar(&enableHub, "enable-hub", false, "")
			fl.BoolVar(&enableRuntimeBroker, "enable-runtime-broker", false, "")
			fl.BoolVar(&enableWeb, "enable-web", false, "")
			require.NoError(t, c.ParseFlags([]string{
				"--hosted=false",
				"--enable-hub=false",
				"--enable-runtime-broker=true",
				"--enable-web=false",
			}))

			err := runServerStartOrDaemon(c, nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, refusal, "the lock's refusal is returned before the spawn")
			assert.Equal(t, 1, lockCalls, "the daemon parent checks the lock once")
		})
	}
}
