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
	"testing"

	"github.com/google/uuid"
)

// tid deterministically maps a human-readable test identifier (e.g. "user-1")
// to a stable UUID string. The Ent-backed store uses UUID primary keys, so test
// fixtures cannot use arbitrary strings as IDs; wrapping a readable name in tid
// preserves test legibility and cross-reference consistency (tid("user-1")
// always returns the same UUID) while satisfying the UUID requirement.
func tid(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

// tidSlugSafe returns a lowercase, slug-safe fragment derived from a test
// name (which may contain "/" from subtests).
func tidSlugSafe(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c-'A'+'a')
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

func agentIDs(resp ListAgentsResponse) []string {
	ids := make([]string, len(resp.Agents))
	for i, a := range resp.Agents {
		ids[i] = a.ID
	}
	return ids
}

func extractAgentIDs(agents []AgentWithCapabilities) []string {
	ids := make([]string, len(agents))
	for i, a := range agents {
		ids[i] = a.ID
	}
	return ids
}

func extractProjectIDs(projects []ProjectWithCapabilities) []string {
	ids := make([]string, len(projects))
	for i, p := range projects {
		ids[i] = p.ID
	}
	return ids
}

func requireUUID(t *testing.T, what, s string) {
	t.Helper()
	if _, err := uuid.Parse(s); err != nil {
		t.Fatalf("%s = %q, want a UUID: %v", what, s, err)
	}
}
