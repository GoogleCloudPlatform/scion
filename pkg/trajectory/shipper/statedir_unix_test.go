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

package shipper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
	"golang.org/x/sys/unix"
)

// simulatedUIDFS tracks simulated per-path ownership across root (0) and
// non-root (homeUID) callers so cross-UID sharing and chown behavior can be
// verified deterministically without requiring real root privileges.
type simulatedUIDFS struct {
	mu           sync.Mutex
	euid         int
	owners       map[string]uint32
	chownedPaths map[string]uint32
}

func installSimulatedUIDFS(t *testing.T, homeDir string, homeUID uint32, initialEUID int) *simulatedUIDFS {
	t.Helper()
	sim := &simulatedUIDFS{
		euid: initialEUID,
		owners: map[string]uint32{
			homeDir: homeUID,
		},
		chownedPaths: make(map[string]uint32),
	}

	origEUID := geteuidFn
	origStat := statOwnerFn
	origFchown := fchownFD
	origRenameat := renameatFD

	geteuidFn = func() int {
		sim.mu.Lock()
		defer sim.mu.Unlock()
		return sim.euid
	}
	statOwnerFn = func(path string, _ *unix.Stat_t) (uint32, uint32) {
		sim.mu.Lock()
		defer sim.mu.Unlock()
		if uid, ok := sim.owners[path]; ok {
			return uid, uid
		}
		// Record the creating euid on first observation so later euid switches
		// cannot rewrite ownership of an un-chowned path.
		uid := uint32(sim.euid)
		sim.owners[path] = uid
		return uid, uid
	}
	fchownFD = func(_ int, uid, _ int, displayPath string) error {
		sim.mu.Lock()
		defer sim.mu.Unlock()
		sim.owners[displayPath] = uint32(uid)
		sim.chownedPaths[displayPath] = uint32(uid)
		return nil
	}
	renameatFD = func(olddirfd int, oldpath string, newdirfd int, newpath string, oldDisplay, newDisplay string) error {
		if err := unix.Renameat(olddirfd, oldpath, newdirfd, newpath); err != nil {
			return err
		}
		sim.mu.Lock()
		defer sim.mu.Unlock()
		if uid, ok := sim.owners[oldDisplay]; ok {
			sim.owners[newDisplay] = uid
			delete(sim.owners, oldDisplay)
		} else {
			sim.owners[newDisplay] = uint32(sim.euid)
		}
		return nil
	}

	t.Cleanup(func() {
		geteuidFn = origEUID
		statOwnerFn = origStat
		fchownFD = origFchown
		renameatFD = origRenameat
	})
	return sim
}

func (s *simulatedUIDFS) setEUID(uid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.euid = uid
}

func (s *simulatedUIDFS) setOwner(path string, uid uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners[path] = uid
}

