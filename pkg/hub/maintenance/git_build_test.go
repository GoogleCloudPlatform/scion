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

package maintenance

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

func TestGitReleaseName(t *testing.T) {
	cases := map[string]string{
		testCommit:                   "git-0123456789ab",
		"  0123456789ABCDEF\n":       "git-0123456789ab",
		"0123456789ab":               "git-0123456789ab",
		"0123456789a":                "", // too short
		"":                           "",
		"unknown":                    "",
		"0123456789abcdef-dirty":     "",
		"../../0123456789abcdef0123": "",
	}
	for in, want := range cases {
		if got := GitReleaseName(in); got != want {
			t.Errorf("GitReleaseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunningReleaseName(t *testing.T) {
	if got := RunningReleaseName("nightly-20261023", testCommit); got != "nightly-20261023" {
		t.Errorf("release build = %q, want its version", got)
	}
	if got := RunningReleaseName("", testCommit); got != "git-0123456789ab" {
		t.Errorf("git build = %q, want git-<sha12>", got)
	}
	if got := RunningReleaseName("", ""); got != "" {
		t.Errorf("unidentified build = %q, want empty", got)
	}
}

func TestGitBuildLDFlags(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 30, 0, 0, time.FixedZone("x", 3600))
	got := GitBuildLDFlags(testCommit, at)
	want := "-X github.com/GoogleCloudPlatform/scion/pkg/version.Commit=" + testCommit +
		" -X github.com/GoogleCloudPlatform/scion/pkg/version.BuildTime=2026-11-01T11:30:00Z"
	if got != want {
		t.Fatalf("GitBuildLDFlags =\n %q\nwant\n %q", got, want)
	}
	if strings.Contains(got, ".Version=") {
		t.Fatalf("a Git build must not set Version: %q", got)
	}
}

// versionScript writes an executable that prints the given version JSON.
func versionScript(t *testing.T, json string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scion")
	script := "#!/bin/sh\ncat <<'EOF'\n" + json + "\nEOF\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVerifyGitBuild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the binary")
	}
	ctx := context.Background()
	cases := []struct {
		name, json, wantErr string
	}{
		{"commit matches", `{"version":"","commit":"` + testCommit + `"}`, ""},
		{"ldflags missing", `{"version":"","commit":""}`, "commit mismatch"},
		{"other commit", `{"version":"","commit":"ffffffffffffffffffffffffffffffffffffffff"}`, "commit mismatch"},
		{"release version set", `{"version":"v0.3.0","commit":"` + testCommit + `"}`, "must not carry a release version"},
		{"not json", `scion dev`, "failed to parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			err := VerifyGitBuild(ctx, versionScript(t, tc.json), testCommit, &log)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("VerifyGitBuild: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("VerifyGitBuild = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// The boot-failure guard for a rebuild from Git (target git-<sha12>): the
// target marks itself migrated and computes previous_safe like a release.
// A release "from" at or above the floor (a preview/stable with its publish
// date) is safe; a Git "from" is not (fail closed: a commit's schema position
// against the floor cannot be proven), nor is an undated preview.
func TestMarkUpdateStateBooting_GitTarget(t *testing.T) {
	cases := []struct {
		name, from, published string
		wantSafe              bool
	}{
		{"release -> git, dated preview", "v0.3.0-preview.4", "2026-10-22T18:00:00Z", true},
		{"release -> git, nightly", "nightly-20261023", "", true},
		{"release -> git, undated preview", "v0.3.0-preview.4", "", false},
		{"git -> git", "git-89abcdef0123", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := WriteUpdateState(root, UpdateState{Target: "git-0123456789ab", From: tc.from, RunID: "r1",
				PreviousSafe: true, FromPublishedAt: tc.published}); err != nil {
				t.Fatal(err)
			}
			running := RunningReleaseName("", testCommit)
			changed, _, err := MarkUpdateStateBooting(root, running, updateStateTestFloor)
			if err != nil || !changed {
				t.Fatalf("changed = %v, err = %v", changed, err)
			}
			st, _ := ReadUpdateState(root)
			if !st.Migrated || st.PreviousSafe != tc.wantSafe {
				t.Fatalf("st = %+v, want migrated and previous_safe=%v", st, tc.wantSafe)
			}
		})
	}
}
