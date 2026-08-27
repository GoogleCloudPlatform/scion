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

package messages

import (
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMConversationKey_Roundtrip(t *testing.T) {
	idA := uuid.NewString()
	idB := uuid.NewString()

	key, err := DMConversationKey("user", idA, "agent", idB)
	require.NoError(t, err)

	kindA, gotIDA, kindB, gotIDB, err := ParseDMKey(key)
	require.NoError(t, err)

	// Tokens are sorted: "agent" < "user".
	assert.Equal(t, "agent", kindA)
	assert.Equal(t, idB, gotIDA) // agent's ID comes first
	assert.Equal(t, "user", kindB)
	assert.Equal(t, idA, gotIDB) // user's ID comes second
}

func TestDMConversationKey_Ordering_AgentUser(t *testing.T) {
	userID := uuid.NewString()
	agentID := uuid.NewString()

	// Regardless of argument order, key is the same.
	key1, err := DMConversationKey("user", userID, "agent", agentID)
	require.NoError(t, err)
	key2, err := DMConversationKey("agent", agentID, "user", userID)
	require.NoError(t, err)

	assert.Equal(t, key1, key2, "key must be order-independent")
	assert.True(t, strings.HasPrefix(key1, "dm:agent:"), "mixed pair must start with dm:agent:")
}

func TestDMConversationKey_Ordering_SameKind(t *testing.T) {
	id1 := "00000000-0000-0000-0000-000000000001"
	id2 := "00000000-0000-0000-0000-000000000002"

	key1, err := DMConversationKey("user", id1, "user", id2)
	require.NoError(t, err)
	key2, err := DMConversationKey("user", id2, "user", id1)
	require.NoError(t, err)

	assert.Equal(t, key1, key2, "same-kind key must be order-independent")
	// Smaller UUID sorts first.
	assert.Equal(t, "dm:user:"+id1+":user:"+id2, key1)
}

func TestDMConversationKey_CaseNormalisation(t *testing.T) {
	id := "AABBCCDD-0011-2233-4455-667788990011"
	lower := strings.ToLower(id)

	key, err := DMConversationKey("USER", id, "AGENT", lower)
	require.NoError(t, err)

	// Kind normalised to lowercase, UUID normalised to lowercase.
	assert.Contains(t, key, "agent:")
	assert.Contains(t, key, "user:")
	assert.NotContains(t, key, "AABBCCDD")
	assert.NotContains(t, key, "USER")
	assert.NotContains(t, key, "AGENT")
}

func TestDMConversationKey_RejectBadUUID(t *testing.T) {
	_, err := DMConversationKey("user", "not-a-uuid", "agent", uuid.NewString())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid UUID")

	_, err = DMConversationKey("user", uuid.NewString(), "agent", "also-bad")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid UUID")
}

func TestDMConversationKey_RejectUnknownKind(t *testing.T) {
	_, err := DMConversationKey("bot", uuid.NewString(), "user", uuid.NewString())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown kind")

	_, err = DMConversationKey("user", uuid.NewString(), "system", uuid.NewString())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown kind")
}

func TestParseDMKey_MissingPrefix(t *testing.T) {
	_, _, _, _, err := ParseDMKey("notdm:user:" + uuid.NewString() + ":agent:" + uuid.NewString())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing dm: prefix")
}

func TestParseDMKey_WrongSegmentCount(t *testing.T) {
	_, _, _, _, err := ParseDMKey("dm:user:" + uuid.NewString())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "4 colon-separated segments")
}

func TestParseDMKey_InvalidKindInKey(t *testing.T) {
	id := uuid.NewString()
	_, _, _, _, err := ParseDMKey("dm:bot:" + id + ":user:" + id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown kind")
}

func TestParseDMKey_InvalidUUIDInKey(t *testing.T) {
	id := uuid.NewString()
	_, _, _, _, err := ParseDMKey("dm:user:not-a-uuid:agent:" + id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid UUID")
}

func TestDMConversationKey_AgentAgent(t *testing.T) {
	id1 := uuid.NewString()
	id2 := uuid.NewString()

	key1, err := DMConversationKey("agent", id1, "agent", id2)
	require.NoError(t, err)
	key2, err := DMConversationKey("agent", id2, "agent", id1)
	require.NoError(t, err)

	assert.Equal(t, key1, key2, "agent-agent key must be order-independent")
}

// ---------------------------------------------------------------------------
// Deliverable 1: Conformance test — DMConversationKey matches production regex
// ---------------------------------------------------------------------------

// TestDMConversationKey_MatchesProductionRegex verifies that every key produced
// by DMConversationKey matches the production DM key regex shipped in the hub.
//
// Source of truth: pkg/hub/handlers_chat_v2.go:391
//   dmKeyRegexp = regexp.MustCompile(`^dm:(user|agent):[0-9a-f-]{36}:(user|agent):[0-9a-f-]{36}$`)
//
// The regex is unexported, so we reproduce the exact pattern here. If the hub
// pattern changes, this test should be updated to match — the comment above
// serves as the cross-reference.
func TestDMConversationKey_MatchesProductionRegex(t *testing.T) {
	// Exact regex from pkg/hub/handlers_chat_v2.go:391.
	dmKeyRegexp := regexp.MustCompile(`^dm:(user|agent):[0-9a-f-]{36}:(user|agent):[0-9a-f-]{36}$`)

	cases := []struct {
		name  string
		kindA string
		idA   string
		kindB string
		idB   string
	}{
		{"agent+user", "agent", uuid.NewString(), "user", uuid.NewString()},
		{"user+agent", "user", uuid.NewString(), "agent", uuid.NewString()},
		{"user+user", "user", uuid.NewString(), "user", uuid.NewString()},
		{"agent+agent", "agent", uuid.NewString(), "agent", uuid.NewString()},
		{"uppercase UUID", "user", "AABBCCDD-0011-2233-4455-667788990011", "agent", uuid.NewString()},
	}

	require.Greater(t, len(cases), 0, "conformance test must have at least one case (rule 14)")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := DMConversationKey(tc.kindA, tc.idA, tc.kindB, tc.idB)
			require.NoError(t, err)
			assert.Regexp(t, dmKeyRegexp, key,
				"DMConversationKey output must match production regex from handlers_chat_v2.go:391")
		})
	}
}

