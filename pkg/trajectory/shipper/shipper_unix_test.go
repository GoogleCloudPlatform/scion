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

//go:build unix

package shipper_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/trajectory/shipper"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

func TestStateAndLockPrivatePermissionsUnix(t *testing.T) {
	home := t.TempDir()
	s, err := shipper.New(shipper.Config{
		HomeDir: home,
		Now:     fixedClock(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	dirInfo, err := os.Stat(shipper.StateDir(home))
	if err != nil {
		t.Fatalf("stat state dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("state dir mode = %04o, want no group/other access", perm)
	}

	stateInfo, err := os.Stat(shipper.StatePath(home))
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if perm := stateInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state.json mode = %04o, want 0600", perm)
	}

	lockInfo, err := os.Stat(shipper.LockPath(home))
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	if perm := lockInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("state.lock mode = %04o, want no group/other access", perm)
	}
}

func TestBaselineReportsSkippedUnreadableArtifactUnix(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 0000 does not block root reads")
	}
	home := t.TempDir()
	rel := ".claude/projects/-workspace/sess-unreadable.jsonl"
	abs := writeArtifact(t, home, rel, "{\"pre\":1}\n")
	if err := os.Chmod(abs, 0o000); err != nil {
		t.Fatalf("chmod 0000: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(abs, 0o644) })

	up := newFakeUploader()
	s, err := shipper.New(shipper.Config{HomeDir: home, Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bres, err := s.Baseline(context.Background())
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if len(bres.SkippedPaths) != 1 || bres.SkippedPaths[0].NativeRelativePath != rel {
		t.Fatalf("expected 1 skipped path %s, got %+v", rel, bres.SkippedPaths)
	}
	st, _, err := shipper.LoadState(home)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !st.Artifacts[rel].NeedsBaseline {
		t.Fatalf("expected NeedsBaseline=true on skipped artifact")
	}

	// Restore permissions: first RunRound lazily baselines without shipping pre-existing bytes.
	if err := os.Chmod(abs, 0o644); err != nil {
		t.Fatalf("chmod 0644: %v", err)
	}
	r, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if r.SegmentsShipped != 0 {
		t.Fatalf("expected 0 segments shipped after lazy baseline, got %+v", r)
	}
}

func TestDeferredBaselinePrefixesUnreadableDirsUnix(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 0000 does not block root reads")
	}

	t.Run("unreadable session dir at baseline baselines at EOF once readable", func(t *testing.T) {
		home := t.TempDir()
		rel := ".claude/projects/-w/old.jsonl"
		pre := "{\"PRE_BASELINE\":1}\n"
		abs := writeArtifact(t, home, rel, pre)
		sessDir := filepath.Dir(abs)

		if err := os.Chmod(sessDir, 0o000); err != nil {
			t.Fatalf("chmod 0000: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(sessDir, 0o755) })

		up := newFakeUploader()
		s, err := shipper.New(shipper.Config{HomeDir: home, RunID: "run-1", Uploader: up, Now: fixedClock()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := s.Baseline(context.Background()); err != nil {
			t.Fatalf("Baseline: %v", err)
		}

		// Directory becomes readable: old.jsonl must be lazily baselined at EOF, 0 bytes shipped.
		if err := os.Chmod(sessDir, 0o755); err != nil {
			t.Fatalf("chmod 0755: %v", err)
		}
		r1, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound 1: %v", err)
		}
		if r1.SegmentsShipped != 0 {
			t.Fatalf("pre-existing file in unreadable session dir must not ship: %+v", r1)
		}

		// Subsequent append + brand-new file created after recovery both ship normally.
		appendArtifact(t, abs, "{\"post\":2}\n")
		newRel := ".claude/projects/-w/new.jsonl"
		writeArtifact(t, home, newRel, "{\"fresh\":1}\n")

		r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound 2: %v", err)
		}
		if r2.SegmentsShipped != 2 {
			t.Fatalf("expected 2 segments (post append + new file), got %+v", r2)
		}
		if got := string(up.reassemble(t, rel, 0)); got != "{\"post\":2}\n" {
			t.Fatalf("old.jsonl reassembled=%q, want only post-baseline append", got)
		}
		if got := string(up.reassemble(t, newRel, 0)); got != "{\"fresh\":1}\n" {
			t.Fatalf("new.jsonl reassembled=%q, want full new file", got)
		}
	})

	t.Run("unreadable rule root at baseline baselines at EOF once readable", func(t *testing.T) {
		home := t.TempDir()
		rel := ".claude/projects/-w/old.jsonl"
		pre := "{\"PRE_BASELINE_ROOT\":1}\n"
		abs := writeArtifact(t, home, rel, pre)
		ruleRoot := filepath.Join(home, ".claude", "projects")

		if err := os.Chmod(ruleRoot, 0o000); err != nil {
			t.Fatalf("chmod 0000: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(ruleRoot, 0o755) })

		up := newFakeUploader()
		s, err := shipper.New(shipper.Config{HomeDir: home, RunID: "run-1", Uploader: up, Now: fixedClock()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := s.Baseline(context.Background()); err != nil {
			t.Fatalf("Baseline: %v", err)
		}

		if err := os.Chmod(ruleRoot, 0o755); err != nil {
			t.Fatalf("chmod 0755: %v", err)
		}
		r1, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound 1: %v", err)
		}
		if r1.SegmentsShipped != 0 {
			t.Fatalf("pre-existing file under unreadable rule root must not ship: %+v", r1)
		}
		appendArtifact(t, abs, "{\"post\":2}\n")
		r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound 2: %v", err)
		}
		if r2.SegmentsShipped != 1 || string(up.reassemble(t, rel, 0)) != "{\"post\":2}\n" {
			t.Fatalf("expected only post-baseline append, got r2=%+v", r2)
		}
	})

	t.Run("control: dir made unreadable only after baseline ships genuinely new files from 0", func(t *testing.T) {
		home := t.TempDir()
		sessDir := filepath.Join(home, ".claude", "projects", "-w")
		if err := os.MkdirAll(sessDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		up := newFakeUploader()
		s, err := shipper.New(shipper.Config{HomeDir: home, RunID: "run-1", Uploader: up, Now: fixedClock()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := s.Baseline(context.Background()); err != nil {
			t.Fatalf("Baseline: %v", err)
		}

		// Create a genuinely new file after baseline, make dir unreadable during
		// round 1, then restore permissions before round 2.
		rel := ".claude/projects/-w/created-after.jsonl"
		body := "{\"genuinely_new\":1}\n"
		writeArtifact(t, home, rel, body)

		if err := os.Chmod(sessDir, 0o000); err != nil {
			t.Fatalf("chmod 0000: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(sessDir, 0o755) })

		if _, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic}); err != nil {
			t.Fatalf("RunRound 1: %v", err)
		}

		if err := os.Chmod(sessDir, 0o755); err != nil {
			t.Fatalf("chmod 0755: %v", err)
		}
		r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound 2: %v", err)
		}
		if r2.SegmentsShipped != 1 || string(up.reassemble(t, rel, 0)) != body {
			t.Fatalf("genuinely new file in post-baseline unreadable dir must ship from 0: r2=%+v got=%q",
				r2, string(up.reassemble(t, rel, 0)))
		}
	})
}

func TestDeferredPrefixFileOpenFailurePreservesLazyBaselineUnix(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 0000 does not block root reads")
	}

	home := t.TempDir()
	rel := ".claude/projects/-w/old.jsonl"
	pre := "{\"PRE_BASELINE\":1}\n"
	abs := writeArtifact(t, home, rel, pre)
	sessDir := filepath.Dir(abs)

	// 1. Directory unreadable at baseline -> added to deferred_baseline_prefixes.
	if err := os.Chmod(sessDir, 0o000); err != nil {
		t.Fatalf("chmod dir 0000: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(sessDir, 0o755)
		_ = os.Chmod(abs, 0o644)
	})

	up := newFakeUploader()
	s, err := shipper.New(shipper.Config{HomeDir: home, RunID: "run-1", Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	// 2. Directory becomes listable, but old.jsonl is unreadable (chmod 0000).
	// Round 1 fails to open old.jsonl and prunes the deferred prefix, yet must
	// still record NeedsBaseline=true for old.jsonl.
	if err := os.Chmod(sessDir, 0o755); err != nil {
		t.Fatalf("chmod dir 0755: %v", err)
	}
	if err := os.Chmod(abs, 0o000); err != nil {
		t.Fatalf("chmod file 0000: %v", err)
	}
	r1, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 1: %v", err)
	}
	if len(r1.ArtifactErrors) != 1 {
		t.Fatalf("expected open error on round 1, got %+v", r1)
	}
	st1, _, err := shipper.LoadState(home)
	if err != nil {
		t.Fatalf("LoadState 1: %v", err)
	}
	if st1.Artifacts[rel] == nil || !st1.Artifacts[rel].NeedsBaseline {
		t.Fatalf("expected lazy-baseline entry preserved despite open failure: %+v", st1.Artifacts[rel])
	}

	// 3. old.jsonl becomes readable: round 2 lazily baselines at EOF; nothing from [0, EOF) ships.
	if err := os.Chmod(abs, 0o644); err != nil {
		t.Fatalf("chmod file 0644: %v", err)
	}
	r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 2: %v", err)
	}
	if r2.SegmentsShipped != 0 {
		t.Fatalf("pre-baseline bytes must not ship after delayed open: %+v", r2)
	}

	// 4. Control: genuinely new file created after prune starts at 0 and ships.
	newRel := ".claude/projects/-w/new.jsonl"
	newBody := "{\"genuinely_new\":1}\n"
	writeArtifact(t, home, newRel, newBody)
	r3, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 3: %v", err)
	}
	if r3.SegmentsShipped != 1 || string(up.reassemble(t, newRel, 0)) != newBody {
		t.Fatalf("new file after prune must ship from 0: r3=%+v got=%q", r3, string(up.reassemble(t, newRel, 0)))
	}
}

func TestStateDirAncestorSymlinksRefusedUnix(t *testing.T) {
	t.Run("symlinked .scion dir refused", func(t *testing.T) {
		home := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(home, ".scion")); err != nil {
			t.Fatalf("symlink .scion: %v", err)
		}

		s, err := shipper.New(shipper.Config{HomeDir: home, Now: fixedClock()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := s.Baseline(context.Background()); !errors.Is(err, privatefs.ErrReparsePoint) {
			t.Fatalf("Baseline: expected ErrReparsePoint, got %v", err)
		}
		if _, _, err := shipper.LoadState(home); !errors.Is(err, privatefs.ErrReparsePoint) {
			t.Fatalf("LoadState: expected ErrReparsePoint, got %v", err)
		}
		if err := shipper.SaveState(home, &shipper.State{Version: shipper.StateVersion}); !errors.Is(err, privatefs.ErrReparsePoint) {
			t.Fatalf("SaveState: expected ErrReparsePoint, got %v", err)
		}
		if err := shipper.WithLockedState(context.Background(), home, 0, func(_ *shipper.State) (bool, error) {
			return false, nil
		}); !errors.Is(err, privatefs.ErrReparsePoint) {
			t.Fatalf("WithLockedState: expected ErrReparsePoint, got %v", err)
		}
	})

	t.Run("symlinked trajectory-shipper dir refused", func(t *testing.T) {
		home := t.TempDir()
		outside := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
			t.Fatalf("mkdir .scion: %v", err)
		}
		if err := os.Symlink(outside, shipper.StateDir(home)); err != nil {
			t.Fatalf("symlink trajectory-shipper: %v", err)
		}

		s, err := shipper.New(shipper.Config{HomeDir: home, Now: fixedClock()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := s.Baseline(context.Background()); !errors.Is(err, privatefs.ErrReparsePoint) {
			t.Fatalf("Baseline: expected ErrReparsePoint, got %v", err)
		}
		if _, _, err := shipper.LoadState(home); !errors.Is(err, privatefs.ErrReparsePoint) {
			t.Fatalf("LoadState: expected ErrReparsePoint, got %v", err)
		}
		if err := shipper.SaveState(home, &shipper.State{Version: shipper.StateVersion}); !errors.Is(err, privatefs.ErrReparsePoint) {
			t.Fatalf("SaveState: expected ErrReparsePoint, got %v", err)
		}
	})
}
