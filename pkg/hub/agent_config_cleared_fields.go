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
//     the request removes every key.
//
// req is the decoded request with the fixed keys already dropped, so a
// cleared_fields value a client sends is never taken. The result is sorted,
// and nil when empty.
//
// The PATCH dispositions and warnings, which still say a cleared value
// waits for the next reincarnation, are deliberately left as they are: a
// broker that predates ClearedFields keeps the previous value. Once every
// broker honours it, the cleared keys can be reported as applied at the
// next start instead of held, and ClearNeedsReincarnate dropped.
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
