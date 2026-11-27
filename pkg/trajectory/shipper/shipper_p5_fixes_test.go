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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type countingRetryableUploader struct {
	attemptedBytes int64
	calls          int
}

func (u *countingRetryableUploader) Ship(_ context.Context, seg Segment) (ShipResult, error) {
	u.attemptedBytes += seg.RawSize
	u.calls++
	return ShipResult{Outcome: OutcomeRetryable, Message: "transient hub error"}, errors.New("transient hub error")
}

func TestFailedShipAttemptsChargedToRoundBudget(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	line := strings.Repeat("x", 999) + "\n" // 1000 bytes
	for i := 0; i < 10; i++ {
		p := filepath.Join(projDir, fmt.Sprintf("sess-%02d.jsonl", i))
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	up := &countingRetryableUploader{}
	s, err := New(Config{
		HomeDir:       home,
		RunID:         "run-budget",
		MaxRoundBytes: 1000,
		Uploader:      up,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	for i := 0; i < 10; i++ {
		p := filepath.Join(projDir, fmt.Sprintf("sess-%02d.jsonl", i))
		if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	res, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if up.attemptedBytes > 1000 {
		t.Fatalf("attemptedBytes=%d exceeds MaxRoundBytes=1000 (calls=%d)", up.attemptedBytes, up.calls)
	}
	if up.calls != 1 {
		t.Fatalf("expected 1 Ship call against 1000-byte budget, got %d", up.calls)
	}
	if !res.BudgetExhausted {
		t.Fatalf("expected BudgetExhausted=true when failed attempt consumes budget")
	}
}

func TestFairnessCursorPersistedAcrossFreshShipperInstances(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pathA := filepath.Join(projDir, "a.jsonl")
	pathB := filepath.Join(projDir, "b.jsonl")
	if err := os.WriteFile(pathA, nil, 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(pathB, nil, 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	baseShipper, err := New(Config{HomeDir: home, RunID: "run-fair"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := baseShipper.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	// Append 1000 bytes to b once, and 2000 bytes to a every round, with a 1000-byte round budget.
	chunk1000 := strings.Repeat("b", 999) + "\n"
	chunk2000 := strings.Repeat("a", 999) + "\n" + strings.Repeat("a", 999) + "\n"
	if err := os.WriteFile(pathB, []byte(chunk1000), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	shippedByPath := make(map[string]int64)
	up := uploaderFunc(func(_ context.Context, seg Segment) (ShipResult, error) {
		shippedByPath[seg.NativeRelativePath] += seg.RawSize
		return ShipResult{Outcome: OutcomeCommitted, SegmentID: "seg-ok"}, nil
	})

	for round := 0; round < 3; round++ {
		f, err := os.OpenFile(pathA, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatalf("open a: %v", err)
		}
		_, _ = f.WriteString(chunk2000)
		_ = f.Close()

		// Fresh Shipper instance every round (simulating short-lived hook/harvest processes).
		s, err := New(Config{
			HomeDir:       home,
			RunID:         "run-fair",
			MaxRoundBytes: 1000,
			Uploader:      up,
		})
		if err != nil {
			t.Fatalf("New round %d: %v", round, err)
		}
		if _, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic}); err != nil {
			t.Fatalf("RunRound %d: %v", round, err)
		}
	}

	relB := ".claude/projects/-workspace/b.jsonl"
	if shippedByPath[relB] != 1000 {
		t.Fatalf("expected b.jsonl to ship 1000 bytes within 3 rounds across fresh Shipper instances, got %v", shippedByPath)
	}

	st, ok, err := LoadState(home)
	if err != nil || !ok {
		t.Fatalf("LoadState: ok=%v err=%v", ok, err)
	}
	if st.NextStartPath == "" {
		t.Fatalf("expected NextStartPath to be persisted in state.json")
	}
}

func TestPolicyRefusalAndRebaseline(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rel := ".claude/projects/-workspace/sess-policy.jsonl"
	artPath := filepath.Join(home, filepath.FromSlash(rel))

	line1 := "{\"seq\":1}\n"
	if err := os.WriteFile(artPath, []byte(line1), 0o644); err != nil {
		t.Fatalf("write line1: %v", err)
	}

	var shipped []string
	policyDenied := true
	up := uploaderFunc(func(_ context.Context, seg Segment) (ShipResult, error) {
		if policyDenied {
			return ShipResult{Outcome: OutcomePolicyRefused, Message: "capture_disabled"}, nil
		}
		shipped = append(shipped, fmt.Sprintf("%d:%d", seg.ByteStart, seg.ByteEnd))
		return ShipResult{Outcome: OutcomeCommitted, SegmentID: "seg-1"}, nil
	})

	s, err := New(Config{HomeDir: home, RunID: "run-policy", Uploader: up})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	// Append line2 while capture is disabled by policy.
	line2 := "{\"seq\":2,\"while_disabled\":true}\n"
	f, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append line2: %v", err)
	}
	_, _ = f.WriteString(line2)
	_ = f.Close()

	res, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if !res.PolicyRefused || res.PolicyRefusedReason != "capture_disabled" {
		t.Fatalf("expected PolicyRefused=true reason=capture_disabled, got %+v", res)
	}

	// Rebaseline moves offsets to current EOF, clears pending, and records reason.
	reb, err := s.Rebaseline(context.Background(), res.PolicyRefusedReason)
	if err != nil {
		t.Fatalf("Rebaseline: %v", err)
	}
	if reb.ArtifactsRebased != 1 || reb.Reason != "capture_disabled" {
		t.Fatalf("unexpected RebaselineResult: %+v", reb)
	}

	st, ok, err := LoadState(home)
	if err != nil || !ok {
		t.Fatalf("LoadState: ok=%v err=%v", ok, err)
	}
	if st.LastRebaselineReason != "capture_disabled" || st.LastRebaselineAt == nil {
		t.Fatalf("expected LastRebaselineReason/At to be recorded, got %+v", st)
	}
	wantEOF := int64(len(line1) + len(line2))
	entry := st.Artifacts[rel]
	if entry.RawCommittedOffset != wantEOF || entry.ParsedOffset != wantEOF || entry.BaselineOffset != int64(len(line1)) || entry.Pending != nil {
		t.Fatalf("rebaselined entry offsets/pending inconsistent: %+v (wantEOF=%d, wantBase=%d)", entry, wantEOF, len(line1))
	}

	// Re-enable capture and append line3: only line3 must ship (no backlog from line2).
	policyDenied = false
	line3 := "{\"seq\":3,\"after_reenable\":true}\n"
	f2, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append line3: %v", err)
	}
	_, _ = f2.WriteString(line3)
	_ = f2.Close()

	res2, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound after re-enable: %v", err)
	}
	if res2.SegmentsShipped != 1 || res2.BytesShipped != int64(len(line3)) {
		t.Fatalf("expected only line3 (%d bytes) to ship, got %+v", len(line3), res2)
	}
	wantRange := fmt.Sprintf("%d:%d", wantEOF, wantEOF+int64(len(line3)))
	if len(shipped) != 1 || shipped[0] != wantRange {
		t.Fatalf("shipped=%v, want [%s]", shipped, wantRange)
	}
}

func TestHardLinkedArtifactRefusedWhenRoot(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := filepath.Join(home, "secret.txt")
	if err := os.WriteFile(outside, []byte("{\"secret\":true}\n"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	hardlinkRel := ".claude/projects/-workspace/linked.jsonl"
	hardlinkPath := filepath.Join(home, filepath.FromSlash(hardlinkRel))
	if err := os.Link(outside, hardlinkPath); err != nil {
		t.Fatalf("os.Link: %v", err)
	}

	origEUID := geteuidFn
	t.Cleanup(func() { geteuidFn = origEUID })

	// As root (euid == 0), openArtifactNoFollow must refuse Nlink > 1.
	geteuidFn = func() int { return 0 }
	f, _, err := openArtifactNoFollow(home, hardlinkRel)
	if f != nil {
		_ = f.Close()
	}
	if !errors.Is(err, ErrHardLinkRefused) {
		t.Fatalf("expected ErrHardLinkRefused as root, got %v", err)
	}

	// Non-root (euid != 0) is allowed to open the hard link.
	geteuidFn = func() int { return 1000 }
	f2, _, err := openArtifactNoFollow(home, hardlinkRel)
	if err != nil {
		t.Fatalf("expected non-root open to succeed, got %v", err)
	}
	_ = f2.Close()
}

type uploaderFunc func(ctx context.Context, seg Segment) (ShipResult, error)

func (f uploaderFunc) Ship(ctx context.Context, seg Segment) (ShipResult, error) {
	return f(ctx, seg)
}

type probeAndShipUploader struct {
	probeFn func(ctx context.Context, seg Segment) (ShipResult, error)
	shipFn  func(ctx context.Context, seg Segment) (ShipResult, error)
}

func (u *probeAndShipUploader) Probe(ctx context.Context, seg Segment) (ShipResult, error) {
	return u.probeFn(ctx, seg)
}

func (u *probeAndShipUploader) Ship(ctx context.Context, seg Segment) (ShipResult, error) {
	return u.shipFn(ctx, seg)
}

func TestRebaselineAfterCommitBumpsGenerationAndAlignsToLineBoundary(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rel := ".claude/projects/-workspace/sess-rebaseline-gen.jsonl"
	artPath := filepath.Join(home, filepath.FromSlash(rel))

	initLine := "{\"init\":0}\n"
	if err := os.WriteFile(artPath, []byte(initLine), 0o644); err != nil {
		t.Fatalf("write init: %v", err)
	}

	var shipped []Segment
	up := uploaderFunc(func(_ context.Context, seg Segment) (ShipResult, error) {
		shipped = append(shipped, seg)
		return ShipResult{Outcome: OutcomeCommitted, SegmentID: "seg-ok"}, nil
	})

	s, err := New(Config{HomeDir: home, RunID: "run-reb-gen", Uploader: up})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	// Commit [B, X) in generation 0.
	lineA := "{\"seq\":1}\n"
	f, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append A: %v", err)
	}
	_, _ = f.WriteString(lineA)
	_ = f.Close()

	res1, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
	if err != nil || res1.SegmentsShipped != 1 {
		t.Fatalf("RunRound 1: res=%+v err=%v", res1, err)
	}

	// Append bytes written while capture is off, ending in a partial line.
	offComplete := "{\"off\":true}\n"
	offPartial := "{\"partial"
	f2, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append off: %v", err)
	}
	_, _ = f2.WriteString(offComplete + offPartial)
	_ = f2.Close()

	if _, err := s.Rebaseline(context.Background(), "capture_unavailable"); err != nil {
		t.Fatalf("Rebaseline: %v", err)
	}

	st, ok, err := LoadState(home)
	if err != nil || !ok {
		t.Fatalf("LoadState: ok=%v err=%v", ok, err)
	}
	entry := st.Artifacts[rel]
	wantEOF := int64(len(initLine) + len(lineA) + len(offComplete) + len(offPartial))
	if entry.Generation != 1 || entry.LastGenerationReason != GenReasonRebaseline {
		t.Fatalf("expected Generation=1 reason=%q, got gen=%d reason=%q",
			GenReasonRebaseline, entry.Generation, entry.LastGenerationReason)
	}
	if entry.RawCommittedOffset != wantEOF || entry.ParsedOffset != wantEOF || !entry.SkipToNextNewline {
		t.Fatalf("expected EOF=%d with SkipToNextNewline=true, got raw=%d parsed=%d skip=%v",
			wantEOF, entry.RawCommittedOffset, entry.ParsedOffset, entry.SkipToNextNewline)
	}
	if entry.BaselineOffset != int64(len(initLine)) {
		t.Fatalf("expected original BaselineOffset=%d preserved, got %d", len(initLine), entry.BaselineOffset)
	}

	// Finish the partial line and append a new line, then ship.
	finishPartial := "\":1}\n"
	lineB := "{\"seq\":2}\n"
	f3, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append finish+B: %v", err)
	}
	_, _ = f3.WriteString(finishPartial + lineB)
	_ = f3.Close()

	res2, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
	if err != nil || res2.SegmentsShipped != 1 {
		t.Fatalf("RunRound 2: res=%+v err=%v", res2, err)
	}
	if len(shipped) != 2 {
		t.Fatalf("expected 2 shipped segments, got %d", len(shipped))
	}
	if shipped[0].Generation != 0 || shipped[1].Generation != 1 {
		t.Fatalf("expected gen 0 then gen 1, got %d then %d", shipped[0].Generation, shipped[1].Generation)
	}
	wantLineBStart := wantEOF + int64(len(finishPartial))
	if shipped[1].ByteStart != wantLineBStart || shipped[1].ByteEnd != wantLineBStart+int64(len(lineB)) {
		t.Fatalf("expected gen 1 segment [%d,%d) starting at line boundary after skipped partial, got [%d,%d)",
			wantLineBStart, wantLineBStart+int64(len(lineB)), shipped[1].ByteStart, shipped[1].ByteEnd)
	}
}

func TestRefusedPendingRebaselineProbesAndDropsBackoffBacklog(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rel := ".claude/projects/-workspace/sess-probe.jsonl"
	artPath := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.WriteFile(artPath, []byte("{\"init\":0}\n"), 0o644); err != nil {
		t.Fatalf("write init: %v", err)
	}

	probeCalls := 0
	var shipped []Segment
	up := &probeAndShipUploader{
		probeFn: func(_ context.Context, _ Segment) (ShipResult, error) {
			probeCalls++
			return ShipResult{Outcome: OutcomeCommitted, SegmentID: "probe-ok"}, nil
		},
		shipFn: func(_ context.Context, seg Segment) (ShipResult, error) {
			shipped = append(shipped, seg)
			return ShipResult{Outcome: OutcomeCommitted, SegmentID: "seg-ok"}, nil
		},
	}

	s1, err := New(Config{HomeDir: home, RunID: "run-1", Uploader: up})
	if err != nil {
		t.Fatalf("New 1: %v", err)
	}
	if _, err := s1.Rebaseline(context.Background(), "capture_disabled"); err != nil {
		t.Fatalf("Rebaseline: %v", err)
	}

	// Write bytes during the backoff window while RefusedPendingRebaseline=true.
	duringBackoff := "{\"during_backoff\":true}\n"
	f, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append backoff: %v", err)
	}
	_, _ = f.WriteString(duringBackoff)
	_ = f.Close()

	// Fresh Shipper (simulating restart) runs ResumeCatchup: must probe,
	// rebaseline past duringBackoff without calling Ship, and clear the flag.
	s2, err := New(Config{HomeDir: home, RunID: "run-2", Uploader: up})
	if err != nil {
		t.Fatalf("New 2: %v", err)
	}
	res, err := s2.ResumeCatchup(context.Background())
	if err != nil {
		t.Fatalf("ResumeCatchup: %v", err)
	}
	if probeCalls != 1 || len(shipped) != 0 || res.SegmentsShipped != 0 {
		t.Fatalf("expected 1 probe and 0 shipped bytes, got probeCalls=%d shipped=%d res=%+v", probeCalls, len(shipped), res)
	}
	st, ok, err := LoadState(home)
	if err != nil || !ok || st.RefusedPendingRebaseline {
		t.Fatalf("expected RefusedPendingRebaseline=false after accepted probe, got ok=%v flag=%v err=%v", ok, st.RefusedPendingRebaseline, err)
	}

	// Append post-acceptance bytes: next round must ship ONLY these bytes.
	afterAccept := "{\"after_accept\":true}\n"
	f2, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append afterAccept: %v", err)
	}
	_, _ = f2.WriteString(afterAccept)
	_ = f2.Close()

	res2, err := s2.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
	if err != nil || res2.SegmentsShipped != 1 || len(shipped) != 1 {
		t.Fatalf("expected 1 post-acceptance segment shipped, got res=%+v shipped=%d err=%v", res2, len(shipped), err)
	}
	if shipped[0].RawSize != int64(len(afterAccept)) {
		t.Fatalf("shipped RawSize=%d, want %d", shipped[0].RawSize, len(afterAccept))
	}
}

func TestRebaselineAfterRestartAtZeroBumpCreatesNewGeneration(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rel := ".claude/projects/-workspace/sess-trunc-reb.jsonl"
	artPath := filepath.Join(home, filepath.FromSlash(rel))

	// Baseline with 46-byte history.
	history := "{\"history\":\"0123456789abcdef0123456789ab\"}\n"
	if err := os.WriteFile(artPath, []byte(history), 0o644); err != nil {
		t.Fatalf("write history: %v", err)
	}

	var shipped []Segment
	up := &probeAndShipUploader{
		probeFn: func(_ context.Context, _ Segment) (ShipResult, error) {
			return ShipResult{Outcome: OutcomeCommitted, SegmentID: "probe-ok"}, nil
		},
		shipFn: func(_ context.Context, seg Segment) (ShipResult, error) {
			shipped = append(shipped, seg)
			return ShipResult{Outcome: OutcomeCommitted, SegmentID: "seg-ok"}, nil
		},
	}

	s, err := New(Config{HomeDir: home, RunID: "run-r1", Uploader: up})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	// Truncate below baseline and rewrite shorter line -> gen 1 starts at 0 and ships [0, 8).
	lineA := "{\"a\":1}\n"
	if err := os.WriteFile(artPath, []byte(lineA), 0o644); err != nil {
		t.Fatalf("truncate+rewrite: %v", err)
	}
	res1, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
	if err != nil || res1.GenerationBumps != 1 || res1.SegmentsShipped != 1 {
		t.Fatalf("RunRound after truncate: res=%+v err=%v", res1, err)
	}
	if shipped[0].Generation != 1 || shipped[0].ByteStart != 0 || shipped[0].ByteEnd != int64(len(lineA)) {
		t.Fatalf("unexpected gen 1 segment: %+v", shipped[0])
	}

	// Append while capture is off, then Rebaseline -> MUST bump to gen 2 (no hole in gen 1).
	lineOff := "{\"off\":1}\n"
	f, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append off: %v", err)
	}
	_, _ = f.WriteString(lineOff)
	_ = f.Close()

	if _, err := s.Rebaseline(context.Background(), "capture_unavailable"); err != nil {
		t.Fatalf("Rebaseline: %v", err)
	}
	st, ok, err := LoadState(home)
	if err != nil || !ok {
		t.Fatalf("LoadState: ok=%v err=%v", ok, err)
	}
	entry := st.Artifacts[rel]
	if entry.Generation != 2 || entry.LastGenerationReason != GenReasonRebaseline {
		t.Fatalf("expected gen=2 reason=%q after Rebaseline following gen-1 commit, got gen=%d reason=%q",
			GenReasonRebaseline, entry.Generation, entry.LastGenerationReason)
	}

	// Probe-accept path: append backoff line, run probe-accept round (rebaselines past it),
	// then append lineB and ship -> lands in gen 2 cleanly at line boundary!
	lineBackoff := "{\"backoff\":1}\n"
	f2, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append backoff: %v", err)
	}
	_, _ = f2.WriteString(lineBackoff)
	_ = f2.Close()

	if _, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonResumeCatchup}); err != nil {
		t.Fatalf("RunRound probe-accept: %v", err)
	}

	lineB := "{\"b\":2}\n"
	f3, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append B: %v", err)
	}
	_, _ = f3.WriteString(lineB)
	_ = f3.Close()

	res2, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
	if err != nil || res2.SegmentsShipped != 1 {
		t.Fatalf("RunRound after probe-accept: res=%+v err=%v", res2, err)
	}
	if len(shipped) != 2 {
		t.Fatalf("expected 2 shipped segments, got %d", len(shipped))
	}
	wantGen2Start := int64(len(lineA) + len(lineOff) + len(lineBackoff))
	if shipped[1].Generation != 2 || shipped[1].ByteStart != wantGen2Start || shipped[1].RawSize != int64(len(lineB)) {
		t.Fatalf("unexpected gen 2 segment: %+v (want gen=2 start=%d len=%d)", shipped[1], wantGen2Start, len(lineB))
	}
}

