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

//go:build linux

package plugin

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeFakePluginSource writes a minimal stdlib-only Go module under
// <repoDir>/extras/scion-<name>/cmd/scion-plugin-<name> that execs the current
// test binary in fake-broker helper mode. It has zero external dependencies so
// go mod tidy and go build run offline.
func writeFakePluginSource(t *testing.T, repoDir, name string) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)

	modDir := filepath.Join(repoDir, "extras", "scion-"+name)
	cmdDir := filepath.Join(modDir, "cmd", "scion-plugin-"+name)
	require.NoError(t, os.MkdirAll(cmdDir, 0o755))

	goMod := fmt.Sprintf("module example.com/scion-%s\n\ngo 1.21\n", name)
	require.NoError(t, os.WriteFile(filepath.Join(modDir, "go.mod"), []byte(goMod), 0o644))

	mainGo := fmt.Sprintf(`package main

import (
	"os"
	"syscall"
)

func main() {
	env := append(os.Environ(), %q)
	_ = syscall.Exec(%q, os.Args, env)
}
`, supervisorHelperEnv+"=1", exe)
	require.NoError(t, os.WriteFile(filepath.Join(cmdDir, "main.go"), []byte(mainGo), 0o644))
}

// writeFakePluginWrapper writes a shell wrapper script at path that execs the
// current test binary in fake-broker helper mode.
func writeFakePluginWrapper(t *testing.T, path string) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	script := fmt.Sprintf("#!/bin/sh\nexport %s=1\nexec %q \"$@\"\n", supervisorHelperEnv, exe)
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
}

func TestUpdatePlugin_UnwritablePathDir_RelocatesToUserPluginsDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 0555 does not prevent writes")
	}

	// Simulate /usr/local/bin owned by root (read/exec only for hub user).
	systemBinDir := filepath.Join(t.TempDir(), "usr-local-bin")
	require.NoError(t, os.MkdirAll(systemBinDir, 0o755))
	systemBinary := filepath.Join(systemBinDir, "scion-plugin-fake")
	writeFakePluginWrapper(t, systemBinary)
	require.NoError(t, os.Chmod(systemBinDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(systemBinDir, 0o755) })

	t.Setenv("PATH", systemBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	pluginsDir := filepath.Join(t.TempDir(), "plugins")
	repoDir := t.TempDir()
	writeFakePluginSource(t, repoDir, "fake")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := NewManager(logger)
	t.Cleanup(mgr.Shutdown)

	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"fake": {
				Config: map[string]string{"token": "initial-token"},
			},
		},
	}
	require.NoError(t, mgr.LoadAll(cfg, pluginsDir))

	before := fakeHealth(t, mgr, "fake")
	require.Equal(t, "initial-token", before["token"])

	// UpdatePlugin should succeed by installing into <pluginsDir>/broker/scion-plugin-fake
	// rather than failing with permission denied on systemBinDir.
	err := mgr.UpdatePlugin("fake", repoDir)
	require.NoError(t, err)

	expectedInstalledPath := filepath.Join(pluginsDir, PluginTypeBroker, "scion-plugin-fake")
	info, statErr := os.Stat(expectedInstalledPath)
	require.NoError(t, statErr, "rebuilt binary must be installed into user plugins directory")
	assert.NotZero(t, info.Mode()&0o111, "installed binary must be executable")

	after := fakeHealth(t, mgr, "fake")
	assert.NotEqual(t, before["pid"], after["pid"], "plugin process must restart on updated binary")
	assert.Equal(t, "initial-token", after["token"])

	// Simulate a subsequent Hub restart: LoadAll must prefer <pluginsDir>/broker
	// over the stale binary on PATH.
	mgr2 := NewManager(logger)
	t.Cleanup(mgr2.Shutdown)
	require.NoError(t, mgr2.LoadAll(cfg, pluginsDir))
	mgr2.mu.RLock()
	reloadedPath := mgr2.configs["broker:fake"].Path
	mgr2.mu.RUnlock()
	assert.Equal(t, expectedInstalledPath, reloadedPath)
}

