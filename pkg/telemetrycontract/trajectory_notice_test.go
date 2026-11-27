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

package telemetrycontract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type trajectoryNoticeFixture struct {
	NoticeVersion string `json:"notice_version"`
	Cases         []struct {
		Policy        string `json:"policy"`
		EnforcedByHub bool   `json:"enforced_by_hub"`
		Effective     bool   `json:"effective"`
		Notice        string `json:"notice"`
	} `json:"cases"`
}

func TestTrajectoryCaptureNotice_MatchesSharedFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "trajectory_capture_notices.json"))
	if err != nil {
		t.Fatalf("read shared notice fixture: %v", err)
	}
	var fix trajectoryNoticeFixture
	if err := json.Unmarshal(raw, &fix); err != nil {
		t.Fatalf("unmarshal shared notice fixture: %v", err)
	}
	if fix.NoticeVersion != TrajectoryNoticeVersion {
		t.Fatalf("TrajectoryNoticeVersion = %q, fixture = %q", TrajectoryNoticeVersion, fix.NoticeVersion)
	}
	if len(fix.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	for _, tc := range fix.Cases {
		gotEffective := IsTrajectoryCaptureEffective(tc.Policy)
		if gotEffective != tc.Effective {
			t.Errorf("IsTrajectoryCaptureEffective(%q) = %v, want %v", tc.Policy, gotEffective, tc.Effective)
		}
		gotNotice := TrajectoryCaptureNotice(TrajectoryCapturePolicyStatus{
			Policy:        tc.Policy,
			EnforcedByHub: tc.EnforcedByHub,
		})
		if gotNotice != tc.Notice {
			t.Errorf("TrajectoryCaptureNotice(%q, enforced=%v) = %q, want %q",
				tc.Policy, tc.EnforcedByHub, gotNotice, tc.Notice)
		}
	}
}

func TestResolveTrajectoryCapturePolicy(t *testing.T) {
	cases := []struct {
		name        string
		allowOptOut bool
		annotation  string
		want        TrajectoryCapturePolicyStatus
	}{
		{
			name:        "unlocked unset defaults to full",
			allowOptOut: true,
			annotation:  "",
			want:        TrajectoryCapturePolicyStatus{Policy: TrajectoryCapturePolicyFull, EnforcedByHub: false},
		},
		{
			name:        "unlocked invalid defaults to full",
			allowOptOut: true,
			annotation:  "bogus",
			want:        TrajectoryCapturePolicyStatus{Policy: TrajectoryCapturePolicyFull, EnforcedByHub: false},
		},
		{
			name:        "unlocked full",
			allowOptOut: true,
			annotation:  "full",
			want:        TrajectoryCapturePolicyStatus{Policy: TrajectoryCapturePolicyFull, EnforcedByHub: false},
		},
		{
			name:        "unlocked metadata_only",
			allowOptOut: true,
			annotation:  " metadata_only ",
			want:        TrajectoryCapturePolicyStatus{Policy: TrajectoryCapturePolicyMetadataOnly, EnforcedByHub: false},
		},
		{
			name:        "unlocked off",
			allowOptOut: true,
			annotation:  "off",
			want:        TrajectoryCapturePolicyStatus{Policy: TrajectoryCapturePolicyOff, EnforcedByHub: false},
		},
		{
			name:        "locked unset enforces full",
			allowOptOut: false,
			annotation:  "",
			want:        TrajectoryCapturePolicyStatus{Policy: TrajectoryCapturePolicyFull, EnforcedByHub: true},
		},
		{
			name:        "locked invalid enforces full",
			allowOptOut: false,
			annotation:  "bogus",
			want:        TrajectoryCapturePolicyStatus{Policy: TrajectoryCapturePolicyFull, EnforcedByHub: true},
		},
		{
			name:        "locked full enforces full",
			allowOptOut: false,
			annotation:  "full",
			want:        TrajectoryCapturePolicyStatus{Policy: TrajectoryCapturePolicyFull, EnforcedByHub: true},
		},
		{
			name:        "locked metadata_only enforces full",
			allowOptOut: false,
			annotation:  "metadata_only",
			want:        TrajectoryCapturePolicyStatus{Policy: TrajectoryCapturePolicyFull, EnforcedByHub: true},
		},
		{
			name:        "locked off enforces full",
			allowOptOut: false,
			annotation:  "off",
			want:        TrajectoryCapturePolicyStatus{Policy: TrajectoryCapturePolicyFull, EnforcedByHub: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveTrajectoryCapturePolicy(tc.allowOptOut, tc.annotation)
			if got != tc.want {
				t.Fatalf("ResolveTrajectoryCapturePolicy(%v, %q) = %+v, want %+v",
					tc.allowOptOut, tc.annotation, got, tc.want)
			}
		})
	}
}