func TestProbeConflictClearsRefusalFlagAndAllowsNextRoundToShip(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rel := ".claude/projects/-workspace/sess-probe-409.jsonl"
	artPath := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.WriteFile(artPath, []byte("{\"init\":0}\n"), 0o644); err != nil {
		t.Fatalf("write init: %v", err)
	}

	var shipped []Segment
	up := &probeAndShipUploader{
		probeFn: func(_ context.Context, _ Segment) (ShipResult, error) {
			// 409 conflict on probe means Hub passed experiment/policy gates.
			return ShipResult{Outcome: OutcomeConflict, Message: "segment_conflict"}, errors.New("conflict")
		},
		shipFn: func(_ context.Context, seg Segment) (ShipResult, error) {
			shipped = append(shipped, seg)
			return ShipResult{Outcome: OutcomeCommitted, SegmentID: "seg-ok"}, nil
		},
	}

	s, err := New(Config{HomeDir: home, RunID: "run-o1", Uploader: up})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Rebaseline(context.Background(), "capture_disabled"); err != nil {
		t.Fatalf("Rebaseline: %v", err)
	}

	f, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append 1: %v", err)
	}
	_, _ = f.WriteString("{\"during_backoff\":1}\n")
	_ = f.Close()

	// Probe returns OutcomeConflict -> must rebaseline and clear RefusedPendingRebaseline.
	if _, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic}); err != nil {
		t.Fatalf("RunRound probe: %v", err)
	}
	st, ok, err := LoadState(home)
	if err != nil || !ok || st.RefusedPendingRebaseline {
		t.Fatalf("expected RefusedPendingRebaseline=false after probe conflict, got ok=%v flag=%v err=%v", ok, st.RefusedPendingRebaseline, err)
	}

	// Next round ships newly appended bytes without livelock.
	nextLine := "{\"after_probe_409\":2}\n"
	f2, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	_, _ = f2.WriteString(nextLine)
	_ = f2.Close()

	res, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
	if err != nil || res.SegmentsShipped != 1 || len(shipped) != 1 || shipped[0].RawSize != int64(len(nextLine)) {
		t.Fatalf("expected next round to ship %d bytes, got res=%+v shipped=%d err=%v", len(nextLine), res, len(shipped), err)
	}
	if shipped[0].Generation != 1 {
		t.Fatalf("expected probe 409 conflict to bump generation to 1, got %d", shipped[0].Generation)
	}
}

