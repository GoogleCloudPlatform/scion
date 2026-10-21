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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPluginHubAPIURLFrom checks that hub-managed broker plugins are handed
// the same loopback URL the co-located runtime broker uses, and nothing when
// the hub's listen port is unknown.
func TestPluginHubAPIURLFrom(t *testing.T) {
	cfg := newDeprecationTestConfig()

	assert.Equal(t, "http://localhost:8080", pluginHubAPIURLFrom(cfg, workstationMode),
		"with the web frontend the hub is served on the web port")
	assert.Equal(t, resolveBrokerHubEndpointFrom(cfg, workstationMode, "", nil), pluginHubAPIURLFrom(cfg, workstationMode),
		"plugins and the co-located broker use the same loopback URL")

	cfg.Hub.Port = 9810
	assert.Equal(t, fmt.Sprintf("http://localhost:%d", 9810), pluginHubAPIURLFrom(cfg, hubOnlyMode),
		"without the web frontend the hub is served on its own port")

	assert.Empty(t, pluginHubAPIURLFrom(cfg, brokerOnlyMode), "no co-located hub, no loopback URL")
	cfg.Hub.Port = 0
	assert.Empty(t, pluginHubAPIURLFrom(cfg, hubOnlyMode), "an unknown port yields no URL")
}
