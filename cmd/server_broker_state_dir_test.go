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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestBrokerStateDir: server.broker.state_dir is a base directory; the
// legacy broker's state directory is <base>/<broker-id> (ptone/scion#4668 C2).
func TestBrokerStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := []struct {
		name, setting, brokerID, want string
	}{
		{"empty keeps the broker default", "", "b-1", ""},
		{"blank keeps the broker default", "  ", "b-1", ""},
		{"absolute", "/srv/scion/broker-state", "b-1", "/srv/scion/broker-state/b-1"},
		{"absolute cleaned", "/srv/scion//broker-state/", "b-1", "/srv/scion/broker-state/b-1"},
		{"home prefix", "~/broker-state", "b-1", filepath.Join(home, "broker-state", "b-1")},
		{"bare home", "~", "b-1", filepath.Join(home, "b-1")},
		{"empty broker ID", "/srv/state", "", "/srv/state/default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := brokerStateDir(tc.setting, tc.brokerID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got, "state dir is not <base>/<broker-id>")
		})
	}
	for _, bad := range []string{"relative/state", "./state", "~other/state"} {
		_, err := brokerStateDir(bad, "b-1")
		require.Error(t, err, "relative state_dir %q accepted by brokerStateDir", bad)
		assert.Contains(t, err.Error(), "server.broker.state_dir", bad)
	}
}

// TestBrokerStateDir_BrokerIDPathElement: under a configured state_dir, a
// broker ID that is not a single path element is an error, not a path;
// with no state_dir the broker's own default (which checks the ID too) is
// left in place.
func TestBrokerStateDir_BrokerIDPathElement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for name, id := range brokerIDsNotPathElement {
		if id == "" {
			continue // "default"
		}
		t.Run(name, func(t *testing.T) {
			got, err := brokerStateDir("/srv/broker-state", id)
			require.Error(t, err, "configured state dir accepted broker ID %q: %q", id, got)
			assert.True(t, errors.Is(err, brokercredentials.ErrBrokerIDNotPathElement))
			assert.Empty(t, got)
			got, err = brokerStateDir("", id)
			require.NoError(t, err)
			assert.Empty(t, got, "no state_dir leaves the broker default")
		})
	}
}

// TestValidateBrokerStateDir: the server preflight rejects a relative
// state_dir and accepts an absolute or empty one.
func TestValidateBrokerStateDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := func(dir string) *config.GlobalConfig {
		c := config.DefaultGlobalConfig()
		c.RuntimeBroker.StateDir = dir
		return &c
	}
	err := validateBrokerStateDir(cfg("var/broker-state"))
	require.Error(t, err, "preflight accepted a relative state_dir")
	assert.Contains(t, err.Error(), "server.broker.state_dir")
	assert.Contains(t, err.Error(), "absolute")
	assert.NoError(t, validateBrokerStateDir(cfg("/var/broker-state")))
	assert.NoError(t, validateBrokerStateDir(cfg("~/broker-state")))
	assert.NoError(t, validateBrokerStateDir(cfg("")))
	assert.NoError(t, validateBrokerStateDir(nil))
}

// TestFlatInstancesStateDirWarning: flat instances keep their default
// state directories, so a set state_dir is warned about.
func TestFlatInstancesStateDirWarning(t *testing.T) {
	c := config.DefaultGlobalConfig()
	assert.Empty(t, flatInstancesStateDirWarning(&c))
	assert.Empty(t, flatInstancesStateDirWarning(nil))
	c.RuntimeBroker.StateDir = "/srv/state"
	w := flatInstancesStateDirWarning(&c)
	assert.Contains(t, w, "server.broker.state_dir", "no flat-instance warning for a set state_dir")
	assert.Contains(t, w, "flat Runtime Broker instances")
}