func TestFindLastNewlineOffset_CapAndContextCancellation(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rel := ".claude/projects/-workspace/sess-nonl.jsonl"
	artPath := filepath.Join(home, filepath.FromSlash(rel))

	// 1024-byte file with NO newline at all; MaxSegmentBytes capped at 256.
	content := strings.Repeat("a", 1024)
	if err := os.WriteFile(artPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := New(Config{
		HomeDir:         home,
		RunID:           "run-o3",
		MaxSegmentBytes: 256,
		MaxRoundBytes:   256,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Beyond the scan cap (256 bytes), Rebaseline falls back to EOF (1024).
	if _, err := s.Rebaseline(context.Background(), "capture_unavailable"); err != nil {
		t.Fatalf("Rebaseline: %v", err)
	}
	st, ok, err := LoadState(home)
	if err != nil || !ok {
		t.Fatalf("LoadState: ok=%v err=%v", ok, err)
	}
	if got := st.Artifacts[rel].RawCommittedOffset; got != 1024 {
		t.Fatalf("expected capped scan without newline to rebaseline to EOF=1024, got %d", got)
	}

	// Beyond the scan cap (256 bytes), findNextNewlineOffset advances by maxScanBytes and reports hitCap=true.
	f, err := os.Open(artPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	nextOff, done, hitCap, err := findNextNewlineOffset(context.Background(), f, 0, 1024, 256)
	if err != nil || !done || !hitCap || nextOff != 256 {
		t.Fatalf("expected capped forward scan to return (256, true, true, nil), got (%d, %v, %v, %v)", nextOff, done, hitCap, err)
	}

	// Cancelled context stops findNextNewlineOffset immediately.
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := findNextNewlineOffset(cancelCtx, f, 0, 1024, 256); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestSkippedRebaselineFollowedByLazyBaselineBumpsGeneration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 does not block root reads")
	}

	for _, mode := range []string{"plain_uploader", "prober_uploader", "delete_and_recreate"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			projDir := filepath.Join(home, ".claude", "projects", "-workspace")
			if err := os.MkdirAll(projDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			rel := ".claude/projects/-workspace/sess-skip.jsonl"
			artPath := filepath.Join(home, filepath.FromSlash(rel))
			if err := os.WriteFile(artPath, nil, 0o644); err != nil {
				t.Fatalf("write empty: %v", err)
			}

			var shipped []Segment
			var up Uploader
			if mode == "prober_uploader" {
				up = &probeAndShipUploader{
					probeFn: func(_ context.Context, _ Segment) (ShipResult, error) {
						return ShipResult{Outcome: OutcomeCommitted, SegmentID: "probe-ok"}, nil
					},
					shipFn: func(_ context.Context, seg Segment) (ShipResult, error) {
						shipped = append(shipped, seg)
						return ShipResult{Outcome: OutcomeCommitted, SegmentID: "seg-ok"}, nil
					},
				}
			} else {
				up = uploaderFunc(func(_ context.Context, seg Segment) (ShipResult, error) {
					shipped = append(shipped, seg)
					return ShipResult{Outcome: OutcomeCommitted, SegmentID: "seg-ok"}, nil
				})
			}

			s, err := New(Config{HomeDir: home, RunID: "run-skip", Uploader: up})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := s.Baseline(context.Background()); err != nil {
				t.Fatalf("Baseline: %v", err)
			}

			// 1. Commit [0, 12) in generation 0.
			line0 := "{\"gen0\":1}\n"
			if err := os.WriteFile(artPath, []byte(line0), 0o644); err != nil {
				t.Fatalf("write line0: %v", err)
			}
			r1, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
			if err != nil || r1.SegmentsShipped != 1 {
				t.Fatalf("RunRound 1: res=%+v err=%v", r1, err)
			}

			// 2. Make file unreadable during Rebaseline so markRebaselineSkipped runs.
			if err := os.Chmod(artPath, 0o000); err != nil {
				t.Fatalf("chmod 000: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(artPath, 0o644) })

			rebRes, err := s.Rebaseline(context.Background(), "capture_disabled")
			if err != nil {
				t.Fatalf("Rebaseline: %v", err)
			}
			if len(rebRes.SkippedPaths) != 1 {
				t.Fatalf("expected 1 skipped path during Rebaseline, got %+v", rebRes.SkippedPaths)
			}

			// 3. Restore file (or delete & recreate it), append bytes written while off, then ship.
			if err := os.Chmod(artPath, 0o644); err != nil {
				t.Fatalf("chmod 0644: %v", err)
			}
			if mode == "delete_and_recreate" {
				if err := os.Remove(artPath); err != nil {
					t.Fatalf("remove: %v", err)
				}
				if err := os.WriteFile(artPath, []byte("{\"recreated_while_off\":true}\n"), 0o644); err != nil {
					t.Fatalf("recreate: %v", err)
				}
			} else {
				f, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatalf("append off: %v", err)
				}
				_, _ = f.WriteString("{\"while_off\":true}\n")
				_ = f.Close()
			}

			// First round after restore completes lazy baseline (and for Prober,
			// clears RefusedPendingRebaseline once new bytes appear).
			if _, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic}); err != nil {
				t.Fatalf("RunRound after restore: %v", err)
			}

			if mode == "prober_uploader" {
				// Trigger probe-accept on a line written during backoff.
				fB, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatalf("append backoff: %v", err)
				}
				_, _ = fB.WriteString("{\"backoff\":true}\n")
				_ = fB.Close()
				if _, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonResumeCatchup}); err != nil {
					t.Fatalf("RunRound probe-accept: %v", err)
				}
			}

			// Append new post-recovery line and ship: MUST land in generation 1 (not gen 0).
			postLine := "{\"post_recovery\":2}\n"
			f2, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatalf("append postLine: %v", err)
			}
			_, _ = f2.WriteString(postLine)
			_ = f2.Close()

			rFinal, err := s.RunRound(context.Background(), RoundOptions{Reason: ReasonPeriodic})
			if err != nil || rFinal.SegmentsShipped != 1 {
				t.Fatalf("RunRound final: res=%+v err=%v", rFinal, err)
			}
			if len(shipped) != 2 {
				t.Fatalf("expected 2 shipped segments, got %d", len(shipped))
			}
			if shipped[0].Generation != 0 || shipped[1].Generation != 1 {
				t.Fatalf("expected gen 0 then gen 1 (no hole in gen 0), got gen %d then gen %d",
					shipped[0].Generation, shipped[1].Generation)
			}
		})
	}
}

