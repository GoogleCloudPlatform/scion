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
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
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
	// global project. "" names the nearest directory above the working
	// directory with a .scion entry (not followed), or the global project
	// when there is none; unlike config.FindProjectRoot it does not skip a
	// marker that does not agree, which is then refused (see
	// runOwnerProjectRoot).
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
	// errRunOwnerDirMismatch is returned by recordRunOwnerInAgentDir when
	// the record's dir is not the agent dir the caller holds.
	errRunOwnerDirMismatch = errors.New("host state root does not address the agent's dir")
)

func refuseHostStateRoot(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errHostStateRootRefused, fmt.Sprintf(format, args...))
}

// runOwnerProjectRoot returns the absolute project root (the directory
// holding .scion) for projectPath, with symlinks resolved, and the project
// dir config.ResolveProjectPath gives for it when its .scion is a
// directory (the input config.GetProjectName names the project from):
//
//   - "global" or "home": the parent of the global dir, and the global
//     dir as config gives it (not resolved);
//   - "": the nearest directory above the working directory that has a
//     .scion entry, which is not followed (a marker found there is
//     checked like any other, so one that does not agree is refused
//     rather than skipped as config.FindProjectRoot skips it); with no
//     .scion entry above, the global project as for "global";
//   - otherwise the path, or its parent when its base name is .scion.
func runOwnerProjectRoot(projectPath string) (root, configDir string, err error) {
	global := func() (string, string, error) {
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			return "", "", refuseHostStateRoot("global project: %v", err)
		}
		return filepath.Dir(globalDir), globalDir, nil
	}
	switch projectPath {
	case "":
		wd, err := os.Getwd()
		if err != nil {
			return "", "", refuseHostStateRoot("working directory: %v", err)
		}
		for dir := wd; ; dir = filepath.Dir(dir) {
			if _, err := os.Lstat(filepath.Join(dir, config.DotScion)); err == nil {
				root = dir
				break
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
		if root == "" {
			if root, configDir, err = global(); err != nil {
				return "", "", err
			}
		}
	case "global", "home":
		if root, configDir, err = global(); err != nil {
			return "", "", err
		}
	default:
		abs, err := filepath.Abs(projectPath)
		if err != nil {
			return "", "", refuseHostStateRoot("project path: %v", err)
		}
		if filepath.Base(abs) == config.DotScion {
			abs = filepath.Dir(abs)
		}
		root = abs
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", refuseHostStateRoot("project root: %v", err)
	}
	if configDir == "" {
		// config resolves a .scion directory with EvalSymlinks; .scion
		// itself is refused below when it is a symlink.
		configDir = filepath.Join(resolved, config.DotScion)
	}
	return resolved, configDir, nil
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
// .scion marker file is read only to check that it agrees.
//
// The project's slug is api.Slugify(config.GetProjectName(dir)) for the
// project dir config resolves (see runOwnerProjectRoot), as
// config.AgentsRootForProject derives it, and the external dir is
// E = ~/.scion/project-configs/<slug>__<short HubProjectID>; with no
// HubProjectID, or a slug or ID outside the dir-name grammar
// (config.ProjectMarker.HasCanonicalDirName), there is no E.
//
//   - <root>/.scion is a symlink, missing, or not a file or directory:
//     refused.
//   - <root>/.scion is a marker file (a hub-managed or non-git project):
//     the root is E. The marker's project ID must equal HubProjectID and
//     its slug must equal the slug; E and E/.scion must be real
//     directories. No E, or anything else, is refused.
//   - <root>/.scion is a shared project configuration directory
//     (config.IsSharedProjectConfigDir): the root holding the agent dir
//     config.AgentDirForProject resolves (the external agents root, for
//     every sharing mode, or the agent's legacy in-project dir); see
//     sharedProjectHostStateRoot.
//   - <root>/.scion is a directory and the agent is a shared-workspace
//     agent (t.SharedWorkspace, or E/.scion/agents/<name>/scion-agent.json
//     is a regular file, checked with Stat as effectiveSharedWorkspace
//     does): the root is E, never the project's own .scion/agents. No E,
//     or an E that is not a real directory, is refused.
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
	root, configDir, err := runOwnerProjectRoot(t.ProjectPath)
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

	slug := api.Slugify(config.GetProjectName(configDir))
	ext, extMissing := "", ""
	switch t.HubProjectID {
	case "":
		extMissing = "the request has no hub project ID"
	default:
		m := config.ProjectMarker{ProjectID: t.HubProjectID, ProjectSlug: slug}
		if !m.HasCanonicalDirName() {
			extMissing = fmt.Sprintf("slug %q or hub project ID %q is outside the project-config dir-name grammar", slug, t.HubProjectID)
			break
		}
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			return "", refuseHostStateRoot("external root: %v", err)
		}
		ext = filepath.Join(globalDir, config.ProjectConfigsDir, m.DirName())
	}

	if fi.Mode().IsRegular() {
		if ext == "" {
			return "", refuseHostStateRoot("%s is a marker and %s", scionPath, extMissing)
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
		// The marker's own resolution is not used: with its ID and slug
		// equal to the broker-held ones it names ext (both are built from
		// them by config.ProjectMarker.DirName).
		if !isRealDir(ext) || !isRealDir(filepath.Join(ext, config.DotScion)) {
			return "", refuseHostStateRoot("%s or its .scion is not a real directory", ext)
		}
		return ext, nil
	}

	// A shared project configuration (config.IsSharedProjectConfigDir: the
	// hub's UUID-keyed tree on a shared Layout) keeps every agent's state,
	// whatever its sharing mode, in the broker-local external agents root
	// (config.AgentsRootForProject), or in the agent's legacy in-project dir
	// for an agent provisioned before the configuration moved
	// (config.AgentDirForProject's look-up fallback). The record goes to
	// that same agent dir, never below the project root on the share.
	if config.IsSharedProjectConfigDir(configDir) {
		return sharedProjectHostStateRoot(configDir, t)
	}

	shared := t.SharedWorkspace
	if !shared && ext != "" {
		sf, err := os.Stat(filepath.Join(ext, config.DotScion, "agents", t.AgentName, "scion-agent.json"))
		shared = err == nil && sf.Mode().IsRegular()
	}
	if shared {
		if ext == "" {
			return "", refuseHostStateRoot("shared-workspace agent %q: %s", t.AgentName, extMissing)
		}
		if !isRealDir(ext) {
			return "", refuseHostStateRoot("%s is not a real directory", ext)
		}
		return ext, nil
	}
	return root, nil
}

// sharedProjectHostStateRoot is HostStateRootFor for a shared project
// configuration directory configDir: the root whose <root>/.scion/agents/
// <name> is the agent dir config.AgentDirForProject resolves from the
// broker-held identity (the hub project ID must name the configuration's
// project). Anything else, or a root that is not a real directory, is
// refused.
func sharedProjectHostStateRoot(configDir string, t RunOwnerTarget) (string, error) {
	agentDir, err := config.AgentDirForProject(configDir, t.AgentName, t.SharedWorkspace, t.HubProjectID)
	if err != nil {
		return "", refuseHostStateRoot("shared project configuration %s: %v", configDir, err)
	}
	agentsRoot := filepath.Dir(agentDir)
	dotScion := filepath.Dir(agentsRoot)
	if filepath.Base(agentDir) != t.AgentName || filepath.Base(agentsRoot) != "agents" || filepath.Base(dotScion) != config.DotScion {
		return "", refuseHostStateRoot("shared project configuration %s: agent dir %s is not <root>/.scion/agents/%s", configDir, agentDir, t.AgentName)
	}
	root := filepath.Dir(dotScion)
	if !isRealDir(root) {
		return "", refuseHostStateRoot("%s is not a real directory", root)
	}
	return root, nil
}

// RecordRunOwner records runID as the run that owns t's files, in
// HostStateFile. It writes nothing when runID is not a valid run ID or no
// trusted root can be derived (both return an error), and it is a no-op
// when the agent's directory does not exist yet (Start records before a
// fresh provision creates it; the provision then records the run itself).
// It never writes agent-info.json.
//
// Callers log a failure and carry on, as they did for the agent-info.json
// record: the files are then recorded as before (or not at all), and the
// broker's readers treat a record they cannot read as unknown, keeping the
// files.
func RecordRunOwner(t RunOwnerTarget, runID string) error {
	root, err := runOwnerRoot(t, runID)
	if err != nil {
		return err
	}
	err = writeRunOwner(root, t.AgentName, runID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// recordRunOwnerInAgentDir is RecordRunOwner for a caller that holds the
// agent's dir (Provision, ProvisionAgent): the record must land in
// agentDir. A root whose agent dir is not agentDir returns
// errRunOwnerDirMismatch, and a missing agent dir returns an error too
// (errRunOwnerDirMismatch, or one matching fs.ErrNotExist when the two
// paths are the same); nothing is written then, so a divergence between
// HostStateRootFor and the agent-dir resolution is logged by the caller
// instead of being lost silently.
func recordRunOwnerInAgentDir(t RunOwnerTarget, agentDir, runID string) error {
	root, err := agentDirRunOwnerRoot(t, agentDir, runID)
	if err != nil {
		return err
	}
	return writeRunOwner(root, t.AgentName, runID)
}

// recordLegacyRunOwnerInAgentDir is recordRunOwnerInAgentDir for the
// transitional legacy copy (migrateLegacyRunOwner): it records runID only
// when HostStateFile records no run, checked and written under one
// UpdateHostState, so a run recorded meanwhile is never replaced by the
// older legacy one. It reports whether HostStateFile records a run
// afterwards.
func recordLegacyRunOwnerInAgentDir(t RunOwnerTarget, agentDir, runID string) (bool, error) {
	root, err := agentDirRunOwnerRoot(t, agentDir, runID)
	if err != nil {
		return false, err
	}
	err = UpdateHostState(root, t.AgentName, func(hs *HostState) error {
		if hs.RunID != "" {
			return errHostStateRunRecorded
		}
		hs.RunID = runID
		return nil
	})
	if errors.Is(err, errHostStateRunRecorded) {
		return true, nil
	}
	return err == nil, err
}

// errHostStateRunRecorded stops recordLegacyRunOwnerInAgentDir's update,
// writing nothing, when HostStateFile already records a run.
var errHostStateRunRecorded = errors.New("host state already records a run")

// agentDirRunOwnerRoot checks runID and returns t's trusted root when its
// agent dir is agentDir (errRunOwnerDirMismatch otherwise).
func agentDirRunOwnerRoot(t RunOwnerTarget, agentDir, runID string) (string, error) {
	root, err := runOwnerRoot(t, runID)
	if err != nil {
		return "", err
	}
	recordDir := filepath.Join(root, config.DotScion, "agents", t.AgentName)
	if !sameDir(recordDir, agentDir) {
		return "", fmt.Errorf("%w: %s, agent dir %s", errRunOwnerDirMismatch, recordDir, agentDir)
	}
	return root, nil
}

// runOwnerRoot checks runID and returns t's trusted root.
func runOwnerRoot(t RunOwnerTarget, runID string) (string, error) {
	if runID == "" || runtime.ValidateRunID(runID) != nil {
		return "", errInvalidRecordedRunID
	}
	return HostStateRootFor(t)
}

// writeRunOwner sets HostState.RunID below root.
func writeRunOwner(root, agentName, runID string) error {
	return UpdateHostState(root, agentName, func(hs *HostState) error {
		hs.RunID = runID
		return nil
	})
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
// keep their ptone/scion#2675 protection. It can be removed once such
// agents have restarted.
//
// It is read only when HostStateFile records no run. Nothing writes a new
// value: migrateLegacyRunOwner copies it into HostStateFile (from
// ProvisionAgent without a run, and from updateSavedAgentInfo), and both
// drop it from agent-info.json only once HostStateFile records a run,
// keeping it as read otherwise, so a host rewrite of agent-info.json does
// not end the protection. Callers compare
// it with the run they act on, so it can only keep files (a different run)
// or change nothing (the same run): a missing, symlinked, unreadable,
// oversized or invalid value gives "", as a missing record did before, and
// so does a home the host cannot read.
func legacyAgentInfoRunID(projectPath, agentName string) string {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return ""
	}
	// Read confined to the agent home (agenthome.ReadFileAt).
	data, err := agenthome.ReadFileAt(config.GetAgentHomePath(projectDir, agentName), agenthome.AgentInfoFile, maxLegacyAgentInfoSize)
	if err != nil {
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

// migrateLegacyRunOwner is transitional (ptone/scion#4771): when
// agent-info.json still carries a valid legacy runId
// (legacyAgentInfoRunID), it hands it to record, which records it in
// HostStateFile only when HostStateFile records no run
// (recordLegacyRunOwnerInAgentDir) and reports whether HostStateFile
// records a run afterwards. migrateLegacyRunOwner returns that, and the
// legacy value it found ("" when there is none or it is invalid). The
// value keeps the meaning it had: it can only keep files for another run.
func migrateLegacyRunOwner(t RunOwnerTarget, record func(runID string) (bool, error)) (recorded bool, legacy string, err error) {
	legacy = legacyAgentInfoRunID(t.ProjectPath, t.AgentName)
	if legacy == "" {
		return false, "", nil
	}
	recorded, err = record(legacy)
	return recorded, legacy, err
}