func TestCrossUIDStateSharingRootAndHomeOwner(t *testing.T) {
	const homeUID uint32 = 1000

	t.Run("root creates state then home-owner runs WithLockedState and RunRound", func(t *testing.T) {
		home := t.TempDir()
		sim := installSimulatedUIDFS(t, home, homeUID, 0)

		rel := ".claude/projects/-workspace/sess-cross1.jsonl"
		artPath := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(artPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		line1 := "{\"seq\":1}\n"
		if err := os.WriteFile(artPath, []byte(line1), 0o644); err != nil {
			t.Fatalf("write line1: %v", err)
		}

		// Root (euid=0) initializes baseline and ships appended line2.
		up := &noopUploader{}
		sRoot, err := New(Config{HomeDir: home, RunID: "run-1", Uploader: up})
		if err != nil {
			t.Fatalf("New root: %v", err)
		}
		if _, err := sRoot.Baseline(context.Background()); err != nil {
			t.Fatalf("Baseline as root: %v", err)
		}
		line2 := "{\"seq\":2}\n"
		f, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatalf("open append: %v", err)
		}
		_, _ = f.WriteString(line2)
		_ = f.Close()

		r1, err := sRoot.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound as root: %v", err)
		}
		if r1.SegmentsShipped != 1 {
			t.Fatalf("root RunRound SegmentsShipped=%d, want 1", r1.SegmentsShipped)
		}

		// Verify chown was called with homeUID on every created directory/file.
		wantChowned := []string{
			filepath.Join(home, ".scion"),
			StateDir(home),
			LockPath(home),
		}
		for _, p := range wantChowned {
			if got, ok := sim.chownedPaths[p]; !ok || got != homeUID {
				t.Fatalf("expected %s to be chowned to %d, got ok=%v uid=%d", p, homeUID, ok, got)
			}
		}
		foundTmpChown := false
		for p, uid := range sim.chownedPaths {
			if strings.HasPrefix(filepath.Base(p), "state-") && strings.HasSuffix(p, ".tmp") && uid == homeUID {
				foundTmpChown = true
				break
			}
		}
		if !foundTmpChown {
			t.Fatalf("expected temp state file to be chowned to %d; chownedPaths=%v", homeUID, sim.chownedPaths)
		}

		// Switch to home owner (euid=1000): WithLockedState and RunRound must
		// succeed and see the exact watermark committed by root.
		sim.setEUID(int(homeUID))
		var seenOffset int64
		err = WithLockedState(context.Background(), home, 0, func(st *State) (bool, error) {
			seenOffset = st.Artifacts[rel].RawCommittedOffset
			st.Artifacts[rel].ParsedOffset = seenOffset
			return true, nil
		})
		if err != nil {
			t.Fatalf("WithLockedState as home owner: %v", err)
		}
		wantOffset := int64(len(line1) + len(line2))
		if seenOffset != wantOffset {
			t.Fatalf("home owner saw offset %d, want %d", seenOffset, wantOffset)
		}

		line3 := "{\"seq\":3}\n"
		f2, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatalf("open append 2: %v", err)
		}
		_, _ = f2.WriteString(line3)
		_ = f2.Close()

		sAgent, err := New(Config{HomeDir: home, RunID: "run-1", Uploader: up})
		if err != nil {
			t.Fatalf("New agent: %v", err)
		}
		r2, err := sAgent.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound as home owner: %v", err)
		}
		if r2.SegmentsShipped != 1 || r2.GenerationBumps != 0 {
			t.Fatalf("home owner RunRound unexpected: %+v", r2)
		}
	})

	t.Run("home-owner creates state then root runs RunRound", func(t *testing.T) {
		home := t.TempDir()
		sim := installSimulatedUIDFS(t, home, homeUID, int(homeUID))

		rel := ".claude/projects/-workspace/sess-cross2.jsonl"
		artPath := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(artPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		line1 := "{\"seq\":1}\n"
		if err := os.WriteFile(artPath, []byte(line1), 0o644); err != nil {
			t.Fatalf("write line1: %v", err)
		}

		// Home owner (euid=1000) baselines and ships line2.
		up := &noopUploader{}
		sAgent, err := New(Config{HomeDir: home, RunID: "run-1", Uploader: up})
		if err != nil {
			t.Fatalf("New agent: %v", err)
		}
		if _, err := sAgent.Baseline(context.Background()); err != nil {
			t.Fatalf("Baseline as home owner: %v", err)
		}
		line2 := "{\"seq\":2}\n"
		f, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatalf("open append: %v", err)
		}
		_, _ = f.WriteString(line2)
		_ = f.Close()
		if _, err := sAgent.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic}); err != nil {
			t.Fatalf("RunRound as home owner: %v", err)
		}

		// Switch to root (euid=0): RunRound must accept home-owner state, ship
		// line3, and keep state.json chowned to homeUID so home owner can read it back.
		sim.setEUID(0)
		line3 := "{\"seq\":3}\n"
		f2, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatalf("open append 2: %v", err)
		}
		_, _ = f2.WriteString(line3)
		_ = f2.Close()

		sRoot, err := New(Config{HomeDir: home, RunID: "run-1", Uploader: up})
		if err != nil {
			t.Fatalf("New root: %v", err)
		}
		rRoot, err := sRoot.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound as root: %v", err)
		}
		if rRoot.SegmentsShipped != 1 || rRoot.GenerationBumps != 0 {
			t.Fatalf("root RunRound unexpected: %+v", rRoot)
		}

		// Home owner can still read the state written by root.
		sim.setEUID(int(homeUID))
		st, ok, err := LoadState(home)
		if err != nil || !ok {
			t.Fatalf("LoadState as home owner after root write: ok=%v err=%v", ok, err)
		}
		wantOffset := int64(len(line1) + len(line2) + len(line3))
		if st.Artifacts[rel].RawCommittedOffset != wantOffset {
			t.Fatalf("RawCommittedOffset=%d, want %d", st.Artifacts[rel].RawCommittedOffset, wantOffset)
		}
	})

	t.Run("root creates lock once then home-owner locks immediately with no intervening root call", func(t *testing.T) {
		home := t.TempDir()
		sim := installSimulatedUIDFS(t, home, homeUID, 0)

		sRoot, err := New(Config{HomeDir: home, RunID: "run-1"})
		if err != nil {
			t.Fatalf("New root: %v", err)
		}
		// Baseline acquires state.lock once (creating it) and does not reopen it.
		if _, err := sRoot.Baseline(context.Background()); err != nil {
			t.Fatalf("Baseline as root: %v", err)
		}
		if got, ok := sim.chownedPaths[LockPath(home)]; !ok || got != homeUID {
			t.Fatalf("expected newly created lock %s to be chowned to %d right after root Baseline, got ok=%v uid=%d",
				LockPath(home), homeUID, ok, got)
		}
		if got := sim.owners[LockPath(home)]; got != homeUID {
			t.Fatalf("expected simulated owner of %s to be %d, got %d", LockPath(home), homeUID, got)
		}

		// Immediately switch to home owner with no second root lock open:
		// this independently pins the fchownFD on newly created state.lock.
		sim.setEUID(int(homeUID))
		called := false
		if err := WithLockedState(context.Background(), home, 0, func(st *State) (bool, error) {
			called = true
			return false, nil
		}); err != nil {
			t.Fatalf("WithLockedState as home owner right after root lock create: %v", err)
		}
		if !called {
			t.Fatalf("expected WithLockedState callback to run")
		}
	})

	t.Run("root-squashed filesystem chown EPERM returns ErrRootSquashChownRefused", func(t *testing.T) {
		home := t.TempDir()
		_ = installSimulatedUIDFS(t, home, homeUID, 0)
		fchownFD = func(_ int, _, _ int, _ string) error {
			return unix.EPERM
		}

		sRoot, err := New(Config{HomeDir: home, RunID: "run-1"})
		if err != nil {
			t.Fatalf("New root: %v", err)
		}
		_, err = sRoot.Baseline(context.Background())
		if !errors.Is(err, ErrRootSquashChownRefused) {
			t.Fatalf("expected ErrRootSquashChownRefused on root-squashed EPERM, got %v", err)
		}
		if !errors.Is(err, unix.EPERM) {
			t.Fatalf("expected underlying EPERM to remain wrapped, got %v", err)
		}
	})

	t.Run("root-squashed filesystem mkdirat EACCES returns ErrRootSquashChownRefused", func(t *testing.T) {
		home := t.TempDir()
		_ = installSimulatedUIDFS(t, home, homeUID, 0)
		origMkdirat := mkdiratFD
		t.Cleanup(func() { mkdiratFD = origMkdirat })
		mkdiratFD = func(_ int, _ string, _ uint32) error {
			return unix.EACCES
		}

		sRoot, err := New(Config{HomeDir: home, RunID: "run-1"})
		if err != nil {
			t.Fatalf("New root: %v", err)
		}
		_, err = sRoot.Baseline(context.Background())
		if !errors.Is(err, ErrRootSquashChownRefused) {
			t.Fatalf("expected ErrRootSquashChownRefused on root-squashed mkdirat EACCES, got %v", err)
		}
		if !errors.Is(err, unix.EACCES) {
			t.Fatalf("expected underlying EACCES to remain wrapped, got %v", err)
		}
	})
}

