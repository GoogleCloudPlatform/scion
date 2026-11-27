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
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type smHubMode int

const (
	smModeAccept smHubMode = iota
	smMode404
	smMode403Disabled
	smModeRetryable
	smModeLostACK
	smModeConflict
	smModeRejected
	smModeRunCap
)

type smCommittedSeg struct {
	ByteStart int64
	ByteEnd   int64
	RawSHA256 string
	Raw       []byte
}

type smFakeHub struct {
	mode      smHubMode
	committed map[string]map[int]map[int64]smCommittedSeg // path -> gen -> byteStart -> seg
	onShip    func()                                      // optional hook invoked after Hub records commit (for mid-round crash snapshot)
}

func newSMFakeHub() *smFakeHub {
	return &smFakeHub{
		mode:      smModeAccept,
		committed: make(map[string]map[int]map[int64]smCommittedSeg),
	}
}

func (h *smFakeHub) decompressSegment(seg Segment) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(seg.Compressed))
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	return io.ReadAll(gz)
}

func (h *smFakeHub) Probe(_ context.Context, seg Segment) (ShipResult, error) {
	switch h.mode {
	case smMode404:
		return ShipResult{Outcome: OutcomeCaptureUnavailable, Message: "not_found"}, nil
	case smMode403Disabled:
		return ShipResult{Outcome: OutcomePolicyRefused, Message: "capture_disabled"}, nil
	case smModeRetryable:
		return ShipResult{Outcome: OutcomeRetryable, Message: "transient"}, errors.New("transient")
	case smModeConflict:
		return ShipResult{Outcome: OutcomeConflict, Message: "segment_conflict"}, errors.New("conflict")
	case smModeRejected:
		return ShipResult{Outcome: OutcomeRejected, Message: "invalid_segment"}, errors.New("rejected")
	case smModeRunCap:
		return ShipResult{Outcome: OutcomeRunCapExceeded, Message: "run_cap_exceeded"}, nil
	default:
		// Check real identity conflict on existing committed segments.
		if byGen, ok := h.committed[seg.NativeRelativePath]; ok {
			if byStart, ok := byGen[seg.Generation]; ok {
				if prev, exists := byStart[seg.ByteStart]; exists && (prev.RawSHA256 != seg.RawSHA256 || prev.ByteEnd != seg.ByteEnd) {
					return ShipResult{Outcome: OutcomeConflict, Message: "segment_conflict"}, errors.New("conflict")
				}
			}
		}
		return ShipResult{Outcome: OutcomeCommitted, SegmentID: "probe-ok"}, nil
	}
}

func (h *smFakeHub) Ship(_ context.Context, seg Segment) (ShipResult, error) {
	switch h.mode {
	case smMode404:
		return ShipResult{Outcome: OutcomeCaptureUnavailable, Message: "not_found"}, nil
	case smMode403Disabled:
		return ShipResult{Outcome: OutcomePolicyRefused, Message: "capture_disabled"}, nil
	case smModeRetryable:
		return ShipResult{Outcome: OutcomeRetryable, Message: "transient"}, errors.New("transient")
	case smModeConflict:
		return ShipResult{Outcome: OutcomeConflict, Message: "segment_conflict"}, errors.New("conflict")
	case smModeRejected:
		return ShipResult{Outcome: OutcomeRejected, Message: "invalid_segment"}, errors.New("rejected")
	case smModeRunCap:
		return ShipResult{Outcome: OutcomeRunCapExceeded, Message: "run_cap_exceeded"}, nil
	}

	raw, err := h.decompressSegment(seg)
	if err != nil {
		return ShipResult{Outcome: OutcomeRetryable, Message: err.Error()}, err
	}

	byGen := h.committed[seg.NativeRelativePath]
	if byGen == nil {
		byGen = make(map[int]map[int64]smCommittedSeg)
		h.committed[seg.NativeRelativePath] = byGen
	}
	byStart := byGen[seg.Generation]
	if byStart == nil {
		byStart = make(map[int64]smCommittedSeg)
		byGen[seg.Generation] = byStart
	}
	if prev, exists := byStart[seg.ByteStart]; exists {
		if prev.RawSHA256 == seg.RawSHA256 && prev.ByteEnd == seg.ByteEnd {
			if h.mode == smModeLostACK {
				return ShipResult{Outcome: OutcomeRetryable, Message: "lost_ack"}, errors.New("lost_ack")
			}
			return ShipResult{Outcome: OutcomeCommitted, SegmentID: "idempotent"}, nil
		}
		return ShipResult{Outcome: OutcomeConflict, Message: "segment_conflict"}, errors.New("conflict")
	}

	byStart[seg.ByteStart] = smCommittedSeg{
		ByteStart: seg.ByteStart,
		ByteEnd:   seg.ByteEnd,
		RawSHA256: seg.RawSHA256,
		Raw:       raw,
	}
	if h.onShip != nil {
		h.onShip()
	}
	if h.mode == smModeLostACK {
		return ShipResult{Outcome: OutcomeRetryable, Message: "lost_ack"}, errors.New("lost_ack")
	}
	return ShipResult{Outcome: OutcomeCommitted, SegmentID: "ok"}, nil
}

