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
//     (auto_inject_gcloud_adc, server.scheduler, ...) go to settings.yaml.
//
// The settings.yaml part is derived from the raw body, leaf by leaf: every
// leaf present in the body is set, an explicit null or an empty value of an
// omitempty field removes the key, and leaves the body does not carry are
// kept (so server.broker.broker_id/broker_token, which the hub writes
// itself, survive a form that omits them). Values come from the decoded
// request after masked secrets are restored, so a "********" echo is never
// written. Unchanged values are skipped; a pure echo writes nothing and
// reports nothing as requires_restart. The file is edited in place through
// config.PrepareSettingsPathEdits, so comments, key order and unknown keys
// survive.
//
// Failure semantics of a mixed (DB + file) workstation PUT:
//
//  1. Every rejection and validation, for both destinations, runs before
//     anything is written. A 4xx means nothing was written anywhere.
//  2. The settings-file lock is taken and the new settings.yaml is prepared
//     in memory (read, edit, round-trip and load checks). A failure here is
//     a 4xx/500 with nothing written.
//  3. The DB sections are written, still under the lock. On a revision
//     conflict (409) or DB error (500) the prepared file is dropped, so
//     settings.yaml is untouched; DB sections written before the failure
//     stay written, exactly as for a DB-only PUT (reported in "applied").
//  4. Last, the prepared file is written atomically and the lock released.
//     If only that write fails, the DB sections are already written and the
//     response is a 500 naming them in "applied".
//
// Lock order (see config.LockSettingsFile): the settings-file lock is taken
// before the DB writes and held across them; ops.Update then takes s.mu
// (ApplySnapshot). Nothing may take the settings-file lock while holding
// s.mu or a DB transaction.
//
// Unknown keys (not in the request type) are rejected in both modes unless
// they echo the GET view: there is nowhere to persist them.

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
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

// bodyLeaf is a leaf of the request body: a field the body carries whose
// type is not a struct (or whose value is null).
type bodyLeaf struct {
	path      []string
	index     []int // field index path from ServerConfigUpdateDBRequest
	omitempty bool
	null      bool
	raw       json.RawMessage
}

type jsonFieldInfo struct {
	index     []int
	typ       reflect.Type
	omitempty bool
}

