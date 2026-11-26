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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubBrokerScopeInference_IgnoresProjectServerBroker verifies that --broker
// scope inference in both "scion hub secret" and "scion hub env" ignores
// server.broker.broker_id set in a project's .scion/settings.yaml and uses
// only the global settings (or fails when global has no broker ID).
func TestHubBrokerScopeInference_IgnoresProjectServerBroker(t *testing.T) {
	t.Run("project only broker_id is ignored", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		projectDir := filepath.Join(home, "proj", ".scion")
		require.NoError(t, os.MkdirAll(projectDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(`schema_version: "1"
server:
  broker:
    broker_id: "project-injected-broker"
`), 0o600))

		settings, err := config.LoadSettings(projectDir)
		require.NoError(t, err)

		// Secret --broker scope inference must fail rather than use project broker_id.
		oldSecretBroker := secretBrokerScope
		secretBrokerScope = scopeInferSentinel
		t.Cleanup(func() { secretBrokerScope = oldSecretBroker })
		secretCmd := &cobra.Command{}
		secretCmd.Flags().StringVar(&secretBrokerScope, "broker", "", "")
		require.NoError(t, secretCmd.Flags().Set("broker", scopeInferSentinel))
		_, err = resolveSecretScope(secretCmd, settings)
		require.Error(t, err)
		assert.ErrorContains(t, err, "cannot infer broker ID")

		// Env --broker scope inference must also fail.
		oldEnvBroker := envBrokerScope
		envBrokerScope = scopeInferSentinel
		t.Cleanup(func() { envBrokerScope = oldEnvBroker })
		envCmd := &cobra.Command{}
		envCmd.Flags().StringVar(&envBrokerScope, "broker", "", "")
		require.NoError(t, envCmd.Flags().Set("broker", scopeInferSentinel))
		_, err = resolveEnvScope(envCmd, settings)
		require.Error(t, err)
		assert.ErrorContains(t, err, "cannot infer broker ID")
	})

	t.Run("global broker_id wins over project broker_id", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		globalDir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(`schema_version: "1"
server:
  broker:
    broker_id: "global-broker-id"
`), 0o600))

		projectDir := filepath.Join(home, "proj", ".scion")
		require.NoError(t, os.MkdirAll(projectDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(`schema_version: "1"
server:
  broker:
    broker_id: "project-injected-broker"
`), 0o600))

		settings, err := config.LoadSettings(projectDir)
		require.NoError(t, err)

		oldSecretBroker := secretBrokerScope
		secretBrokerScope = scopeInferSentinel
		t.Cleanup(func() { secretBrokerScope = oldSecretBroker })
		secretCmd := &cobra.Command{}
		secretCmd.Flags().StringVar(&secretBrokerScope, "broker", "", "")
		require.NoError(t, secretCmd.Flags().Set("broker", scopeInferSentinel))
		target, err := resolveSecretScope(secretCmd, settings)
		require.NoError(t, err)
		assert.Equal(t, "runtime_broker", target.scope)
		assert.Equal(t, "global-broker-id", target.id)

		oldEnvBroker := envBrokerScope
		envBrokerScope = scopeInferSentinel
		t.Cleanup(func() { envBrokerScope = oldEnvBroker })
		envCmd := &cobra.Command{}
		envCmd.Flags().StringVar(&envBrokerScope, "broker", "", "")
		require.NoError(t, envCmd.Flags().Set("broker", scopeInferSentinel))
		envTarget, err := resolveEnvScope(envCmd, settings)
		require.NoError(t, err)
		assert.Equal(t, "runtime_broker", envTarget.scope)
		assert.Equal(t, "global-broker-id", envTarget.id)
	})
}