type smArtifactTracker struct {
	rel              string
	abs              string
	baselineBytes    []byte
	baselineIntact   bool
	baselineBrokenAt bool // true once baseline prefix has ever been modified/truncated
	unreadable       bool
	versions         [][]byte
	current          []byte
}

func (a *smArtifactTracker) recordVersion(data []byte) {
	cp := append([]byte(nil), data...)
	a.current = cp
	a.versions = append(a.versions, cp)
	if !bytes.HasPrefix(cp, a.baselineBytes) {
		a.baselineIntact = false
		a.baselineBrokenAt = true
	}
}

// TestShipperStateMachineInvariants drives randomized sequences of file
// mutations, upload outcomes, refusals, rebaselines, crashes (state snapshot
// restores), and restarts over 1..3 artifacts and verifies all 5 shipper
// invariants after every step.
//
// Set SCION_SHIPPER_SM_SEEDS and SCION_SHIPPER_SM_STEPS to run deeper sweeps.
func TestShipperStateMachineInvariants(t *testing.T) {
	// Pinned regression seeds (R6-1: 1332; R6-2: 236, 627, 977, 1064, 1423)
	// always run at 80 steps so known failure classes never regress.
	pinnedSeeds := []int64{236, 627, 977, 1064, 1332, 1423}
	for _, seed := range pinnedSeeds {
		seed := seed
		t.Run(fmt.Sprintf("pinned_%d", seed), func(t *testing.T) {
			t.Parallel()
			runStateMachineSeed(t, seed, 80)
		})
	}

	numSeeds := 250
	numSteps := 60
	if v := os.Getenv("SCION_SHIPPER_SM_SEEDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			numSeeds = n
		}
	}
	if v := os.Getenv("SCION_SHIPPER_SM_STEPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			numSteps = n
		}
	}

	for seed := 1; seed <= numSeeds; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			t.Parallel()
			runStateMachineSeed(t, int64(seed), numSteps)
		})
	}
}

