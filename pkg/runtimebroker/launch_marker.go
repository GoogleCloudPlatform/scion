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
// projectPath is resolved through config.GetResolvedProjectDir first, the
// same way ProvisionAgent and DeleteAgentFiles resolve their project
// directory, so the marker lands next to the agents/ directory those
// functions actually use (the project's .scion dir, or the external
// project-configs dir for a shared-workspace project) rather than under the
// unresolved project root.
//
// sharedWorkspace mirrors the flag GetAgentDir/SelectAgentsRoot use: for a
// shared-workspace git project, the agents root (and so the marker root) is
// the external per-project directory, not a path under the resolved project
// dir.
func launchMarkersDir(projectPath string, sharedWorkspace bool) (string, error) {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return "", err
	}
	agentsRoot := config.SelectAgentsRoot(projectDir, sharedWorkspace)
	return filepath.Join(filepath.Dir(agentsRoot), "launch-markers"), nil
}

// writeLaunchMarker writes launchID as the marker for slug, atomically
// (write-then-rename, as the broker's dispatch-attempt persistence does in
// state_store.go), overwriting any previous holder. A newer launch's marker
// write always wins over an older launch that is about to check or delete
// files, because the check-then-delete window matches the accepted N-7
// residual (design §3.8.4).
func writeLaunchMarker(projectPath string, sharedWorkspace bool, slug, launchID string) error {
	dir, err := launchMarkersDir(projectPath, sharedWorkspace)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, slug)
	// A fixed "<slug>.tmp" name would collide if two brokers (or two
	// launches writing the same slug's marker on the same replica)
	// overlapped; os.CreateTemp gives each writer its own name in the same
	// directory, so the final os.Rename is still the atomic, same-filesystem
	// rename the marker's guarantee depends on.
	tmp, err := os.CreateTemp(dir, slug+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.Write([]byte(launchID))
	closeErr := tmp.Close()
	if writeErr != nil {
		_ = os.Remove(tmpPath)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}
	if renameErr := os.Rename(tmpPath, path); renameErr != nil {
		_ = os.Remove(tmpPath)
		return renameErr
	}
	return nil
}

// readLaunchMarker returns the launch ID currently recorded for slug, or ""
// if there is no marker (e.g. a synchronous create, one from before T1, or
// the project path cannot be resolved).
func readLaunchMarker(projectPath string, sharedWorkspace bool, slug string) string {
	dir, err := launchMarkersDir(projectPath, sharedWorkspace)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, slug))
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
func launchMarkerMatches(projectPath string, sharedWorkspace bool, slug, launchID string) bool {
	return launchID != "" && readLaunchMarker(projectPath, sharedWorkspace, slug) == launchID
}

// removeLaunchMarkerIfMatches deletes slug's marker if it still holds
// launchID (design §3.8.4: "The launch removes the marker when it ends, if
// it still holds L"). A newer launch's marker is left untouched.
func removeLaunchMarkerIfMatches(projectPath string, sharedWorkspace bool, slug, launchID string) {
	if !launchMarkerMatches(projectPath, sharedWorkspace, slug, launchID) {
		return
	}
	dir, err := launchMarkersDir(projectPath, sharedWorkspace)
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(dir, slug))
}
