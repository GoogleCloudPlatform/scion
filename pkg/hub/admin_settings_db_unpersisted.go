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
// client that sends the GET body back keeps getting 200.

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
	// Layer-0 but not mapped by extractKoanfKeysFromRequest today. Error
	// precedence does not depend on this entry (hostedBootstrapChanges
	// classifies by the registry, so a hosted PUT is layer0_rejected either
	// way). MERGE NOTE: when the kr-nfshome stack lands it maps this key;
	// TestDBUnpersistedRequestPaths_CoverUnmappedFields then fails and this
	// one line must be removed. Nothing else needs to change.
	{"server", "shared_dir_storage"},
	{"server", "maintenance"},
	{"server", "scheduler"},
	{"server", "oidc_login"},
	{"server", "oidc"},
	{"server", "hub", "agent_endpoint"},
	{"server", "hub", "gcp_iam_check_mode"},
	{"server", "hub", "gcp_iam_deny_unknown_policy"},
	{"server", "hub", "missing_agent_grace"},
	{"server", "hub", "disable_legacy_storage_fallback"},
	{"server", "auth", "username"},
	{"server", "auth", "display_name"},
	{"server", "auth", "email"},
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
// With routeFileOnly (workstation hubs) the dbFileOnlyRequestPaths present in
// the body are not candidates; they are returned as koanf-style keys for the
// settings.yaml write. done is true if the response has been written.
func (s *Server) rejectUnpersistedKeys(ctx context.Context, w http.ResponseWriter, ops *OperationalSettings, rawBody []byte, routeFileOnly bool) (fileKeys []string, done bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &top); err != nil {
		return nil, false
	}

	candidates := unknownJSONPaths(top, reflect.TypeOf(ServerConfigUpdateDBRequest{}), nil)
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
	candidates = dropNestedPaths(candidates)
	if len(candidates) == 0 {
		return fileKeys, false
	}

	view, err := s.serverConfigDBView(ctx, ops)
	if err != nil {
		slog.Error("PUT server-config: failed to build GET view for echo check", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to read existing settings", nil)
		return nil, true
	}

	var rejected []string
	for _, c := range candidates {
		if !isEchoOfView(c, view) {
			rejected = append(rejected, strings.Join(c.path, "."))
		}
	}
	if len(rejected) == 0 {
		return fileKeys, false
	}
	sort.Strings(rejected)
	slog.Warn("PUT server-config: rejecting keys that cannot be persisted", "keys", rejected)
	writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
		"error":   "unpersisted_keys_rejected",
		"message": "These settings are not recognised or cannot be saved through the server config API.",
		"keys":    rejected,
	})
	return nil, true
}

// serverConfigDBView returns the GET /api/v1/admin/server-config body as a
// generic JSON value, the reference for echo detection.
func (s *Server) serverConfigDBView(ctx context.Context, ops *OperationalSettings) (map[string]any, error) {
	resp, err := s.buildServerConfigDBResponse(ctx, ops)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(resp)
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
		ft, ok := fields[key]
		if !ok {
			for name, f := range fields {
				if strings.EqualFold(name, key) {
					ft, ok = f, true
					break
				}
			}
		}
		path := append(append([]string{}, prefix...), key)
		if !ok {
			out = append(out, rawPath{path: path, value: val})
			continue
		}
		out = append(out, unknownJSONPathsInValue(val, ft, path)...)
	}
	return out
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
