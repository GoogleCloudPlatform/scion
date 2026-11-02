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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"gopkg.in/yaml.v3"
)

// Workspace configuration records.
//
// A hub-managed project's workspace (~/.scion/projects/<slug>) holds the
// project's .scion entry: a project marker file, a .scion directory, or
// nothing. The broker reads project configuration through that entry, and
// the workspace may be mounted into agents. The broker keeps its own
// record of the entry's state, outside every agent mount, and refuses a
// start that finds a different state, before anything is read through the
// entry. Only the broker changes the entry (it writes the marker or the
// project-id); it records the state after doing so.
//
// A record is keyed by the workspace path and is valid only for the Hub
// project ID it was written for: a different ID (a re-link, or a project
// deleted and recreated under the same slug) or a workspace that no longer
// exists starts a fresh record.

const (
	// workspaceConfigRecordsDir is the records' directory under the
	// global directory.
	workspaceConfigRecordsDir = "workspace-config-records"
	workspaceConfigRecordVer  = 1
	// workspaceConfigMarkerMax bounds the marker bytes read for the digest.
	workspaceConfigMarkerMax = 64 << 10
)

// Kinds of a workspace's .scion entry.
const (
	workspaceConfigAbsent    = "absent"
	workspaceConfigMarker    = "marker"
	workspaceConfigDirectory = "directory"
	workspaceConfigSymlink   = "symlink"
	workspaceConfigOther     = "other"
)

// workspaceConfigState is the observed state of a workspace's .scion
// entry. Detail is the marker's content digest, the directory's
// project-id, or the symlink's target.
type workspaceConfigState struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
}

func (s workspaceConfigState) describe() string {
	switch s.Kind {
	case workspaceConfigMarker:
		return "a project marker file"
	case workspaceConfigDirectory:
		if s.Detail == "" {
			return "a directory without a project-id"
		}
		return "a directory with project-id " + s.Detail
	case workspaceConfigSymlink:
		return "a symbolic link"
	case workspaceConfigAbsent:
		return "absent"
	default:
		return "an unexpected file type"
	}
}

type workspaceConfigRecord struct {
	Version      int                  `json:"version"`
	Path         string               `json:"path"`
	HubProjectID string               `json:"hubProjectId"`
	State        workspaceConfigState `json:"state"`
}

// workspaceConfigChangedError reports a workspace whose .scion entry is not
// in the state the broker recorded. Error() carries no host path.
type workspaceConfigChangedError struct {
	Path            string
	Recorded, Found workspaceConfigState
	// Reason replaces the state comparison when the check itself could
	// not be made (the record or the entry could not be read).
	Reason string
}

func (e *workspaceConfigChangedError) Error() string {
	what := fmt.Sprintf("it was %s and is now %s", e.Recorded.describe(), e.Found.describe())
	if e.Recorded.Kind == workspaceConfigMarker && e.Found.Kind == workspaceConfigMarker {
		what = "the project marker file was changed"
	}
	if e.Reason != "" {
		what = e.Reason
	}
	return "the project configuration entry (.scion) of this hub-managed workspace is not in the state the Runtime Broker recorded (" + what +
		"); the start is refused without reading it. Restore the entry, or have the Runtime Broker operator re-provision the project workspace"
}

// observeWorkspaceConfigState returns the state of <projectPath>/.scion.
func observeWorkspaceConfigState(projectPath string) (workspaceConfigState, error) {
	state, _, err := observeWorkspaceConfigStateData(projectPath)
	return state, err
}

// observeWorkspaceConfigStateData is observeWorkspaceConfigState, also
// returning, for a marker, the bytes its digest was computed from, so that
// a caller validates exactly what it records.
func observeWorkspaceConfigStateData(projectPath string) (workspaceConfigState, []byte, error) {
	p := filepath.Join(projectPath, config.DotScion)
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return workspaceConfigState{Kind: workspaceConfigAbsent}, nil, nil
	}
	if err != nil {
		return workspaceConfigState{}, nil, err
	}
	switch mode := info.Mode(); {
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			return workspaceConfigState{}, nil, err
		}
		return workspaceConfigState{Kind: workspaceConfigSymlink, Detail: target}, nil, nil
	case mode.IsDir():
		id, err := config.ReadProjectID(p)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return workspaceConfigState{}, nil, err
		}
		return workspaceConfigState{Kind: workspaceConfigDirectory, Detail: id}, nil, nil
	case mode.IsRegular():
		f, err := os.Open(p)
		if err != nil {
			return workspaceConfigState{}, nil, err
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(io.LimitReader(f, workspaceConfigMarkerMax))
		if err != nil {
			return workspaceConfigState{}, nil, err
		}
		sum := sha256.Sum256(data)
		return workspaceConfigState{Kind: workspaceConfigMarker, Detail: hex.EncodeToString(sum[:])}, data, nil
	default:
		return workspaceConfigState{Kind: workspaceConfigOther, Detail: mode.Type().String()}, nil, nil
	}
}

