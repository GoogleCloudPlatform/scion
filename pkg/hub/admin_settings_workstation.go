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

package hub

// Workstation split for PUT /api/v1/admin/server-config (ptone/scion#1091,
// option C). Whether Layer-0 settings are editable through the API depends on
// the deployment mode, not the DB driver:
//
//   - Hosted hubs: Layer-1 keys go to the DB; Layer-0 and unclassified keys
//     are rejected with 422 (deployment tooling owns settings.yaml).
//   - Workstation hubs: Layer-1 keys go to the DB; Layer-0, unclassified
//     (active_profile, workspace_path, schema_version) and file-only keys
//     (auto_inject_gcloud_adc, server.scheduler, ...) go to settings.yaml, as
//     the file-mode handler always did. The file write keeps that handler's
//     side effects: log_level is applied live and the co-located broker
//     runtime is reloaded; everything else is reported as requires_restart.
//
// Failure semantics of a mixed (DB + file) workstation PUT:
//
//  1. Every rejection and validation, for both destinations, runs before
//     anything is written. A 4xx means nothing was written anywhere.
//  2. The new settings.yaml is then staged as a temp file next to it (the
//     marshal and the disk write happen here, so an I/O failure is a 500 with
//     nothing written).
//  3. The DB sections are written. On a revision conflict (409) or DB error
//     (500) the staged file is discarded, so settings.yaml is untouched; DB
//     sections written before the failure stay written, exactly as for a
//     DB-only PUT (reported in "applied").
//  4. Last, the staged file is renamed over settings.yaml (atomic on POSIX).
//     If only that rename fails, the DB sections are already written and the
//     response is a 500 naming them in "applied".
//
// Unknown keys (not in the request type) are rejected in both modes unless
// they echo the GET view: there is nowhere to persist them.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	yamlv3 "gopkg.in/yaml.v3"
)

// layer0Editable reports whether this hub accepts Layer-0 / file-only
// settings through the server-config API: workstation hubs only.
func (s *Server) layer0Editable() bool {
	return s.workstation
}

// fileLiveKeys are file-routed keys that take effect without a restart:
// log_level is applied live, auto_inject_gcloud_adc is read from
// settings.yaml at each agent start, schema_version is bookkeeping.
var fileLiveKeys = map[string]bool{
	"server.log_level":       true,
	"auto_inject_gcloud_adc": true,
	"schema_version":         true,
}

// fileTopLevelKeys are the single-segment file-routed keys; they are written
// through applySettingsUpdates so they keep its ""-deletes semantics.
var fileTopLevelKeys = map[string]bool{
	"schema_version":         true,
	"active_profile":         true,
	"workspace_path":         true,
	"auto_inject_gcloud_adc": true,
}

// stagedSettingsWrite is a settings.yaml update written to a temp file in the
// same directory, to be renamed into place (commit) or removed (abort).
type stagedSettingsWrite struct {
	path string
	tmp  string
}

func (sw *stagedSettingsWrite) commit() error {
	if err := os.Rename(sw.tmp, sw.path); err != nil {
		_ = os.Remove(sw.tmp)
		return err
	}
	return nil
}

func (sw *stagedSettingsWrite) abort() {
	_ = os.Remove(sw.tmp)
}

// serverConfigFileValidationError is a client error in the file-routed part
// of a workstation PUT (400).
type serverConfigFileValidationError struct{ msg string }

func (e *serverConfigFileValidationError) Error() string { return e.msg }

// validateServerConfigFileKeys checks the file-routed part of a workstation
// PUT with the same rules the file-mode handler applies, normalising
// req.Server.Hub.AgentEndpoint in place. ops supplies the current runtimes
// and profiles when the request changes server.shared_dir_storage.
func validateServerConfigFileKeys(req *ServerConfigUpdateRequest, fileKeys []string, ops *OperationalSettings) error {
	has := map[string]bool{}
	for _, k := range fileKeys {
		has[k] = true
	}
	if has["server.hub.agent_endpoint"] && req.Server != nil && req.Server.Hub != nil && req.Server.Hub.AgentEndpoint != "" {
		normalized, err := config.ValidateAgentEndpoint(req.Server.Hub.AgentEndpoint)
		if err != nil {
			return &serverConfigFileValidationError{err.Error()}
		}
		req.Server.Hub.AgentEndpoint = normalized
	}
	if has["server.shared_dir_storage"] && req.Server != nil {
		runtimes, profiles := req.Runtimes, req.Profiles
		snap := ops.Snapshot()
		if runtimes == nil {
			runtimes = snap.Runtimes
		}
		if profiles == nil {
			profiles = snap.Profiles
		}
		if errs := config.ValidateSharedDirStorageBackends(runtimes, profiles, req.Server.SharedDirStorage); len(errs) > 0 {
			return &serverConfigFileValidationError{errs[0].Error()}
		}
	}
	return nil
}

