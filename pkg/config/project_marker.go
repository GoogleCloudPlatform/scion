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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"gopkg.in/yaml.v3"
)

// ProjectMarker represents the content of a .scion marker file.
// When .scion is a file (not a directory), it points to an external
// project-config directory under ~/.scion/project-configs/.
type ProjectMarker struct {
	ProjectID   string `yaml:"project-id"`
	ProjectName string `yaml:"project-name"`
	ProjectSlug string `yaml:"project-slug"`
	Type        string `yaml:"type,omitempty"` // "shadow" for shadowed projects
}

// ErrInvalidProjectID reports a project ID that does not match the project
// ID format accepted by ValidateProjectID.
var ErrInvalidProjectID = errors.New("invalid project ID")

// projectIDPattern is the project ID format. GenerateProjectID produces
// canonical UUIDs, which match it; it also admits other tokens of up to 128
// letters, digits, '.', '_' and '-' that start with a letter or digit. Every
// matching ID is a single, non-special path element.
var projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateProjectID returns an error wrapping ErrInvalidProjectID unless id
// matches the project ID format.
func ValidateProjectID(id string) error {
	if !projectIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrInvalidProjectID, id)
	}
	return nil
}

// invalidShortID is the ShortUUID of a project ID that fails
// ValidateProjectID. It contains a '-', which never appears in the short form
// of a valid ID, so it cannot coincide with one.
const invalidShortID = "invalid-id"

// IsShadow returns true if this marker represents a shadowed project.
func (m *ProjectMarker) IsShadow() bool {
	return m.Type == "shadow"
}

// ShortUUID returns a short form of the project ID for use in directory names.
// The result is always a single path element: an ID that fails
// ValidateProjectID yields a fixed placeholder.
func (m ProjectMarker) ShortUUID() string {
	if ValidateProjectID(m.ProjectID) != nil {
		return invalidShortID
	}
	id := strings.ReplaceAll(m.ProjectID, "-", "")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// DirName returns the directory name used under ~/.scion/project-configs/.
// The result is always a single path element: a slug containing a path
// separator or NUL is replaced by its slugified form.
func (m ProjectMarker) DirName() string {
	slug := m.ProjectSlug
	if strings.ContainsAny(slug, "/\\\x00") {
		slug = api.Slugify(slug)
	}
	return fmt.Sprintf("%s__%s", slug, m.ShortUUID())
}

// ExternalProjectPath returns the absolute path to the external project config
// directory: ~/.scion/project-configs/<project-slug>__<short-uuid>/.scion/
// It returns an error if the project ID does not match the project ID format.
func (m ProjectMarker) ExternalProjectPath() (string, error) {
	if err := ValidateProjectID(m.ProjectID); err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, GlobalDir, ProjectConfigsDir, m.DirName(), DotScion), nil
}