// workspaceConfigRecordFor returns the canonical workspace path and its
// record file, or ok=false when projectPath is not a hub-managed workspace
// root (a linked project, or a project's own .scion directory) or there is
// no Hub project ID to key the record by.
func workspaceConfigRecordFor(projectPath, hubProjectID string) (canonical, recordPath string, ok bool, err error) {
	if projectPath == "" || hubProjectID == "" || filepath.Base(projectPath) == config.DotScion ||
		(!isHubManagedProjectPath(projectPath) && !isSharedHubProjectRoot(projectPath)) {
		return "", "", false, nil
	}
	canonical, err = CanonicalWorkspacePath(projectPath)
	if err != nil {
		return "", "", false, err
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return "", "", false, err
	}
	sum := sha256.Sum256([]byte(canonical))
	return canonical, filepath.Join(globalDir, workspaceConfigRecordsDir, hex.EncodeToString(sum[:16])+".json"), true, nil
}

// recordHubProjectFilesChecked runs recordHubProjectFiles inside
// withCheckedWorkspaceConfig. The caller holds the workspace lock.
func (s *Server) recordHubProjectFilesChecked(ctx context.Context, in startContextInputs) error {
	return s.withCheckedWorkspaceConfig(in.ProjectPath, in.ProjectID, in.ProjectSlug, func() {
		s.recordHubProjectFiles(ctx, in)
	})
}

// alignHubManagedProjectIdentityChecked runs alignHubManagedProjectIdentity
// inside withCheckedWorkspaceConfig, so an identity the alignment rewrites
// is recorded as the broker's own change. The caller holds the workspace
// lock.
func (s *Server) alignHubManagedProjectIdentityChecked(ctx context.Context, agentID, projectPath, slug, projectID string) error {
	return s.withCheckedWorkspaceConfig(projectPath, projectID, slug, func() {
		s.alignHubManagedProjectIdentity(ctx, agentID, projectPath, slug, projectID)
	})
}

// withCheckedWorkspaceConfig runs write, which may change a hub-managed
// workspace's .scion entry, between a check of the entry against the
// record and a record of the result, all under the workspace lock the
// caller holds. The entry must match the record before, and may differ
// afterwards only by a change the broker itself makes
// (brokerWorkspaceConfigChange). Any other difference, such as a marker
// replaced while the broker was writing, is refused and not recorded.
func (s *Server) withCheckedWorkspaceConfig(projectPath, projectID, slug string, write func()) error {
	before, applies, err := checkWorkspaceConfigRecordState(projectPath, projectID)
	if err != nil {
		return err
	}
	if afterWorkspaceConfigCheckHook != nil {
		afterWorkspaceConfigCheckHook()
	}
	write()
	if !applies {
		return nil
	}
	canonical, recordPath, _, _ := workspaceConfigRecordFor(projectPath, projectID)
	after, markerData, err := observeWorkspaceConfigStateData(canonical)
	if err != nil {
		return &workspaceConfigChangedError{Path: canonical, Reason: "the entry cannot be read"}
	}
	if !brokerWorkspaceConfigChange(before, after, markerData, projectID, slug) {
		return &workspaceConfigChangedError{Path: canonical, Recorded: before, Found: after}
	}
	// The state just validated is what is recorded; the entry is not read
	// again.
	if err := writeWorkspaceConfigRecord(canonical, recordPath, projectID, after); err != nil {
		// Without a record the next start would run unchecked.
		return &workspaceConfigChangedError{Path: canonical, Reason: "the Runtime Broker could not record it: " + err.Error()}
	}
	return nil
}

// afterWorkspaceConfigCheckHook, when set (tests only), runs between the
// check and recordHubProjectFiles in recordHubProjectFilesChecked, where a
// change made by someone other than the broker must be caught.
var afterWorkspaceConfigCheckHook func()

// brokerWorkspaceConfigChange reports whether the .scion entry going from
// before to after is a change the broker makes: none; a marker written
// where there was none or where a stale one was, whose content (markerData,
// the bytes after's digest covers) names this Hub project and slug and is
// not a shadow marker; or this Hub project ID written into a .scion
// directory that was already there.
func brokerWorkspaceConfigChange(before, after workspaceConfigState, markerData []byte, hubProjectID, slug string) bool {
	if after == before {
		return true
	}
	switch {
	case after.Kind == workspaceConfigMarker && (before.Kind == workspaceConfigAbsent || before.Kind == workspaceConfigMarker):
		var m config.ProjectMarker
		if err := yaml.Unmarshal(markerData, &m); err != nil {
			return false
		}
		return m.ProjectID == hubProjectID && m.ProjectSlug == slug && m.Type == "" && config.ValidateProjectID(m.ProjectID) == nil
	case after.Kind == workspaceConfigDirectory && before.Kind == workspaceConfigDirectory:
		return after.Detail == hubProjectID
	default:
		return false
	}
}

