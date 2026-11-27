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
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/trajectory/shipper"
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