func TestRebaselinePreservesArtifactExtraFields(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rel := ".claude/projects/-workspace/sess-extra.jsonl"
	artPath := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.WriteFile(artPath, []byte("{\"seq\":1}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := New(Config{HomeDir: home, RunID: "run-extra"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	// Attach a P7 normalizer unknown field in Extra via WithLockedState.
	err = WithLockedState(context.Background(), home, 0, func(st *State) (bool, error) {
		st.Artifacts[rel].Extra = map[string]json.RawMessage{
			"p7_normalizer_cursor": json.RawMessage(`"cursor-42"`),
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("WithLockedState: %v", err)
	}

	if _, err := s.Rebaseline(context.Background(), "capture_disabled"); err != nil {
		t.Fatalf("Rebaseline: %v", err)
	}

	st, ok, err := LoadState(home)
	if err != nil || !ok {
		t.Fatalf("LoadState: ok=%v err=%v", ok, err)
	}
	gotExtra := string(st.Artifacts[rel].Extra["p7_normalizer_cursor"])
	if gotExtra != `"cursor-42"` {
		t.Fatalf("expected ArtifactState.Extra to survive Rebaseline, got %q", gotExtra)
	}
}

func TestIdleSiblingAndRefusedFinalTailRebaselineContiguity(t *testing.T) {
	t.Run("idle sibling across refusal and probe-accept", func(t *testing.T) {
		home := t.TempDir()
		dir := filepath.Join(home, ".claude", "projects", "-workspace")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		relA := ".claude/projects/-workspace/a.jsonl"
		relB := ".claude/projects/-workspace/b.jsonl"
		pA := filepath.Join(home, filepath.FromSlash(relA))
		pB := filepath.Join(home, filepath.FromSlash(relB))
		if err := os.WriteFile(pA, []byte("{\"a0\":0}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pB, []byte("{\"b0\":0}\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		on := true
		type segKey struct {
			p string
			g int
		}
		committed := map[segKey][][2]int64{}
		up := &probeAndShipUploader{
			probeFn: func(_ context.Context, _ Segment) (ShipResult, error) {
				if !on {
					return ShipResult{Outcome: OutcomeCaptureUnavailable}, nil
				}
				return ShipResult{Outcome: OutcomeCommitted}, nil
			},
			shipFn: func(_ context.Context, seg Segment) (ShipResult, error) {
				if !on {
					return ShipResult{Outcome: OutcomeCaptureUnavailable}, nil
				}
				k := segKey{seg.NativeRelativePath, seg.Generation}
				committed[k] = append(committed[k], [2]int64{seg.ByteStart, seg.ByteEnd})
				return ShipResult{Outcome: OutcomeCommitted}, nil
			},
		}

		s, err := New(Config{HomeDir: home, RunID: "r", Uploader: up})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if _, err := s.Baseline(ctx); err != nil {
			t.Fatal(err)
		}

		appendStr := func(path, data string) {
			f, oerr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if oerr != nil {
				t.Fatal(oerr)
			}
			_, _ = f.WriteString(data)
			_ = f.Close()
		}

		// Both A and B commit in gen 0.
		appendStr(pA, "{\"a1\":1}\n")
		appendStr(pB, "{\"b1\":1}\n")
		if r, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic}); err != nil || r.SegmentsShipped != 2 {
			t.Fatalf("round1: %+v %v", r, err)
		}

		// Capture goes off; A grows while B stays idle -> 404 -> Rebaseline.
		on = false
		appendStr(pA, "{\"a2\":2}\n")
		if r, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic}); err != nil || !r.CaptureUnavailable {
			t.Fatalf("round2: %+v %v", r, err)
		}
		if _, err := s.Rebaseline(ctx, "capture_unavailable"); err != nil {
			t.Fatal(err)
		}

		// During backoff B grows; capture comes back -> probe-accept rebaseline.
		appendStr(pB, "{\"b_backoff\":1}\n")
		on = true
		if _, err := s.RunRound(ctx, RoundOptions{Reason: ReasonResumeCatchup}); err != nil {
			t.Fatal(err)
		}

		// B grows again after acceptance and ships.
		appendStr(pB, "{\"b_after\":1}\n")
		if _, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic}); err != nil {
			t.Fatal(err)
		}

		for k, rs := range committed {
			for i := 1; i < len(rs); i++ {
				if rs[i][0] != rs[i-1][1] {
					t.Fatalf("hole in %s gen %d: %v", k.p, k.g, rs)
				}
			}
		}
		if len(committed[segKey{relB, 0}]) != 1 || len(committed[segKey{relB, 1}]) != 1 {
			t.Fatalf("expected B to have 1 segment in gen 0 and 1 in gen 1, got %v", committed)
		}
	})

	t.Run("refused Final partial tail", func(t *testing.T) {
		home := t.TempDir()
		dir := filepath.Join(home, ".claude", "projects", "-workspace")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		rel := ".claude/projects/-workspace/tail.jsonl"
		p := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte("{\"t0\":0}\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		on := true
		committed := map[int][][2]int64{}
		up := &probeAndShipUploader{
			probeFn: func(_ context.Context, _ Segment) (ShipResult, error) {
				if !on {
					return ShipResult{Outcome: OutcomeCaptureUnavailable}, nil
				}
				return ShipResult{Outcome: OutcomeCommitted}, nil
			},
			shipFn: func(_ context.Context, seg Segment) (ShipResult, error) {
				if !on {
					return ShipResult{Outcome: OutcomeCaptureUnavailable}, nil
				}
				committed[seg.Generation] = append(committed[seg.Generation], [2]int64{seg.ByteStart, seg.ByteEnd})
				return ShipResult{Outcome: OutcomeCommitted}, nil
			},
		}

		s, err := New(Config{HomeDir: home, RunID: "r", Uploader: up})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if _, err := s.Baseline(ctx); err != nil {
			t.Fatal(err)
		}

		appendStr := func(data string) {
			f, oerr := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if oerr != nil {
				t.Fatal(oerr)
			}
			_, _ = f.WriteString(data)
			_ = f.Close()
		}

		// Commit line 1 in gen 0.
		appendStr("{\"t1\":1}\n")
		if r, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic}); err != nil || r.SegmentsShipped != 1 {
			t.Fatalf("round1: %+v %v", r, err)
		}

		// Append partial tail (no newline) and refuse on Final=true round -> Pending != nil -> Rebaseline MUST bump.
		on = false
		appendStr("{\"partial")
		if r, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPreStop, Final: true}); err != nil || !r.CaptureUnavailable {
			t.Fatalf("final round: %+v %v", r, err)
		}
		if _, err := s.Rebaseline(ctx, "capture_unavailable"); err != nil {
			t.Fatal(err)
		}

		// Finish partial line + backoff line, turn on -> probe-accept -> append post line -> ship.
		appendStr("\":1}\n{\"backoff\":1}\n")
		on = true
		if _, err := s.RunRound(ctx, RoundOptions{Reason: ReasonResumeCatchup}); err != nil {
			t.Fatal(err)
		}
		appendStr("{\"after\":2}\n")
		if r, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic}); err != nil || r.SegmentsShipped != 1 {
			t.Fatalf("round after: %+v %v", r, err)
		}

		for g, rs := range committed {
			for i := 1; i < len(rs); i++ {
				if rs[i][0] != rs[i-1][1] {
					t.Fatalf("hole in gen %d: %v", g, rs)
				}
			}
		}
		if len(committed[0]) != 1 || len(committed[1]) != 1 {
			t.Fatalf("expected 1 segment in gen 0 and 1 in gen 1, got %v", committed)
		}
	})
}

