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
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// Shared project configuration (design-v7 §P1a, ptone/scion#5823).
//
// On a shared storage backend a hub-managed project's configuration lives
// at <ProjectRoot>/.scion, where ProjectRoot is <host base>/<subpath
// root>/<project UUID> (projectfs.Layout.ProjectRoot). The hub seeds it
// with settings.yaml, a project-id file and a project-slug file; brokers
// read it and never write it. It sits beside workspace/ and inside no
// agent mount.
//
// It differs from a git project's .scion directory (split storage) in two
// ways, both decided here so every caller agrees:
//   - project configuration (settings.yaml, templates/, harness-configs/)
//     is the directory itself, never an external project-configs directory
//     (GetGitProjectExternalConfigDir returns "");
//   - every agent's broker-side state directory is broker-local, in the
//     external agents root ~/.scion/project-configs/<slug>__<id8>/.scion/
//     agents, whatever the agent's sharing mode (SelectAgentsRoot,
//     AgentsRootForProject), so nothing broker-private lands on the share
//     and two brokers of the project never share it.
//
// The project name is the slug from the project-slug file, never the
// directory name (a UUID).

// IsSharedProjectConfigDir reports whether projectDir is a shared-backend
// project configuration directory: a directory named .scion whose parent
// directory's name is a valid project ID equal to the project-id file
// inside it. Only the hub's UUID-keyed project tree has that shape.
func IsSharedProjectConfigDir(projectDir string) bool {
	_, ok := sharedProjectConfigID(projectDir)
	return ok
}

// sharedProjectConfigID returns the project ID of a shared project
// configuration directory (IsSharedProjectConfigDir).
func sharedProjectConfigID(projectDir string) (string, bool) {
	if projectDir == "" {
		return "", false
	}
	abs, err := filepath.Abs(projectDir)
	if err != nil || filepath.Base(abs) != DotScion {
		return "", false
	}
	parentName := filepath.Base(filepath.Dir(abs))
	if ValidateProjectID(parentName) != nil {
		return "", false
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() {
		return "", false
	}
	id, err := readProjectIDInDir(abs)
	if err != nil || id != parentName {
		return "", false
	}
	return id, true
}

// ReadProjectSlug reads the project-slug file of a shared project
// configuration directory, through an os.Root on projectDir. A slug that is
// not in the api.Slugify grammar is an error. A missing file reports an
// error for which errors.Is(err, fs.ErrNotExist) is true.
func ReadProjectSlug(projectDir string) (string, error) {
	data, err := readFileInDir(projectDir, projectkeys.ProjectSlugFile)
	if err != nil {
		return "", err
	}
	slug := strings.TrimSpace(string(data))
	if !projectDirSlugPattern.MatchString(slug) {
		return "", errors.New("project-slug is not a valid project slug")
	}
	return slug, nil
}

// SharedProjectSlug returns the slug of a shared project configuration
// directory: ok is false when projectDir is not one, and slug is "" when
// its project-slug file is absent or unusable.
func SharedProjectSlug(projectDir string) (slug string, ok bool) {
	if !IsSharedProjectConfigDir(projectDir) {
		return "", false
	}
	slug, err := ReadProjectSlug(projectDir)
	if err != nil {
		warnSharedProjectSlugOnce(projectDir, err)
		return "", true
	}
	return slug, true
}

var sharedProjectSlugWarned sync.Map

// warnSharedProjectSlugOnce logs, once per directory, that a shared
// project configuration has no usable project-slug (the project name then
// falls back to the directory name; brokers refuse such a dispatch).
func warnSharedProjectSlugOnce(projectDir string, err error) {
	if _, loaded := sharedProjectSlugWarned.LoadOrStore(projectDir, struct{}{}); loaded {
		return
	}
	reason := "unusable"
	if errors.Is(err, fs.ErrNotExist) {
		reason = "absent"
	}
	slog.Warn("shared project configuration has no usable project-slug; using the directory name as the project name",
		"path", projectDir, "reason", reason)
}

// sharedProjectExternalAgentsDir returns the broker-local external agents
// root of a shared project configuration directory:
// ~/.scion/project-configs/<slug>__<id8>/.scion/agents. ok is false when
// projectDir is not a shared project configuration directory.
func sharedProjectExternalAgentsDir(projectDir string) (dir string, ok bool, err error) {
	id, isShared := sharedProjectConfigID(projectDir)
	if !isShared {
		return "", false, nil
	}
	name := GetProjectName(projectDir)
	marker := &ProjectMarker{ProjectID: id, ProjectName: name, ProjectSlug: name}
	ext, err := marker.ExternalProjectPath()
	if err != nil {
		return "", true, err
	}
	return filepath.Join(ext, "agents"), true, nil
}

// LegacySharedProjectAgentDir returns the in-project agent directory an
// agent of a shared project had before its configuration moved to the
// project root (ptone/scion#5823): ~/.scion/projects/<slug>/.scion/agents/
// <name>, when that directory holds a regular scion-agent.json and the
// legacy .scion directory names the same project. It is a read-only
// look-up fallback for agents provisioned before the move (temporary;
// ptone/scion#5835); "" when there is none.
func LegacySharedProjectAgentDir(projectDir, agentName string) string {
	id, ok := sharedProjectConfigID(projectDir)
	if !ok || agentName == "" || agentName != filepath.Base(agentName) || agentName == "." || agentName == ".." {
		return ""
	}
	slug, err := ReadProjectSlug(projectDir)
	if err != nil {
		return ""
	}
	globalDir, err := GetGlobalDir()
	if err != nil {
		return ""
	}
	legacyScion := filepath.Join(globalDir, projectkeys.ProjectsDir, slug, DotScion)
	if info, err := os.Lstat(legacyScion); err != nil || !info.IsDir() {
		return ""
	}
	if legacyID, err := readProjectIDInDir(legacyScion); err != nil || legacyID != id {
		return ""
	}
	dir := filepath.Join(legacyScion, "agents", agentName)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return ""
	}
	if info, err := os.Lstat(filepath.Join(dir, "scion-agent.json")); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return dir
}

// sharedProjectAgentDir resolves an agent's broker-side state directory
// for a shared project configuration directory: the external agents root,
// or, when the agent has no scion-agent.json there, its legacy in-project
// directory (LegacySharedProjectAgentDir) if one exists.
func sharedProjectAgentDir(projectDir, agentName string) (dir string, ok bool, err error) {
	root, ok, err := sharedProjectExternalAgentsDir(projectDir)
	if !ok || err != nil {
		return "", ok, err
	}
	ext := filepath.Join(root, agentName)
	if info, err := os.Lstat(filepath.Join(ext, "scion-agent.json")); err == nil && info.Mode().IsRegular() {
		return ext, true, nil
	}
	if legacy := LegacySharedProjectAgentDir(projectDir, agentName); legacy != "" {
		return legacy, true, nil
	}
	return ext, true, nil
}
