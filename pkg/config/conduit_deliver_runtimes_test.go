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

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConduitDeliverRuntimes_DefaultsAndValidation(t *testing.T) {
	t.Run("default returns cloudrun, kubernetes, docker as a fresh copy", func(t *testing.T) {
		got := DefaultConduitDeliverRuntimes()
		assert.Equal(t, []string{"cloudrun", "kubernetes", "docker"}, got)
		got[0] = "mutated"
		assert.Equal(t, []string{"cloudrun", "kubernetes", "docker"}, DefaultConduitDeliverRuntimes())
	})

	t.Run("unset nil resolves to default", func(t *testing.T) {
		norm, err := NormalizeConduitDeliverRuntimes(nil)
		require.NoError(t, err)
		assert.Nil(t, norm)

		resolved, err := ResolveConduitDeliverRuntimes(nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"cloudrun", "kubernetes", "docker"}, resolved)
	})

	t.Run("explicit empty list disables all runtimes", func(t *testing.T) {
		norm, err := NormalizeConduitDeliverRuntimes([]string{})
		require.NoError(t, err)
		assert.NotNil(t, norm)
		assert.Empty(t, norm)

		resolved, err := ResolveConduitDeliverRuntimes([]string{})
		require.NoError(t, err)
		assert.NotNil(t, resolved)
		assert.Empty(t, resolved)
	})

	t.Run("all known runtimes accepted and deduplicated", func(t *testing.T) {
		in := []string{" cloudrun ", "kubernetes", "docker", "podman", "container", "substrate", "cloudrun-sandbox", "docker"}
		got, err := ResolveConduitDeliverRuntimes(in)
		require.NoError(t, err)
		assert.Equal(t, []string{"cloudrun", "kubernetes", "docker", "podman", "container", "substrate", "cloudrun-sandbox"}, got)
	})

	t.Run("unknown runtime names fail", func(t *testing.T) {
		for _, bad := range []string{"bogus", "k8s", "apple", "Docker", ""} {
			_, err := ResolveConduitDeliverRuntimes([]string{"cloudrun", bad})
			require.Error(t, err, "expected error for %q", bad)
			assert.Contains(t, err.Error(), bad)
		}
	})
}

func TestConduitDeliverRuntimes_V1RoundTrip(t *testing.T) {
	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{
		Broker: &V1BrokerConfig{
			ConduitDeliverRuntimes: []string{"cloudrun", "podman"},
		},
	})
	assert.Equal(t, []string{"cloudrun", "podman"}, gc.RuntimeBroker.ConduitDeliverRuntimes)

	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.Broker)
	assert.Equal(t, []string{"cloudrun", "podman"}, v1.Broker.ConduitDeliverRuntimes)

	// Unset stays nil.
	unset := ConvertV1ServerToGlobalConfig(&V1ServerConfig{Broker: &V1BrokerConfig{}})
	assert.Nil(t, unset.RuntimeBroker.ConduitDeliverRuntimes)

	// Explicit empty list stays non-nil empty.
	empty := ConvertV1ServerToGlobalConfig(&V1ServerConfig{
		Broker: &V1BrokerConfig{ConduitDeliverRuntimes: []string{}},
	})
	assert.NotNil(t, empty.RuntimeBroker.ConduitDeliverRuntimes)
	assert.Empty(t, empty.RuntimeBroker.ConduitDeliverRuntimes)
}

func TestConduitDeliverRuntimes_LoadGlobalConfig(t *testing.T) {
	writeSettings := func(t *testing.T, yamlBody string) {
		t.Helper()
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		scionDir := filepath.Join(dir, ".scion")
		require.NoError(t, os.MkdirAll(scionDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(yamlBody), 0o600))
	}

	t.Run("unset in settings.yaml resolves to default", func(t *testing.T) {
		writeSettings(t, "schema_version: \"1\"\nserver:\n  broker:\n    enabled: true\n")
		gc, err := LoadGlobalConfig("")
		require.NoError(t, err)
		assert.Nil(t, gc.RuntimeBroker.ConduitDeliverRuntimes)
		resolved, err := ResolveConduitDeliverRuntimes(gc.RuntimeBroker.ConduitDeliverRuntimes)
		require.NoError(t, err)
		assert.Equal(t, []string{"cloudrun", "kubernetes", "docker"}, resolved)
	})

	t.Run("valid settings.yaml list loads and deduplicates", func(t *testing.T) {
		writeSettings(t, `schema_version: "1"
server:
  broker:
    enabled: true
    conduit_deliver_runtimes:
      - cloudrun
      - podman
      - cloudrun
`)
		gc, err := LoadGlobalConfig("")
		require.NoError(t, err)
		assert.Equal(t, []string{"cloudrun", "podman"}, gc.RuntimeBroker.ConduitDeliverRuntimes)
	})

	t.Run("explicit empty list in settings.yaml stays empty", func(t *testing.T) {
		writeSettings(t, `schema_version: "1"
server:
  broker:
    enabled: true
    conduit_deliver_runtimes: []
`)
		gc, err := LoadGlobalConfig("")
		require.NoError(t, err)
		assert.NotNil(t, gc.RuntimeBroker.ConduitDeliverRuntimes)
		assert.Empty(t, gc.RuntimeBroker.ConduitDeliverRuntimes)
	})

	t.Run("unknown runtime in settings.yaml fails LoadGlobalConfig", func(t *testing.T) {
		writeSettings(t, `schema_version: "1"
server:
  broker:
    enabled: true
    conduit_deliver_runtimes:
      - cloudrun
      - not-a-runtime
`)
		_, err := LoadGlobalConfig("")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "server.broker.conduit_deliver_runtimes")
		assert.Contains(t, err.Error(), "not-a-runtime")
	})

	t.Run("env var overrides and validates", func(t *testing.T) {
		writeSettings(t, "schema_version: \"1\"\nserver:\n  broker:\n    enabled: true\n")
		t.Setenv("SCION_SERVER_RUNTIMEBROKER_CONDUITDELIVERRUNTIMES", "podman,container")

		gc, err := LoadGlobalConfig("")
		require.NoError(t, err)
		assert.Equal(t, []string{"podman", "container"}, gc.RuntimeBroker.ConduitDeliverRuntimes)
	})

	t.Run("invalid env var fails LoadGlobalConfig", func(t *testing.T) {
		writeSettings(t, "schema_version: \"1\"\nserver:\n  broker:\n    enabled: true\n")
		t.Setenv("SCION_SERVER_RUNTIMEBROKER_CONDUITDELIVERRUNTIMES", "cloudrun,unknown-rt")

		_, err := LoadGlobalConfig("")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "server.broker.conduit_deliver_runtimes")
		assert.Contains(t, err.Error(), "unknown-rt")
	})
}
