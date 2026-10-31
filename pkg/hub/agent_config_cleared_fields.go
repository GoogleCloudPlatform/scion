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
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// nextClearedFields returns the agent's live inline config's ClearedFields
// after a config PATCH (ptone/scion#3906): the paths the broker removes from
// the agent's persisted config at each start, so the inherited value
// applies instead of the previous one, which the broker's start-time merge
// would otherwise keep.
//
// It starts from old's list. A key the request sets to a non-empty value is
// dropped from it, as is each env key the request sends. It then adds:
//   - cleared, the config keys reincarnateOnlyConfigEdits reports as cleared
//     (prefixed wire keys), that api.ClearableConfigPath accepts;
//   - system_prompt, when the request empties a previously set prompt;
//   - env.KEY for each env key the request removes, under the same rule
//     removedConfigEntries uses (the caller can view the agent's env; the
//     auto-expose keys and TZ are never counted as removed), and also when
//     the request removes every key;
//   - resources.<sub-field> for each resources sub-field old stores that a
//     request presenting resources with a value that is not empty leaves
//     out (droppedResourceSubPaths, ptone/scion#5676). Each sub-field such a
//     request sets is dropped from the list, and a recorded whole resources
//     clear replaces every resources.<sub-field> entry, so the list never
//     holds both.
//
// req is the decoded request with the fixed keys already dropped, so a
// cleared_fields value a client sends is never taken. The result is sorted,
// and nil when empty.
//
// The PATCH dispositions and warnings report a recorded clear as applied at
// the next start only when the agent's broker advertises clearedFields
// (brokerStartSupport.heldClearEdits, ptone/scion#4890), and a resources
// sub-field clear only when it also advertises clearedResourceSubfields: an
// older broker keeps the previous value, so for it the clear still waits
// for the next reincarnation. The list is recorded whatever the broker
// supports, so a broker upgraded later applies it.
func nextClearedFields(old, req *api.ScionConfig, rawConfig map[string]json.RawMessage, cleared []string, canViewEnv bool) []string {
	set := map[string]bool{}
	if old != nil {
		for _, p := range old.ClearedFields {
			set[p] = true
		}
	}

	present := map[string]bool{}
	for _, f := range configPatchKeys(rawConfig) {
		k := strings.TrimPrefix(f.Key, agentConfigKeyPrefix)
		present[k] = true
		if !configFieldZero(k, req) {
			delete(set, k)
		}
	}
	if present["env"] && req != nil {
		for k := range req.Env {
			delete(set, api.ClearedEnvPathPrefix+k)
		}
	}
	if present["resources"] && req != nil && req.Resources != nil {
		for _, p := range api.ResourceSubPaths() {
			if api.ResourceSubFieldValue(req.Resources, p) != "" {
				delete(set, p)
			}
		}
	}

	for _, key := range cleared {
		if k := strings.TrimPrefix(key, agentConfigKeyPrefix); api.ClearableConfigPath(k) {
			set[k] = true
		}
	}
	if present["system_prompt"] && configFieldZero("system_prompt", req) && !configFieldZero("system_prompt", old) {
		set["system_prompt"] = true
	}
	if present["env"] && canViewEnv && old != nil && req != nil {
		for k := range old.Env {
			if _, kept := req.Env[k]; kept || autoExposeEnvKeys[k] || k == agentTZEnvKey {
				continue
			}
			set[api.ClearedEnvPathPrefix+k] = true
		}
	}
	for _, p := range droppedResourceSubPaths(old, req, present) {
		set[p] = true
	}
	if set["resources"] {
		for _, p := range api.ResourceSubPaths() {
			delete(set, p)
		}
	}

	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// droppedResourceSubPaths returns the resources sub-paths
// (api.ResourceSubPaths, sorted) of the sub-fields old stores that the
// request leaves out when it presents resources with a value that is not
// empty. The start merge (config.MergeResourceSpec) applies only non-empty
// sub-fields, so a dropped one keeps its previous value unless it is
// recorded in cleared_fields (ptone/scion#5676). A resources value that is
// empty as a whole is a whole clear, which reincarnateOnlyConfigEdits
// reports, and gives nil here.
func droppedResourceSubPaths(old, req *api.ScionConfig, present map[string]bool) []string {
	if !present["resources"] || old == nil || old.Resources == nil || req == nil || req.Resources == nil || *req.Resources == (api.ResourceSpec{}) {
		return nil
	}
	var out []string
	for _, p := range api.ResourceSubPaths() {
		if api.ResourceSubFieldValue(old.Resources, p) != "" && api.ResourceSubFieldValue(req.Resources, p) == "" {
			out = append(out, p)
		}
	}
	return out
}

// withDroppedResourcesSubField returns cleared (wire keys of cleared
// container keys, from reincarnateOnlyConfigEdits) with config.resources
// added, in sorted order, when the request drops a resources sub-field old
// stores (droppedResourceSubPaths). nextClearedFields records each dropped
// sub-field; whether the next start applies it depends on the broker
// (brokerStartSupport.heldClearEdits). A resources value that is empty as a
// whole is a clear reincarnateOnlyConfigEdits already reports.
func withDroppedResourcesSubField(cleared []string, old, req *api.ScionConfig, present map[string]bool) []string {
	const key = agentConfigKeyPrefix + "resources"
	if slices.Contains(cleared, key) || len(droppedResourceSubPaths(old, req, present)) == 0 {
		return cleared
	}
	out := append(append([]string(nil), cleared...), key)
	sort.Strings(out)
	return out
}