func TestOwnerUIDRuleTable(t *testing.T) {
	const (
		homeUID  uint32 = 1000
		otherUID uint32 = 2000
	)

	tests := []struct {
		name     string
		euid     int
		scionUID uint32
		wantErr  bool
	}{
		{
			name:     "root with agent-owned .scion accepted",
			euid:     0,
			scionUID: homeUID,
			wantErr:  false,
		},
		{
			name:     "root with third-UID .scion refused",
			euid:     0,
			scionUID: otherUID,
			wantErr:  true,
		},
		{
			name:     "non-root with own dirs accepted",
			euid:     int(homeUID),
			scionUID: homeUID,
			wantErr:  false,
		},
		{
			name:     "non-root with foreign-owned dir refused",
			euid:     int(homeUID),
			scionUID: otherUID,
			wantErr:  true,
		},
		{
			name:     "non-root with root-owned dir refused",
			euid:     int(homeUID),
			scionUID: 0,
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			scionDir := filepath.Join(home, ".scion")
			if err := os.MkdirAll(scionDir, 0o700); err != nil {
				t.Fatalf("mkdir .scion: %v", err)
			}

			sim := installSimulatedUIDFS(t, home, homeUID, tc.euid)
			sim.setOwner(scionDir, tc.scionUID)

			s, err := New(Config{HomeDir: home, RunID: "run-1"})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = s.Baseline(context.Background())
			if tc.wantErr {
				if !errors.Is(err, privatefs.ErrNotPrivate) {
					t.Fatalf("expected ErrNotPrivate, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