// jsonFieldInfos maps the JSON names encoding/json decodes for struct t,
// including promoted fields of embedded structs, to their index paths.
func jsonFieldInfos(t reflect.Type) map[string]jsonFieldInfo {
	out := map[string]jsonFieldInfo{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			et := f.Type
			for et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				for n, fi := range jsonFieldInfos(et) {
					if _, dup := out[n]; !dup {
						fi.index = append([]int{i}, fi.index...)
						out[n] = fi
					}
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = jsonFieldInfo{index: []int{i}, typ: f.Type, omitempty: strings.Contains(","+opts+",", ",omitempty,")}
	}
	return out
}

// presentBodyLeaves returns the leaves of obj known to type t (unknown keys
// are rejected elsewhere), descending into struct-typed fields.
func presentBodyLeaves(obj map[string]json.RawMessage, t reflect.Type, prefix []string, prefixIndex []int) []bodyLeaf {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	infos := jsonFieldInfos(t)
	var out []bodyLeaf
	for key, raw := range obj {
		fi, ok := infos[key]
		name := key
		if !ok {
			for n, f := range infos {
				if strings.EqualFold(n, key) {
					fi, name, ok = f, n, true
					break
				}
			}
		}
		if !ok {
			continue
		}
		path := append(append([]string{}, prefix...), name)
		index := append(append([]int{}, prefixIndex...), fi.index...)
		if strings.TrimSpace(string(raw)) == "null" {
			out = append(out, bodyLeaf{path: path, index: index, omitempty: fi.omitempty, null: true, raw: raw})
			continue
		}
		ft := fi.typ
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && !reflect.PointerTo(ft).Implements(jsonUnmarshalerType) {
			var child map[string]json.RawMessage
			if json.Unmarshal(raw, &child) == nil {
				out = append(out, presentBodyLeaves(child, ft, path, index)...)
				continue
			}
		}
		out = append(out, bodyLeaf{path: path, index: index, omitempty: fi.omitempty, raw: raw})
	}
	return out
}

// hostedLayer0Changes returns, for a hosted hub, the Layer-0 leaves present
// in the body whose value differs from what GET reports. Equal leaves are
// echoes and are ignored; anything else, an explicit zero such as
// dev_mode:false over a stored true included, is a change the hub will not
// make, so the PUT must reject it rather than report "saved". Leaves under
// the unpersisted lists are left to rejectUnpersistedKeys.
func (s *Server) hostedLayer0Changes(ctx context.Context, ops *OperationalSettings, rawBody []byte) ([]string, error) {
	var top map[string]json.RawMessage
	if json.Unmarshal(rawBody, &top) != nil {
		return nil, nil
	}
	var leaves []bodyLeaf
	for _, l := range presentBodyLeaves(top, reflect.TypeOf(ServerConfigUpdateDBRequest{}), nil, nil) {
		if underUnpersistedList(l.path) {
			continue
		}
		if _, l0, _ := opsettings.ClassifyKeys([]string{requestPathKoanfKey(l.path)}); len(l0) > 0 {
			leaves = append(leaves, l)
		}
	}
	if len(leaves) == 0 {
		return nil, nil
	}
	view, err := s.serverConfigDBView(ctx, ops)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, l := range leaves {
		if !isEchoOfView(rawPath{path: l.path, value: l.raw}, view) {
			changed = append(changed, strings.Join(l.path, "."))
		}
	}
	sort.Strings(changed)
	return changed, nil
}

func underUnpersistedList(path []string) bool {
	for _, p := range dbUnpersistedRequestPaths {
		if pathHasPrefixPath(path, p) {
			return true
		}
	}
	return false
}

// requestPathKoanfKey maps a request JSON path to the koanf key the
// opsettings registry classifies. The request's top-level federation block
// is server.federation in the settings tree.
func requestPathKoanfKey(path []string) string {
	if len(path) > 0 && path[0] == "federation" {
		return "server." + strings.Join(path, ".")
	}
	return strings.Join(path, ".")
}

func pathHasPrefixPath(p, prefix []string) bool {
	if len(prefix) > len(p) {
		return false
	}
	for i := range prefix {
		if p[i] != prefix[i] {
			return false
		}
	}
	return true
}

// isWorkstationFileLeaf reports whether a request leaf goes to settings.yaml
// on a workstation hub: Layer-0, unclassified or file-only. Layer-1 leaves
// go to the DB; the unwritten Layer-1 keys are rejected elsewhere.
func isWorkstationFileLeaf(path []string) bool {
	if len(path) == 0 || path[0] == "expected_revisions" {
		return false
	}
	for _, p := range dbUnwrittenLayer1Paths {
		if pathHasPrefixPath(path, p) {
			return false
		}
	}
	for _, p := range dbFileOnlyRequestPaths {
		if pathHasPrefixPath(path, p) {
			return true
		}
	}
	l1, _, _ := opsettings.ClassifyKeys([]string{requestPathKoanfKey(path)})
	return len(l1) == 0
}

// workstationFileLeaves returns the body leaves a workstation hub writes to
// settings.yaml, sorted by path.
func workstationFileLeaves(rawBody []byte) []bodyLeaf {
	var top map[string]json.RawMessage
	if json.Unmarshal(rawBody, &top) != nil {
		return nil
	}
	var out []bodyLeaf
	for _, l := range presentBodyLeaves(top, reflect.TypeOf(ServerConfigUpdateDBRequest{}), nil, nil) {
		if isWorkstationFileLeaf(l.path) {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i].path, ".") < strings.Join(out[j].path, ".") })
	return out
}