// TestCommittedFinalTailThenNoGrowthRebaselineContiguity pins R6-1: a
// committed Final=true incomplete tail followed by a zero-growth Rebaseline
// must never leave a byte hole in the same generation, for both Prober and
// non-Prober uploaders.
func TestCommittedFinalTailThenNoGrowthRebaselineContiguity(t *testing.T) {
	for _, withProber := range []bool{true, false} {
		name := "non_prober"
		if withProber {
			name = "prober"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			dir := filepath.Join(home, ".claude", "projects", "-workspace")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			rel := ".claude/projects/-workspace/a.jsonl"
			p := filepath.Join(home, filepath.FromSlash(rel))
			initLine := "{\"init\":0}\n"
			if err := os.WriteFile(p, []byte(initLine), 0o644); err != nil {
				t.Fatal(err)
			}

			committed := map[int][][2]int64{}
			var shippedPayloads []string
			shipFn := func(_ context.Context, seg Segment) (ShipResult, error) {
				committed[seg.Generation] = append(committed[seg.Generation], [2]int64{seg.ByteStart, seg.ByteEnd})
				gz, err := gzip.NewReader(bytes.NewReader(seg.Compressed))
				if err != nil {
					return ShipResult{}, err
				}
				raw, err := io.ReadAll(gz)
				_ = gz.Close()
				if err != nil {
					return ShipResult{}, err
				}
				shippedPayloads = append(shippedPayloads, string(raw))
				return ShipResult{Outcome: OutcomeCommitted, SegmentID: "ok"}, nil
			}

			var up Uploader
			if withProber {
				up = &probeAndShipUploader{
					probeFn: func(_ context.Context, _ Segment) (ShipResult, error) {
						return ShipResult{Outcome: OutcomeCommitted, SegmentID: "probe-ok"}, nil
					},
					shipFn: shipFn,
				}
			} else {
				up = uploaderFunc(shipFn)
			}

			s, err := New(Config{HomeDir: home, RunID: "run-r6-1", Uploader: up})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Baseline(ctx); err != nil {
				t.Fatal(err)
			}

			appendStr := func(data string) {
				f, oerr := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
				if oerr != nil {
					t.Fatal(oerr)
				}
				_, _ = f.WriteString(data)
				_ = f.Close()
			}

			// 1. Commit a Final=true incomplete tail in gen 0 (RawCommittedOffset lands mid-line).
			tail := "{\"a\":1}\n{\"partial\":"
			appendStr(tail)
			r1, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPreStop, Final: true})
			if err != nil || r1.SegmentsShipped != 1 {
				t.Fatalf("round1: res=%+v err=%v", r1, err)
			}

			// 2. Rebaseline with ZERO growth: file still ends mid-line at RawCommittedOffset.
			if _, err := s.Rebaseline(ctx, "capture_unavailable"); err != nil {
				t.Fatalf("Rebaseline: %v", err)
			}

			// 3. Complete the partial line and append a new line.
			appendStr("\"fin\"}\n")
			if withProber {
				appendStr("{\"backoff\":1}\n")
				// Probe-accept round rebaselines past backoff bytes and clears RefusedPendingRebaseline.
				if _, err := s.RunRound(ctx, RoundOptions{Reason: ReasonResumeCatchup}); err != nil {
					t.Fatalf("probe-accept round: %v", err)
				}
			}
			afterLine := "{\"c\":3}\n"
			appendStr(afterLine)

			r2, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic})
			if err != nil || r2.SegmentsShipped != 1 {
				t.Fatalf("round2: res=%+v err=%v", r2, err)
			}

			// Verify every generation is contiguous with no holes or overlaps,
			// and the second segment landed in gen 1 (not gen 0).
			for g, rs := range committed {
				for i := 1; i < len(rs); i++ {
					if rs[i][0] != rs[i-1][1] {
						t.Fatalf("byte hole/overlap in gen %d: %v", g, rs)
					}
				}
			}
			if len(committed[0]) != 1 || len(committed[1]) != 1 {
				t.Fatalf("expected 1 segment in gen 0 and 1 in gen 1, got %v", committed)
			}
			if shippedPayloads[1] != afterLine {
				t.Fatalf("expected gen 1 payload %q, got %q", afterLine, shippedPayloads[1])
			}
		})
	}
}

