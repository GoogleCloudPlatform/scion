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