// stageServerConfigFileWrite merges the file-routed keys of req into the
// current settings.yaml and stages the result (see the failure semantics
// above). Only the listed paths change: siblings under server.hub,
// server.auth, ... are kept, so Layer-1 seed values in the file are never
// wiped by a Layer-0 edit.
func stageServerConfigFileWrite(req *ServerConfigUpdateRequest, fileKeys []string) (*stagedSettingsWrite, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return nil, fmt.Errorf("resolve settings directory: %w", err)
	}
	settingsPath := filepath.Join(globalDir, "settings.yaml")

	var raw map[string]interface{}
	mode := os.FileMode(0o644)
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := yamlv3.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("parse settings file: %w", err)
		}
		if fi, err := os.Stat(settingsPath); err == nil {
			mode = fi.Mode().Perm()
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read settings file: %w", err)
	}
	if raw == nil {
		raw = map[string]interface{}{}
	}

	var top ServerConfigUpdateRequest
	var srvMap map[string]interface{}
	if req.Server != nil {
		srvMap, _ = marshalToMap(req.Server).(map[string]interface{})
	}
	for _, k := range fileKeys {
		if fileTopLevelKeys[k] {
			switch k {
			case "schema_version":
				top.SchemaVersion = req.SchemaVersion
			case "active_profile":
				top.ActiveProfile = req.ActiveProfile
			case "workspace_path":
				top.WorkspacePath = req.WorkspacePath
			case "auto_inject_gcloud_adc":
				top.AutoInjectGcloudADC = req.AutoInjectGcloudADC
			}
			continue
		}
		path := strings.Split(k, ".")
		if len(path) < 2 || path[0] != "server" {
			return nil, fmt.Errorf("unroutable settings key %q", k)
		}
		if v, ok := mapAtPath(srvMap, path[1:]); ok {
			mergeMapPath(raw, path, v)
		} else {
			deleteMapPath(raw, path)
		}
	}
	applySettingsUpdates(raw, &top)
	if _, ok := raw["schema_version"]; !ok {
		raw["schema_version"] = "1"
	}

	newData, err := yamlv3.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal settings: %w", err)
	}
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		return nil, fmt.Errorf("create settings directory: %w", err)
	}
	f, err := os.CreateTemp(globalDir, ".settings.yaml.tmp-*")
	if err != nil {
		return nil, fmt.Errorf("stage settings file: %w", err)
	}
	tmp := f.Name()
	_, werr := f.Write(newData)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, mode)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("stage settings file: %w", werr)
	}
	return &stagedSettingsWrite{path: settingsPath, tmp: tmp}, nil
}

// applyServerConfigFileSideEffects runs the live-apply steps the file-mode
// reloadSettings ran, minus re-applying a file-built Layer-1 snapshot (on a
// DB-backed hub the DB owns Layer-1). It returns the applied items and the
// file-routed keys that need a restart.
func (s *Server) applyServerConfigFileSideEffects(fileKeys []string) (applied, requiresRestart []string) {
	applied = []string{}
	requiresRestart = []string{}
	for _, k := range fileKeys {
		if k == "server.log_level" {
			if gc, err := config.LoadGlobalConfig(""); err != nil {
				slog.Error("PUT server-config: failed to reload settings for log_level", "error", err)
				requiresRestart = append(requiresRestart, k)
			} else if gc.LogLevel != "" {
				applySnapshotLogLevel(gc.LogLevel)
				applied = append(applied, "log_level")
			}
			continue
		}
		if !fileLiveKeys[k] {
			requiresRestart = append(requiresRestart, k)
		}
	}
	s.mu.RLock()
	reloadFn := s.runtimeReloadFunc
	s.mu.RUnlock()
	if reloadFn != nil && reloadFn() {
		applied = append(applied, "broker_runtime")
	}
	sort.Strings(requiresRestart)
	return applied, requiresRestart
}

func mapAtPath(m map[string]interface{}, path []string) (interface{}, bool) {
	var cur interface{} = m
	for _, seg := range path {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		cur, ok = mm[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// mergeMapPath sets path to v, deep-merging when both the current value and
// v are maps. A form that sends only some fields of a block (server.broker
// without broker_id / broker_token, which the hub writes itself) must not
// wipe the fields it did not send.
func mergeMapPath(m map[string]interface{}, path []string, v interface{}) {
	cur := m
	for _, seg := range path[:len(path)-1] {
		next, ok := cur[seg].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			cur[seg] = next
		}
		cur = next
	}
	last := path[len(path)-1]
	if existing, ok := cur[last].(map[string]interface{}); ok {
		if nv, ok := v.(map[string]interface{}); ok {
			deepMergeMaps(existing, nv)
			return
		}
	}
	cur[last] = v
}

func deepMergeMaps(dst, src map[string]interface{}) {
	for k, v := range src {
		if dm, ok := dst[k].(map[string]interface{}); ok {
			if sm, ok := v.(map[string]interface{}); ok {
				deepMergeMaps(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

func deleteMapPath(m map[string]interface{}, path []string) {
	cur := m
	for _, seg := range path[:len(path)-1] {
		next, ok := cur[seg].(map[string]interface{})
		if !ok {
			return
		}
		cur = next
	}
	delete(cur, path[len(path)-1])
}
