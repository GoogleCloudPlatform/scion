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

//go:build unix

package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestIsAgentHomePersistent(t *testing.T) {
	tests := []struct {
		name string
		in   homePersistenceInput
		want bool
	}{
		{name: "docker host-bound home", in: homePersistenceInput{Runtime: "docker"}, want: true},
		{name: "podman host-bound home", in: homePersistenceInput{Runtime: "podman"}, want: true},
		{name: "apple container host-bound home", in: homePersistenceInput{Runtime: "container"}, want: true},
		{name: "apple-container alias", in: homePersistenceInput{Runtime: "apple-container"}, want: true},
		{name: "kubernetes pod-local home", in: homePersistenceInput{Runtime: "kubernetes"}, want: false},
		{name: "kubernetes local backend explicit", in: homePersistenceInput{Runtime: "kubernetes", HomeStorageBackend: "local"}, want: false},
		{name: "kubernetes nfs home", in: homePersistenceInput{Runtime: "kubernetes", HomeStorageBackend: runtime.HomeStorageNFS}, want: true},
		{name: "k8s alias nfs home", in: homePersistenceInput{Runtime: "k8s", HomeStorageBackend: runtime.HomeStorageNFS}, want: true},
		{name: "cloudrun persistent nfs home", in: homePersistenceInput{Runtime: "cloudrun"}, want: true},
		{name: "cloudrun ephemeral", in: homePersistenceInput{Runtime: "cloudrun", Ephemeral: true}, want: false},
		{name: "docker ephemeral overrides", in: homePersistenceInput{Runtime: "docker", Ephemeral: true}, want: false},
		{name: "substrate non-persistent", in: homePersistenceInput{Runtime: "substrate"}, want: false},
		{name: "unknown runtime fails closed", in: homePersistenceInput{Runtime: "custom-ephemeral"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isAgentHomePersistent(tc.in))
		})
	}
}

type ephemeralSDSMockRuntime struct {
	*runtime.MockRuntime
}

func (ephemeralSDSMockRuntime) SupportsEphemeralPersistence() bool { return true }

func TestStartShellExecContainer_HomePersistentEnvAndRuntimeState(t *testing.T) {
	t.Run("docker sets SCION_HOME_PERSISTENT=true", func(t *testing.T) {
		f := newSharedDirStorageRunFixture(t)
		f.writeRawGlobalSettings(t, homeStorageStartSettings(f.tmpDir))
		var c sdsCapture
		opts := homeStorageStartOpts(f, "shell-dock", "local")
		opts.HarnessConfig = config.ShellHarnessConfigName
		info, err := NewManager(newSDSMockRuntime("docker", &c)).Start(context.Background(), opts)
		require.NoError(t, err)
		require.Equal(t, 1, c.ran)
		assert.Contains(t, c.cfg.Env, "SCION_HOME_PERSISTENT=true")
		p, known := api.ParseHomePersistentRuntimeState(info.RuntimeState)
		assert.True(t, known, "expected home_persistent in RuntimeState, got %q", info.RuntimeState)
		assert.True(t, p)
	})

	t.Run("kubernetes without NFS home sets SCION_HOME_PERSISTENT=false", func(t *testing.T) {
		f := newSharedDirStorageRunFixture(t)
		f.writeRawGlobalSettings(t, homeStorageStartSettings(f.tmpDir))
		var c sdsCapture
		opts := homeStorageStartOpts(f, "shell-k8s-local", "gke")
		opts.HarnessConfig = config.ShellHarnessConfigName
		info, err := NewManager(newSDSMockRuntime("kubernetes", &c)).Start(context.Background(), opts)
		require.NoError(t, err)
		require.Equal(t, 1, c.ran)
		assert.Contains(t, c.cfg.Env, "SCION_HOME_PERSISTENT=false")
		p, known := api.ParseHomePersistentRuntimeState(info.RuntimeState)
		assert.True(t, known, "expected home_persistent in RuntimeState, got %q", info.RuntimeState)
		assert.False(t, p)
	})

	t.Run("kubernetes with NFS home sets SCION_HOME_PERSISTENT=true", func(t *testing.T) {
		f := newSharedDirStorageRunFixture(t)
		f.writeRawGlobalSettings(t, homeStorageStartSettings(f.tmpDir))
		var c sdsCapture
		opts := homeStorageStartOpts(f, "shell-k8s-nfs", "gke")
		opts.HarnessConfig = config.ShellHarnessConfigName
		info, err := NewManager(newSDSMockRuntime("kubernetes", &c)).Start(withNFSHomeExperiment(context.Background()), opts)
		require.NoError(t, err)
		require.Equal(t, 1, c.ran)
		assert.Contains(t, c.cfg.Env, "SCION_HOME_PERSISTENT=true")
		p, known := api.ParseHomePersistentRuntimeState(info.RuntimeState)
		assert.True(t, known, "expected home_persistent in RuntimeState, got %q", info.RuntimeState)
		assert.True(t, p)
	})

	t.Run("cloudrun ephemeral sets SCION_HOME_PERSISTENT=false", func(t *testing.T) {
		f := newSharedDirStorageRunFixture(t)
		f.writeRawGlobalSettings(t, homeStorageStartSettings(f.tmpDir))
		var c sdsCapture
		rt := ephemeralSDSMockRuntime{MockRuntime: newSDSMockRuntime("cloudrun", &c)}
		opts := homeStorageStartOpts(f, "shell-cr-eph", "local")
		opts.HarnessConfig = config.ShellHarnessConfigName
		opts.Persistence = api.PersistenceEphemeral
		info, err := NewManager(rt).Start(context.Background(), opts)
		require.NoError(t, err)
		require.Equal(t, 1, c.ran)
		assert.Contains(t, c.cfg.Env, "SCION_HOME_PERSISTENT=false")
		p, known := api.ParseHomePersistentRuntimeState(info.RuntimeState)
		assert.True(t, known, "expected home_persistent in RuntimeState, got %q", info.RuntimeState)
		assert.False(t, p)
	})

	t.Run("non-shell agent does not receive SCION_HOME_PERSISTENT", func(t *testing.T) {
		f := newSharedDirStorageRunFixture(t)
		f.writeRawGlobalSettings(t, homeStorageStartSettings(f.tmpDir))
		var c sdsCapture
		opts := homeStorageStartOpts(f, "regular-agent", "local")
		info, err := NewManager(newSDSMockRuntime("docker", &c)).Start(context.Background(), opts)
		require.NoError(t, err)
		require.Equal(t, 1, c.ran)
		for _, kv := range c.cfg.Env {
			assert.NotContains(t, kv, "SCION_HOME_PERSISTENT=")
		}
		_, known := api.ParseHomePersistentRuntimeState(info.RuntimeState)
		assert.False(t, known)
	})
}