// TestNeedsBaselineRebaselinePreservesOriginalBaselineOffset pins R6-2:
// rebaselining an entry that was marked NeedsBaseline after an unreadable
// Rebaseline must never overwrite its original BaselineOffset/BaselineGuard*.
func TestNeedsBaselineRebaselinePreservesOriginalBaselineOffset(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 does not block root reads")
	}

	for _, mode := range []string{"probe_accept", "second_rebaseline"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			dir := filepath.Join(home, ".claude", "projects", "-workspace")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			rel := ".claude/projects/-workspace/art-r6-2.jsonl"
			p := filepath.Join(home, filepath.FromSlash(rel))
			preBase := "{\"tag\":\"PREBASE_SECRET_R6_2\"}\n"
			if err := os.WriteFile(p, []byte(preBase), 0o644); err != nil {
				t.Fatal(err)
			}

			var shippedPayloads []string
			var shippedStarts []int64
			up := &probeAndShipUploader{
				probeFn: func(_ context.Context, _ Segment) (ShipResult, error) {
					return ShipResult{Outcome: OutcomeCommitted, SegmentID: "probe-ok"}, nil
				},
				shipFn: func(_ context.Context, seg Segment) (ShipResult, error) {
					gz, err := gzip.NewReader(bytes.NewReader(seg.Compressed))
					if err != nil {
						return ShipResult{}, err
					}
					raw, err := io.ReadAll(gz)
					_ = gz.Close()
					if err != nil {
						return ShipResult{}, err
					}
					shippedPayloads = append(shippedPayloads, string(raw))
					shippedStarts = append(shippedStarts, seg.ByteStart)
					return ShipResult{Outcome: OutcomeCommitted, SegmentID: "ok"}, nil
				},
			}

			s, err := New(Config{HomeDir: home, RunID: "run-r6-2", Uploader: up})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Baseline(ctx); err != nil {
				t.Fatal(err)
			}

			// Commit initial bytes.
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString("{\"c\":1}\n")
			_ = f.Close()
			if r, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic}); err != nil || r.SegmentsShipped != 1 {
				t.Fatalf("round1: %+v %v", r, err)
			}

			// Make unreadable during Rebaseline so markRebaselineSkipped sets NeedsBaseline=true.
			if err := os.Chmod(p, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
			if _, err := s.Rebaseline(ctx, "capture_disabled"); err != nil {
				t.Fatalf("Rebaseline (unreadable): %v", err)
			}

			// Make readable and grow the file well past preBase.
			if err := os.Chmod(p, 0o644); err != nil {
				t.Fatal(err)
			}
			f2, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f2.WriteString("{\"while_off\":\"long_line_one\"}\n{\"while_off\":\"long_line_two\"}\n")
			_ = f2.Close()

			// Complete via probe-accept or a second Rebaseline while NeedsBaseline=true.
			if mode == "probe_accept" {
				// First round completes lazy baseline; append another line to trigger probe-accept.
				if _, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic}); err != nil {
					t.Fatalf("lazy baseline round: %v", err)
				}
				f3, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f3.WriteString("{\"trigger_probe\":true}\n")
				_ = f3.Close()
				if _, err := s.RunRound(ctx, RoundOptions{Reason: ReasonResumeCatchup}); err != nil {
					t.Fatalf("probe-accept round: %v", err)
				}
			} else {
				if _, err := s.Rebaseline(ctx, "capture_disabled"); err != nil {
					t.Fatalf("second Rebaseline: %v", err)
				}
				// Clear refusal via probe-accept so next round can ship.
				f3, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f3.WriteString("{\"trigger_probe\":true}\n")
				_ = f3.Close()
				if _, err := s.RunRound(ctx, RoundOptions{Reason: ReasonResumeCatchup}); err != nil {
					t.Fatalf("probe-accept round: %v", err)
				}
			}

			// Rewrite file as original pre-baseline prefix + short tail (shorter than rebaseline EOF).
			shortTail := "{\"s\":1}\n"
			if err := os.WriteFile(p, []byte(preBase+shortTail), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic}); err != nil {
				t.Fatalf("post-truncate round: %v", err)
			}

			for i, payload := range shippedPayloads {
				if shippedStarts[i] < int64(len(preBase)) || strings.Contains(payload, "PREBASE_SECRET_R6_2") {
					t.Fatalf("pre-baseline bytes leaked (start=%d baseLen=%d payload=%q)",
						shippedStarts[i], len(preBase), payload)
				}
			}
		})
	}
}

