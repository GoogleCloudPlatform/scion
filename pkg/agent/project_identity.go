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

package agent

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// This file holds the one rule every project-scoped operation uses to
// decide whether a runtime entry stands for an agent of a project, so that
// the broker's lookups and this package's message, keys, stop and delete
// resolution treat entries without a project identity the same way.

// HasNoProjectIdentity reports whether an entry carries no project ID in
// either labels or fields (a container that predates project labels).
func HasNoProjectIdentity(a api.AgentInfo) bool {
	return projectkeys.ProjectIDFromLabels(a.Labels) == "" && a.ProjectID == ""
}

// InProjectStrict reports whether an entry is positively identified as
// belonging to projectID (label first, then the ProjectID field). An entry
// with no project identity does not match.
func InProjectStrict(a api.AgentInfo, projectID string) bool {
	if labelProjectID := projectkeys.ProjectIDFromLabels(a.Labels); labelProjectID != "" {
		return labelProjectID == projectID
	}
	return a.ProjectID != "" && a.ProjectID == projectID
}

// LegacyEntryInProject reports whether an entry with no project identity
// may stand for an agent of projectID: it must be a container (ContainerID
// set) whose recorded project path positively identifies as projectID
// (PathIdentifiesAs). Every project-scoped operation accepts such entries
// by this one rule, so they all resolve the same instance.
func LegacyEntryInProject(a api.AgentInfo, projectID string) bool {
	return a.ContainerID != "" && HasNoProjectIdentity(a) &&
		PathIdentifiesAs(a.ProjectPath, projectID)
}

// EntryInProject reports whether an entry may stand for an agent of
// projectID: positively identified as belonging to it (InProjectStrict), or
// an entry with no project identity accepted by LegacyEntryInProject.
func EntryInProject(a api.AgentInfo, projectID string) bool {
	return InProjectStrict(a, projectID) || LegacyEntryInProject(a, projectID)
}

// ScionEntry returns the .scion entry for path, which may be the project root
// or the .scion entry itself.
func ScionEntry(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if filepath.Base(abs) == config.DotScion {
		return abs
	}
	return filepath.Join(abs, config.DotScion)
}

// ProjectIDAtPath returns the project identity recorded at path, or "".
// For a .scion directory it is the project-id file. For a
// .scion marker file (non-git linked project) it is the marker's project ID.
// If path is a project's external config dir, which has no project-id file,
// the result is "".
func ProjectIDAtPath(path string) string {
	if path == "" {
		return ""
	}
	marker := ScionEntry(path)
	if marker == "" {
		return ""
	}
	info, err := os.Stat(marker)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		id, _ := config.ReadProjectID(marker)
		return id
	}
	if m, err := config.ReadProjectMarker(marker); err == nil {
		return m.ProjectID
	}
	return ""
}

// PathIdentifiesAs reports whether the project at path verifiably belongs to
// projectID: either its recorded identity (ProjectIDAtPath) equals projectID,
// or path is the project's external config dir
// ~/.scion/project-configs/<slug>__<short-id>/.scion, whose name encodes the
// project ID (non-git linked projects record that dir as the agent's project
// path). A projectID outside the project ID format never matches.
func PathIdentifiesAs(path, projectID string) bool {
	if path == "" || config.ValidateProjectID(projectID) != nil {
		return false
	}
	if ProjectIDAtPath(path) == projectID {
		return true
	}
	short, ok := ExternalConfigShortID(path)
	return ok && short == (config.ProjectMarker{ProjectID: projectID}).ShortUUID()
}

// ExternalConfigShortID returns the short project ID encoded in path if path
// is an external project config dir ~/.scion/project-configs/<slug>__<short-id>/.scion.
// path is resolved through symlinks before it is split, so a path reached
// through a pre-rename entry (config.MigrateLegacyGlobalLayout leaves a
// symlink at each individual entry it moves, e.g.
// ~/.scion/grove-configs/<name> -> ../project-configs/<name>, while the
// legacy root itself stays a real directory) still identifies as this
// project's external config dir.
func ExternalConfigShortID(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	abs = projectkeys.ResolvePathForCompare(abs)
	if filepath.Base(abs) != config.DotScion {
		return "", false
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return "", false
	}
	projectDir := filepath.Dir(abs)
	parent := filepath.Dir(projectDir)
	if !projectkeys.ResolvedPathEqual(parent, filepath.Join(globalDir, config.ProjectConfigsDir)) {
		return "", false
	}
	name := filepath.Base(projectDir)
	i := strings.LastIndex(name, "__")
	if i <= 0 || i+2 >= len(name) {
		return "", false
	}
	return name[i+2:], true
}
