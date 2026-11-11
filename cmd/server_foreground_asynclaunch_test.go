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
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildHubServerConfig_CopiesAsyncLaunchSettings pins the two field
// copies buildHubServerConfig makes from cfg.Hub: without them, the
// async-launch settings would parse from settings.yaml/env into
// config.GlobalConfig but never reach the running hub.ServerConfig that
// New() and the reaper actually read.
func TestBuildHubServerConfig_CopiesAsyncLaunchSettings(t *testing.T) {
	cfg := &config.GlobalConfig{}
	cfg.Hub.LaunchTimeout = 7 * time.Minute
	cfg.Hub.LaunchKeepaliveSeconds = 20

	hubCfg := buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)

	assert.Equal(t, 7*time.Minute, hubCfg.LaunchTimeout)
	assert.Equal(t, 20, hubCfg.LaunchKeepaliveSeconds)
}

// TestWarnRetiredServerSettings_AsyncAgentLaunch pins the startup warning
// for the retired server.hub.async_agent_launch setting (ptone/scion#4991).
func TestWarnRetiredServerSettings_AsyncAgentLaunch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")

	run := func(t *testing.T) string {
		t.Helper()
		var buf bytes.Buffer
		warnRetiredServerSettings(slog.New(slog.NewTextHandler(&buf, nil)), path)
		return buf.String()
	}

	t.Run("set", func(t *testing.T) {
		t.Setenv("SCION_SERVER_HUB_ASYNCAGENTLAUNCH", "")
		require.NoError(t, os.WriteFile(path, []byte("schema_version: \"1\"\nserver:\n  hub:\n    async_agent_launch: true\n"), 0o644))
		out := run(t)
		assert.Contains(t, out, "level=WARN")
		assert.Contains(t, out, config.RetiredAsyncAgentLaunchWarning)
	})

	t.Run("not set", func(t *testing.T) {
		t.Setenv("SCION_SERVER_HUB_ASYNCAGENTLAUNCH", "")
		require.NoError(t, os.WriteFile(path, []byte("schema_version: \"1\"\nserver:\n  hub:\n    port: 9999\n"), 0o644))
		assert.Empty(t, run(t))
	})
}
