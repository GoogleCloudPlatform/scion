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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// opRemoveProjectConfigDir is the deleteProject op (writeRuntimeOpError)
// for a project-configs entry that could not be removed.
const opRemoveProjectConfigDir = "remove project config directory"

// orphanProjectConfigDir returns the project-configs entry
// <globalDir>/project-configs/<slug>__<short projectID> of a hub-managed
// project whose project directory is already gone, or "" when there is
// nothing to remove or it must be left alone. Without the project
// directory's .scion entry, the entry is accepted by its canonical name,
// derived from the request's slug and project ID, provided that:
//
//   - the request carries a project ID and both form a canonical name;
//   - the entry is a real directory strictly inside project-configs
//     (projectConfigPathContained);
//   - the project ID recorded in the entry's own .scion, when present,
//     is the requested one (two IDs can share a short ID).
//
// An unreadable recorded ID is an error: whether the entry is this
// project's is not known, so the delete must not report success.
func orphanProjectConfigDir(globalDir, slug, projectID string) (string, error) {
	if projectID == "" {
		return "", nil
	}
	dir := expectedProjectConfigDir(globalDir, slug, projectID)
	if dir == "" {
		return "", nil
	}
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	if !projectConfigPathContained(globalDir, dir) {
		return "", nil
	}
	recorded, err := config.ReadProjectID(filepath.Join(dir, config.DotScion))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dir, nil
	case err != nil:
		return "", fmt.Errorf("read the project ID recorded in %s: %w", dir, err)
	case recorded != projectID:
		return "", nil
	}
	return dir, nil
}

// removeProjectConfigs removes a project delete's project-configs entry
// (removeProjectConfigsSubtree, or the test seam projectConfigsRemove).
func (s *Server) removeProjectConfigs(globalDir, path string) error {
	s.mu.RLock()
	fn := s.projectConfigsRemove
	s.mu.RUnlock()
	if fn != nil {
		return fn(globalDir, path)
	}
	return removeProjectConfigsSubtree(globalDir, path)
}
