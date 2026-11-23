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

package rebuildpreflight

import (
	"net/http"
	"testing"
)

func TestEffectiveBranch(t *testing.T) {
	cases := []struct {
		name         string
		param        string
		repoBranch   string
		revParse     string
		wantBranch   string
		wantDetached bool
	}{
		{"param wins", "feat/x", "main", "other", "feat/x", false},
		{"repoBranch fallback", "", "main", "other", "main", false},
		{"revParse branch", "", "", "release/1.0\n", "release/1.0", false},
		{"revParse HEAD -> detached:true", "", "", "HEAD", DetachedSentinel, true},
		{"all empty -> detached:true (S36)", "", "", "", DetachedSentinel, true},
		// CN5-d: a real branch literally named "detached" has detached: false.
		{"real branch named detached (CN5-d)", "", "", "detached", DetachedSentinel, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, d := EffectiveBranch(tc.param, tc.repoBranch, tc.revParse)
			if b != tc.wantBranch || d != tc.wantDetached {
				t.Errorf("EffectiveBranch(%q,%q,%q) = (%q, %v), want (%q, %v)",
					tc.param, tc.repoBranch, tc.revParse, b, d, tc.wantBranch, tc.wantDetached)
			}
		})
	}
}

func TestRecordCommit(t *testing.T) {
	got, err := RecordCommit("  " + testValidSHA + "\n")
	if err != nil || got != testValidSHA {
		t.Fatalf("RecordCommit valid = (%q, %v)", got, err)
	}
	if _, err := RecordCommit("short"); err == nil {
		t.Error("S89: RecordCommit accepted invalid sha")
	}
}

func TestRecordForRun_CN5b(t *testing.T) {
	// Unforced with valid branch -> branchPinnable: true.
	rec, err := RecordForRun(nil, "main", false, testValidSHA)
	if err != nil || rec.Branch != "main" || rec.Detached || !rec.BranchPinnable || rec.Commit != testValidSHA {
		t.Fatalf("unforced valid = (%+v, %v)", rec, err)
	}

	// Unforced with real branch named "detached" -> detached: false, branchPinnable: false.
	rec, err = RecordForRun(nil, DetachedSentinel, false, testValidSHA)
	if err != nil || rec.Detached || rec.BranchPinnable {
		t.Fatalf("unforced real branch 'detached' = (%+v, %v), want detached=false branchPinnable=false", rec, err)
	}

	// Forced with grant records pinned branch (ignoring EffectiveBranch / RepoBranch).
	grant := &ForceGrant{
		RerunOf:      "run-1",
		PinnedBranch: "release/1.2",
		Detached:     false,
		PinnedCommit: testValidSHA,
	}
	rec, err = RecordForRun(grant, "main", false, testValidSHA)
	if err != nil || rec.Branch != "release/1.2" || rec.Detached || !rec.BranchPinnable || rec.Commit != testValidSHA {
		t.Fatalf("forced branch pin = (%+v, %v)", rec, err)
	}

	// Forced detached grant records detached: true even when effBranch is RepoBranch.
	detGrant := &ForceGrant{
		RerunOf:      "run-1",
		PinnedBranch: DetachedSentinel,
		Detached:     true,
		PinnedCommit: testValidSHA,
	}
	rec, err = RecordForRun(detGrant, "main", false, testValidSHA)
	if err != nil || rec.Branch != DetachedSentinel || !rec.Detached || !rec.BranchPinnable {
		t.Fatalf("forced detached pin = (%+v, %v)", rec, err)
	}

	// Head mismatch fails before build.
	otherSHA := "fedcba9876543210fedcba9876543210fedcba98"
	if _, err := RecordForRun(grant, "main", false, otherSHA); err == nil {
		t.Error("expected error on HEAD mismatch")
	}

	// CN6 chain: a forceRejected run's record fed back to NewGrant yields the same pins.
	pinnable := rec.BranchPinnable
	resMap := RunResultMap(RunState{
		Branch:         rec.Branch,
		Detached:       rec.Detached,
		BranchPinnable: &pinnable,
		Commit:         rec.Commit,
		RerunOf:        "run-1",
		ForceRejected:  "findings changed",
	})
	g2, rej := NewGrant(RunView{Exists: true, ID: "run-2", OperationKey: RebuildServerKey, Status: "failed", Result: resMap}, "u2", "u2@e")
	if rej != nil {
		t.Fatalf("CN6 chain NewGrant failed: %+v (status %d)", rej, http.StatusConflict)
	}
	if g2.PinnedBranch != detGrant.PinnedBranch || g2.Detached != detGrant.Detached || g2.PinnedCommit != detGrant.PinnedCommit {
		t.Errorf("CN6 chain pins mismatch: got %+v, want %+v", g2, detGrant)
	}
}
