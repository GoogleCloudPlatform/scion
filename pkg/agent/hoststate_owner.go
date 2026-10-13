// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// The run that owns an agent's files (ptone/scion#2675) is recorded as
// HostState.RunID in HostStateFile, which the host owns, rather than in
// agent-info.json in the agent home (ptone/scion#4771): some runtimes do
// not let the host and the container share a writable home. A delete or
// failure cleanup for one run leaves alone the files of an agent recreated
// under the same name by another run.
//
// RecordRunOwner is the only writer of the record and RecordedRunOwner the
// only reader. Both locate the file with HostStateRootFor, so they always
// address the same file.

// RunOwnerTarget names the agent whose files-owner run is recorded, by
// identity the broker holds. None of it is read from the project tree.
type RunOwnerTarget struct {
	// ProjectPath is the project path the broker holds for the agent: a
	// hub-managed ~/.scion/projects/<slug>, a linked project's registered
	// path, or either one's .scion entry. "global" or "home" names the
	// global project, and "" the broker's working project (as
	// DeleteAgentFiles resolves it).
	ProjectPath string
	// AgentName comes from the request or dispatch.
	AgentName string
	// HubProjectID is the hub project ID of the request or dispatch, ""
	// for a local start.
	HubProjectID string
	// SharedWorkspace is the shared-workspace flag of the request.
	SharedWorkspace bool
}

// RunOwnerTargetFor returns the RunOwnerTarget of a start or provision:
// the broker-held project path, agent name, hub project ID and
// shared-workspace flag of opts (the broker sets HubProjectID from the
// request, never from the project tree).
func RunOwnerTargetFor(opts api.StartOptions) RunOwnerTarget {
	return RunOwnerTarget{
		ProjectPath:     opts.ProjectPath,
		AgentName:       opts.Name,
		HubProjectID:    opts.HubProjectID,
		SharedWorkspace: opts.SharedWorkspace,
	}
}

var (
	// errHostStateRootRefused is returned when no trusted root can be
	// derived for a RunOwnerTarget.
	errHostStateRootRefused = errors.New("no trusted host state root")
	// errInvalidRecordedRunID is returned for a run ID that is not a
	// valid run ID (see runtime.ValidateRunID).
	errInvalidRecordedRunID = errors.New("invalid recorded run ID")
)

