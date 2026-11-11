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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// ambientTestEnvVars lists the environment variables cleared once for the
// whole package before any test runs. The settings loader overlays each of
// them onto project_id, so a value exported by the environment running the
// tests (an agent container exports them, for example) would otherwise
// override the project ID that a test's own settings fixtures provide.
//
//   - SCION_PROJECT_ID: overlaid onto project_id.
//   - SCION_HUB_PROJECT_ID: overlaid onto project_id (legacy loader) and
//     hub.project_id (versioned loader).
//   - SCION_PROJECT_TYPE: overlaid onto project_type by the generic SCION_
//     environment provider, which overrides the project type that a test's
//     settings fixtures provide (for example a shadow project's).
//
// Keep this set minimal: add a variable only when its ambient value changes
// a test's outcome. Tests that need one of these variables set it themselves
// with t.Setenv, which keeps working because t.Setenv restores the cleared
// state when the test ends.
var ambientTestEnvVars = []string{
	projectkeys.EnvProjectID,
	projectkeys.EnvHubProjectID,
	// No pkg/projectkeys constant exists for this one: it is not a project
	// identity alias, only a plain settings key reached through the env provider.
	"SCION_PROJECT_TYPE",
}

func TestMain(m *testing.M) {
	for _, name := range ambientTestEnvVars {
		_ = os.Unsetenv(name)
	}
	clearAmbientSettingsEnv()
	// A re-exec'd copy of this test binary acting as a separate settings
	// writer process (see settings_lock_test.go).
	if mode := os.Getenv(settingsHelperEnv); mode != "" {
		os.Exit(runSettingsHelper(mode))
	}
	os.Exit(m.Run())
}

// clearAmbientSettingsEnv unsets every SCION_SERVER_* and SCION_SEED_*
// variable the environment running the tests exports. The env mappers read
// the process env to let a canonical name win over its aliases
// (resolveServerEnv), so an ambient value would change what a test's
// alias mapping returns. Tests that need one set it with t.Setenv.
func clearAmbientSettingsEnv() {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, serverEnvPrefix) || strings.HasPrefix(name, seedEnvPrefix) {
			_ = os.Unsetenv(name)
		}
	}
}
