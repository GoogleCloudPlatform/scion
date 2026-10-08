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

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strings"
)

// The DB-backed server-config PUT must never answer 200 "saved" for a key it
// neither persists nor rejects (the onboarding gcloud ADC choice was once
// silently dropped on SQLite this way). Layer-0 and unclassified
// keys are already rejected by koanf-key classification; what slipped through
// were keys that never became a koanf key at all:
//
//   - keys the request type does not know, which the JSON decoder drops at
//     any depth (e.g. a flat "server.hub.auto_suspend_stalled" top-level key);
//   - request fields that extractKoanfKeysFromRequest does not map and that
//     have no DB home (dbUnpersistedRequestPaths).
//
// Either kind is accepted only when it is a no-op echo of the GET view, so a
// client that sends the GET body back keeps getting 200. A key the GET view
// type does not know either is never an echo and is always rejected
// (ptone/scion#3463). The file-mode PUT applies the same rule
// (rejectUnknownFileConfigKeys).

// dbUnwrittenLayer1Paths are Layer-1 agent_defaults request fields that the
// DB path does not write (buildSingleSectionDoc), report (GET) or apply
// (ApplySnapshot). Writing them to settings.yaml would not help either: the
// DB-built snapshot never carries them. Until that is fixed they are rejected
// in every mode rather than silently dropped.
//
// server.federation is the same case: the request type has a federation
// block under server, which classifies as the Layer-1 federation section,
// but only the top-level federation field is mapped.
var dbUnwrittenLayer1Paths = [][]string{
	{"default_max_agent_role"},
	{"default_agent_role"},
	{"server", "federation"},
}

// dbFileOnlyRequestPaths lists ServerConfigUpdateRequest JSON paths that
// decode into the request but are never mapped to a koanf key and have no DB
// home. A workstation hub writes them to settings.yaml; a hosted hub rejects
// them.
var dbFileOnlyRequestPaths = [][]string{
	{"auto_inject_gcloud_adc"},
	{"server", "maintenance"},
	{"server", "scheduler"},
	{"server", "oidc_login"},
	{"server", "oidc"},
	{"server", "hub", "agent_endpoint"},
	{"server", "hub", "gcp_iam_check_mode"},
	{"server", "hub", "gcp_iam_deny_unknown_policy"},
	{"server", "hub", "missing_agent_grace"},
	{"server", "hub", "conduit"},
	{"server", "hub", "disable_legacy_storage_fallback"},
	{"server", "auth", "username"},
	{"server", "auth", "display_name"},
	{"server", "auth", "email"},
	{"server", "auth", "agent_run_scope"},
	{"server", "auth", "agent_run_scope_legacy_until"},
}

// dbUnpersistedRequestPaths is every request path the DB-backed PUT does not
// map to a koanf key. TestDBUnpersistedRequestPaths_CoverUnmappedFields keeps
// it in step with the request types.
var dbUnpersistedRequestPaths = append(append([][]string{}, dbUnwrittenLayer1Paths...), dbFileOnlyRequestPaths...)

// isEmptySettingsBody reports whether a PUT body carries no settings at all
// ({} or only expected_revisions).
func isEmptySettingsBody(rawBody []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &top); err != nil {
		return false // the typed decode reports malformed bodies
	}
	for k := range top {
		if !strings.EqualFold(k, "expected_revisions") {
			return false
		}
	}
	return true
}

// rejectUnpersistedKeys writes a 422 naming every key in rawBody that the
// DB-backed PUT would drop, unless that key is a no-op echo of the GET view.
// A key that neither the request nor the GET view type knows (a flat dotted
// key, a misspelt field) is always rejected (ptone/scion#3463).
// With routeFileOnly (workstation hubs) the dbFileOnlyRequestPaths present in
// the body are not candidates; they are returned as koanf-style keys for the
// settings.yaml write. done is true if the response has been written.
func (s *Server) rejectUnpersistedKeys(ctx context.Context, w http.ResponseWriter, ops *OperationalSettings, rawBody []byte, routeFileOnly bool) (fileKeys []string, done bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &top); err != nil {
		return nil, false
	}

	candidates := unknownSettingsKeys(top, reflect.TypeOf(ServerConfigUpdateDBRequest{}), reflect.TypeOf(ServerConfigDBResponse{}))
	for _, p := range dbUnwrittenLayer1Paths {
		if v, ok := rawAtPath(top, p); ok {
			candidates = append(candidates, rawPath{path: p, value: v})
		}
	}
	for _, p := range dbFileOnlyRequestPaths {
		if v, ok := rawAtPath(top, p); ok {
			if routeFileOnly {
				fileKeys = append(fileKeys, strings.Join(p, "."))
			} else {
				candidates = append(candidates, rawPath{path: p, value: v})
			}
		}
	}
	// Unknown keys under a file-routed path (server.scheduler.bogus) stay
	// candidates: the decoder drops them, so the file write would too.
	rejected, err := rejectedSettingsKeys(candidates, func() (map[string]any, error) {
		return s.serverConfigDBView(ctx, ops)
	})
	if err != nil {
		slog.Error("PUT server-config: failed to build GET view for echo check", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to read existing settings", nil)
		return nil, true
	}
	if len(rejected) == 0 {
		return fileKeys, false
	}
	writeUnpersistedKeysRejected(w, rejected)
	return nil, true
}

