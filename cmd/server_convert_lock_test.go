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

//go:build !windows

package cmd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func lockHeldElsewhere(t *testing.T, g string) bool {
	t.Helper()
	h, err := brokerconvert.AcquireHostLock(g)
	if err != nil {
		return true
	}
	h.Release()
	return false
}

// Test (a): while a server start holds the lock (from its step 5b take
// until its broker listener is bound), a convert operation refuses.
func TestServerStartLock_ConvertRefusesUntilListening(t *testing.T) {
	g := t.TempDir()
	lock, err := acquireConvertStartLock(g)
	require.NoError(t, err)

	listening := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		releaseConvertLockWhenListening(ctx, lock, []<-chan struct{}{listening})
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	assert.True(t, lockHeldElsewhere(t, g), "not released before Listening closes")
	_, err = brokerconvert.AcquireHostLock(g)
	assert.ErrorIs(t, err, brokerconvert.ErrHostLockHeld)

	close(listening)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was not released after Listening closed")
	}
	assert.False(t, lockHeldElsewhere(t, g), "released once the listener is bound")
}

// Test (b): a server start refuses while a convert operation holds the
// lock (the try-lock refuses; it never waits).
func TestServerStartLock_RefusesWhileConvertHoldsIt(t *testing.T) {
	g := t.TempDir()
	h, err := brokerconvert.AcquireHostLock(g)
	require.NoError(t, err)
	defer h.Release()

	start := time.Now()
	_, err = acquireConvertStartLock(g)
	assert.ErrorIs(t, err, brokerconvert.ErrHostLockHeld)
	assert.Less(t, time.Since(start), 2*time.Second, "a refusing try-lock, never a blocking wait")
}

// Test (c): after the bind the lock is released, and convert's P-4 sees
// the running server through its own listener.
func TestServerStartLock_AfterBindP4SeesServer(t *testing.T) {
	g := t.TempDir()
	lock, err := acquireConvertStartLock(g)
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	listening := make(chan struct{})
	close(listening)
	releaseConvertLockWhenListening(context.Background(), lock, []<-chan struct{}{listening})

	h, err := brokerconvert.AcquireHostLock(g)
	require.NoError(t, err, "released after the bind")
	defer h.Release()
	port := ln.Addr().(*net.TCPAddr).Port
	assert.True(t, checkPort("127.0.0.1", port).inUse, "P-4 finds the server on its port")
}

// A start whose own listener never binds (it fails) does not release the
// lock from the goroutine: the start's deferred Release frees it as it
// fails, and apply's P-4 still refuses while anything holds the port.
func TestServerStartLock_FailedBindReleasesThroughDefer(t *testing.T) {
	g := t.TempDir()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = busy.Close() }()

	func() {
		lock, err := acquireConvertStartLock(g)
		require.NoError(t, err)
		defer lock.Release() // the start's deferred release

		neverListening := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			releaseConvertLockWhenListening(ctx, lock, []<-chan struct{}{neverListening})
			close(done)
		}()
		time.Sleep(50 * time.Millisecond)
		assert.True(t, lockHeldElsewhere(t, g))
		cancel() // the start fails and stops
		<-done
		assert.True(t, lockHeldElsewhere(t, g), "the goroutine does not release on a failed bind")
	}()
	assert.False(t, lockHeldElsewhere(t, g), "the deferred release frees it")
	assert.True(t, checkPort("127.0.0.1", busy.Addr().(*net.TCPAddr).Port).inUse, "P-4 still refuses: something holds the port")
}

func TestServerStartLock_NoListenersReleasesAtOnce(t *testing.T) {
	g := t.TempDir()
	lock, err := acquireConvertStartLock(g)
	require.NoError(t, err)
	releaseConvertLockWhenListening(context.Background(), lock, nil)
	assert.False(t, lockHeldElsewhere(t, g))
}

func initMachineGlobalDirPerm(t *testing.T) os.FileMode {
	t.Helper()
	refHome := t.TempDir()
	t.Setenv("HOME", refHome)
	require.NoError(t, config.InitMachine(harness.EmbedOnlyHarnesses(), config.InitMachineOpts{SkipRuntimeCheck: true}))
	fi, err := os.Lstat(filepath.Join(refHome, ".scion"))
	require.NoError(t, err)
	return fi.Mode().Perm()
}

