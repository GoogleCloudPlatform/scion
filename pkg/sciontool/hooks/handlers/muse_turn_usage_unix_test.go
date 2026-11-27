//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// TestMuseTurnUsage_FileTypeHardening verifies that non-regular files (FIFOs,
// directories), symlinks, and hard-linked files at either the state path or
// lock path are refused without blocking, without writing through to any link
// target, with fail-closed skipped-step semantics (no panic), and with at most
// one warning per process, while plain regular files at either path are
// accepted as controls.
func TestMuseTurnUsage_FileTypeHardening(t *testing.T) {
	const (
		opTimeout       = 2 * time.Second
		sentinelContent = "sentinel-untouched-target"
	)

	type plantResult struct {
		checkPath     string // target or peer file that must remain byte-identical
		wantSentinel  string
		fifoToUnblock string // non-empty for FIFO cases so a timeout can unblock O_RDONLY
		wantModeMask  os.FileMode
		wantModeEq    os.FileMode
		plantedPath   string // path whose mode/type must remain unchanged when refused
	}

	cases := []struct {
		name      string
		target    string // "state" or "lock"
		wantAllow bool
		wantErrIs error // optional errors.Is expectation when refused
		plant     func(t *testing.T, base, statePath, lockPath string) plantResult
	}{
		{
			name:      "state/fifo",
			target:    "state",
			wantAllow: false,
			plant: func(t *testing.T, _, statePath, _ string) plantResult {
				if err := syscall.Mkfifo(statePath, 0o600); err != nil {
					t.Fatalf("Mkfifo(%s): %v", statePath, err)
				}
				return plantResult{
					fifoToUnblock: statePath,
					plantedPath:   statePath,
					wantModeMask:  os.ModeNamedPipe,
					wantModeEq:    os.ModeNamedPipe,
				}
			},
		},
		{
			name:      "lock/fifo",
			target:    "lock",
			wantAllow: false,
			plant: func(t *testing.T, _, _, lockPath string) plantResult {
				if err := syscall.Mkfifo(lockPath, 0o600); err != nil {
					t.Fatalf("Mkfifo(%s): %v", lockPath, err)
				}
				return plantResult{
					fifoToUnblock: lockPath,
					plantedPath:   lockPath,
					wantModeMask:  os.ModeNamedPipe,
					wantModeEq:    os.ModeNamedPipe,
				}
			},
		},
		{
			name:      "state/directory",
			target:    "state",
			wantAllow: false,
			plant: func(t *testing.T, _, statePath, _ string) plantResult {
				if err := os.Mkdir(statePath, 0o700); err != nil {
					t.Fatalf("Mkdir(%s): %v", statePath, err)
				}
				return plantResult{
					plantedPath:  statePath,
					wantModeMask: os.ModeDir,
					wantModeEq:   os.ModeDir,
				}
			},
		},
		{
			name:      "lock/directory",
			target:    "lock",
			wantAllow: false,
			plant: func(t *testing.T, _, _, lockPath string) plantResult {
				if err := os.Mkdir(lockPath, 0o700); err != nil {
					t.Fatalf("Mkdir(%s): %v", lockPath, err)
				}
				return plantResult{
					plantedPath:  lockPath,
					wantModeMask: os.ModeDir,
					wantModeEq:   os.ModeDir,
				}
			},
		},
		{
			name:      "state/symlink",
			target:    "state",
			wantAllow: false,
			wantErrIs: privatefs.ErrReparsePoint,
			plant: func(t *testing.T, base, statePath, _ string) plantResult {
				target := filepath.Join(base, "symlink-state-target.txt")
				if err := os.WriteFile(target, []byte(sentinelContent), 0o600); err != nil {
					t.Fatalf("WriteFile(%s): %v", target, err)
				}
				if err := os.Symlink(target, statePath); err != nil {
					t.Fatalf("Symlink(%s -> %s): %v", statePath, target, err)
				}
				return plantResult{
					checkPath:    target,
					wantSentinel: sentinelContent,
					plantedPath:  statePath,
					wantModeMask: os.ModeSymlink,
					wantModeEq:   os.ModeSymlink,
				}
			},
		},
		{
			name:      "lock/symlink",
			target:    "lock",
			wantAllow: false,
			wantErrIs: privatefs.ErrReparsePoint,
			plant: func(t *testing.T, base, _, lockPath string) plantResult {
				target := filepath.Join(base, "symlink-lock-target.txt")
				if err := os.WriteFile(target, []byte(sentinelContent), 0o600); err != nil {
					t.Fatalf("WriteFile(%s): %v", target, err)
				}
				if err := os.Symlink(target, lockPath); err != nil {
					t.Fatalf("Symlink(%s -> %s): %v", lockPath, target, err)
				}
				return plantResult{
					checkPath:    target,
					wantSentinel: sentinelContent,
					plantedPath:  lockPath,
					wantModeMask: os.ModeSymlink,
					wantModeEq:   os.ModeSymlink,
				}
			},
		},
		{
			name:      "state/hardlink",
			target:    "state",
			wantAllow: false,
			wantErrIs: privatefs.ErrHardLinked,
			plant: func(t *testing.T, base, statePath, _ string) plantResult {
				peer := filepath.Join(base, "hardlink-state-peer.json")
				const peerJSON = `{"order":["s1:t1"],"turns":{"s1:t1":{"input":10,"output":2}}}`
				if err := os.WriteFile(peer, []byte(peerJSON), 0o600); err != nil {
					t.Fatalf("WriteFile(%s): %v", peer, err)
				}
				if err := os.Link(peer, statePath); err != nil {
					t.Fatalf("Link(%s -> %s): %v", peer, statePath, err)
				}
				return plantResult{
					checkPath:    peer,
					wantSentinel: peerJSON,
				}
			},
		},
		{
			name:      "lock/hardlink",
			target:    "lock",
			wantAllow: false,
			wantErrIs: privatefs.ErrHardLinked,
			plant: func(t *testing.T, base, _, lockPath string) plantResult {
				peer := filepath.Join(base, "hardlink-lock-peer.txt")
				if err := os.WriteFile(peer, []byte(sentinelContent), 0o600); err != nil {
					t.Fatalf("WriteFile(%s): %v", peer, err)
				}
				if err := os.Link(peer, lockPath); err != nil {
					t.Fatalf("Link(%s -> %s): %v", peer, lockPath, err)
				}
				return plantResult{
					checkPath:    peer,
					wantSentinel: sentinelContent,
				}
			},
		},
		{
			name:      "state/regular-control",
			target:    "state",
			wantAllow: true,
			plant: func(t *testing.T, _, statePath, _ string) plantResult {
				if err := os.WriteFile(statePath, []byte(`{}`), 0o600); err != nil {
					t.Fatalf("WriteFile(%s): %v", statePath, err)
				}
				return plantResult{}
			},
		},
		{
			name:      "lock/regular-control",
			target:    "lock",
			wantAllow: true,
			plant: func(t *testing.T, _, _, lockPath string) plantResult {
				if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
					t.Fatalf("WriteFile(%s): %v", lockPath, err)
				}
				return plantResult{}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &museCaptureSlogHandler{}
			prevLogger := slog.Default()
			slog.SetDefault(slog.New(cap))
			t.Cleanup(func() { slog.SetDefault(prevLogger) })

			base := t.TempDir()
			dir := filepath.Join(base, ".scion")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(dir, "muse-turn-usage.json")
			lockPath := statePath + ".lock"
			t.Cleanup(func() { resetMuseTurnUsageWarned(statePath) })

			planted := tc.plant(t, base, statePath, lockPath)

			unblockFIFO := func() {
				if planted.fifoToUnblock == "" {
					return
				}
				if f, err := os.OpenFile(planted.fifoToUnblock, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = f.Close()
				}
			}

			if !tc.wantAllow {
				// 1. Direct applyMuseTurnUsageFile call must refuse within opTimeout.
				errCh := make(chan error, 1)
				go func() {
					_, err := applyMuseTurnUsageFile(statePath, "s1:t1", museTurnUsageTotals{Input: 100, Output: 20})
					errCh <- err
				}()
				var applyErr error
				select {
				case applyErr = <-errCh:
				case <-time.After(opTimeout):
					unblockFIFO()
					t.Fatalf("applyMuseTurnUsageFile blocked past %v", opTimeout)
				}
				if applyErr == nil {
					t.Fatalf("applyMuseTurnUsageFile succeeded, want refusal error")
				}
				if tc.wantErrIs != nil && !errors.Is(applyErr, tc.wantErrIs) {
					t.Fatalf("applyMuseTurnUsageFile err = %v, want errors.Is(_, %v)", applyErr, tc.wantErrIs)
				}

				// 2. Two normalizeMuseTurnUsage calls must fail closed (no panic,
				// cleared tokens) without blocking, and emit only one warning.
				for i, cum := range []int64{100, 250} {
					evCh := make(chan *hooks.Event, 1)
					go func(in int64) {
						h := &TelemetryHandler{
							MuseTurnUsageStatePath: statePath,
							museVersionProbe:       func(context.Context) string { return "1.4.25" },
						}
						evCh <- h.normalizeMuseTurnUsage(makeMuseModelEndEvent("s1", "t1", in, 20))
					}(cum)
					var got *hooks.Event
					select {
					case got = <-evCh:
					case <-time.After(opTimeout):
						unblockFIFO()
						t.Fatalf("normalizeMuseTurnUsage call %d blocked past %v", i+1, opTimeout)
					}
					if got == nil {
						t.Fatalf("call %d returned nil event", i+1)
					}
					if got.Data.HasInputTokens() || got.Data.HasOutputTokens() || got.Data.InputTokens != 0 || got.Data.OutputTokens != 0 {
						t.Fatalf("call %d did not fail closed: in=%d (has=%v) out=%d (has=%v)",
							i+1, got.Data.InputTokens, got.Data.HasInputTokens(), got.Data.OutputTokens, got.Data.HasOutputTokens())
					}
				}

				if warns := cap.warnings(); len(warns) != 1 {
					t.Fatalf("warnings across 2 refused calls = %d, want 1 (%+v)", len(warns), warns)
				}

				// 3. Link target / hardlink peer must be untouched, planted
				// non-regular entry must remain in place, and lock-path refusals
				// must not create a state file.
				if planted.checkPath != "" {
					raw, err := os.ReadFile(planted.checkPath)
					if err != nil || string(raw) != planted.wantSentinel {
						t.Fatalf("link target %s altered: got=%q err=%v, want %q", planted.checkPath, string(raw), err, planted.wantSentinel)
					}
				}
				if planted.plantedPath != "" {
					fi, err := os.Lstat(planted.plantedPath)
					if err != nil {
						t.Fatalf("Lstat(%s): %v", planted.plantedPath, err)
					}
					if fi.Mode()&planted.wantModeMask != planted.wantModeEq {
						t.Fatalf("planted %s mode = %v, want mask %v == %v", planted.plantedPath, fi.Mode(), planted.wantModeMask, planted.wantModeEq)
					}
				}
				if tc.target == "lock" {
					if _, err := os.Lstat(statePath); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("state file %s should not exist after lock refusal, lstat err = %v", statePath, err)
					}
				}
				return
			}

			// Control cases: plain regular file at state or lock path is accepted.
			steps := []struct {
				in, out         int64
				wantIn, wantOut int64
			}{
				{in: 200, out: 40, wantIn: 200, wantOut: 40},
				{in: 350, out: 90, wantIn: 150, wantOut: 50},
			}
			for i, step := range steps {
				evCh := make(chan *hooks.Event, 1)
				go func(in, out int64) {
					h := &TelemetryHandler{
						MuseTurnUsageStatePath: statePath,
						museVersionProbe:       func(context.Context) string { return "1.4.25" },
					}
					evCh <- h.normalizeMuseTurnUsage(makeMuseModelEndEvent("s1", "t1", in, out))
				}(step.in, step.out)
				var got *hooks.Event
				select {
				case got = <-evCh:
				case <-time.After(opTimeout):
					t.Fatalf("control call %d blocked past %v", i+1, opTimeout)
				}
				if !got.Data.HasInputTokens() || !got.Data.HasOutputTokens() ||
					got.Data.InputTokens != step.wantIn || got.Data.OutputTokens != step.wantOut {
					t.Fatalf("control call %d delta = (%d/has=%v, %d/has=%v), want (%d/true, %d/true)",
						i+1, got.Data.InputTokens, got.Data.HasInputTokens(), got.Data.OutputTokens, got.Data.HasOutputTokens(),
						step.wantIn, step.wantOut)
				}
			}
			if warns := cap.warnings(); len(warns) != 0 {
				t.Fatalf("control emitted %d warnings, want 0 (%+v)", len(warns), warns)
			}
			raw, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatalf("read updated state file: %v", err)
			}
			var st museTurnUsageState
			if err := json.Unmarshal(raw, &st); err != nil {
				t.Fatalf("unmarshal updated state file: %v", err)
			}
			if got := st.Turns["s1:t1"]; got.Input != 350 || got.Output != 90 {
				t.Fatalf("persisted state[s1:t1] = %+v, want {Input:350 Output:90}", got)
			}
		})
	}
}
