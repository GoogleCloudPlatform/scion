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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
)

// TestBuildHubServerConfig_CopiesAsyncLaunchSettings pins the three field
// copies buildHubServerConfig makes from cfg.Hub: without them, the
// async-launch settings would parse from settings.yaml/env into
// config.GlobalConfig but never reach the running hub.ServerConfig that
// New() and the reaper actually read.
func TestBuildHubServerConfig_CopiesAsyncLaunchSettings(t *testing.T) {
	cfg := &config.GlobalConfig{}
	cfg.Hub.AsyncAgentLaunch = true
	cfg.Hub.LaunchTimeout = 7 * time.Minute
	cfg.Hub.LaunchKeepaliveSeconds = 20

	hubCfg := buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)

	assert.True(t, hubCfg.AsyncAgentLaunch)
	assert.Equal(t, 7*time.Minute, hubCfg.LaunchTimeout)
	assert.Equal(t, 20, hubCfg.LaunchKeepaliveSeconds)
}