// workstationFileEdits turns file leaves into settings.yaml edits, taking
// each value from the decoded request (masked secrets already restored).
func workstationFileEdits(req *ServerConfigUpdateDBRequest, leaves []bodyLeaf) []config.SettingsPathEdit {
	root := reflect.ValueOf(req).Elem()
	edits := make([]config.SettingsPathEdit, 0, len(leaves))
	for _, l := range leaves {
		if l.null {
			edits = append(edits, config.SettingsPathEdit{Path: l.path, Delete: true})
			continue
		}
		fv, ok := fieldByIndexPath(root, l.index)
		if !ok || (l.omitempty && isEmptyJSONValue(fv)) {
			edits = append(edits, config.SettingsPathEdit{Path: l.path, Delete: true})
			continue
		}
		edits = append(edits, config.SettingsPathEdit{Path: l.path, Value: fv.Interface()})
	}
	return edits
}

// fieldByIndexPath is v.FieldByIndex that reports a nil pointer on the way
// instead of panicking.
func fieldByIndexPath(v reflect.Value, index []int) (reflect.Value, bool) {
	for _, x := range index {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		v = v.Field(x)
	}
	return v, true
}

// isEmptyJSONValue mirrors encoding/json's omitempty test.
func isEmptyJSONValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

func leafKeys(leaves []bodyLeaf) []string {
	out := make([]string, 0, len(leaves))
	for _, l := range leaves {
		out = append(out, strings.Join(l.path, "."))
	}
	return out
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
	under := func(prefix string) bool {
		for _, k := range fileKeys {
			if k == prefix || strings.HasPrefix(k, prefix+".") {
				return true
			}
		}
		return false
	}
	if under("server.hub.agent_endpoint") && req.Server != nil && req.Server.Hub != nil && req.Server.Hub.AgentEndpoint != "" {
		normalized, err := config.ValidateAgentEndpoint(req.Server.Hub.AgentEndpoint)
		if err != nil {
			return &serverConfigFileValidationError{err.Error()}
		}
		req.Server.Hub.AgentEndpoint = normalized
	}
	if under("server.shared_dir_storage") && req.Server != nil {
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

// settingsFileTxn holds the settings-file lock and a prepared edit between
// the pre-DB preparation and the post-DB commit of a workstation PUT.
type settingsFileTxn struct {
	unlock func()
	staged *config.StagedSettingsEdit
}

// prepareSettingsFileTxn takes the settings-file lock and prepares edits to
// the global settings file. On error the lock is released.
func prepareSettingsFileTxn(edits []config.SettingsPathEdit) (*settingsFileTxn, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return nil, err
	}
	unlock := config.LockSettingsFile()
	staged, err := config.PrepareSettingsPathEdits(globalDir, edits)
	if err != nil {
		unlock()
		return nil, err
	}
	return &settingsFileTxn{unlock: unlock, staged: staged}, nil
}

// abort drops the prepared edit and releases the lock.
func (t *settingsFileTxn) abort() {
	if t != nil && t.unlock != nil {
		t.unlock()
		t.unlock = nil
	}
}

// commit writes the prepared edit and releases the lock. It returns the
// dotted paths that changed.
func (t *settingsFileTxn) commit() ([]string, error) {
	defer t.abort()
	if err := t.staged.Commit(); err != nil {
		return nil, err
	}
	return t.staged.Changed, nil
}

// applyServerConfigFileSideEffects runs the live-apply steps the file-mode
// reloadSettings ran, minus re-applying a file-built Layer-1 snapshot (on a
// DB-backed hub the DB owns Layer-1), for the keys that changed in
// settings.yaml. It returns the applied items and the changed keys that
// need a restart. Called after the settings-file lock is released.
func (s *Server) applyServerConfigFileSideEffects(changed []string) (applied, requiresRestart []string) {
	applied = []string{}
	requiresRestart = []string{}
	if len(changed) == 0 {
		return applied, requiresRestart
	}
	for _, k := range changed {
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
