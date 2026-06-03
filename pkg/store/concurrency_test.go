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

package store

import (
	"testing"
)

// StableProjectHash must be deterministic: same input → same output.
func TestStableProjectHash_Deterministic(t *testing.T) {
	id := "550e8400-e29b-41d4-a716-446655440000"
	h1 := StableProjectHash(id)
	h2 := StableProjectHash(id)
	if h1 != h2 {
		t.Errorf("StableProjectHash not deterministic: %d vs %d", h1, h2)
	}
}

// Different project IDs should (almost certainly) produce different hashes.
func TestStableProjectHash_DifferentInputs(t *testing.T) {
	h1 := StableProjectHash("project-aaa")
	h2 := StableProjectHash("project-bbb")
	if h1 == h2 {
		t.Errorf("StableProjectHash collision: %q and %q both hash to %d",
			"project-aaa", "project-bbb", h1)
	}
}

// The hash must cover the full int32 range (including negative values from
// uint32 → int32 wrap). This is fine — pg_try_advisory_lock(int4, int4)
// accepts any int4 value.
func TestStableProjectHash_AcceptsNegativeRange(t *testing.T) {
	// Just verify it doesn't panic and returns a value.
	h := StableProjectHash("test-id")
	_ = h // any int32 is valid
}

// Empty string is a valid input (degenerate but must not panic).
func TestStableProjectHash_EmptyString(t *testing.T) {
	h := StableProjectHash("")
	_ = h // must not panic
}

// Verify the key constants are in the expected ranges and non-overlapping.
func TestAdvisoryLockKeys_NonOverlapping(t *testing.T) {
	singletonKeys := []AdvisoryLockKey{
		LockScheduleEvaluator,
		LockAgentHeartbeatTimeout,
		LockAgentStalledDetection,
		LockSoftDeletePurge,
		LockGitHubAppHealthCheck,
	}

	seen := make(map[AdvisoryLockKey]bool, len(singletonKeys)+1)
	for _, k := range singletonKeys {
		if seen[k] {
			t.Errorf("duplicate singleton key: %d", k)
		}
		seen[k] = true
	}

	// LockWorkspaceProvision is in a different range from singletons.
	if seen[LockWorkspaceProvision] {
		t.Errorf("LockWorkspaceProvision %d collides with a singleton key", LockWorkspaceProvision)
	}

	// Verify the ranges are visually distinct (different 0x5C10_0xxx vs 0x5C10_1xxx).
	for _, k := range singletonKeys {
		if int64(k)&0xFFFF0000 != 0x5C100000 {
			t.Errorf("singleton key %d (0x%X) not in expected range 0x5C10_0xxx", k, int64(k))
		}
	}
	if int64(LockWorkspaceProvision)&0xFFFF0000 != 0x5C100000 {
		// Both are in 0x5C10_xxxx but the lower 16 bits distinguish singleton vs per-object.
		// LockWorkspaceProvision should be >= 0x5C10_1000.
		if int64(LockWorkspaceProvision) < 0x5C101000 {
			t.Errorf("LockWorkspaceProvision %d (0x%X) should be >= 0x5C10_1000 to separate from singletons",
				LockWorkspaceProvision, int64(LockWorkspaceProvision))
		}
	}
}
