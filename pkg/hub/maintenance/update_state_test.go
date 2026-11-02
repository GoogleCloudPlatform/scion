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
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const updateStateTestFloor = "nightly-20261009"

func putUpdateStateFile(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, UpdateStateFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadUpdateState(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		if st, err := ReadUpdateState(t.TempDir()); st != nil || err != nil {
			t.Fatalf("st = %+v, err = %v", st, err)
		}
		if st, err := ReadUpdateState(""); st != nil || err != nil {
			t.Fatalf("no root: st = %+v, err = %v", st, err)
		}
	})
	t.Run("valid", func(t *testing.T) {
		root := t.TempDir()
		putUpdateStateFile(t, root, `{"target":"nightly-20261024","from":"nightly-20261023","runId":"r1","verified":false,"migrated":true,"previous_safe":true}`+"\n")
		st, err := ReadUpdateState(root)
		if err != nil || st == nil || *st != (UpdateState{Target: "nightly-20261024", From: "nightly-20261023", RunID: "r1", Migrated: true, PreviousSafe: true}) {
			t.Fatalf("st = %+v, err = %v", st, err)
		}
	})
	for name, content := range map[string]string{
		"malformed":  "{",
		"oversized":  `{"target":"` + strings.Repeat("x", maxUpdateStateSize) + `"}`,
		"wrong type": `{"verified":"no"}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			putUpdateStateFile(t, root, content)
			if st, err := ReadUpdateState(root); err == nil {
				t.Fatalf("st = %+v, want an error", st)
			}
		})
	}
	t.Run("a directory is refused", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, UpdateStateFileName), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadUpdateState(root); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("a symlink is not followed", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need privileges on Windows")
		}
		root := t.TempDir()
		target := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(target, []byte(`{"target":"x"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, UpdateStateFileName)); err != nil {
			t.Fatal(err)
		}
		if st, err := ReadUpdateState(root); err == nil || st != nil {
			t.Fatalf("st = %+v, err = %v; want a refusal", st, err)
		}
	})
}

func TestWriteUpdateState(t *testing.T) {
	root := t.TempDir()
	want := UpdateState{Target: "nightly-20261024", From: "nightly-20261023", RunID: "r1", PreviousSafe: true}
	if err := WriteUpdateState(root, want); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, UpdateStateFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	// The exact keys the guard reads.
	for _, k := range []string{"target", "from", "runId", "verified", "migrated", "previous_safe"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("key %q missing: %s", k, data)
		}
	}
	if _, ok := raw["from_published_at"]; ok {
		t.Errorf("empty from_published_at must be omitted: %s", data)
	}
	if !bytes.HasSuffix(data, []byte("}\n")) || bytes.Count(data, []byte("\n")) != 1 {
		t.Errorf("want one line of JSON: %q", data)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o644 != 0o644 || fi.Mode().Perm()&0o022 != 0 {
			t.Fatalf("mode = %v, want 0644", fi.Mode().Perm())
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(root, UpdateStateFileName+".tmp-*")); len(matches) != 0 {
		t.Fatalf("temporary files left: %v", matches)
	}

	t.Run("a symlink at the path is replaced, not written through", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need privileges on Windows")
		}
		root := t.TempDir()
		elsewhere := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(elsewhere, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, filepath.Join(root, UpdateStateFileName)); err != nil {
			t.Fatal(err)
		}
		if err := WriteUpdateState(root, want); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(elsewhere); string(b) != "keep" {
			t.Fatalf("wrote through the symlink: %q", b)
		}
		if st, err := ReadUpdateState(root); err != nil || st.Target != want.Target {
			t.Fatalf("st = %+v, err = %v", st, err)
		}
	})
}

func TestPreviousSafe(t *testing.T) {
	day := func(s string) time.Time {
		d, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	tests := []struct {
		name      string
		from      string
		published time.Time
		want      bool
	}{
		{"nightly above the floor", "nightly-20261023", time.Time{}, true},
		{"nightly at the floor", "nightly-20261009", time.Time{}, true},
		{"nightly below the floor", "nightly-20261008", time.Time{}, false},
		{"preview published after the floor", "v0.3.0-preview.4", day("2026-10-22T18:00:00Z"), true},
		{"preview published before the floor", "v0.3.0-preview.1", day("2026-10-01T18:00:00Z"), false},
		{"preview without a date", "v0.3.0-preview.4", time.Time{}, false},
		{"stable published after the floor", "v1.0.0", day("2026-11-01T00:00:00Z"), true},
		{"git build release dir", "git-0123456789ab", time.Time{}, false},
		{"empty", "", time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PreviousSafe(tt.from, tt.published, updateStateTestFloor); got != tt.want {
				t.Fatalf("PreviousSafe(%q, %v) = %v, want %v", tt.from, tt.published, got, tt.want)
			}
		})
	}
}

