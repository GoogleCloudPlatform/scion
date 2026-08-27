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
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// validDMKinds enumerates the principal kinds allowed in a DM key.
var validDMKinds = map[string]bool{
	"user":  true,
	"agent": true,
}

// DMConversationKey builds a deterministic, order-independent external
// reference key for a direct-message conversation between two principals.
//
// Format: dm:<token1>:<token2> where each token is <kind>:<uuid>.
//   - kind must be "user" or "agent" (lowercase).
//   - UUID is normalised to canonical lowercase hex-with-hyphens.
//   - The two tokens are sorted byte-wise lexicographically.
//
// Because "agent:" < "user:" lexically, mixed pairs always render as
// dm:agent:<aid>:user:<uid>. Same-kind pairs sort by UUID.
func DMConversationKey(kindA, idA, kindB, idB string) (string, error) {
	kindA = strings.ToLower(kindA)
	kindB = strings.ToLower(kindB)

	if !validDMKinds[kindA] {
		return "", fmt.Errorf("dm key: unknown kind %q (must be user or agent)", kindA)
	}
	if !validDMKinds[kindB] {
		return "", fmt.Errorf("dm key: unknown kind %q (must be user or agent)", kindB)
	}

	// Parse and re-format UUIDs to canonical lowercase.
	uA, err := uuid.Parse(idA)
	if err != nil {
		return "", fmt.Errorf("dm key: invalid UUID for %s: %w", kindA, err)
	}
	uB, err := uuid.Parse(idB)
	if err != nil {
		return "", fmt.Errorf("dm key: invalid UUID for %s: %w", kindB, err)
	}

	tokenA := kindA + ":" + uA.String()
	tokenB := kindB + ":" + uB.String()

	// Sort tokens lexicographically.
	if tokenA > tokenB {
		tokenA, tokenB = tokenB, tokenA
	}

	return "dm:" + tokenA + ":" + tokenB, nil
}

// PrincipalKindFromAddress extracts the kind prefix ("user" or "agent") from
// a principal address string like "user:alice" or "agent:my-bot". Returns the
// kind and true if the prefix is a known kind, or ("", false) otherwise.
func PrincipalKindFromAddress(address string) (string, bool) {
	idx := strings.IndexByte(address, ':')
	if idx < 0 {
		return "", false
	}
	kind := strings.ToLower(address[:idx])
	if validDMKinds[kind] {
		return kind, true
	}
	return "", false
}

// ParseDMKey parses a key produced by DMConversationKey back into its
// constituent parts. The returned kinds and IDs are in sorted token order
// (the same order they appear in the key).
func ParseDMKey(key string) (kindA, idA, kindB, idB string, err error) {
	if !strings.HasPrefix(key, "dm:") {
		return "", "", "", "", fmt.Errorf("dm key: missing dm: prefix in %q", key)
	}

	body := key[3:] // strip "dm:"

	// body is "<kind>:<uuid>:<kind>:<uuid>"
	// We need to split into exactly 4 parts: kindA, idA, kindB, idB.
	// Since kind contains no colons and UUID is hex-with-hyphens (no colons),
	// we can split on ":" and expect exactly 4 segments.
	parts := strings.SplitN(body, ":", 4)
	if len(parts) != 4 {
		return "", "", "", "", fmt.Errorf("dm key: expected 4 colon-separated segments in %q", key)
	}

	kindA = parts[0]
	idA = parts[1]
	kindB = parts[2]
	idB = parts[3]

	if !validDMKinds[kindA] {
		return "", "", "", "", fmt.Errorf("dm key: unknown kind %q in %q", kindA, key)
	}
	if !validDMKinds[kindB] {
		return "", "", "", "", fmt.Errorf("dm key: unknown kind %q in %q", kindB, key)
	}

	if _, err := uuid.Parse(idA); err != nil {
		return "", "", "", "", fmt.Errorf("dm key: invalid UUID %q in %q: %w", idA, key, err)
	}
	if _, err := uuid.Parse(idB); err != nil {
		return "", "", "", "", fmt.Errorf("dm key: invalid UUID %q in %q: %w", idB, key, err)
	}

	return kindA, idA, kindB, idB, nil
}
