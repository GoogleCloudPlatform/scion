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

package runtimebroker

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// launchMarkersDir returns "<project .scion dir>/launch-markers" (design
// t1-async-create-v11.md §3.8.2 step 5.2, F5): a sibling of the agents root
// config.SelectAgentsRoot computes, so a launch marker sits on the same
// storage as the agent files it guards (replicas that share agent files
// share the marker too), but outside agents/, where provisioning and resume
// probes never look.
//
// sharedWorkspace mirrors the flag GetAgentDir/SelectAgentsRoot use: for a
// shared-workspace git project, the agents root (and so the marker root) is
// the external per-project directory, not a path under projectDir.
func launchMarkersDir(projectDir string, sharedWorkspace bool) string {
	agentsRoot := config.SelectAgentsRoot(projectDir, sharedWorkspace)
	return filepath.Join(filepath.Dir(agentsRoot), "launch-markers")
}

// writeLaunchMarker writes launchID as the marker for slug, atomically
// (write-then-rename, as the broker's dispatch-attempt persistence does in
// state_store.go), overwriting any previous holder. A newer launch's marker
// write always wins a race with an older launch that is about to check or
// delete files, because the check-then-delete window matches the accepted
// N-7 residual (design §3.8.4).
func writeLaunchMarker(projectDir string, sharedWorkspace bool, slug, launchID string) error {
	dir := launchMarkersDir(projectDir, sharedWorkspace)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, slug)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(launchID), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readLaunchMarker returns the launch ID currently recorded for slug, or ""
// if there is no marker (e.g. a synchronous create, or one from before T1).
func readLaunchMarker(projectDir string, sharedWorkspace bool, slug string) string {
	path := filepath.Join(launchMarkersDir(projectDir, sharedWorkspace), slug)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// launchMarkerMatches reports whether slug's marker still holds launchID
// (design §3.8.4): file deletion for a create launch proceeds only when this
// is true. A newer launch's marker write, on any replica sharing the
// storage, makes this false for the older launch, so it keeps the newer
// launch's files.
func launchMarkerMatches(projectDir string, sharedWorkspace bool, slug, launchID string) bool {
	return launchID != "" && readLaunchMarker(projectDir, sharedWorkspace, slug) == launchID
}

// removeLaunchMarkerIfMatches deletes slug's marker if it still holds
// launchID (design §3.8.4: "The launch removes the marker when it ends, if
// it still holds L"). A newer launch's marker is left untouched.
func removeLaunchMarkerIfMatches(projectDir string, sharedWorkspace bool, slug, launchID string) {
	if !launchMarkerMatches(projectDir, sharedWorkspace, slug, launchID) {
		return
	}
	_ = os.Remove(filepath.Join(launchMarkersDir(projectDir, sharedWorkspace), slug))
}