// rejectUnknownFileConfigKeys is the file-mode PUT's counterpart of
// rejectUnpersistedKeys: it writes a 422 naming every key in rawBody that
// the ServerConfigUpdateRequest decode would drop, unless that key is a
// no-op echo of the file-mode GET view. It runs before settings.yaml is
// read for the merge, so a rejected request writes nothing. done is true
// if the response has been written.
func rejectUnknownFileConfigKeys(w http.ResponseWriter, rawBody []byte) (done bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &top); err != nil {
		return false
	}
	candidates := unknownSettingsKeys(top, reflect.TypeOf(ServerConfigUpdateRequest{}), reflect.TypeOf(ServerConfigResponse{}))
	rejected, err := rejectedSettingsKeys(candidates, serverConfigFileView)
	if err != nil {
		slog.Error("PUT server-config: failed to build GET view for echo check", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to read existing settings", nil)
		return true
	}
	if len(rejected) == 0 {
		return false
	}
	writeUnpersistedKeysRejected(w, rejected)
	return true
}

// unknownSettingsKeys returns every key in top that a decode into reqType
// would silently drop. A key viewType (the GET response) does not know
// either can never be an echo of the GET view, so it is marked noEcho and
// always rejected: a flat dotted key such as
// "server.hub.auto_suspend_stalled", or a misspelt field, even when its
// value is a zero value. A key only viewType knows (a read-only GET field
// such as scion_version) stays subject to the echo rule, so a client that
// sends the GET body back keeps getting 200.
func unknownSettingsKeys(top map[string]json.RawMessage, reqType, viewType reflect.Type) []rawPath {
	paths := unknownJSONPaths(top, reqType, nil)
	for i := range paths {
		paths[i].noEcho = !jsonPathKnown(viewType, paths[i].path)
	}
	return paths
}

// rejectedSettingsKeys returns the sorted dotted keys of the candidates that
// are not a no-op echo of the GET view. view is only called when a
// candidate needs the echo check.
func rejectedSettingsKeys(candidates []rawPath, view func() (map[string]any, error)) ([]string, error) {
	candidates = dropNestedPaths(candidates)
	var v map[string]any
	var rejected []string
	for _, c := range candidates {
		if !c.noEcho {
			if v == nil {
				var err error
				if v, err = view(); err != nil {
					return nil, err
				}
			}
			if isEchoOfView(c, v) {
				continue
			}
		}
		rejected = append(rejected, strings.Join(c.path, "."))
	}
	sort.Strings(rejected)
	return rejected, nil
}

// writeUnpersistedKeysRejected writes the 422 both server-config PUT
// handlers return for keys they would otherwise drop.
func writeUnpersistedKeysRejected(w http.ResponseWriter, rejected []string) {
	slog.Warn("PUT server-config: rejecting keys that cannot be persisted", "keys", rejected)
	writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
		"error":   "unpersisted_keys_rejected",
		"message": "These settings are not recognised or cannot be saved through the server config API. Nothing was saved.",
		"keys":    rejected,
	})
}

// serverConfigFileView returns the file-mode GET /api/v1/admin/server-config
// body as a generic JSON value, the reference for echo detection.
func serverConfigFileView() (map[string]any, error) {
	resp, err := buildServerConfigFileResponse()
	if err != nil {
		return nil, err
	}
	return toJSONView(resp)
}

// serverConfigDBView returns the GET /api/v1/admin/server-config body as a
// generic JSON value, the reference for echo detection.
func (s *Server) serverConfigDBView(ctx context.Context, ops *OperationalSettings) (map[string]any, error) {
	resp, err := s.buildServerConfigDBResponse(ctx, ops)
	if err != nil {
		return nil, err
	}
	return toJSONView(resp)
}

// toJSONView round-trips v through JSON into a generic map.
func toJSONView(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var view map[string]any
	if err := json.Unmarshal(b, &view); err != nil {
		return nil, err
	}
	return view, nil
}

// dropNestedPaths removes candidates under another candidate's path, so a
// rejected server.scheduler is not reported again as server.scheduler.x.
func dropNestedPaths(in []rawPath) []rawPath {
	var out []rawPath
	for i, c := range in {
		nested := false
		for j, o := range in {
			if i != j && len(o.path) < len(c.path) && jsonPathHasPrefix(c.path, o.path) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, c)
		}
	}
	return out
}

