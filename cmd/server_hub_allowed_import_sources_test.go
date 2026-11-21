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
	"github.com/stretchr/testify/require"
)

// TestValidateServerPreflight_AllowedImportSources: a hub process refuses
// to start with an invalid server.hub.allowed_import_sources entry, and
// hands the normalized list to the hub.
func TestValidateServerPreflight_AllowedImportSources(t *testing.T) {
	t.Cleanup(resetServerFlags)

	resetServerFlags()
	enableHub = true
	cfg := &config.GlobalConfig{}
	cfg.Hub.AllowedImportSources = []string{"gs://team-bucket/templates", ":local:/"}
	err := validateServerPreflight(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), config.AllowedImportSourcesKey+"[1]")

	cfg.Hub.AllowedImportSources = []string{":gcs:team-bucket/templates/", "https://Artifacts.Example.com/scion"}
	require.NoError(t, validateServerPreflight(cfg))
	assert.Equal(t, []string{"gs://team-bucket/templates/", "https://artifacts.example.com/scion"}, cfg.Hub.AllowedImportSources)

	hubCfg := buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)
	assert.Equal(t, cfg.Hub.AllowedImportSources, hubCfg.AllowedImportSources)
}