// ptone/scion#8437: on a clean HOME with no ~/.scion directory, acquiring the
// server start convert lock creates ~/.scion with the same permissions as
// `scion init --machine` while keeping runtime-brokers/.convert owner-only.
func TestServerStartLock_CleanHomeCreatesGlobalDir(t *testing.T) {
	wantPerm := initMachineGlobalDirPerm(t)

	t.Run("missing ~/.scion is created with init --machine permissions", func(t *testing.T) {
		home := t.TempDir()
		globalDir := filepath.Join(home, ".scion")
		_, err := os.Lstat(globalDir)
		require.ErrorIs(t, err, os.ErrNotExist)

		lock, err := acquireConvertStartLock(globalDir)
		require.NoError(t, err)
		t.Cleanup(func() { lock.Release() })

		fi, err := os.Lstat(globalDir)
		require.NoError(t, err)
		assert.True(t, fi.IsDir())
		assert.Equal(t, wantPerm, fi.Mode().Perm(), "~/.scion permissions must match scion init --machine")
		assert.Zero(t, fi.Mode().Perm()&0o022, "~/.scion must not be group- or world-writable")

		convertInfo, err := os.Lstat(brokerconvert.ConvertDir(globalDir))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), convertInfo.Mode().Perm(), "convert state directory must remain owner-only")
	})

	t.Run("regular file at ~/.scion fails closed", func(t *testing.T) {
		home := t.TempDir()
		globalDir := filepath.Join(home, ".scion")
		require.NoError(t, os.WriteFile(globalDir, []byte("not-a-dir"), 0o600))

		lock, err := acquireConvertStartLock(globalDir)
		require.Error(t, err)
		assert.Nil(t, lock)
	})
}

func unsetEnvWithPrefix(t *testing.T, prefix string) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, prefix) {
			t.Setenv(k, v)
			require.NoError(t, os.Unsetenv(k))
		}
	}
}

// ptone/scion#8437: bare `scion server start` (daemon mode, workstation
// defaults) on an empty HOME with no ~/.scion succeeds through pre-spawn setup,
// creates ~/.scion with `scion init --machine` permissions, releases the
// convert host lock before spawning the child, and leaves settings.yaml absent
// so onboarding detection still sees a fresh home.
func TestRunServerStartOrDaemon_CleanHome(t *testing.T) {
	unsetEnvWithPrefix(t, "SCION_")
	unsetEnvWithPrefix(t, "K_SERVICE")
	isolateServerSecretEnv(t)

	wantPerm := initMachineGlobalDirPerm(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	globalDir := filepath.Join(home, ".scion")
	_, err := os.Lstat(globalDir)
	require.ErrorIs(t, err, os.ErrNotExist)

	resetServerFlags()
	savedGlobal, savedDebugEndpoints, savedSpawn := globalMode, enableDebugEndpoints, startServerDaemonFn
	t.Cleanup(func() {
		resetServerFlags()
		globalMode, enableDebugEndpoints, startServerDaemonFn = savedGlobal, savedDebugEndpoints, savedSpawn
	})
	globalMode, enableDebugEndpoints = false, false

	var ports [3]int
	for i := range ports {
		l, listenErr := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, listenErr)
		ports[i] = l.Addr().(*net.TCPAddr).Port
		require.NoError(t, l.Close())
	}
	webPort, hubPort, runtimeBrokerPort = ports[0], ports[1], ports[2]
	serverStartForeground = false

	errStopBeforeSpawn := errors.New("stop before daemon spawn")
	spawnCalls := 0
	startServerDaemonFn = func(component, executable string, args []string, dir string) error {
		spawnCalls++
		assert.Equal(t, serverDaemonComponent, component)
		assert.Equal(t, globalDir, dir)
		assert.Empty(t, config.GetSettingsPath(dir), "settings.yaml must not exist yet before the daemon child starts")
		return errStopBeforeSpawn
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
	require.NoError(t, c.ParseFlags(nil))

	_ = captureStdout(t, func() {
		err = runServerStartOrDaemon(c, nil)
	})
	require.ErrorIs(t, err, errStopBeforeSpawn, "server start setup on a clean HOME must reach daemon launch without failing on missing ~/.scion")
	assert.Equal(t, 1, spawnCalls)

	fi, err := os.Lstat(globalDir)
	require.NoError(t, err)
	assert.True(t, fi.IsDir())
	assert.Equal(t, wantPerm, fi.Mode().Perm(), "~/.scion permissions must match scion init --machine")
	assert.False(t, lockHeldElsewhere(t, globalDir), "daemon parent must release the convert lock before spawning the child")
}