func runStateMachineSeed(t *testing.T, seed int64, numSteps int) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	ctx := context.Background()
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	numArts := 1 + rng.Intn(3)
	arts := make([]*smArtifactTracker, numArts)
	for i := 0; i < numArts; i++ {
		rel := fmt.Sprintf(".claude/projects/-workspace/art-%d.jsonl", i)
		abs := filepath.Join(home, filepath.FromSlash(rel))
		initBytes := []byte(fmt.Sprintf("{\"tag\":\"PREBASE_TAG_%d\",\"seed\":%d}\n", i, seed))
		if err := os.WriteFile(abs, initBytes, 0o644); err != nil {
			t.Fatalf("write init %d: %v", i, err)
		}
		a := &smArtifactTracker{
			rel:            rel,
			abs:            abs,
			baselineBytes:  initBytes,
			baselineIntact: true,
		}
		a.recordVersion(initBytes)
		arts[i] = a
	}

	hub := newSMFakeHub()
	runSeq := 1
	newShipper := func() *Shipper {
		s, err := New(Config{
			HomeDir:         home,
			RunID:           fmt.Sprintf("run-%d", runSeq),
			MaxSegmentBytes: 256,
			MaxRoundBytes:   1024,
			Uploader:        hub,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return s
	}
	s := newShipper()
	if _, err := s.Baseline(ctx); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	var history []string
	// Track client-observed refusal ONLY: bytes are tagged REFUSED_TAG_ only
	// when the client has actually observed a refusal (after a round returned
	// CaptureUnavailable/PolicyRefused or after an explicit Rebaseline, which
	// persists RefusedPendingRebaseline=true) until a probe-accept clears
	// RefusedPendingRebaseline. Never inspect hub.mode directly — the client
	// cannot know about a Hub mode flip that no round observed.
	isRefusalActive := func() bool {
		st, ok, err := LoadState(home)
		return err == nil && ok && st.RefusedPendingRebaseline
	}

	for step := 0; step < numSteps; step++ {
		art := arts[rng.Intn(numArts)]
		action := rng.Intn(12)

		switch action {
		case 0, 1: // Append complete JSONL line
			if art.unreadable {
				continue
			}
			tag := fmt.Sprintf("OK_%d", step)
			if isRefusalActive() {
				tag = fmt.Sprintf("REFUSED_TAG_%d", step)
			}
			line := fmt.Sprintf("{\"step\":%d,\"tag\":\"%s\"}\n", step, tag)
			f, err := os.OpenFile(art.abs, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatalf("step %d append: %v", step, err)
			}
			_, _ = f.WriteString(line)
			_ = f.Close()
			art.recordVersion(append(append([]byte(nil), art.current...), []byte(line)...))
			history = append(history, fmt.Sprintf("step=%d append(%s, %q)", step, art.rel, tag))

		case 2: // Append partial line (no newline) or finish partial line
			if art.unreadable {
				continue
			}
			tag := fmt.Sprintf("PART_%d", step)
			if isRefusalActive() {
				tag = fmt.Sprintf("REFUSED_TAG_%d", step)
			}
			var chunk string
			if len(art.current) > 0 && art.current[len(art.current)-1] != '\n' {
				chunk = fmt.Sprintf(",\"fin\":\"%s\"}\n", tag)
			} else {
				chunk = fmt.Sprintf("{\"part\":\"%s\"", tag)
			}
			f, err := os.OpenFile(art.abs, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatalf("step %d partial: %v", step, err)
			}
			_, _ = f.WriteString(chunk)
			_ = f.Close()
			art.recordVersion(append(append([]byte(nil), art.current...), []byte(chunk)...))
			history = append(history, fmt.Sprintf("step=%d partial(%s, %q)", step, art.rel, tag))

		case 3: // Truncate & rewrite (50% preserve baseline prefix, 50% full rewrite)
			if art.unreadable {
				continue
			}
			tag := fmt.Sprintf("REWRITE_%d", step)
			if isRefusalActive() {
				tag = fmt.Sprintf("REFUSED_TAG_%d", step)
			}
			var next []byte
			if rng.Intn(2) == 0 && art.baselineIntact {
				next = append(append([]byte(nil), art.baselineBytes...), []byte(fmt.Sprintf("{\"rw\":%d,\"tag\":\"%s\"}\n", step, tag))...)
			} else {
				next = []byte(fmt.Sprintf("{\"trunc\":%d,\"tag\":\"%s\"}\n", step, tag))
			}
			if err := os.WriteFile(art.abs, next, 0o644); err != nil {
				t.Fatalf("step %d rewrite: %v", step, err)
			}
			art.recordVersion(next)
			history = append(history, fmt.Sprintf("step=%d rewrite(%s, intact=%v)", step, art.rel, art.baselineIntact))

		case 4: // Inode replace (write temp + rename)
			if art.unreadable {
				continue
			}
			tag := fmt.Sprintf("INODE_%d", step)
			if isRefusalActive() {
				tag = fmt.Sprintf("REFUSED_TAG_%d", step)
			}
			var next []byte
			if rng.Intn(2) == 0 && art.baselineIntact {
				next = append(append([]byte(nil), art.baselineBytes...), []byte(fmt.Sprintf("{\"ino\":%d,\"tag\":\"%s\"}\n", step, tag))...)
			} else {
				next = []byte(fmt.Sprintf("{\"new_ino\":%d,\"tag\":\"%s\"}\n", step, tag))
			}
			tmp := art.abs + ".swap"
			if err := os.WriteFile(tmp, next, 0o644); err != nil {
				t.Fatalf("step %d write swap: %v", step, err)
			}
			if err := os.Rename(tmp, art.abs); err != nil {
				t.Fatalf("step %d rename: %v", step, err)
			}
			art.recordVersion(next)
			history = append(history, fmt.Sprintf("step=%d inode_replace(%s)", step, art.rel))

		case 5: // Toggle unreadable / readable (non-root only)
			if os.Geteuid() == 0 {
				continue
			}
			if art.unreadable {
				_ = os.Chmod(art.abs, 0o644)
				art.unreadable = false
				history = append(history, fmt.Sprintf("step=%d chmod_readable(%s)", step, art.rel))
			} else {
				_ = os.Chmod(art.abs, 0o000)
				art.unreadable = true
				t.Cleanup(func() { _ = os.Chmod(art.abs, 0o644) })
				history = append(history, fmt.Sprintf("step=%d chmod_unreadable(%s)", step, art.rel))
			}

		case 6: // Switch Hub mode
			modes := []smHubMode{
				smModeAccept, smModeAccept, smModeAccept,
				smMode404, smMode403Disabled, smModeRetryable,
				smModeLostACK, smModeConflict, smModeRejected, smModeRunCap,
			}
			hub.mode = modes[rng.Intn(len(modes))]
			history = append(history, fmt.Sprintf("step=%d hub_mode=%d", step, hub.mode))

		case 7, 8: // RunRound (periodic / session_end / Final pre_stop)
			final := rng.Intn(4) == 0
			reason := ReasonPeriodic
			if final {
				reason = ReasonPreStop
			}
			res, err := s.RunRound(ctx, RoundOptions{Reason: reason, Final: final})
			if err != nil {
				t.Fatalf("step %d RunRound: %v\nhistory:\n%s", step, err, strings.Join(history, "\n"))
			}
			history = append(history, fmt.Sprintf("step=%d round(final=%v, shipped=%d, unavail=%v, policy=%v)",
				step, final, res.SegmentsShipped, res.CaptureUnavailable, res.PolicyRefused))
			if res.CaptureUnavailable {
				if _, rerr := s.Rebaseline(ctx, "capture_unavailable"); rerr != nil {
					t.Fatalf("step %d Rebaseline(404): %v", step, rerr)
				}
				history = append(history, fmt.Sprintf("step=%d rebaseline(404)", step))
			} else if res.PolicyRefused {
				if _, rerr := s.Rebaseline(ctx, res.PolicyRefusedReason); rerr != nil {
					t.Fatalf("step %d Rebaseline(403): %v", step, rerr)
				}
				history = append(history, fmt.Sprintf("step=%d rebaseline(403)", step))
			}

		case 9: // Explicit Rebaseline
			if _, err := s.Rebaseline(ctx, "capture_disabled"); err != nil {
				t.Fatalf("step %d explicit Rebaseline: %v", step, err)
			}
			history = append(history, fmt.Sprintf("step=%d explicit_rebaseline", step))

		case 10: // Simulate mid-round kill -9 after Hub commit (while Pending is on disk) + ResumeCatchup
			var midCrashSnap []byte
			hub.onShip = func() {
				if len(midCrashSnap) == 0 {
					if snap, err := os.ReadFile(StatePath(home)); err == nil {
						midCrashSnap = snap
					}
				}
			}
			res, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic})
			hub.onShip = nil
			if err != nil {
				t.Fatalf("step %d mid-crash RunRound: %v\nhistory:\n%s", step, err, strings.Join(history, "\n"))
			}
			if len(midCrashSnap) > 0 {
				// Restore the in-flight Pending state snapshot as if killed
				// right after the Hub committed but before SaveState cleared Pending.
				if err := os.WriteFile(StatePath(home), midCrashSnap, 0o600); err != nil {
					t.Fatalf("step %d restore mid-crash snapshot: %v", step, err)
				}
				s = newShipper()
				cres, cerr := s.ResumeCatchup(ctx)
				if cerr != nil {
					t.Fatalf("step %d crash ResumeCatchup: %v", step, cerr)
				}
				history = append(history, fmt.Sprintf("step=%d mid_crash_replay(shipped=%d)", step, cres.SegmentsShipped))
				if cres.CaptureUnavailable {
					_, _ = s.Rebaseline(ctx, "capture_unavailable")
				} else if cres.PolicyRefused {
					_, _ = s.Rebaseline(ctx, cres.PolicyRefusedReason)
				}
			} else {
				history = append(history, fmt.Sprintf("step=%d mid_crash_noop(unavail=%v, policy=%v)", step, res.CaptureUnavailable, res.PolicyRefused))
				if res.CaptureUnavailable {
					_, _ = s.Rebaseline(ctx, "capture_unavailable")
				} else if res.PolicyRefused {
					_, _ = s.Rebaseline(ctx, res.PolicyRefusedReason)
				}
			}

		case 11: // Simulated container restart (fresh Shipper, optionally new RunID)
			if rng.Intn(2) == 0 {
				runSeq++
			}
			s = newShipper()
			res, err := s.ResumeCatchup(ctx)
			if err != nil {
				t.Fatalf("step %d restart ResumeCatchup: %v", step, err)
			}
			history = append(history, fmt.Sprintf("step=%d restart(run=%d, shipped=%d)", step, runSeq, res.SegmentsShipped))
			if res.CaptureUnavailable {
				_, _ = s.Rebaseline(ctx, "capture_unavailable")
			} else if res.PolicyRefused {
				_, _ = s.Rebaseline(ctx, res.PolicyRefusedReason)
			}
		}

		// Assert all 5 invariants after every step.
		assertStateMachineInvariants(t, home, hub, arts, seed, step, history)
	}
}