func TestUpdatePlugin_ExplicitUnwritablePath_FailsClearlyWithoutStoppingPlugin(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 0555 does not prevent writes")
	}

	systemBinDir := filepath.Join(t.TempDir(), "usr-local-bin")
	require.NoError(t, os.MkdirAll(systemBinDir, 0o755))
	systemBinary := filepath.Join(systemBinDir, "scion-plugin-fake")
	writeFakePluginWrapper(t, systemBinary)
	require.NoError(t, os.Chmod(systemBinDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(systemBinDir, 0o755) })

	pluginsDir := filepath.Join(t.TempDir(), "plugins")
	repoDir := t.TempDir()
	writeFakePluginSource(t, repoDir, "fake")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := NewManager(logger)
	t.Cleanup(mgr.Shutdown)

	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"fake": {
				Path:   systemBinary,
				Config: map[string]string{"token": "initial-token"},
			},
		},
	}
	require.NoError(t, mgr.LoadAll(cfg, pluginsDir))
	before := fakeHealth(t, mgr, "fake")

	err := mgr.UpdatePlugin("fake", repoDir)
	require.Error(t, err)

	var permErr *PermissionError
	require.ErrorAs(t, err, &permErr)
	assert.ErrorIs(t, err, os.ErrPermission)
	assert.True(t, permErr.ExplicitPath)
	assert.Equal(t, systemBinary, permErr.BinaryPath)
	assert.Contains(t, permErr.UserMessage(), systemBinary)
	assert.Contains(t, permErr.Guidance(repoDir), "server.plugins.broker.fake.path")
	assert.Contains(t, permErr.Guidance(repoDir), "sudo install -m 0755")

	// Must not write to <pluginsDir>/broker because explicit path in settings
	// would shadow it on the next Hub restart.
	_, statErr := os.Stat(filepath.Join(pluginsDir, PluginTypeBroker, "scion-plugin-fake"))
	assert.True(t, os.IsNotExist(statErr))

	// Running plugin must remain alive with the same PID.
	after := fakeHealth(t, mgr, "fake")
	assert.Equal(t, before["pid"], after["pid"])
}

func TestUpdatePlugin_UnwritableFallbackPluginsDir_FailsClearlyWithoutStoppingPlugin(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 0555 does not prevent writes")
	}

	systemBinDir := filepath.Join(t.TempDir(), "usr-local-bin")
	require.NoError(t, os.MkdirAll(systemBinDir, 0o755))
	systemBinary := filepath.Join(systemBinDir, "scion-plugin-fake")
	writeFakePluginWrapper(t, systemBinary)
	require.NoError(t, os.Chmod(systemBinDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(systemBinDir, 0o755) })

	t.Setenv("PATH", systemBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	pluginsDir := filepath.Join(t.TempDir(), "plugins")
	require.NoError(t, os.MkdirAll(pluginsDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(pluginsDir, 0o755) })

	repoDir := t.TempDir()
	writeFakePluginSource(t, repoDir, "fake")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := NewManager(logger)
	t.Cleanup(mgr.Shutdown)

	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"fake": {
				Config: map[string]string{"token": "initial-token"},
			},
		},
	}
	require.NoError(t, mgr.LoadAll(cfg, pluginsDir))
	before := fakeHealth(t, mgr, "fake")

	err := mgr.UpdatePlugin("fake", repoDir)
	require.Error(t, err)

	var permErr *PermissionError
	require.ErrorAs(t, err, &permErr)
	assert.ErrorIs(t, err, os.ErrPermission)
	assert.False(t, permErr.ExplicitPath)
	assert.Contains(t, permErr.UserMessage(), filepath.Join(pluginsDir, PluginTypeBroker))
	assert.Contains(t, permErr.Guidance(repoDir), "sudo install -d -m 0755")

	after := fakeHealth(t, mgr, "fake")
	assert.Equal(t, before["pid"], after["pid"])
}

func TestUpdatePlugin_WritablePath_UpdatesInPlace(t *testing.T) {
	pluginsDir := filepath.Join(t.TempDir(), "plugins")
	brokerDir := filepath.Join(pluginsDir, PluginTypeBroker)
	require.NoError(t, os.MkdirAll(brokerDir, 0o755))
	binaryPath := filepath.Join(brokerDir, "scion-plugin-fake")
	writeFakePluginWrapper(t, binaryPath)

	repoDir := t.TempDir()
	writeFakePluginSource(t, repoDir, "fake")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := NewManager(logger)
	t.Cleanup(mgr.Shutdown)

	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"fake": {
				Config: map[string]string{"token": "initial-token"},
			},
		},
	}
	require.NoError(t, mgr.LoadAll(cfg, pluginsDir))
	before := fakeHealth(t, mgr, "fake")

	require.NoError(t, mgr.UpdatePlugin("fake", repoDir))

	after := fakeHealth(t, mgr, "fake")
	assert.NotEqual(t, before["pid"], after["pid"])

	entries, err := os.ReadDir(brokerDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "scion-plugin-fake", entries[0].Name())
}
