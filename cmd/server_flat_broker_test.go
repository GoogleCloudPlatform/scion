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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// flatTestHub returns a Hub server over s with hub.flat_runtime_brokers set
// as requested.
func flatTestHub(t *testing.T, s store.Store, experimentOn bool) *hub.Server {
	t.Helper()
	ctx := context.Background()
	_, err := s.UpsertHubSetting(ctx, "experiments",
		json.RawMessage(fmt.Sprintf(`{"overrides":{%q:%t}}`, experiments.FlatRuntimeBrokers, experimentOn)), "test", -1, "managed")
	require.NoError(t, err)
	srv, err := hub.New(hub.ServerConfig{}, s)
	require.NoError(t, err)
	ops := hub.NewOperationalSettings(s, koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	return srv
}

func flatTestInstance() config.V1RuntimeBrokerInstanceConfig {
	return config.V1RuntimeBrokerInstanceConfig{Key: "local-docker", Name: "example-docker",
		RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker", DisplayName: "Local Docker"}}
}

func fakeDockerProbe(daemonID string, err error) func(context.Context, string) (brokeridentity.ExecutionScope, error) {
	return func(context.Context, string) (brokeridentity.ExecutionScope, error) {
		if err != nil {
			return brokeridentity.ExecutionScope{}, err
		}
		return brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: daemonID, Endpoint: "unix:///var/run/docker.sock"}}, nil
	}
}

func TestPrepareFlatInstance_RegistersBoundRow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	srv := flatTestHub(t, s, true)
	globalDir := t.TempDir()

	prep, err := prepareFlatInstance(ctx, srv, runtime.NewDockerRuntime(), flatTestInstance(), nil, globalDir,
		hub.EmbeddedFlatRegistrationOptions{Endpoint: "http://localhost:9800"}, fakeDockerProbe("daemon-1", nil))
	require.NoError(t, err)
	require.NotNil(t, prep.row.RuntimeTarget)
	assert.Equal(t, prep.identity.RuntimeBrokerID, prep.row.ID)
	assert.Equal(t, prep.identity.RuntimeTarget.ID, prep.row.RuntimeTarget.ID)
	assert.Equal(t, "Local Docker", prep.row.RuntimeTarget.DisplayName)
	assert.Empty(t, prep.row.Profiles)

	// The identity persists: a restart registers the same Runtime Broker.
	again, err := prepareFlatInstance(ctx, srv, runtime.NewDockerRuntime(), flatTestInstance(), nil, globalDir,
		hub.EmbeddedFlatRegistrationOptions{Endpoint: "http://localhost:9800"}, fakeDockerProbe("daemon-1", nil))
	require.NoError(t, err)
	assert.Equal(t, prep.identity.RuntimeBrokerID, again.row.ID)

	// No provider link or default for the flat row.
	global, err := s.GetProjectBySlug(ctx, "global")
	require.NoError(t, err)
	assert.NotEqual(t, prep.row.ID, global.DefaultRuntimeBrokerID)
	providers, err := s.GetProjectProviders(ctx, global.ID)
	require.NoError(t, err)
	for _, p := range providers {
		assert.NotEqual(t, prep.row.ID, p.BrokerID)
	}
}