func TestMarkUpdateStateBooting(t *testing.T) {
	t.Run("the target sets migrated and computes previous_safe", func(t *testing.T) {
		for from, safe := range map[string]bool{"nightly-20261023": true, "nightly-20261001": false} {
			root := t.TempDir()
			if err := WriteUpdateState(root, UpdateState{Target: "nightly-20261024", From: from, RunID: "r1", PreviousSafe: true}); err != nil {
				t.Fatal(err)
			}
			changed, _, err := MarkUpdateStateBooting(root, "nightly-20261024", updateStateTestFloor)
			if err != nil || !changed {
				t.Fatalf("changed = %v, err = %v", changed, err)
			}
			st, _ := ReadUpdateState(root)
			if !st.Migrated || st.PreviousSafe != safe || st.Verified || st.RunID != "r1" || st.From != from {
				t.Fatalf("from %s: st = %+v, want migrated, previous_safe=%v", from, st, safe)
			}
		}
	})

	t.Run("deploy.sh installs and the publish-date hint", func(t *testing.T) {
		root := t.TempDir()
		if err := WriteUpdateState(root, UpdateState{Target: "nightly-20261024", From: "v0.3.0-preview.4",
			RunID: UpdateStateRunIDDeploy, PreviousSafe: true, FromPublishedAt: "2026-10-22T18:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := MarkUpdateStateBooting(root, "nightly-20261024", updateStateTestFloor); err != nil {
			t.Fatal(err)
		}
		if st, _ := ReadUpdateState(root); !st.Migrated || !st.PreviousSafe {
			t.Fatalf("st = %+v", st)
		}
	})

	t.Run("another target is left alone", func(t *testing.T) {
		root := t.TempDir()
		content := `{"target":"nightly-20261025","from":"nightly-20261023","runId":"r1","verified":false,"migrated":false,"previous_safe":true}` + "\n"
		putUpdateStateFile(t, root, content)
		changed, _, err := MarkUpdateStateBooting(root, "nightly-20261024", updateStateTestFloor)
		if err != nil || changed {
			t.Fatalf("changed = %v, err = %v", changed, err)
		}
		if b, _ := os.ReadFile(filepath.Join(root, UpdateStateFileName)); string(b) != content {
			t.Fatalf("file rewritten: %q", b)
		}
	})

	t.Run("missing file and git build", func(t *testing.T) {
		if changed, _, err := MarkUpdateStateBooting(t.TempDir(), "nightly-20261024", updateStateTestFloor); changed || err != nil {
			t.Fatalf("changed = %v, err = %v", changed, err)
		}
		root := t.TempDir()
		putUpdateStateFile(t, root, `{"target":""}`)
		if changed, _, err := MarkUpdateStateBooting(root, "", updateStateTestFloor); changed || err != nil {
			t.Fatalf("git build: changed = %v, err = %v", changed, err)
		}
	})

	t.Run("an unreadable file is an error, not a write", func(t *testing.T) {
		root := t.TempDir()
		putUpdateStateFile(t, root, "{")
		if changed, _, err := MarkUpdateStateBooting(root, "nightly-20261024", updateStateTestFloor); changed || err == nil {
			t.Fatalf("changed = %v, err = %v", changed, err)
		}
	})
}

func TestMarkUpdateStateVerified(t *testing.T) {
	for _, runID := range []string{"r1", UpdateStateRunIDDeploy} {
		root := t.TempDir()
		if err := WriteUpdateState(root, UpdateState{Target: "nightly-20261024", From: "nightly-20261023", RunID: runID,
			Migrated: true, PreviousSafe: true}); err != nil {
			t.Fatal(err)
		}
		changed, err := MarkUpdateStateVerified(root, "nightly-20261024")
		if err != nil || !changed {
			t.Fatalf("%s: changed = %v, err = %v", runID, changed, err)
		}
		if st, _ := ReadUpdateState(root); !st.Verified || !st.Migrated || st.RunID != runID {
			t.Fatalf("%s: st = %+v", runID, st)
		}
		if changed, err := MarkUpdateStateVerified(root, "nightly-20261024"); changed || err != nil {
			t.Fatalf("second call: changed = %v, err = %v", changed, err)
		}
	}
	root := t.TempDir()
	if err := WriteUpdateState(root, UpdateState{Target: "nightly-20261024", From: "nightly-20261023", RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if changed, err := MarkUpdateStateVerified(root, "nightly-20261023"); changed || err != nil {
		t.Fatalf("rolled-back hub: changed = %v, err = %v", changed, err)
	}
	if st, _ := ReadUpdateState(root); st.Verified {
		t.Fatal("verified a target the hub does not run")
	}
}

func TestBinaryUpdate_FromPublishedAt(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addRelease("v0.3.0-preview.4", "2026-10-22T18:00:00Z", "v0.3.0-preview.4")
	gh.addRelease("v0.3.0-preview.5", "2026-10-25T18:00:00Z", "v0.3.0-preview.5")
	e := newTestExecutor(gh, &fakeInstaller{}, "v0.3.0-preview.4")
	hooked, _, err := runHooked(e, map[string]string{"target_version": "v0.3.0-preview.5"})
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC); !hooked.FromPublishedAt.Equal(want) {
		t.Fatalf("FromPublishedAt = %v, want %v", hooked.FromPublishedAt, want)
	}

	gh.addRelease("nightly-20261024", "2026-10-24T04:00:00Z", "nightly-20261024")
	e = newTestExecutor(gh, &fakeInstaller{}, "nightly-20261023")
	hooked, _, err = runHooked(e, map[string]string{"target_version": "nightly-20261024"})
	if err != nil || !hooked.FromPublishedAt.IsZero() {
		t.Fatalf("nightly from: err = %v, FromPublishedAt = %v", err, hooked.FromPublishedAt)
	}
}

func TestSyncDirToleratesMissingDir(t *testing.T) {
	syncDir(filepath.Join(t.TempDir(), "missing")) // must not panic
	syncDir(t.TempDir())
}