// TestBrokerLocalStatePathsFor_ConfiguredStateDir: deregister
// --purge-local removes the state directory under the configured
// server.broker.state_dir, and the default one when it is unset. When the
// settings cannot be loaded or the value is invalid, the paths hold the
// default directory only as a guess and an error says why, so the purge
// can refuse (fail closed).
func TestBrokerLocalStatePathsFor_ConfiguredStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	defaultState := filepath.Join(home, ".scion", "runtime-broker-state", "b-1")
	setConfig := func(t *testing.T, dir string, err error) {
		prev := loadBrokerStateConfig
		t.Cleanup(func() { loadBrokerStateConfig = prev })
		loadBrokerStateConfig = func(string) (*config.GlobalConfig, error) {
			if err != nil {
				return nil, err
			}
			c := config.DefaultGlobalConfig()
			c.RuntimeBroker.StateDir = dir
			return &c, nil
		}
	}

	t.Run("configured", func(t *testing.T) {
		setConfig(t, "/srv/broker-state", nil)
		paths, err := brokerLocalStatePathsFor(globalDir, "b-1")
		require.NoError(t, err)
		assert.Contains(t, paths, "/srv/broker-state/b-1", "purge-local ignores the configured state_dir")
		assert.NotContains(t, paths, defaultState)
	})
	t.Run("unset", func(t *testing.T) {
		setConfig(t, "", nil)
		paths, err := brokerLocalStatePathsFor(globalDir, "b-1")
		require.NoError(t, err)
		assert.Contains(t, paths, defaultState, "purge-local did not use the default state dir")
	})
	for name, tc := range map[string]struct {
		dir     string
		err     error
		errText string
	}{
		"invalid":     {"relative", nil, `server.broker.state_dir "relative" must be an absolute path`},
		"load failed": {"", errors.New("boom"), "server.broker.state_dir could not be read"},
	} {
		t.Run(name, func(t *testing.T) {
			setConfig(t, tc.dir, tc.err)
			paths, err := brokerLocalStatePathsFor(globalDir, "b-1")
			require.Error(t, err, "unresolved state_dir fell back silently")
			assert.Contains(t, err.Error(), tc.errText)
			assert.Contains(t, paths, defaultState, "the default state dir is not listed as the guess")
		})
	}
}

// TestCleanupAfterDeregister_StateDirUnresolved: with server.broker.state_dir
// unresolved, --purge-local refuses, removes nothing and returns an error
// that names the cause; a plain deregister warns and lists the default
// path with a note.
func TestCleanupAfterDeregister_StateDirUnresolved(t *testing.T) {
	stateErr := errors.New(`server.broker.state_dir "relative" must be an absolute path or start with ~/`)
	t.Run("purge refuses", func(t *testing.T) {
		home, paths := brokerStateFixture(t)
		var out bytes.Buffer
		err := cleanupAfterDeregister(&out, filepath.Join(home, "hub-credentials"), nil, nil, stateErr, false, true, paths)
		require.Error(t, err, "purge-local ran with an unresolved state_dir")
		assert.Contains(t, err.Error(), "cannot resolve the broker state directory")
		assert.Contains(t, err.Error(), `"relative"`)
		assert.Contains(t, out.String(), "listed is the default", "the refusal does not mark the listed state dir as a guess")
		assert.FileExists(t, filepath.Join(home, "runtime-broker-state", "b-1", "state.db"), "purge removed the state dir")
		assert.FileExists(t, filepath.Join(home, "broker.log"), "purge removed the broker log")
	})
	t.Run("plain deregister warns", func(t *testing.T) {
		home, paths := brokerStateFixture(t)
		var out bytes.Buffer
		require.NoError(t, cleanupAfterDeregister(&out, filepath.Join(home, "hub-credentials"), nil, nil, stateErr, false, false, paths))
		assert.Contains(t, out.String(), "Warning: cannot resolve the broker state directory", "no warning for an unresolved state_dir")
		assert.Contains(t, out.String(), "listed is the default")
		assert.Contains(t, out.String(), "Fix server.broker.state_dir, then remove it")
		assert.NotContains(t, out.String(), "\nRemove it with")
		assert.Contains(t, out.String(), filepath.Join(home, "runtime-broker-state", "b-1"))
		assert.FileExists(t, filepath.Join(home, "runtime-broker-state", "b-1", "state.db"))
	})
}
