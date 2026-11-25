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

package api

import "strings"

// EnvHomePersistent is the reserved SCION_* environment variable injected by
// the Runtime Broker into execution containers (harness_config=shell) to
// indicate whether $HOME survives container restarts on the active runtime
// ("true" or "false"). See design §3.6 (ptone/scion#8820).
const EnvHomePersistent = "SCION_HOME_PERSISTENT"

const runtimeStateHomePersistentPrefix = "home_persistent="

// EncodeHomePersistentRuntimeState appends or replaces the home_persistent
// token in a semicolon-delimited RuntimeState string. Any existing non-empty
// segments (such as "container:<id>") are preserved in order.
func EncodeHomePersistentRuntimeState(existing string, persistent bool) string {
	val := "false"
	if persistent {
		val = "true"
	}
	token := runtimeStateHomePersistentPrefix + val
	if strings.TrimSpace(existing) == "" {
		return token
	}
	parts := strings.Split(existing, ";")
	out := make([]string, 0, len(parts)+1)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, runtimeStateHomePersistentPrefix) {
			continue
		}
		out = append(out, p)
	}
	out = append(out, token)
	return strings.Join(out, ";")
}

// ParseHomePersistentRuntimeState extracts the home_persistent boolean from a
// RuntimeState string. When no valid home_persistent token is present, known is
// false.
func ParseHomePersistentRuntimeState(runtimeState string) (persistent bool, known bool) {
	if runtimeState == "" {
		return false, false
	}
	for _, part := range strings.Split(runtimeState, ";") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, runtimeStateHomePersistentPrefix) {
			continue
		}
		switch strings.TrimPrefix(part, runtimeStateHomePersistentPrefix) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}
