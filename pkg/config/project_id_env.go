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
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// envHubProjectID returns the value of SCION_HUB_PROJECT_ID, or "" when it
// is unset or empty (an exported but empty variable counts as unset, as in
// both settings loaders).
//
// SCION_HUB_PROJECT_ID outranks every file source of the hub project ID in
// both loaders: the project-id file and the settings key hub.project_id.
// SCION_PROJECT_ID keeps its own, lower rank (see the loaders).
func envHubProjectID() string {
	return os.Getenv(projectkeys.EnvHubProjectID)
}

// projectIDFileSource names the project-id file of the project at
// projectPath, for the precedence warning.
func projectIDFileSource(projectPath string) string {
	return filepath.Join(projectPath, projectkeys.ProjectIDFile)
}

// warnedProjectIDOverrides records the project-ID overrides already
// reported in this process, keyed by both sources and both values, so each
// distinct disagreement is reported and an identical repeat (the same
// settings loaded again) is not.
var warnedProjectIDOverrides sync.Map

// resetWarnedProjectIDOverrides clears warnedProjectIDOverrides (tests).
func resetWarnedProjectIDOverrides() {
	warnedProjectIDOverrides = sync.Map{}
}

// warnProjectIDOverride logs that winnerSource's project ID replaced a
// different one from loserSource. It names the two sources (an environment
// variable, a file path or a settings key) and never the values. Nothing is
// logged when either value is empty or the two agree.
func warnProjectIDOverride(winnerSource, winnerValue, loserSource, loserValue string) {
	if winnerValue == "" || loserValue == "" || winnerValue == loserValue {
		return
	}
	key := winnerSource + "\x00" + loserSource + "\x00" + winnerValue + "\x00" + loserValue
	if _, seen := warnedProjectIDOverrides.LoadOrStore(key, struct{}{}); seen {
		return
	}
	slog.Warn("settings: project ID from "+winnerSource+" overrides a different project ID from "+loserSource,
		"source", winnerSource, "overridden", loserSource)
}
