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
//
// Keep this set minimal: add a variable only when its ambient value changes
// a test's outcome. Tests that need one of these variables set it themselves
// with t.Setenv, which keeps working because t.Setenv restores the cleared
// state when the test ends.
var ambientTestEnvVars = []string{
	projectkeys.EnvProjectID,
	projectkeys.EnvHubProjectID,
}

func TestMain(m *testing.M) {
	for _, name := range ambientTestEnvVars {
		_ = os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
