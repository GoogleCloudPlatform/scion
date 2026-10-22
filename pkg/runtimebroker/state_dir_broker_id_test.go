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

package runtimebroker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// stateDirBadBrokerIDs are broker IDs that are not a single path element.
var stateDirBadBrokerIDs = map[string]string{
	"dot":           ".",
	"dot dot":       "..",
	"slash":         "/",
	"absolute":      "/abs",
	"nested":        "a/b",
	"parent prefix": "../x",
	"inner dot dot": "a..b",
	"backslash":     `a\b`,
	"nul":           "a\x00b",
}

// TestDefaultStateDir_BrokerIDPathElement: the default state directory is
// <home>/.scion/runtime-broker-state/<broker-id>, and a broker ID that is
// not a single path element is an error, not a path.
func TestDefaultStateDir_BrokerIDPathElement(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for name, id := range stateDirBadBrokerIDs {
		t.Run(name, func(t *testing.T) {
			dir, err := DefaultStateDir(id)
			require.Error(t, err, "DefaultStateDir accepted broker ID %q", id)
			assert.True(t, errors.Is(err, brokercredentials.ErrBrokerIDNotPathElement))
			assert.Empty(t, dir)
		})
	}
	dir, err := DefaultStateDir("")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".scion", "runtime-broker-state", "default"), dir)
	dir, err = DefaultStateDir("11111111-2222-3333-4444-555555555555")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".scion", "runtime-broker-state", "11111111-2222-3333-4444-555555555555"), dir)
}

// TestNew_BrokerIDNotPathElementRefusesStart: a server whose broker ID
// cannot name its default state directory has no state directory and
// refuses to start; an explicit StateDir is used as is.
func TestNew_BrokerIDNotPathElementRefusesStart(t *testing.T) {
	setupTestScionEnv(t)
	rt := &runtime.MockRuntime{NameFunc: func() string { return "mock" }}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "../x"
	cfg.ForceRuntime = "mock"
	srv := New(cfg, &mockManager{}, rt, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, srv)
	assert.Empty(t, srv.stateDir, "a state directory was derived from the broker ID")
	err := srv.startServices(context.Background(), false)
	require.Error(t, err, "server started with a broker ID that is not a path element")
	assert.True(t, errors.Is(err, brokercredentials.ErrBrokerIDNotPathElement), "start was not refused for the broker ID: %v", err)

	explicit := t.TempDir()
	cfg.StateDir = explicit
	srv = New(cfg, &mockManager{}, rt, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, srv)
	assert.Equal(t, explicit, srv.stateDir)
	assert.NoError(t, srv.stateDirSetupErr)
}
