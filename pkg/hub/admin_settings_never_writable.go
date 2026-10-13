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
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// neverAPIWritableSettingsKeys are settings keys that no settings write path
// accepts, in any mode (file, DB, and workstation, where other Layer-0 keys
// are API-writable), for any caller. They are deploy-time inputs of the test
// identity tiers (ptone/scion#4240): the hub class of the admin tier gate
// (phase A0) and the workload issuer allowlist (phase X2, whose trust set
// must not grow through the API), and the relay-peer secret (design §3.9:
// never stored in the database or returned by the API). A key matches
// itself and every key nested under it.
var neverAPIWritableSettingsKeys = []string{
	config.HubClassKey,
	config.TestIdentitiesKey,
	config.ConduitPeerSecretKey,
}

// testAdminTierRefusedSettingsKeys are settings keys refused by every
// settings write path, for every caller, while the test admin tier is on
// (G8: federation stays off). "federation" is the top-level member of the
// server-config request; "server.federation" is the same tree in the
// settings layout.
var testAdminTierRefusedSettingsKeys = []string{
	"federation",
	"server.federation",
}

// settingsBodyKeyPaths returns the dotted path of every object member in a
// JSON body, at any depth (arrays are not descended: no refused key lives
// under a list). Each path segment is folded the way encoding/json matches
// member names to struct fields, and a member name containing dots is
// split, so "Server", "server.hub" and {"server":{"hub":...}} all reach the
// same path. The body has already passed rejectRepeatedJSONMembers.
func settingsBodyKeyPaths(rawBody []byte) [][]string {
	var root any
	if err := json.Unmarshal(rawBody, &root); err != nil {
		return nil
	}
	var out [][]string
	var walk func(prefix []string, v any)
	walk = func(prefix []string, v any) {
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		for name, child := range obj {
			path := append([]string(nil), prefix...)
			for _, seg := range strings.Split(name, ".") {
				path = append(path, foldJSONMemberName(seg))
			}
			out = append(out, path)
			walk(path, child)
		}
	}
	walk(nil, root)
	return out
}

// matchedSettingsKeys returns, sorted, each key of keys that a body path
// equals or nests under.
func matchedSettingsKeys(paths [][]string, keys []string) []string {
	hit := map[string]bool{}
	for _, key := range keys {
		var want []string
		for _, seg := range strings.Split(key, ".") {
			want = append(want, foldJSONMemberName(seg))
		}
		for _, p := range paths {
			if len(p) < len(want) {
				continue
			}
			match := true
			for i := range want {
				if p[i] != want[i] {
					match = false
					break
				}
			}
			if match {
				hit[key] = true
				break
			}
		}
	}
	out := make([]string, 0, len(hit))
	for k := range hit {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dropEmptyPeerSecret returns rawBody without its
// server.hub.conduit.peer_secret members whose value is "" or null, and
// without the objects that removal leaves empty: an empty relay-peer secret
// is treated as absent (ignored, nothing written), while any other value is
// refused by rejectGateProtectedSettingsKeys. The body is returned unchanged
// when it holds no such member or does not parse (the typed decode reports
// that). Numbers keep their spelling. The body has already passed
// rejectRepeatedJSONMembers.
func dropEmptyPeerSecret(rawBody []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(rawBody))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return rawBody
	}
	var want []string
	for _, seg := range strings.Split(config.ConduitPeerSecretKey, ".") {
		want = append(want, foldJSONMemberName(seg))
	}
	var walk func(prefix []string, obj map[string]any) bool
	walk = func(prefix []string, obj map[string]any) (dropped bool) {
		for name, child := range obj {
			path := append([]string(nil), prefix...)
			for _, seg := range strings.Split(name, ".") {
				path = append(path, foldJSONMemberName(seg))
			}
			if slices.Equal(path, want) {
				if s, isString := child.(string); child == nil || (isString && s == "") {
					delete(obj, name)
					dropped = true
				}
				continue
			}
			if sub, ok := child.(map[string]any); ok && len(sub) > 0 && walk(path, sub) {
				dropped = true
				if len(sub) == 0 {
					delete(obj, name)
				}
			}
		}
		return dropped
	}
	obj, ok := root.(map[string]any)
	if !ok || !walk(nil, obj) {
		return rawBody
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return rawBody
	}
	return out
}

// gateProtectedSettingsKeys returns the refused keys a settings write body
// carries: every never-writable key, plus the federation keys while the
// test admin tier is on.
func (s *Server) gateProtectedSettingsKeys(rawBody []byte) []string {
	paths := settingsBodyKeyPaths(rawBody)
	keys := matchedSettingsKeys(paths, neverAPIWritableSettingsKeys)
	if s.testAdminTierOn() {
		keys = append(keys, matchedSettingsKeys(paths, testAdminTierRefusedSettingsKeys)...)
	}
	return keys
}

// rejectGateProtectedSettingsKeys answers 422 and returns true when a
// settings write body carries a gate-protected key. Every settings write
// path calls it before anything is decoded into a write.
func (s *Server) rejectGateProtectedSettingsKeys(w http.ResponseWriter, rawBody []byte) bool {
	keys := s.gateProtectedSettingsKeys(rawBody)
	if len(keys) == 0 {
		return false
	}
	msg := "these settings cannot be written through the API: " + strings.Join(keys, ", ")
	for _, k := range keys {
		if k == "federation" || k == "server.federation" {
			msg += " (" + errTestAdminFederationRefused.Error() + ")"
			break
		}
	}
	writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError, msg,
		map[string]interface{}{"keys": keys})
	return true
}
