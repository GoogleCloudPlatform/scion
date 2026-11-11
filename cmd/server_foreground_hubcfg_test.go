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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
)

// TestBuildHubServerConfig_CopiesDefaultUserRole pins the field copy the
// startup super-admin reconciler depends on: without it, admins removed from
// admin_emails would be demoted to member even when default_user_role is
// viewer.
func TestBuildHubServerConfig_CopiesDefaultUserRole(t *testing.T) {
	cfg := &config.GlobalConfig{}
	cfg.Auth.DefaultUserRole = "viewer"

	hubCfg := buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)

	assert.Equal(t, "viewer", hubCfg.DefaultUserRole)
}

// TestBuildHubServerConfig_DefaultUserRoleEmptyByDefault proves an unset
// value stays empty (Server.DefaultUserRole maps empty to member).
func TestBuildHubServerConfig_DefaultUserRoleEmptyByDefault(t *testing.T) {
	cfg := &config.GlobalConfig{}

	hubCfg := buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)

	assert.Empty(t, hubCfg.DefaultUserRole)
}

// TestBuildHubServerConfig_StartupServerSettings proves the hub receives the
// server settings as start-up resolved them (after the workstation defaults
// and flags), which the server-config GET compares environment values with
// to report whether they are in effect.
func TestBuildHubServerConfig_StartupServerSettings(t *testing.T) {
	cfg := &config.GlobalConfig{}
	cfg.Hub.Host = "127.0.0.1"
	cfg.Hub.Port = 9810
	cfg.RuntimeBroker.Enabled = true
	cfg.Storage.Provider = "local"

	hubCfg := buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)

	if assert.NotNil(t, hubCfg.StartupServerSettings) && assert.NotNil(t, hubCfg.StartupServerSettings.Hub) {
		assert.Equal(t, "127.0.0.1", hubCfg.StartupServerSettings.Hub.Host)
		assert.Equal(t, 9810, hubCfg.StartupServerSettings.Hub.Port)
	}
	if assert.NotNil(t, hubCfg.StartupServerSettings.Broker) {
		assert.True(t, hubCfg.StartupServerSettings.Broker.Enabled)
	}
	if assert.NotNil(t, hubCfg.StartupServerSettings.Storage) {
		assert.Equal(t, "local", hubCfg.StartupServerSettings.Storage.Provider)
	}
}
