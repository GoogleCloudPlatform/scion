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
	"strings"
	"testing"
)

const testValidSHA = "0123456789abcdef0123456789abcdef01234567"

func validFailedRunView() RunView {
	return RunView{
		Exists:       true,
		ID:           "run-failed-1",
		OperationKey: RebuildServerKey,
		Status:       "failed",
		Result: map[string]any{
			"branch": "main",
			"commit": testValidSHA,
			"preflight": map[string]any{
				"version":  1,
				"checks":   []any{"settings.profiles"},
				"blocking": float64(1),
				"warnings": float64(0),
				"findings": []any{
					map[string]any{
						"check":    "settings.profiles",
						"code":     "settings.profiles.missing",
						"severity": "block",
						"message":  "profile missing",
						"data":     map[string]any{"profile": "dev", "project": "p1"},
					},
				},
			},
		},
	}
}

func TestCheckRerunTarget(t *testing.T) {
	// Valid target with blocking findings.
	if rej := CheckRerunTarget(validFailedRunView(), "run-failed-1"); rej != nil {
		t.Fatalf("valid target rejected: %+v", rej)
	}

	// Valid target with couldNotRun only.
	cnrTarget := validFailedRunView()
	cnrTarget.Result = map[string]any{
		"branch":      "main",
		"commit":      testValidSHA,
		"couldNotRun": map[string]any{"kind": KindFailed, "class": "db error", "reason": "db error"},
	}
	if rej := CheckRerunTarget(cnrTarget, "run-failed-1"); rej != nil {
		t.Fatalf("valid couldNotRun target rejected: %+v", rej)
	}

	// Valid CN6 target: forceRejected run without forced:true IS forceable.
	cn6Target := validFailedRunView()
	cn6Target.Result["rerunOf"] = "run-0"
	cn6Target.Result["forceRejected"] = "findings changed"
	if rej := CheckRerunTarget(cn6Target, "run-failed-1"); rej != nil {
		t.Fatalf("CN6 forceRejected target rejected: %+v", rej)
	}

	cases := []struct {
		name       string
		mutate     func(*RunView)
		latestID   string
		wantStatus int
	}{
		{"not found", func(v *RunView) { v.Exists = false }, "run-failed-1", http.StatusBadRequest},
		{"other operation (S47)", func(v *RunView) { v.OperationKey = RebuildWebKey }, "run-failed-1", http.StatusBadRequest},
		{"not failed (S03)", func(v *RunView) { v.Status = "completed" }, "run-failed-1", http.StatusBadRequest},
		{"clean preflight (S04)", func(v *RunView) {
			v.Result = map[string]any{"branch": "main", "commit": testValidSHA, "preflight": map[string]any{"blocking": float64(0)}}
		}, "run-failed-1", http.StatusConflict},
		{"already forcedBy (S05)", func(v *RunView) {
			v.Result["forcedBy"] = map[string]any{"runId": "run-2"}
		}, "run-failed-1", http.StatusConflict},
		{"already forced:true (S06)", func(v *RunView) {
			v.Result["forced"] = true
		}, "run-failed-1", http.StatusConflict},
		{"not latest run (S07/S1)", func(v *RunView) {}, "run-newer-2", http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := validFailedRunView()
			tc.mutate(&v)
			rej := CheckRerunTarget(v, tc.latestID)
			if rej == nil {
				t.Fatalf("expected reject status %d, got nil", tc.wantStatus)
			}
			if rej.Status != tc.wantStatus {
				t.Errorf("status = %d, want %d (msg=%q)", rej.Status, tc.wantStatus, rej.Message)
			}
		})
	}
}

func TestNewGrant(t *testing.T) {
	v := validFailedRunView()
	g, rej := NewGrant(v, "user-42", "admin@example.com")
	if rej != nil {
		t.Fatalf("NewGrant failed: %+v", rej)
	}
	if g.ActorID != "user-42" || g.ActorEmail != "admin@example.com" {
		t.Errorf("actor = (%q, %q), want (user-42, admin@example.com)", g.ActorID, g.ActorEmail)
	}
	if g.PinnedBranch != "main" || g.Detached || g.PinnedCommit != testValidSHA {
		t.Errorf("pins = (%q, detached=%v, %q)", g.PinnedBranch, g.Detached, g.PinnedCommit)
	}
	if len(g.FailedFindings) != 1 || g.FailedFindings[0].Code != "settings.profiles.missing" {
		t.Errorf("FailedFindings = %+v", g.FailedFindings)
	}

	// Detached pin accepted ONLY with detached: true.
	det := validFailedRunView()
	det.Result["branch"] = DetachedSentinel
	det.Result["detached"] = true
	dg, rej := NewGrant(det, "user-42", "admin@example.com")
	if rej != nil {
		t.Fatalf("detached NewGrant failed: %+v", rej)
	}
	if !dg.Detached || dg.PinnedBranch != DetachedSentinel {
		t.Errorf("detached grant = %+v", dg)
	}

	// S101: branch "detached" WITHOUT detached: true must be rejected with 409.
	noFlag := validFailedRunView()
	noFlag.Result["branch"] = DetachedSentinel
	if _, rej := NewGrant(noFlag, "u", "e"); rej == nil || rej.Status != http.StatusConflict {
		t.Errorf("S101: detached without flag = %+v, want 409", rej)
	}

	// S53: invalid branch pin rejected with 409 naming BranchRule.
	badBranch := validFailedRunView()
	badBranch.Result["branch"] = "-bad"
	if _, rej := NewGrant(badBranch, "u", "e"); rej == nil || rej.Status != http.StatusConflict || !strings.Contains(rej.Message, BranchRule) {
		t.Errorf("S53: invalid branch = %+v, want 409 with BranchRule", rej)
	}

	// S51: invalid commit rejected with 409.
	badCommit := validFailedRunView()
	badCommit.Result["commit"] = "not-a-sha"
	if _, rej := NewGrant(badCommit, "u", "e"); rej == nil || rej.Status != http.StatusConflict {
		t.Errorf("S51: invalid commit = %+v, want 409", rej)
	}
}
