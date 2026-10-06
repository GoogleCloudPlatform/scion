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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HubNamePattern must stay identical to the schema's server.hub.hub_name
// pattern, which the admin API validates against.
func TestHubNamePattern_MatchesSchema(t *testing.T) {
	raw, err := GetSettingsSchemaJSON("1")
	require.NoError(t, err)
	var root struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Pattern string `json:"pattern"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &root))
	assert.Equal(t, root.Defs["serverHub"].Properties["hub_name"].Pattern, HubNamePattern)
}

func TestHubNameMatchesSchema(t *testing.T) {
	for _, ok := range []string{"a", "prod-hub", "hub1"} {
		assert.True(t, HubNameMatchesSchema(ok), ok)
	}
	for _, bad := range []string{"", "Prod.Hub", "prod_hub", "-hub", "hub-", "1hub"} {
		assert.False(t, HubNameMatchesSchema(bad), bad)
	}
}
