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

// Package suppgroups decides which supplementary groups sciontool keeps
// when it drops from root to the agent user (ptone/scion#3155).
//
// Go's exec with SysProcAttr.Credential calls setgroups(Groups), so an
// empty Groups clears every supplementary group, including those the
// runtime added with --group-add for nfs shared dirs. The broker names the
// groups to keep in EnvVar. That variable alone is not trusted, since
// template or user env could set it, so only ids that are ALSO in this
// process's own supplementary groups (what the runtime actually granted)
// are kept, and 0 is never kept.
package suppgroups

import (
	"os"
	"strconv"
	"strings"
)

// EnvVar mirrors runtime.SupplementalGIDsEnvVar (pkg/runtime/common.go).
const EnvVar = "SCION_SUPPLEMENTAL_GIDS"

// getgroups is os.Getgroups, replaceable in tests.
var getgroups = os.Getgroups

// FromEnv returns the groups to keep across the privilege drop: ids listed
// in EnvVar (comma separated) that are also in the current process's
// supplementary groups, excluding 0, in EnvVar order without duplicates.
// It returns nil when EnvVar is unset or nothing qualifies, which keeps
// today's behaviour (no supplementary groups). Malformed entries are
// ignored.
func FromEnv() []uint32 {
	raw := os.Getenv(EnvVar)
	if raw == "" {
		return nil
	}
	granted, err := getgroups()
	if err != nil {
		return nil
	}
	have := make(map[uint32]bool, len(granted))
	for _, g := range granted {
		if g > 0 {
			have[uint32(g)] = true
		}
	}
	var out []uint32
	seen := make(map[uint32]bool)
	for _, field := range strings.Split(raw, ",") {
		n, err := strconv.ParseUint(strings.TrimSpace(field), 10, 32)
		if err != nil || n == 0 {
			continue
		}
		gid := uint32(n)
		if !have[gid] || seen[gid] {
			continue
		}
		seen[gid] = true
		out = append(out, gid)
	}
	return out
}