func TestPrepareFlatInstance_RefusalsAreNotActivated(t *testing.T) {
	ctx := context.Background()
	opts := hub.EmbeddedFlatRegistrationOptions{Endpoint: "http://localhost:9800"}

	t.Run("experiment off at first boot", func(t *testing.T) {
		s := newTestStore(t)
		srv := flatTestHub(t, s, false)
		_, err := prepareFlatInstance(ctx, srv, runtime.NewDockerRuntime(), flatTestInstance(), nil, t.TempDir(), opts, fakeDockerProbe("daemon-1", nil))
		require.Error(t, err)
		assert.Contains(t, err.Error(), hub.ErrCodeExperimentDisabled)
		_, legacyErr := s.GetLegacyRuntimeBrokerByName(ctx, "example-docker")
		assert.ErrorIs(t, legacyErr, store.ErrNotFound, "no legacy fallback row")
	})

	t.Run("unidentified docker daemon", func(t *testing.T) {
		s := newTestStore(t)
		srv := flatTestHub(t, s, true)
		globalDir := t.TempDir()
		_, err := prepareFlatInstance(ctx, srv, runtime.NewDockerRuntime(), flatTestInstance(), nil, globalDir, opts,
			fakeDockerProbe("", fmt.Errorf("%w: no daemon", brokeridentity.ErrExecutionScopeUnidentified)))
		require.ErrorIs(t, err, brokeridentity.ErrExecutionScopeUnidentified)
		_, statErr := os.Stat(filepath.Join(brokeridentity.InstanceDir(globalDir, "local-docker"), brokeridentity.IdentityFileName))
		assert.True(t, errors.Is(statErr, os.ErrNotExist), "no identity is minted without a scope")
	})

	t.Run("docker daemon changed", func(t *testing.T) {
		s := newTestStore(t)
		srv := flatTestHub(t, s, true)
		globalDir := t.TempDir()
		first, err := prepareFlatInstance(ctx, srv, runtime.NewDockerRuntime(), flatTestInstance(), nil, globalDir, opts, fakeDockerProbe("daemon-1", nil))
		require.NoError(t, err)
		_, err = prepareFlatInstance(ctx, srv, runtime.NewDockerRuntime(), flatTestInstance(), nil, globalDir, opts, fakeDockerProbe("daemon-2", nil))
		require.ErrorIs(t, err, brokeridentity.ErrExecutionScopeChanged)
		row, getErr := s.GetRuntimeBroker(ctx, first.row.ID)
		require.NoError(t, getErr)
		assert.Equal(t, first.row.RuntimeTarget.ID, row.RuntimeTarget.ID, "existing placement is not re-pointed")
	})

	t.Run("identity collides with a legacy ID", func(t *testing.T) {
		s := newTestStore(t)
		srv := flatTestHub(t, s, true)
		globalDir := t.TempDir()
		first, err := prepareFlatInstance(ctx, srv, runtime.NewDockerRuntime(), flatTestInstance(), nil, globalDir, opts, fakeDockerProbe("daemon-1", nil))
		require.NoError(t, err)
		_, err = prepareFlatInstance(ctx, srv, runtime.NewDockerRuntime(), flatTestInstance(), []string{first.identity.RuntimeBrokerID}, globalDir, opts, fakeDockerProbe("daemon-1", nil))
		require.ErrorIs(t, err, brokeridentity.ErrIdentityCollidesWithLegacy)
	})

	t.Run("non-docker target", func(t *testing.T) {
		s := newTestStore(t)
		srv := flatTestHub(t, s, true)
		inst := flatTestInstance()
		inst.RuntimeTarget.Type = "kubernetes"
		_, err := prepareFlatInstance(ctx, srv, runtime.NewDockerRuntime(), inst, nil, t.TempDir(), opts, fakeDockerProbe("daemon-1", nil))
		require.Error(t, err)
	})
}

func TestLoadServerRuntimeBrokerInstances_RefusesSilentFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(`schema_version: "1"
server:
  broker:
    enabled: true
    instances:
      - key: local-docker
        name: example-docker
        runtime_target:
          type: docker
`), 0o600))

	// The lenient config saw the same instances: accepted.
	matching := &config.GlobalConfig{RuntimeBroker: config.RuntimeBrokerConfig{
		Instances: config.RuntimeBrokerInstancesToGlobal([]config.V1RuntimeBrokerInstanceConfig{{Key: "local-docker", Name: "example-docker",
			RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}}}),
	}}
	got, err := loadServerRuntimeBrokerInstances(matching, "")
	require.NoError(t, err)
	require.Len(t, got, 1)

	// The lenient config dropped them (the silent-fallback path): refused.
	_, err = loadServerRuntimeBrokerInstances(&config.GlobalConfig{}, "")
	require.Error(t, err)
}

func TestWarnUnhostedFlatIdentities_NoInstancesConfigured(t *testing.T) {
	globalDir := t.TempDir()
	id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(globalDir, "local-docker"), "local-docker", "docker",
		brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "daemon-1"}}, nil)
	require.NoError(t, err)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	warnUnhostedFlatIdentities(globalDir)
	assert.Contains(t, buf.String(), id.RuntimeBrokerID, "the warning names the unhosted Runtime Broker ID")
}
