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
	"testing"

	"github.com/stretchr/testify/assert"
)

// SCION_HUB_PROFILE and SCION_HUB_RUN select a CLI target-hub profile
// (ptone/scion#4240). They are never settings overrides: both env key
// mappers drop them, so they cannot land on hub.profile or hub.run.
func TestSettingsExcludedEnv_HubProfileVariables(t *testing.T) {
	for _, name := range []string{"SCION_HUB_PROFILE", "SCION_HUB_RUN"} {
		assert.True(t, isSettingsExcludedEnv(name), name)
		assert.Empty(t, versionedEnvKeyMapper(name), name)
	}
	// The other SCION_HUB_* variables still map.
	assert.False(t, isSettingsExcludedEnv("SCION_HUB_ENDPOINT"))
	assert.NotEmpty(t, versionedEnvKeyMapper("SCION_HUB_ENDPOINT"))
}