// ReadProjectMarker reads and parses a .scion marker file. A legacy
// grove-id/grove-name/grove-slug key is migrated to project-id/project-name/
// project-slug as a side effect on every call (see migrateLegacyMarkerFile;
// each migration event is reported at most once per process, but the
// filesystem is always re-checked): when the rewrite cannot happen (e.g. a
// read-only filesystem), the legacy value is used for this call only.
func ReadProjectMarker(path string) (*ProjectMarker, error) {
	overrides := migrateLegacyMarkerFile(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var marker ProjectMarker
	if err := yaml.Unmarshal(data, &marker); err != nil {
		return nil, fmt.Errorf("invalid project marker at %s: %w", path, err)
	}
	if marker.ProjectID == "" {
		marker.ProjectID = overrides["project-id"]
	}
	if marker.ProjectName == "" {
		marker.ProjectName = overrides["project-name"]
	}
	if marker.ProjectSlug == "" {
		marker.ProjectSlug = overrides["project-slug"]
	}
	if marker.ProjectID == "" || marker.ProjectSlug == "" {
		return nil, fmt.Errorf("invalid project marker at %s: missing project-id or project-slug", path)
	}
	if err := ValidateProjectID(marker.ProjectID); err != nil {
		return nil, fmt.Errorf("invalid project marker at %s: %w", path, err)
	}
	return &marker, nil
}

// WriteProjectMarker writes a ProjectMarker to the given path as a YAML file.
func WriteProjectMarker(path string, marker *ProjectMarker) error {
	data, err := yaml.Marshal(marker)
	if err != nil {
		return fmt.Errorf("failed to marshal project marker: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// ResolveProjectMarker reads a .scion marker file and returns the resolved
// external project path. Returns an error if the marker is invalid or the
// external path cannot be computed.
func ResolveProjectMarker(markerPath string) (string, error) {
	marker, err := ReadProjectMarker(markerPath)
	if err != nil {
		return "", err
	}
	return marker.ExternalProjectPath()
}

// IsProjectMarkerFile returns true if the given path is a regular file
// (not a directory) that could be a project marker. Does not validate content.
func IsProjectMarkerFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// IsOldStyleNonGitProject returns true if the path is a .scion directory
// in a non-git project (not the global ~/.scion/). This indicates an
// old-format project that needs to be re-initialized.
func IsOldStyleNonGitProject(scionPath string) bool {
	info, err := os.Stat(scionPath)
	if err != nil || !info.IsDir() {
		return false
	}

	// Don't flag the global project
	home, err := os.UserHomeDir()
	if err == nil {
		globalDir := filepath.Join(home, GlobalDir)
		if abs, err := filepath.Abs(scionPath); err == nil {
			evalAbs, _ := filepath.EvalSymlinks(abs)
			evalGlobal, _ := filepath.EvalSymlinks(globalDir)
			if evalAbs == evalGlobal {
				return false
			}
		}
	}

	// Check if the parent directory is a git repo
	parent := filepath.Dir(scionPath)
	gitDir := filepath.Join(parent, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		return false // Git project — not old-style (handled by Phase 3)
	}

	return true
}

// IsHubContext returns true if hub context environment variables are available,
// indicating the CLI is running inside a hub-connected agent container where
// project data should be accessed via the Hub API rather than the local filesystem.
// Checks SCION_HUB_ENDPOINT (primary), SCION_HUB_URL (legacy), and
// SCION_PROJECT_ID (always set for broker-dispatched agents).
func IsHubContext() bool {
	return os.Getenv("SCION_HUB_ENDPOINT") != "" ||
		os.Getenv("SCION_HUB_URL") != "" ||
		os.Getenv(projectkeys.EnvProjectID) != ""
}

// WriteWorkspaceMarker writes a minimal .scion marker file into a workspace
// directory so that in-container CLI can discover the project context.
// This is called during agent provisioning for git projects (where the worktree
// doesn't contain .scion because it's gitignored) and for hub-managed projects.
func WriteWorkspaceMarker(workspacePath string, projectID, projectName, projectSlug string) error {
	if projectID == "" || projectSlug == "" {
		return fmt.Errorf("project-id and project-slug are required for workspace marker")
	}
	marker := &ProjectMarker{
		ProjectID:   projectID,
		ProjectName: projectName,
		ProjectSlug: projectSlug,
	}
	return WriteProjectMarker(filepath.Join(workspacePath, DotScion), marker)
}

// ExtractSlugFromExternalDir extracts the project slug from an external
// project-config directory name in the format "slug__shortuuid".
func ExtractSlugFromExternalDir(dirName string) string {
	if parts := strings.SplitN(dirName, "__", 2); len(parts) == 2 {
		return parts[0]
	}
	return ""
}

// ReadProjectID reads the project-id file from a git project's .scion
// directory. A legacy .scion/grove-id file is migrated to project-id as a
// side effect on every call (see MigrateLegacyProject; each migration event
// is reported at most once per process, but the filesystem is always
// re-checked, so a project-id removed later or a grove-id that appears
// later are both handled correctly): when the rewrite cannot happen (e.g. a
// read-only filesystem), the legacy value is used for this call only.
// A value that does not match the project ID format (see ValidateProjectID)
// is reported as an error wrapping ErrInvalidProjectID.
func ReadProjectID(projectDir string) (string, error) {
	overrides := MigrateLegacyProject(projectDir, currentProjectMigrationReporter())

	path := filepath.Join(projectDir, projectkeys.ProjectIDFile)
	data, err := os.ReadFile(path)
	if err == nil {
		return checkedProjectID(path, strings.TrimSpace(string(data)))
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	if overrides.ProjectID != "" {
		return checkedProjectID(path, overrides.ProjectID)
	}
	return "", err
}

// checkedProjectID returns id if it matches the project ID format, and
// otherwise an error naming the file it was read from.
func checkedProjectID(path, id string) (string, error) {
	if err := ValidateProjectID(id); err != nil {
		return "", fmt.Errorf("project-id at %s: %w", path, err)
	}
	return id, nil
}

// mkdirUnderProjectConfigs creates dir and any missing parents. dir must be
// below ~/.scion/project-configs; everything below that directory is created
// through an os.Root opened on it, so creation stays within it.
func mkdirUnderProjectConfigs(dir string, perm os.FileMode) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	parent := filepath.Join(home, GlobalDir, ProjectConfigsDir)
	rel, err := filepath.Rel(parent, dir)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return fmt.Errorf("directory %s is not below %s", dir, parent)
	}
	if err := os.MkdirAll(parent, perm); err != nil {
		return err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.MkdirAll(rel, perm)
}

// WriteProjectID writes a project-id file to a git project's .scion directory.
func WriteProjectID(projectDir string, projectID string) error {
	return os.WriteFile(filepath.Join(projectDir, projectkeys.ProjectIDFile), []byte(projectID+"\n"), 0644)
}

// GetGitProjectExternalConfigDir returns the external config directory for a git project.
// Git projects store settings and templates externally at ~/.scion/project-configs/<slug>__<uuid>/.scion/
// while keeping worktrees in-repo.
// Returns ("", nil) if the project-id file does not exist (not yet initialized for split storage).
func GetGitProjectExternalConfigDir(projectDir string) (string, error) {
	projectID, err := ReadProjectID(projectDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	projectName := GetProjectName(projectDir)
	projectSlug := api.Slugify(projectName)
	marker := &ProjectMarker{
		ProjectID:   projectID,
		ProjectName: projectName,
		ProjectSlug: projectSlug,
	}

	return marker.ExternalProjectPath()
}

// GetGitProjectExternalAgentsDir returns the external agents directory for a git project.
// Git projects store agent homes externally at ~/.scion/project-configs/<slug>__<uuid>/.scion/agents/
// while keeping worktrees in-repo.
// Returns ("", nil) if the project-id file does not exist (not yet initialized for split storage).
func GetGitProjectExternalAgentsDir(projectDir string) (string, error) {
	projectID, err := ReadProjectID(projectDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	projectName := GetProjectName(projectDir)
	projectSlug := api.Slugify(projectName)
	marker := &ProjectMarker{
		ProjectID:   projectID,
		ProjectName: projectName,
		ProjectSlug: projectSlug,
	}

	extPath, err := marker.ExternalProjectPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(extPath, "agents"), nil
}

// GetAgentHomePath returns the correct home directory path for an agent.
// For git projects with split storage (project-id file exists), this returns
// the external path under ~/.scion/project-configs/.
// For non-git projects (projectDir already resolved to external via marker),
// or git projects without split storage, returns the in-repo path.
func GetAgentHomePath(projectDir, agentName string) string {
	if externalDir, err := GetGitProjectExternalAgentsDir(projectDir); err == nil && externalDir != "" {
		return filepath.Join(externalDir, agentName, "home")
	}
	return filepath.Join(projectDir, "agents", agentName, "home")
}

// SelectAgentsRoot returns the directory GetAgentDir addresses a given
// agent's directory under, for the same (projectDir, sharedWorkspace)
// inputs: the external agents directory when sharedWorkspace is true and one
// is configured, otherwise <projectDir>/agents. GetAgentDir is defined in
// terms of this function so the two can never select different roots; a
// caller that needs to confirm a computed agent directory is still under
// the intended root (for example, before a directory removal) should
// recompute the root with this function rather than assuming
// <projectDir>/agents.
func SelectAgentsRoot(projectDir string, sharedWorkspace bool) string {
	if sharedWorkspace {
		if externalDir, err := GetGitProjectExternalAgentsDir(projectDir); err == nil && externalDir != "" {
			return externalDir
		}
	}
	return filepath.Join(projectDir, "agents")
}

// GetAgentDir returns the broker-side directory for an agent's per-agent state
// files (prompt.md, scion-agent.json, and — in worktree mode — the workspace
// subdir).
//
// For shared-workspace git projects (sharedWorkspace == true and a project-id
// marker exists), this returns the external path under
// ~/.scion/project-configs/<slug>__<uuid>/.scion/agents/<name>/ so that sibling
// agents do not see each other's state via the shared /workspace mount. See
// .design/hub-shared-workspace-isolation.md for the threat model.
//
// For all other modes (worktree mode, non-git projects, or shared-workspace
// projects without an initialized project-id), this returns the in-project path
// <projectDir>/agents/<name>/ — preserving the worktree-relative layout that
// git's worktree pointers depend on.
func GetAgentDir(projectDir, agentName string, sharedWorkspace bool) string {
	return filepath.Join(SelectAgentsRoot(projectDir, sharedWorkspace), agentName)
}

// ResolveAgentDir returns the broker-side per-agent state directory when the
// shared-workspace mode is not known to the caller. It probes the external
// path first (used by shared-workspace projects), then falls back to the
// in-project path. A read-time companion to GetAgentDir, used by code paths
// that look up an existing agent by name without carrying the
// sharedWorkspace flag through the call stack.
//
// Returns the external path only when a project-id marker exists *and* the
// external per-agent directory contains scion-agent.json (which never lives
// external in worktree mode — only home/ does). Otherwise returns the
// in-project path.
func ResolveAgentDir(projectDir, agentName string) string {
	if externalDir, err := GetGitProjectExternalAgentsDir(projectDir); err == nil && externalDir != "" {
		ext := filepath.Join(externalDir, agentName)
		if _, err := os.Stat(filepath.Join(ext, "scion-agent.json")); err == nil {
			return ext
		}
	}
	return filepath.Join(projectDir, "agents", agentName)
}