// ---------------------------------------------------------------------------
// Deliverable 6: Golden test vectors for cross-language conformance
// ---------------------------------------------------------------------------

// TestDMConversationKey_GoldenVectors provides deterministic, hardcoded test
// vectors whose expected output can be consumed by both the Go test suite and
// the TypeScript client test suite (web/src/components/pages/chat.ts:2325
// buildDMKey). Every input UUID and expected key string is byte-identical
// across languages — do NOT replace these with uuid.NewString().
func TestDMConversationKey_GoldenVectors(t *testing.T) {
	// Exact regex from pkg/hub/handlers_chat_v2.go:391.
	dmKeyRegexp := regexp.MustCompile(`^dm:(user|agent):[0-9a-f-]{36}:(user|agent):[0-9a-f-]{36}$`)

	vectors := []struct {
		name     string
		kindA    string
		idA      string
		kindB    string
		idB      string
		expected string
	}{
		{
			name:     "agent+user (mixed kind, standard order)",
			kindA:    "agent",
			idA:      "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
			kindB:    "user",
			idB:      "550e8400-e29b-41d4-a716-446655440000",
			expected: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000",
		},
		{
			name:     "user+agent (mixed kind, reversed argument order — must produce same key as case 1)",
			kindA:    "user",
			idA:      "550e8400-e29b-41d4-a716-446655440000",
			kindB:    "agent",
			idB:      "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
			expected: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000",
		},
		{
			name:     "user+user (same kind, different UUIDs)",
			kindA:    "user",
			idA:      "f47ac10b-58cc-4372-a567-0e02b2c3d479",
			kindB:    "user",
			idB:      "7c9e6679-7425-40de-944b-e07fc1f90ae7",
			expected: "dm:user:7c9e6679-7425-40de-944b-e07fc1f90ae7:user:f47ac10b-58cc-4372-a567-0e02b2c3d479",
		},
		{
			name:     "agent+agent (same kind)",
			kindA:    "agent",
			idA:      "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
			kindB:    "agent",
			idB:      "12345678-1234-5678-1234-567812345678",
			expected: "dm:agent:12345678-1234-5678-1234-567812345678:agent:a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
		},
		{
			name:     "uppercase UUID requires lowercase normalisation",
			kindA:    "user",
			idA:      "AABBCCDD-0011-2233-4455-667788990011",
			kindB:    "agent",
			idB:      "6BA7B810-9DAD-11D1-80B4-00C04FD430C8",
			expected: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:aabbccdd-0011-2233-4455-667788990011",
		},
	}

	require.Greater(t, len(vectors), 0, "golden vectors must have at least one case (rule 14)")

	for _, tc := range vectors {
		t.Run(tc.name, func(t *testing.T) {
			key, err := DMConversationKey(tc.kindA, tc.idA, tc.kindB, tc.idB)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, key,
				"golden vector mismatch — if this fails, the TS buildDMKey must also be updated")
			assert.Regexp(t, dmKeyRegexp, key,
				"golden vector must match production regex from handlers_chat_v2.go:391")
		})
	}

	// Cross-check: case 1 and case 2 use the same UUIDs in reversed argument
	// order and must produce byte-identical keys.
	key1, _ := DMConversationKey(vectors[0].kindA, vectors[0].idA, vectors[0].kindB, vectors[0].idB)
	key2, _ := DMConversationKey(vectors[1].kindA, vectors[1].idA, vectors[1].kindB, vectors[1].idB)
	assert.Equal(t, key1, key2, "reversed argument order must produce the same key")
}
