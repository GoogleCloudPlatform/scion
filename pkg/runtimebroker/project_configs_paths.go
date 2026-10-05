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
	"regexp"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Helpers that locate a hub-managed project's storage under
// ~/.scion/project-configs from trusted inputs only: the project slug the
// broker was addressed with (the ~/.scion/projects/<slug> directory name, or
// the slug on a hub request) and the project ID the hub asked for. Values
// read from a project's .scion entry are checked against the path computed
// from those inputs, never used to build it.

// hubProjectSlugPattern is the grammar of a hub project slug as produced by
// api.Slugify: lowercase letters, digits and inner dashes, at most
// api.MaxSlugLength characters. Only a slug matching it is used as a path
// component.
var hubProjectSlugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// projectShortIDPattern is the grammar of config.ProjectMarker.ShortUUID for
// a project ID: up to eight letters or digits.
var projectShortIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,8}$`)

// expectedProjectConfigDir returns
// <globalDir>/project-configs/<slug>__<short project ID>, the directory that
// config.ProjectMarker.DirName names for this project, or "" when slug or
// the short project ID does not match its grammar.
func expectedProjectConfigDir(globalDir, slug, projectID string) string {
	if !hubProjectSlugPattern.MatchString(slug) {
		return ""
	}
	marker := config.ProjectMarker{ProjectID: projectID, ProjectSlug: slug}
	if !projectShortIDPattern.MatchString(marker.ShortUUID()) {
		return ""
	}
	return filepath.Join(globalDir, config.ProjectConfigsDir, marker.DirName())
}

// projectConfigPathContained reports whether path is an existing real
// directory strictly inside <globalDir>/project-configs: no component from
// project-configs down to path is a symlink, and path with every symlink
// resolved is still the same location inside the resolved project-configs
// root (so a symlinked ~/.scion or project-configs root, a supported layout,
// is fine, but nothing below it may point elsewhere).
func projectConfigPathContained(globalDir, path string) bool {
	root := filepath.Join(globalDir, config.ProjectConfigsDir)
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false
		}
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	// Equality with the resolved root joined with rel implies containment
	// at a separator boundary.
	return realPath == filepath.Join(realRoot, rel)
}

// markerProjectPathTrusted is the structural identity check applied to a
// marker-resolved project path before it is used (trustedEntryProjectPath:
// the path must be this project's external config dir under
// project-configs). It is a variable only so a test can prove the marker
// branch of findAgentInHubManagedProjects consults it.
var markerProjectPathTrusted = trustedEntryProjectPath

// hubMarkerProjectScionDir returns the external .scion dir
// <globalDir>/project-configs/<slug>__<short projectID>/.scion of the
// hub-managed project ~/.scion/projects/<slug>, whose .scion entry at
// markerPath is a marker file, or "" when it cannot be trusted. projectID
// must be the project the caller was asked about; the path is computed from
// slug and projectID, and the marker only has to agree with it: its project
// ID and slug must equal them and the directory it resolves to must be that
// same path. The resolved dir must also be a real directory inside
// project-configs (projectConfigPathContained) and pass
// markerProjectPathTrusted.
func hubMarkerProjectScionDir(globalDir, slug, projectID, markerPath string) string {
	if projectID == "" {
		return ""
	}
	dir := expectedProjectConfigDir(globalDir, slug, projectID)
	if dir == "" {
		return ""
	}
	marker, err := config.ReadProjectMarker(markerPath)
	if err != nil || marker.ProjectID != projectID || marker.ProjectSlug != slug {
		return ""
	}
	want := filepath.Join(dir, config.DotScion)
	resolved, err := marker.ExternalProjectPath()
	if err != nil || filepath.Clean(resolved) != want {
		return ""
	}
	if !projectConfigPathContained(globalDir, want) || !markerProjectPathTrusted(want, projectID) {
		return ""
	}
	// An external config dir normally records no project ID; one that
	// records a different project is not this project's.
	if id := projectIDAtPath(want); id != "" && id != projectID {
		return ""
	}
	return want
}