// TestRebaselineOffsetPreservedOnConflictAndPrefixPreservingInodeReplace pins O6-1:
// after a refusal + accept + commit, a 409 OutcomeConflict or a prefix-preserving
// inode replacement must restart at RebaselineOffset so refused-window bytes never ship.
func TestRebaselineOffsetPreservedOnConflictAndPrefixPreservingInodeReplace(t *testing.T) {
	for _, trigger := range []string{"conflict_409", "prefix_preserving_inode_replace"} {
		t.Run(trigger, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			dir := filepath.Join(home, ".claude", "projects", "-workspace")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			rel := ".claude/projects/-workspace/art-o6-1.jsonl"
			p := filepath.Join(home, filepath.FromSlash(rel))
			preBase := "{\"init\":0}\n"
			if err := os.WriteFile(p, []byte(preBase), 0o644); err != nil {
				t.Fatal(err)
			}

			returnConflictOnce := false
			var shippedPayloads []string
			up := &probeAndShipUploader{
				probeFn: func(_ context.Context, _ Segment) (ShipResult, error) {
					return ShipResult{Outcome: OutcomeCommitted, SegmentID: "probe-ok"}, nil
				},
				shipFn: func(_ context.Context, seg Segment) (ShipResult, error) {
					if returnConflictOnce {
						returnConflictOnce = false
						return ShipResult{Outcome: OutcomeConflict, Message: "segment_conflict"}, errors.New("conflict")
					}
					gz, err := gzip.NewReader(bytes.NewReader(seg.Compressed))
					if err != nil {
						return ShipResult{}, err
					}
					raw, err := io.ReadAll(gz)
					_ = gz.Close()
					if err != nil {
						return ShipResult{}, err
					}
					shippedPayloads = append(shippedPayloads, string(raw))
					return ShipResult{Outcome: OutcomeCommitted, SegmentID: "ok"}, nil
				},
			}

			s, err := New(Config{HomeDir: home, RunID: "run-o6-1", Uploader: up})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Baseline(ctx); err != nil {
				t.Fatal(err)
			}

			// Refusal window: write REFUSED_SECRET, Rebaseline, then probe-accept.
			refusedLine := "{\"secret\":\"REFUSED_SECRET_O6_1\"}\n"
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString(refusedLine)
			_ = f.Close()

			if _, err := s.Rebaseline(ctx, "capture_disabled"); err != nil {
				t.Fatalf("Rebaseline: %v", err)
			}
			// Append backoff line and probe-accept.
			backoffLine := "{\"secret\":\"REFUSED_BACKOFF_O6_1\"}\n"
			f2, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f2.WriteString(backoffLine)
			_ = f2.Close()
			if _, err := s.RunRound(ctx, RoundOptions{Reason: ReasonResumeCatchup}); err != nil {
				t.Fatalf("probe-accept: %v", err)
			}

			// Commit ok1 in the post-rebaseline generation.
			ok1 := "{\"ok\":1}\n"
			f3, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f3.WriteString(ok1)
			_ = f3.Close()
			if r, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic}); err != nil || r.SegmentsShipped != 1 {
				t.Fatalf("commit ok1: %+v %v", r, err)
			}

			// Trigger generation bump via 409 conflict or prefix-preserving inode replace.
			ok2 := "{\"ok\":2}\n"
			if trigger == "conflict_409" {
				f4, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f4.WriteString(ok2)
				_ = f4.Close()
				returnConflictOnce = true
				if _, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic}); err != nil {
					t.Fatalf("conflict round: %v", err)
				}
				// Next round ships in the bumped generation.
				if r, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic}); err != nil || r.SegmentsShipped == 0 {
					t.Fatalf("post-conflict round: %+v %v", r, err)
				}
			} else {
				tmp := p + ".swap"
				full := preBase + refusedLine + backoffLine + ok1 + ok2
				if err := os.WriteFile(tmp, []byte(full), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(tmp, p); err != nil {
					t.Fatal(err)
				}
				if r, err := s.RunRound(ctx, RoundOptions{Reason: ReasonPeriodic}); err != nil || r.SegmentsShipped == 0 {
					t.Fatalf("post-inode-replace round: %+v %v", r, err)
				}
			}

			for _, payload := range shippedPayloads {
				if strings.Contains(payload, "REFUSED_SECRET_O6_1") || strings.Contains(payload, "REFUSED_BACKOFF_O6_1") {
					t.Fatalf("refused-window bytes leaked after %s: %q", trigger, payload)
				}
			}
		})
	}
}