// hostStateSlugPattern is the grammar api.Slugify produces (the same as
// runtimebroker's hubProjectSlugPattern). Only a slug matching it names an
// external project-config dir.
var hostStateSlugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// hostStateShortIDPattern is the grammar of config.ProjectMarker.ShortUUID
// for a valid project ID.
var hostStateShortIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,8}$`)

func refuseHostStateRoot(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errHostStateRootRefused, fmt.Sprintf(format, args...))
}

// runOwnerProjectRoot returns the absolute project root (the directory
// holding .scion) for projectPath, with symlinks resolved, as
// config.ResolveProjectPath addresses the project.
func runOwnerProjectRoot(projectPath string) (string, error) {
	var root string
	switch projectPath {
	case "":
		// The nearest directory above the working directory with a .scion
		// entry, as config.FindProjectRoot finds it, but without following
		// the entry: HostStateRootFor checks it.
		wd, err := os.Getwd()
		if err != nil {
			return "", refuseHostStateRoot("working directory: %v", err)
		}
		for dir := wd; ; dir = filepath.Dir(dir) {
			if _, err := os.Lstat(filepath.Join(dir, config.DotScion)); err == nil {
				root = dir
				break
			}
			if filepath.Dir(dir) == dir {
				return "", refuseHostStateRoot("no working project")
			}
		}
	case "global", "home":
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			return "", refuseHostStateRoot("global project: %v", err)
		}
		root = filepath.Dir(globalDir)
	default:
		abs, err := filepath.Abs(projectPath)
		if err != nil {
			return "", refuseHostStateRoot("project path: %v", err)
		}
		if filepath.Base(abs) == config.DotScion {
			abs = filepath.Dir(abs)
		}
		root = abs
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", refuseHostStateRoot("project root: %v", err)
	}
	return resolved, nil
}

// externalHostStateRoot returns
// <globalDir>/project-configs/<slug>__<short hubProjectID>, the directory
// config.ProjectMarker.DirName names for the project, or "" when slug or
// the short project ID is outside its grammar.
func externalHostStateRoot(slug, hubProjectID string) (string, error) {
	if !hostStateSlugPattern.MatchString(slug) {
		return "", nil
	}
	marker := config.ProjectMarker{ProjectID: hubProjectID, ProjectSlug: slug}
	if !hostStateShortIDPattern.MatchString(marker.ShortUUID()) {
		return "", nil
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(globalDir, config.ProjectConfigsDir, marker.DirName()), nil
}

// isRealDir reports whether path is a directory and not a symlink.
func isRealDir(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsDir()
}

// HostStateRootFor returns the trusted root (see HostStateFile) of the
// HostStateFile holding t's files-owner run, or an error wrapping
// errHostStateRootRefused when there is none. The root is derived from
// t only; the project tree contributes one Lstat of <root>/.scion, and a
// .scion marker file is read only to check that it agrees:
//
//   - <root>/.scion is a symlink, missing, or not a file or directory:
//     refused.
//   - <root>/.scion is a marker file (a hub-managed or non-git project):
//     the root is the external dir E =
//     ~/.scion/project-configs/<slug>__<short HubProjectID>, where slug is
//     the project's name as config derives it from the project path. The
//     marker's project ID must equal HubProjectID, its slug must equal
//     slug, and the path it resolves to must be E/.scion; E and E/.scion
//     must be real directories. Anything else, or no HubProjectID, is
//     refused.
//   - <root>/.scion is a directory and the agent is a shared-workspace
//     agent (t.SharedWorkspace, or E/.scion/agents/<name> holds a regular
//     scion-agent.json): the root is E, never the project's own
//     .scion/agents. No HubProjectID, or an E that is not a real
//     directory, is refused.
//   - <root>/.scion is a directory otherwise: the root is the project
//     root. A project-id file inside .scion is not read.
//
// E is the root config.AgentsRootForProject picks from the hub project ID,
// so the record sits in the agent's own directory. The content of any
// HostStateFile is never consulted, so a file found under a root does not
// make that root right.
func HostStateRootFor(t RunOwnerTarget) (string, error) {
	if err := validateHostStateAgentName(t.AgentName); err != nil {
		return "", err
	}
	root, err := runOwnerProjectRoot(t.ProjectPath)
	if err != nil {
		return "", err
	}
	scionPath := filepath.Join(root, config.DotScion)
	fi, err := os.Lstat(scionPath)
	if err != nil {
		return "", refuseHostStateRoot("%s: %v", scionPath, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return "", refuseHostStateRoot("%s is a symlink", scionPath)
	}
	if !fi.Mode().IsRegular() && !fi.Mode().IsDir() {
		return "", refuseHostStateRoot("%s is not a file or directory", scionPath)
	}

	slug := config.GetProjectName(scionPath)
	ext := ""
	if t.HubProjectID != "" {
		ext, err = externalHostStateRoot(slug, t.HubProjectID)
		if err != nil {
			return "", refuseHostStateRoot("external root: %v", err)
		}
	}

	if fi.Mode().IsRegular() {
		if ext == "" {
			return "", refuseHostStateRoot("%s is a marker and the project has no hub identity", scionPath)
		}
		marker, err := config.ReadProjectMarker(scionPath)
		if err != nil {
			return "", refuseHostStateRoot("%s: %v", scionPath, err)
		}
		if marker.ProjectID != t.HubProjectID {
			return "", refuseHostStateRoot("%s names another project", scionPath)
		}
		if marker.ProjectSlug != slug {
			return "", refuseHostStateRoot("%s names slug %q, want %q", scionPath, marker.ProjectSlug, slug)
		}
		resolved, err := marker.ExternalProjectPath()
		if err != nil || filepath.Clean(resolved) != filepath.Join(ext, config.DotScion) {
			return "", refuseHostStateRoot("%s does not resolve to %s", scionPath, ext)
		}
		if !isRealDir(ext) || !isRealDir(filepath.Join(ext, config.DotScion)) {
			return "", refuseHostStateRoot("%s is not a real directory", ext)
		}
		return ext, nil
	}

	shared := t.SharedWorkspace
	if !shared && ext != "" {
		sf, err := os.Lstat(filepath.Join(ext, config.DotScion, "agents", t.AgentName, "scion-agent.json"))
		shared = err == nil && sf.Mode().IsRegular()
	}
	if shared {
		if ext == "" {
			return "", refuseHostStateRoot("shared-workspace agent %q has no hub identity", t.AgentName)
		}
		if !isRealDir(ext) {
			return "", refuseHostStateRoot("%s is not a real directory", ext)
		}
		return ext, nil
	}
	return root, nil
}

// RecordRunOwner records runID as the run that owns t's files, in
// HostStateFile. It writes nothing when runID is not a valid run ID or no
// trusted root can be derived (both return an error), and it is a no-op
// when the agent's directory does not exist yet (a later provision records
// the run itself). It never writes agent-info.json.
//
// Callers log a failure and carry on, as they did for the agent-info.json
// record: the files are then recorded as before (or not at all), and the
// broker's readers treat a record they cannot read as unknown, keeping the
// files.
func RecordRunOwner(t RunOwnerTarget, runID string) error {
	if runID == "" || runtime.ValidateRunID(runID) != nil {
		return errInvalidRecordedRunID
	}
	root, err := HostStateRootFor(t)
	if err != nil {
		return err
	}
	err = UpdateHostState(root, t.AgentName, func(hs *HostState) error {
		hs.RunID = runID
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// RecordedRunOwner returns the run recorded as the owner of t's files, or
// "" when none is recorded. An error means the owner is unknown: no
// trusted root, an unreadable, symlinked or corrupt HostStateFile, or a
// recorded value that is not a valid run ID. Callers keep the files then.
// A recorded value is data, not proof: it can only name the run the files
// belong to, and a caller deletes only for the run it is acting on.
//
// When HostStateFile records no run, the transitional fallback
// legacyAgentInfoRunID is returned (see there).
func RecordedRunOwner(t RunOwnerTarget) (string, error) {
	root, err := HostStateRootFor(t)
	if err != nil {
		return "", err
	}
	hs, err := ReadHostState(root, t.AgentName)
	if err != nil {
		return "", err
	}
	if hs.RunID != "" {
		if runtime.ValidateRunID(hs.RunID) != nil {
			return "", errInvalidRecordedRunID
		}
		return hs.RunID, nil
	}
	return legacyAgentInfoRunID(t.ProjectPath, t.AgentName), nil
}

// maxLegacyAgentInfoSize caps the legacyAgentInfoRunID read.
const maxLegacyAgentInfoSize = 64 << 10

// legacyAgentInfoRunID is transitional (ptone/scion#4771): it returns the
// runId that agent-info.json in the agent home recorded before the record
// moved to HostStateFile, so agents provisioned or started before the move
// keep their ptone/scion#2675 protection until their next start or
// provision records the run in HostStateFile. It can be removed once such
// agents have restarted.
//
// It is read only when HostStateFile records no run, and nothing writes
// it (updateSavedAgentInfo drops it). Callers compare it with the run they
// act on, so it can only keep files (a different run) or change nothing
// (the same run): a missing, symlinked, unreadable, oversized or invalid
// value gives "", as a missing record did before, and so does a home the
// host cannot read.
func legacyAgentInfoRunID(projectPath, agentName string) string {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return ""
	}
	path := filepath.Join(config.GetAgentHomePath(projectDir, agentName), "agent-info.json")
	lfi, err := os.Lstat(path)
	if err != nil || !lfi.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !os.SameFile(lfi, fi) {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, maxLegacyAgentInfoSize+1))
	if err != nil || len(data) > maxLegacyAgentInfoSize {
		return ""
	}
	var info struct {
		RunID string `json:"runId"`
	}
	if json.Unmarshal(data, &info) != nil || info.RunID == "" || runtime.ValidateRunID(info.RunID) != nil {
		return ""
	}
	return info.RunID
}