func jsonPathHasPrefix(p, prefix []string) bool {
	for i := range prefix {
		if !strings.EqualFold(p[i], prefix[i]) {
			return false
		}
	}
	return true
}

type rawPath struct {
	path  []string
	value json.RawMessage
	// noEcho marks a key the GET view can never carry, so it is rejected
	// whatever its value.
	noEcho bool
}

// isEchoOfView reports whether the value sent at c.path equals the GET
// view's value there. A key the view omits matches only a JSON zero value
// (null, "", false, 0, {}, []), since GET drops empty fields.
func isEchoOfView(c rawPath, view map[string]any) bool {
	var sent any
	if err := json.Unmarshal(c.value, &sent); err != nil {
		return false
	}
	var cur any = view
	for _, seg := range c.path {
		m, ok := cur.(map[string]any)
		if !ok {
			return isZeroJSON(sent)
		}
		next, ok := m[seg]
		if !ok {
			return isZeroJSON(sent)
		}
		cur = next
	}
	return reflect.DeepEqual(sent, cur)
}

func isZeroJSON(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	case float64:
		return t == 0
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

func rawAtPath(top map[string]json.RawMessage, path []string) (json.RawMessage, bool) {
	cur := top
	for i, seg := range path {
		v, ok := lookupFold(cur, seg)
		if !ok {
			return nil, false
		}
		if i == len(path)-1 {
			return v, true
		}
		var next map[string]json.RawMessage
		if err := json.Unmarshal(v, &next); err != nil {
			return nil, false
		}
		cur = next
	}
	return nil, false
}

// lookupFold finds key in m the way encoding/json matches field names:
// exact match first, then case-insensitive.
func lookupFold(m map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}

var jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

// unknownJSONPaths walks a JSON object against the Go type it is decoded
// into and returns every key the decoder would silently drop.
func unknownJSONPaths(obj map[string]json.RawMessage, t reflect.Type, prefix []string) []rawPath {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || reflect.PointerTo(t).Implements(jsonUnmarshalerType) {
		return nil
	}
	fields := jsonFields(t)
	var out []rawPath
	for key, val := range obj {
		ft, ok := fieldFold(fields, key)
		path := append(append([]string{}, prefix...), key)
		if !ok {
			out = append(out, rawPath{path: path, value: val})
			continue
		}
		out = append(out, unknownJSONPathsInValue(val, ft, path)...)
	}
	return out
}

// fieldFold finds key in fields the way encoding/json matches field names:
// exact match first, then case-insensitive.
func fieldFold(fields map[string]reflect.Type, key string) (reflect.Type, bool) {
	if ft, ok := fields[key]; ok {
		return ft, true
	}
	for name, ft := range fields {
		if strings.EqualFold(name, key) {
			return ft, true
		}
	}
	return nil, false
}

// jsonPathKnown reports whether a JSON object path (in the form
// unknownJSONPaths returns: map keys are segments, slice indexes are not)
// names a field encoding/json would decode into t.
func jsonPathKnown(t reflect.Type, path []string) bool {
	for _, seg := range path {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			t = t.Elem()
		}
		if reflect.PointerTo(t).Implements(jsonUnmarshalerType) {
			return true
		}
		switch t.Kind() {
		case reflect.Struct:
			ft, ok := fieldFold(jsonFields(t), seg)
			if !ok {
				return false
			}
			t = ft
		case reflect.Map:
			t = t.Elem()
		case reflect.Interface:
			return true
		default:
			return false
		}
	}
	return true
}

func unknownJSONPathsInValue(val json.RawMessage, t reflect.Type, path []string) []rawPath {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(jsonUnmarshalerType) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if json.Unmarshal(val, &obj) != nil {
			return nil
		}
		return unknownJSONPaths(obj, t, path)
	case reflect.Map:
		var obj map[string]json.RawMessage
		if json.Unmarshal(val, &obj) != nil {
			return nil
		}
		var out []rawPath
		for k, v := range obj {
			out = append(out, unknownJSONPathsInValue(v, t.Elem(), append(append([]string{}, path...), k))...)
		}
		return out
	case reflect.Slice, reflect.Array:
		var arr []json.RawMessage
		if json.Unmarshal(val, &arr) != nil {
			return nil
		}
		var out []rawPath
		for _, v := range arr {
			out = append(out, unknownJSONPathsInValue(v, t.Elem(), path)...)
		}
		return out
	}
	return nil
}

// jsonFields maps the JSON names encoding/json would decode for struct t,
// including promoted fields of embedded structs, to their types.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			et := f.Type
			for et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				for n, ft := range jsonFields(et) {
					if _, dup := out[n]; !dup {
						out[n] = ft
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
		out[name] = f.Type
	}
	return out
}