func assertStateMachineInvariants(
	t *testing.T,
	home string,
	hub *smFakeHub,
	arts []*smArtifactTracker,
	seed int64,
	step int,
	history []string,
) {
	t.Helper()
	artByRel := make(map[string]*smArtifactTracker, len(arts))
	for _, a := range arts {
		artByRel[a.rel] = a
	}

	// Invariant (5): parsed_offset <= raw_committed_offset within a generation.
	st, ok, err := LoadState(home)
	if err != nil {
		t.Fatalf("seed=%d step=%d LoadState: %v\nhistory:\n%s", seed, step, err, strings.Join(history, "\n"))
	}
	if ok {
		for rel, entry := range st.Artifacts {
			if entry.ParsedOffset > entry.RawCommittedOffset {
				t.Fatalf("seed=%d step=%d INVARIANT 5 VIOLATION (%s): parsed_offset=%d > raw_committed_offset=%d\nhistory:\n%s",
					seed, step, rel, entry.ParsedOffset, entry.RawCommittedOffset, strings.Join(history, "\n"))
			}
		}
	}

	for rel, byGen := range hub.committed {
		art := artByRel[rel]
		for gen, byStart := range byGen {
			if len(byStart) == 0 {
				continue
			}
			segs := make([]smCommittedSeg, 0, len(byStart))
			for _, seg := range byStart {
				segs = append(segs, seg)
			}
			sort.Slice(segs, func(i, j int) bool {
				return segs[i].ByteStart < segs[j].ByteStart
			})

			// Invariant (1): contiguous with no hole or overlap.
			var reassembled bytes.Buffer
			for i, seg := range segs {
				if i > 0 && seg.ByteStart != segs[i-1].ByteEnd {
					t.Fatalf("seed=%d step=%d INVARIANT 1 VIOLATION (%s gen %d): hole/overlap prev=[%d,%d) cur=[%d,%d)\nhistory:\n%s",
						seed, step, rel, gen, segs[i-1].ByteStart, segs[i-1].ByteEnd, seg.ByteStart, seg.ByteEnd, strings.Join(history, "\n"))
				}
				reassembled.Write(seg.Raw)
			}

			firstStart := segs[0].ByteStart
			lastEnd := segs[len(segs)-1].ByteEnd
			gotBytes := reassembled.Bytes()

			// Invariant (2): reassembled generation equals a contiguous byte range
			// [firstStart:lastEnd] of some recorded version of the file.
			matchedVersion := false
			for _, v := range art.versions {
				if int64(len(v)) >= lastEnd && bytes.Equal(v[firstStart:lastEnd], gotBytes) {
					matchedVersion = true
					break
				}
			}
			if !matchedVersion {
				t.Fatalf("seed=%d step=%d INVARIANT 2 VIOLATION (%s gen %d [%d,%d)): reassembled bytes %q match no recorded version\nhistory:\n%s",
					seed, step, rel, gen, firstStart, lastEnd, string(gotBytes), strings.Join(history, "\n"))
			}

			// Invariant (3): no pre-baseline bytes ever ship while baseline prefix
			// remained intact.
			if !art.baselineBrokenAt {
				if firstStart < int64(len(art.baselineBytes)) || bytes.Contains(gotBytes, []byte("PREBASE_TAG_")) {
					t.Fatalf("seed=%d step=%d INVARIANT 3 VIOLATION (%s gen %d): pre-baseline bytes leaked (firstStart=%d baseLen=%d raw=%q)\nhistory:\n%s",
						seed, step, rel, gen, firstStart, len(art.baselineBytes), string(gotBytes), strings.Join(history, "\n"))
				}
			}

			// Invariant (4): nothing written while capture was refused ships.
			if bytes.Contains(gotBytes, []byte("REFUSED_TAG_")) {
				t.Fatalf("seed=%d step=%d INVARIANT 4 VIOLATION (%s gen %d): refused-window bytes leaked: %q\nhistory:\n%s",
					seed, step, rel, gen, string(gotBytes), strings.Join(history, "\n"))
			}
		}
	}
}
