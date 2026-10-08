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
	"net/http"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// projectIdentityError reports that a shared-workspace dispatch's project
// identity check failed. Error() carries no host path (Path is for logs).
type projectIdentityError struct {
	Path   string
	Reason string
}

func (e *projectIdentityError) Error() string {
	return "shared-workspace dispatch verifies the project identity before loading project settings: " + e.Reason +
		"; remove or correct the workspace's .scion/project-id (or the .scion marker file) so it names this agent's Hub project, or re-link the project"
}

// verifySharedProjectIdentity is the shared-workspace dispatch check that
// runs before any project settings are loaded (create, start and restart):
// in a shared-workspace project the project's .scion — its project-id marker
// or, for a marker-file project, the marker file itself — sits inside the
// shared workspace mount, and settings resolution follows it (the
// marker chooses the external settings dir and hub.project_id; a marker file
// chooses the project dir itself). The marker is never used to select
// anything here: it is only compared with the Hub-supplied project ID of the
// dispatch, and a marker that disagrees with the Hub project ID is refused
// (ptone/scion#1799).
//
//   - .scion is a directory (git project): its project-id must be absent or
//     empty (settings then come from the in-repo .scion only, as before; the
//     broker writes the Hub ID itself) or equal hubProjectID.
//   - .scion is a marker file: its project-id must equal hubProjectID; an
//     unreadable or empty marker file is denied, since it would select the
//     project dir.
//   - no .scion: nothing to check (the broker creates it from the Hub ID).
//
// It is a no-op when there is no project path or no Hub project ID.
func verifySharedProjectIdentity(projectPath, hubProjectID string) error {
	if projectPath == "" || hubProjectID == "" {
		return nil
	}
	abs, err := filepath.Abs(projectPath)
	if err != nil {
		return &projectIdentityError{Path: projectPath, Reason: "the project path cannot be resolved"}
	}
	scionPath := abs
	if filepath.Base(abs) != config.DotScion {
		scionPath = filepath.Join(abs, config.DotScion)
	}
	info, err := os.Stat(scionPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return &projectIdentityError{Path: scionPath, Reason: "the project's .scion cannot be read"}
	}
	if info.IsDir() {
		id, err := config.ReadProjectID(scionPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return &projectIdentityError{Path: scionPath, Reason: "the project-id marker cannot be read"}
		}
		if id == "" || id == hubProjectID {
			return nil
		}
		return &projectIdentityError{Path: scionPath, Reason: fmt.Sprintf("the project-id marker names project %q, not this agent's Hub project %q", id, hubProjectID)}
	}
	marker, err := config.ReadProjectMarker(scionPath)
	if err != nil {
		return &projectIdentityError{Path: scionPath, Reason: "the project marker file cannot be read"}
	}
	if marker.ProjectID != hubProjectID {
		return &projectIdentityError{Path: scionPath, Reason: fmt.Sprintf("the project marker file names project %q, not this agent's Hub project %q", marker.ProjectID, hubProjectID)}
	}
	return nil
}

// projectIdentityStartContextError wraps a verifySharedProjectIdentity
// failure as a 409 startContextError, logging the host path.
func (s *Server) projectIdentityStartContextError(err error, agentName string) *startContextError {
	var pe *projectIdentityError
	if errors.As(err, &pe) {
		s.agentLifecycleLog.Error("shared-workspace project identity check failed", "agent", agentName, "path", pe.Path, "reason", pe.Reason)
	}
	return &startContextError{Status: http.StatusConflict, Message: err.Error(), OriginalErr: err}
}