// checkWorkspaceConfigRecord refuses (with *workspaceConfigChangedError) a
// hub-managed workspace whose .scion entry is not in the recorded state.
// It is a no-op where workspaceConfigRecordFor does not apply, when there
// is no record, or when the record is for another Hub project ID. The
// caller holds the workspace lock on projectPath.
func checkWorkspaceConfigRecord(projectPath, hubProjectID string) error {
	_, _, err := checkWorkspaceConfigRecordState(projectPath, hubProjectID)
	return err
}

// checkWorkspaceConfigRecordState is checkWorkspaceConfigRecord, also
// returning the observed state and whether the workspace is recorded at all
// (a hub-managed workspace root that exists, with a Hub project ID).
func checkWorkspaceConfigRecordState(projectPath, hubProjectID string) (workspaceConfigState, bool, error) {
	canonical, recordPath, ok, err := workspaceConfigRecordFor(projectPath, hubProjectID)
	if err != nil {
		return workspaceConfigState{}, false, &workspaceConfigChangedError{Path: projectPath, Reason: "the workspace path cannot be resolved"}
	}
	if !ok {
		return workspaceConfigState{}, false, nil
	}
	if _, err := os.Stat(canonical); errors.Is(err, fs.ErrNotExist) {
		// The workspace is gone (removed, about to be provisioned again):
		// its record no longer describes anything.
		_ = os.Remove(recordPath)
		return workspaceConfigState{}, false, nil
	}
	found, err := observeWorkspaceConfigState(canonical)
	if err != nil {
		return workspaceConfigState{}, true, &workspaceConfigChangedError{Path: canonical, Reason: "the entry cannot be read"}
	}
	data, err := os.ReadFile(recordPath)
	if errors.Is(err, fs.ErrNotExist) {
		return found, true, nil
	}
	var rec workspaceConfigRecord
	if err == nil {
		err = json.Unmarshal(data, &rec)
	}
	if err != nil {
		return found, true, &workspaceConfigChangedError{Path: canonical, Reason: "the Runtime Broker's record of it cannot be read"}
	}
	if rec.Path != canonical || rec.HubProjectID != hubProjectID {
		return found, true, nil
	}
	if found != rec.State {
		return found, true, &workspaceConfigChangedError{Path: canonical, Recorded: rec.State, Found: found}
	}
	return found, true, nil
}

// recordWorkspaceConfigState records the current state of a hub-managed
// workspace's .scion entry for hubProjectID. It is a no-op where
// workspaceConfigRecordFor does not apply or the workspace does not exist.
// The caller holds the workspace lock on projectPath.
func recordWorkspaceConfigState(projectPath, hubProjectID string) error {
	canonical, recordPath, ok, err := workspaceConfigRecordFor(projectPath, hubProjectID)
	if err != nil || !ok {
		return err
	}
	if _, err := os.Stat(canonical); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	state, err := observeWorkspaceConfigState(canonical)
	if err != nil {
		return err
	}
	return writeWorkspaceConfigRecord(canonical, recordPath, hubProjectID, state)
}

// writeWorkspaceConfigRecord writes the record of state for the workspace
// canonical and hubProjectID to recordPath (private, atomic).
func writeWorkspaceConfigRecord(canonical, recordPath, hubProjectID string, state workspaceConfigState) error {
	data, err := json.Marshal(workspaceConfigRecord{Version: workspaceConfigRecordVer, Path: canonical, HubProjectID: hubProjectID, State: state})
	if err != nil {
		return err
	}
	dir := filepath.Dir(recordPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), recordPath)
}

// workspaceConfigStartContextError wraps a checkWorkspaceConfigRecord
// failure as a 409 startContextError, logging the host path.
func (s *Server) workspaceConfigStartContextError(err error, agentName string) *startContextError {
	var ce *workspaceConfigChangedError
	if errors.As(err, &ce) {
		s.agentLifecycleLog.Error("Hub-managed workspace .scion entry is not in the recorded state",
			"agent", agentName, "path", ce.Path, "recorded", ce.Recorded.Kind, "found", ce.Found.Kind, "reason", ce.Reason)
	}
	return &startContextError{Status: http.StatusConflict, Message: err.Error(), OriginalErr: err}
}
