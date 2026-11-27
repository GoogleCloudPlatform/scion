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

package shipper_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/trajectory/shipper"
)

// fakeHubKey uniquely identifies a committed segment on the fake Hub.
type fakeHubKey struct {
	Path       string
	Generation int
	ByteStart  int64
}

// fakeUploader simulates the Hub prepare -> PUT -> commit contract in memory.
type fakeUploader struct {
	mu          sync.Mutex
	committed   map[fakeHubKey]shipper.Segment
	attempts    []shipper.Segment
	onShip      func(ctx context.Context, seg shipper.Segment, attempt int) (shipper.ShipResult, error, bool)
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

func newFakeUploader() *fakeUploader {
	return &fakeUploader{
		committed: make(map[fakeHubKey]shipper.Segment),
	}
}

func (u *fakeUploader) Ship(ctx context.Context, seg shipper.Segment) (shipper.ShipResult, error) {
	cur := u.inFlight.Add(1)
	for {
		prev := u.maxInFlight.Load()
		if cur <= prev || u.maxInFlight.CompareAndSwap(prev, cur) {
			break
		}
	}
	defer u.inFlight.Add(-1)

	// Verify gzip and hashes match the wire contract on every upload.
	raw, err := gunzip(seg.Compressed)
	if err != nil {
		return shipper.ShipResult{}, fmt.Errorf("invalid gzip payload: %w", err)
	}
	if int64(len(raw)) != seg.RawSize || seg.ByteEnd-seg.ByteStart != seg.RawSize {
		return shipper.ShipResult{}, fmt.Errorf("raw size mismatch: got %d, want %d", len(raw), seg.RawSize)
	}
	rawSum := sha256.Sum256(raw)
	if hex.EncodeToString(rawSum[:]) != seg.RawSHA256 {
		return shipper.ShipResult{}, fmt.Errorf("raw_sha256 mismatch")
	}
	compSum := sha256.Sum256(seg.Compressed)
	if hex.EncodeToString(compSum[:]) != seg.CompressedSHA256 {
		return shipper.ShipResult{}, fmt.Errorf("compressed_sha256 mismatch")
	}
	if int64(len(seg.Compressed)) != seg.CompressedSize {
		return shipper.ShipResult{}, fmt.Errorf("compressed_size mismatch")
	}
	if seg.ContentEncoding != shipper.ContentEncodingGzip {
		return shipper.ShipResult{}, fmt.Errorf("unexpected content_encoding %q", seg.ContentEncoding)
	}

	u.mu.Lock()
	u.attempts = append(u.attempts, seg)
	attempt := len(u.attempts)
	hook := u.onShip
	u.mu.Unlock()

	if hook != nil {
		res, err, handled := hook(ctx, seg, attempt)
		if handled {
			return res, err
		}
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	key := fakeHubKey{
		Path:       seg.NativeRelativePath,
		Generation: seg.Generation,
		ByteStart:  seg.ByteStart,
	}
	if existing, ok := u.committed[key]; ok {
		if existing.RawSHA256 == seg.RawSHA256 {
			// Idempotent replay.
			return shipper.ShipResult{Outcome: shipper.OutcomeCommitted, SegmentID: "seg-replay"}, nil
		}
		return shipper.ShipResult{Outcome: shipper.OutcomeConflict, Message: "hash mismatch"}, nil
	}
	u.committed[key] = seg
	return shipper.ShipResult{Outcome: shipper.OutcomeCommitted, SegmentID: fmt.Sprintf("seg-%d", attempt)}, nil
}

// reassemble decompresses and concatenates all committed segments for (path, gen)
// in byte_start order.
func (u *fakeUploader) reassemble(t *testing.T, path string, gen int) []byte {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()

	var segs []shipper.Segment
	for k, s := range u.committed {
		if k.Path == path && k.Generation == gen {
			segs = append(segs, s)
		}
	}
	sort.Slice(segs, func(i, j int) bool {
		return segs[i].ByteStart < segs[j].ByteStart
	})

	var out bytes.Buffer
	var expectStart int64
	for _, s := range segs {
		if len(segs) > 0 && expectStart == 0 && s.ByteStart > 0 {
			// Baseline started at non-zero offset.
			expectStart = s.ByteStart
		}
		if s.ByteStart != expectStart {
			t.Fatalf("gap or overlap in segments for %s gen %d: got start %d, want %d", path, gen, s.ByteStart, expectStart)
		}
		raw, err := gunzip(s.Compressed)
		if err != nil {
			t.Fatalf("gunzip segment: %v", err)
		}
		out.Write(raw)
		expectStart = s.ByteEnd
	}
	return out.Bytes()
}

func gunzip(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(zr)
}

func writeArtifact(t *testing.T, homeDir, rel, content string) string {
	t.Helper()
	full := filepath.Join(homeDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	return full
}

func appendArtifact(t *testing.T, path, extra string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open append %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(extra); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

func fixedClock() func() time.Time {
	t0 := time.Date(2026, 11, 27, 6, 0, 0, 0, time.UTC)
	return func() time.Time { return t0 }
}

func TestValidateNativeRelativePath(t *testing.T) {
	spec := shipper.ClaudeHarnessSpec()
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name: "valid session",
			path: ".claude/projects/-workspace/sess-001.jsonl",
		},
		{
			name: "valid subagent",
			path: ".claude/projects/-workspace/sess-001/subagents/agent-a1.jsonl",
		},
		{
			name:    "empty",
			path:    "",
			wantErr: true,
		},
		{
			name:    "absolute",
			path:    "/.claude/projects/-workspace/sess-001.jsonl",
			wantErr: true,
		},
		{
			name:    "dotdot",
			path:    ".claude/projects/../escape.jsonl",
			wantErr: true,
		},
		{
			name:    "empty segment",
			path:    ".claude/projects//sess-001.jsonl",
			wantErr: true,
		},
		{
			name:    "backslash",
			path:    `.claude/projects\-workspace/sess-001.jsonl`,
			wantErr: true,
		},
		{
			name:    "nul byte",
			path:    ".claude/projects/-workspace/sess\x00.jsonl",
			wantErr: true,
		},
		{
			name:    "wrong root",
			path:    ".codex/sessions/sess-001.jsonl",
			wantErr: true,
		},
		{
			name:    "wrong suffix",
			path:    ".claude/projects/-workspace/sess-001.txt",
			wantErr: true,
		},
		{
			name:    "too long",
			path:    ".claude/projects/" + strings.Repeat("a", 1020) + ".jsonl",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, err := shipper.ValidateNativeRelativePath(tc.path, spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got kind=%q", tc.path, kind)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.path, err)
			}
			if kind != shipper.ArtifactKindJSONLAppend {
				t.Fatalf("got kind=%q, want %q", kind, shipper.ArtifactKindJSONLAppend)
			}
		})
	}
}

func TestBaselineSkipsPreExistingAndBackfillOption(t *testing.T) {
	tests := []struct {
		name             string
		backfill         bool
		wantInitialBytes int64
		wantAfterAppend  string
	}{
		{
			name:             "default baseline skips pre-existing history",
			backfill:         false,
			wantInitialBytes: 0,
			wantAfterAppend:  "{\"seq\":3,\"type\":\"assistant\"}\n",
		},
		{
			name:             "backfill_existing ships from offset 0",
			backfill:         true,
			wantInitialBytes: int64(len("{\"seq\":1,\"type\":\"user\"}\n{\"seq\":2,\"type\":\"assistant\"}\n")),
			wantAfterAppend:  "{\"seq\":1,\"type\":\"user\"}\n{\"seq\":2,\"type\":\"assistant\"}\n{\"seq\":3,\"type\":\"assistant\"}\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			rel := ".claude/projects/-workspace/sess-base.jsonl"
			preExisting := "{\"seq\":1,\"type\":\"user\"}\n{\"seq\":2,\"type\":\"assistant\"}\n"
			abs := writeArtifact(t, home, rel, preExisting)

			up := newFakeUploader()
			s, err := shipper.New(shipper.Config{
				HomeDir:          home,
				OriginHubID:      "hub-origin-1",
				BackfillExisting: tc.backfill,
				Uploader:         up,
				Now:              fixedClock(),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			bres, err := s.Baseline(context.Background())
			if err != nil {
				t.Fatalf("Baseline: %v", err)
			}
			if bres.AlreadyInitialized || bres.ArtifactsTracked != 1 {
				t.Fatalf("unexpected baseline result: %+v", bres)
			}

			// Second Baseline call is an idempotent no-op.
			bres2, err := s.Baseline(context.Background())
			if err != nil {
				t.Fatalf("second Baseline: %v", err)
			}
			if !bres2.AlreadyInitialized {
				t.Fatalf("expected AlreadyInitialized=true on second Baseline")
			}

			r1, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
			if err != nil {
				t.Fatalf("RunRound 1: %v", err)
			}
			if r1.BytesShipped != tc.wantInitialBytes {
				t.Fatalf("round 1 BytesShipped=%d, want %d", r1.BytesShipped, tc.wantInitialBytes)
			}

			// Append new line after baseline.
			appendArtifact(t, abs, "{\"seq\":3,\"type\":\"assistant\"}\n")
			r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
			if err != nil {
				t.Fatalf("RunRound 2: %v", err)
			}
			if r2.SegmentsShipped != 1 {
				t.Fatalf("round 2 SegmentsShipped=%d, want 1", r2.SegmentsShipped)
			}

			got := string(up.reassemble(t, rel, 0))
			if got != tc.wantAfterAppend {
				t.Fatalf("reassembled=%q, want %q", got, tc.wantAfterAppend)
			}

			// A brand-new file created after baseline starts at offset 0.
			newRel := ".claude/projects/-workspace/sess-new.jsonl"
			newBody := "{\"seq\":10,\"type\":\"user\"}\n"
			writeArtifact(t, home, newRel, newBody)

			r3, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
			if err != nil {
				t.Fatalf("RunRound 3: %v", err)
			}
			if r3.SegmentsShipped != 1 {
				t.Fatalf("round 3 SegmentsShipped=%d, want 1", r3.SegmentsShipped)
			}
			if gotNew := string(up.reassemble(t, newRel, 0)); gotNew != newBody {
				t.Fatalf("new file reassembled=%q, want %q", gotNew, newBody)
			}
		})
	}
}

func TestIncrementalAppendsPartialTailAndSegmentSplitting(t *testing.T) {
	home := t.TempDir()
	up := newFakeUploader()
	s, err := shipper.New(shipper.Config{
		HomeDir:         home,
		OriginHubID:     "hub-1",
		MaxSegmentBytes: 40, // small cap to exercise splitting
		Uploader:        up,
		Now:             fixedClock(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	rel := ".claude/projects/-workspace/sess-inc.jsonl"
	line1 := "{\"i\":1,\"msg\":\"alpha\"}\n" // 23 bytes
	line2 := "{\"i\":2,\"msg\":\"beta\"}\n"  // 22 bytes (combined 45 > 40 -> splits at newline)
	partial := "{\"i\":3,\"msg\":\"part"     // no newline
	abs := writeArtifact(t, home, rel, line1+line2+partial)

	// Non-final round ships line1 and line2 in separate segments, holds back partial.
	r1, err := s.RunRound(context.Background(), shipper.RoundOptions{Final: false, Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 1: %v", err)
	}
	if r1.SegmentsShipped != 2 {
		t.Fatalf("round 1 SegmentsShipped=%d, want 2", r1.SegmentsShipped)
	}
	if got := string(up.reassemble(t, rel, 0)); got != line1+line2 {
		t.Fatalf("after round 1 reassembled=%q, want %q", got, line1+line2)
	}

	// Oversized single line (> MaxSegmentBytes=40) without newline yet still splits at cap.
	overLine := strings.Repeat("x", 50)
	appendArtifact(t, abs, "ial\"}\n"+overLine)

	r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Final: false, Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 2: %v", err)
	}
	if r2.SegmentsShipped != 2 {
		t.Fatalf("round 2 SegmentsShipped=%d, want 2 (completed line3 + 40B cap chunk)", r2.SegmentsShipped)
	}

	// Final round ships the remaining 10-byte incomplete tail with incomplete_tail=true.
	r3, err := s.RunRound(context.Background(), shipper.RoundOptions{Final: true, Reason: shipper.ReasonPreStop})
	if err != nil {
		t.Fatalf("RunRound 3: %v", err)
	}
	if r3.SegmentsShipped != 1 {
		t.Fatalf("round 3 SegmentsShipped=%d, want 1", r3.SegmentsShipped)
	}
	lastSeg := up.attempts[len(up.attempts)-1]
	if !lastSeg.IncompleteTail {
		t.Fatalf("expected final tail segment IncompleteTail=true")
	}

	// Byte-for-byte reassembly check against source file.
	srcBytes, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	reassembled := up.reassemble(t, rel, 0)
	if !bytes.Equal(reassembled, srcBytes) {
		t.Fatalf("reassembled (%d bytes) != source (%d bytes)", len(reassembled), len(srcBytes))
	}
}

func TestGenerationBumpsOnTruncationGuardMismatchAndInodeChange(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(t *testing.T, abs string) string
		wantReason string
	}{
		{
			name: "truncation below committed offset",
			mutate: func(t *testing.T, abs string) string {
				content := "{\"gen\":1,\"k\":\"short\"}\n"
				if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
					t.Fatalf("truncate rewrite: %v", err)
				}
				return content
			},
			wantReason: shipper.GenReasonTruncated,
		},
		{
			name: "guard hash mismatch in-place same size and inode",
			mutate: func(t *testing.T, abs string) string {
				f, err := os.OpenFile(abs, os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("open rw: %v", err)
				}
				defer func() { _ = f.Close() }()
				// Overwrite bytes inside the committed region with same length (same inode, same size).
				replacement := "{\"seq\":1,\"payload\":\"MMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMMM\"}\n"
				if _, err := f.WriteAt([]byte(replacement), 0); err != nil {
					t.Fatalf("writeAt: %v", err)
				}
				if _, err := f.Seek(0, io.SeekEnd); err != nil {
					t.Fatalf("seek: %v", err)
				}
				extra := "{\"seq\":2,\"payload\":\"new\"}\n"
				if _, err := f.WriteString(extra); err != nil {
					t.Fatalf("append: %v", err)
				}
				data, err := os.ReadFile(abs)
				if err != nil {
					t.Fatalf("read back: %v", err)
				}
				return string(data)
			},
			wantReason: shipper.GenReasonGuardMismatch,
		},
		{
			name: "inode replacement via atomic rename with larger file",
			mutate: func(t *testing.T, abs string) string {
				content := "{\"seq\":1,\"payload\":\"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\"}\n{\"seq\":2,\"payload\":\"replaced-inode\"}\n"
				tmp := abs + ".new"
				if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
					t.Fatalf("write tmp: %v", err)
				}
				if err := os.Rename(tmp, abs); err != nil {
					t.Fatalf("rename: %v", err)
				}
				return content
			},
			wantReason: shipper.GenReasonInodeChanged,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			up := newFakeUploader()
			s, err := shipper.New(shipper.Config{
				HomeDir:  home,
				Uploader: up,
				Now:      fixedClock(),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := s.Baseline(context.Background()); err != nil {
				t.Fatalf("Baseline: %v", err)
			}

			rel := ".claude/projects/-workspace/sess-rot.jsonl"
			initial := "{\"seq\":1,\"payload\":\"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\"}\n"
			abs := writeArtifact(t, home, rel, initial)

			if _, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic}); err != nil {
				t.Fatalf("RunRound 1: %v", err)
			}

			wantGen1 := tc.mutate(t, abs)

			r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
			if err != nil {
				t.Fatalf("RunRound 2: %v", err)
			}
			if r2.GenerationBumps != 1 {
				t.Fatalf("GenerationBumps=%d, want 1", r2.GenerationBumps)
			}

			st, ok, err := shipper.LoadState(home)
			if err != nil || !ok {
				t.Fatalf("LoadState: ok=%v err=%v", ok, err)
			}
			entry := st.Artifacts[rel]
			if entry.Generation != 1 {
				t.Fatalf("Generation=%d, want 1", entry.Generation)
			}
			if entry.LastGenerationReason != tc.wantReason {
				t.Fatalf("LastGenerationReason=%q, want %q", entry.LastGenerationReason, tc.wantReason)
			}
			if got := string(up.reassemble(t, rel, 1)); got != wantGen1 {
				t.Fatalf("gen 1 reassembled=%q, want %q", got, wantGen1)
			}
			// Gen 0 remains intact and un-overwritten.
			if got0 := string(up.reassemble(t, rel, 0)); got0 != initial {
				t.Fatalf("gen 0 reassembled=%q, want %q", got0, initial)
			}
		})
	}
}

func TestCrashBetweenUploadAndCommitReplaysIdempotently(t *testing.T) {
	home := t.TempDir()
	up := newFakeUploader()

	// Attempt 1: Hub stores the segment (simulating PUT + commit succeeded on
	// Hub) but the ACK is lost in transit, returning a retryable error.
	up.onShip = func(ctx context.Context, seg shipper.Segment, attempt int) (shipper.ShipResult, error, bool) {
		if attempt == 1 {
			up.mu.Lock()
			up.committed[fakeHubKey{
				Path:       seg.NativeRelativePath,
				Generation: seg.Generation,
				ByteStart:  seg.ByteStart,
			}] = seg
			up.mu.Unlock()
			return shipper.ShipResult{Outcome: shipper.OutcomeRetryable, Message: "connection reset after commit"}, errors.New("eof"), true
		}
		return shipper.ShipResult{}, nil, false
	}

	s, err := shipper.New(shipper.Config{HomeDir: home, Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	rel := ".claude/projects/-workspace/sess-replay.jsonl"
	body := "{\"seq\":1,\"event\":\"turn\"}\n"
	writeArtifact(t, home, rel, body)

	r1, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 1: %v", err)
	}
	if r1.SegmentsShipped != 0 || len(r1.ArtifactErrors) != 1 {
		t.Fatalf("expected retryable failure on round 1, got %+v", r1)
	}
	st1, _, err := shipper.LoadState(home)
	if err != nil {
		t.Fatalf("LoadState 1: %v", err)
	}
	if st1.Artifacts[rel].RawCommittedOffset != 0 || st1.Artifacts[rel].ConsecutiveFailures != 1 {
		t.Fatalf("watermark must not advance on lost ACK: %+v", st1.Artifacts[rel])
	}

	// Attempt 2: replays the exact same segment; fakeUploader returns
	// idempotent OutcomeCommitted and watermark advances.
	r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 2: %v", err)
	}
	if r2.SegmentsShipped != 1 {
		t.Fatalf("round 2 SegmentsShipped=%d, want 1", r2.SegmentsShipped)
	}
	st2, _, err := shipper.LoadState(home)
	if err != nil {
		t.Fatalf("LoadState 2: %v", err)
	}
	if st2.Artifacts[rel].RawCommittedOffset != int64(len(body)) || st2.Artifacts[rel].ConsecutiveFailures != 0 {
		t.Fatalf("unexpected state after idempotent replay: %+v", st2.Artifacts[rel])
	}
	if got := string(up.reassemble(t, rel, 0)); got != body {
		t.Fatalf("reassembled=%q, want %q", got, body)
	}
}

func TestSimulatedKill9MidRoundAndResumeCatchup(t *testing.T) {
	home := t.TempDir()
	up := newFakeUploader()

	// Create two artifacts; simulate kill -9 by cancelling ctx during the
	// second artifact's Ship before it commits, leaving state un-advanced for
	// the second artifact while the first is already committed.
	ctx, cancel := context.WithCancel(context.Background())
	up.onShip = func(_ context.Context, seg shipper.Segment, attempt int) (shipper.ShipResult, error, bool) {
		if attempt == 2 {
			cancel()
			return shipper.ShipResult{Outcome: shipper.OutcomeRetryable}, context.Canceled, true
		}
		return shipper.ShipResult{}, nil, false
	}

	s, err := shipper.New(shipper.Config{HomeDir: home, Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	rel1 := ".claude/projects/-workspace/sess-a.jsonl"
	rel2 := ".claude/projects/-workspace/sess-b.jsonl"
	body1 := "{\"a\":1}\n"
	body2 := "{\"b\":1}\n"
	writeArtifact(t, home, rel1, body1)
	writeArtifact(t, home, rel2, body2)

	r1, _ := s.RunRound(ctx, shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if r1 == nil || r1.SegmentsShipped != 1 {
		t.Fatalf("expected 1 segment committed before simulated kill, got %+v", r1)
	}

	// New container starts up: ResumeCatchup ships the unshipped second file.
	up.onShip = nil
	s2, err := shipper.New(shipper.Config{HomeDir: home, Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New s2: %v", err)
	}
	r2, err := s2.ResumeCatchup(context.Background())
	if err != nil {
		t.Fatalf("ResumeCatchup: %v", err)
	}
	if r2.Reason != shipper.ReasonResumeCatchup || r2.SegmentsShipped != 1 {
		t.Fatalf("unexpected ResumeCatchup result: %+v", r2)
	}
	if got := string(up.reassemble(t, rel1, 0)); got != body1 {
		t.Fatalf("rel1=%q, want %q", got, body1)
	}
	if got := string(up.reassemble(t, rel2, 0)); got != body2 {
		t.Fatalf("rel2=%q, want %q", got, body2)
	}
}

func TestConflictCaptureUnavailableCapAndFaultIsolation(t *testing.T) {
	t.Run("conflict bumps generation and isolates sibling artifact", func(t *testing.T) {
		home := t.TempDir()
		up := newFakeUploader()
		relBad := ".claude/projects/-workspace/sess-a-conflict.jsonl"
		relGood := ".claude/projects/-workspace/sess-b-good.jsonl"

		up.onShip = func(_ context.Context, seg shipper.Segment, _ int) (shipper.ShipResult, error, bool) {
			if seg.NativeRelativePath == relBad && seg.Generation == 0 {
				return shipper.ShipResult{Outcome: shipper.OutcomeConflict, Message: "409 segment_conflict"}, nil, true
			}
			return shipper.ShipResult{}, nil, false
		}

		s, err := shipper.New(shipper.Config{HomeDir: home, Uploader: up, Now: fixedClock()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := s.Baseline(context.Background()); err != nil {
			t.Fatalf("Baseline: %v", err)
		}

		writeArtifact(t, home, relBad, "{\"bad\":1}\n")
		writeArtifact(t, home, relGood, "{\"good\":1}\n")

		r1, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound 1: %v", err)
		}
		if r1.GenerationBumps != 1 || r1.SegmentsShipped != 1 {
			t.Fatalf("expected 1 bump + 1 sibling shipped, got %+v", r1)
		}
		if got := string(up.reassemble(t, relGood, 0)); got != "{\"good\":1}\n" {
			t.Fatalf("sibling artifact was blocked: got %q", got)
		}

		// Next round ships relBad on generation 1.
		r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound 2: %v", err)
		}
		if r2.SegmentsShipped != 1 {
			t.Fatalf("round 2 SegmentsShipped=%d, want 1", r2.SegmentsShipped)
		}
		if got := string(up.reassemble(t, relBad, 1)); got != "{\"bad\":1}\n" {
			t.Fatalf("relBad gen 1=%q, want %q", got, "{\"bad\":1}\n")
		}
	})

	t.Run("capture unavailable stops round without advancing or bumping", func(t *testing.T) {
		home := t.TempDir()
		up := newFakeUploader()
		up.onShip = func(_ context.Context, _ shipper.Segment, _ int) (shipper.ShipResult, error, bool) {
			return shipper.ShipResult{Outcome: shipper.OutcomeCaptureUnavailable, Message: "404 experiment off"}, nil, true
		}

		s, err := shipper.New(shipper.Config{HomeDir: home, Uploader: up, Now: fixedClock()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := s.Baseline(context.Background()); err != nil {
			t.Fatalf("Baseline: %v", err)
		}
		rel1 := ".claude/projects/-workspace/sess-1.jsonl"
		rel2 := ".claude/projects/-workspace/sess-2.jsonl"
		writeArtifact(t, home, rel1, "{\"a\":1}\n")
		writeArtifact(t, home, rel2, "{\"b\":1}\n")

		r, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound: %v", err)
		}
		if !r.CaptureUnavailable || r.SegmentsShipped != 0 || r.GenerationBumps != 0 {
			t.Fatalf("unexpected result: %+v", r)
		}
		if len(up.attempts) != 1 {
			t.Fatalf("expected round to stop after first unavailable response, got %d attempts", len(up.attempts))
		}
	})

	t.Run("run cap exceeded stops current run and clears for next run", func(t *testing.T) {
		home := t.TempDir()
		up := newFakeUploader()
		up.onShip = func(_ context.Context, _ shipper.Segment, _ int) (shipper.ShipResult, error, bool) {
			return shipper.ShipResult{Outcome: shipper.OutcomeRunCapExceeded, Message: "413 run_cap_exceeded"}, nil, true
		}

		sA, err := shipper.New(shipper.Config{HomeDir: home, RunID: "run-A", Uploader: up, Now: fixedClock()})
		if err != nil {
			t.Fatalf("New sA: %v", err)
		}
		if _, err := sA.Baseline(context.Background()); err != nil {
			t.Fatalf("Baseline: %v", err)
		}
		rel := ".claude/projects/-workspace/sess-cap.jsonl"
		abs := writeArtifact(t, home, rel, "{\"a\":1}\n")

		r1, err := sA.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound 1: %v", err)
		}
		if !r1.Truncated || r1.SegmentsShipped != 0 {
			t.Fatalf("expected Truncated=true on round 1, got %+v", r1)
		}

		// Subsequent round for the SAME run must stay stopped without calling Uploader.
		r2, err := sA.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound 2: %v", err)
		}
		if !r2.Truncated || len(up.attempts) != 1 {
			t.Fatalf("expected same run to stay stopped: r2=%+v attempts=%d", r2, len(up.attempts))
		}

		st, _, err := shipper.LoadState(home)
		if err != nil {
			t.Fatalf("LoadState: %v", err)
		}
		if !st.Truncated || st.TruncatedRunID != "run-A" || !st.Artifacts[rel].Truncated {
			t.Fatalf("expected state truncated for run-A: %+v", st)
		}

		// A new run (run-B) clears run-A truncation and ships pending + new appends.
		up.onShip = nil
		appendArtifact(t, abs, "{\"b\":2}\n")
		sB, err := shipper.New(shipper.Config{HomeDir: home, RunID: "run-B", Uploader: up, Now: fixedClock()})
		if err != nil {
			t.Fatalf("New sB: %v", err)
		}
		r3, err := sB.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
		if err != nil {
			t.Fatalf("RunRound 3 (run-B): %v", err)
		}
		if r3.Truncated || r3.SegmentsShipped == 0 {
			t.Fatalf("expected run-B to resume shipping, got %+v", r3)
		}
		if got := string(up.reassemble(t, rel, 0)); got != "{\"a\":1}\n{\"b\":2}\n" {
			t.Fatalf("run-B reassembled=%q, want %q", got, "{\"a\":1}\n{\"b\":2}\n")
		}
	})
}

func TestConcurrentRunRoundSerializedByLock(t *testing.T) {
	home := t.TempDir()
	up := newFakeUploader()
	up.onShip = func(_ context.Context, _ shipper.Segment, _ int) (shipper.ShipResult, error, bool) {
		time.Sleep(25 * time.Millisecond)
		return shipper.ShipResult{}, nil, false
	}

	s1, err := shipper.New(shipper.Config{HomeDir: home, Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New s1: %v", err)
	}
	s2, err := shipper.New(shipper.Config{HomeDir: home, Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New s2: %v", err)
	}
	if _, err := s1.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	rel := ".claude/projects/-workspace/sess-conc.jsonl"
	writeArtifact(t, home, rel, "{\"k\":1}\n")

	var wg sync.WaitGroup
	var totalShipped atomic.Int32
	errs := make(chan error, 2)

	for _, sh := range []*shipper.Shipper{s1, s2} {
		wg.Add(1)
		go func(sh *shipper.Shipper) {
			defer wg.Done()
			r, err := sh.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
			if err != nil {
				errs <- err
				return
			}
			totalShipped.Add(int32(r.SegmentsShipped))
		}(sh)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent RunRound error: %v", err)
	}

	if got := up.maxInFlight.Load(); got != 1 {
		t.Fatalf("maxInFlight=%d, want 1 (lock must serialize concurrent rounds)", got)
	}
	if got := totalShipped.Load(); got != 1 {
		t.Fatalf("totalShipped=%d, want 1 (second round must see updated watermark)", got)
	}
}

func TestSubagentDiscoveryAndSymlinkEscapeRefusal(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()

	// Valid parent session + subagent transcript.
	parentRel := ".claude/projects/-workspace/sess-main.jsonl"
	subRel := ".claude/projects/-workspace/sess-main/subagents/agent-sub1.jsonl"
	writeArtifact(t, home, parentRel, "{\"role\":\"user\"}\n")
	writeArtifact(t, home, subRel, "{\"role\":\"assistant\"}\n")

	// Outside secret file + symlink escapes (file, dir, and dangling).
	outsideFile := filepath.Join(outside, "secret.jsonl")
	if err := os.WriteFile(outsideFile, []byte("{\"secret\":true}\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	escapeFileLink := filepath.Join(home, ".claude", "projects", "-workspace", "escape.jsonl")
	if err := os.Symlink(outsideFile, escapeFileLink); err != nil {
		t.Fatalf("symlink file: %v", err)
	}
	escapeDirLink := filepath.Join(home, ".claude", "projects", "escape-dir")
	if err := os.Symlink(outside, escapeDirLink); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}
	danglingLink := filepath.Join(home, ".claude", "projects", "-workspace", "dangling.jsonl")
	if err := os.Symlink(filepath.Join(home, "does-not-exist.jsonl"), danglingLink); err != nil {
		t.Fatalf("symlink dangling: %v", err)
	}

	disc, err := shipper.Discover(home, shipper.ClaudeHarnessSpec())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(disc.Artifacts) != 2 {
		t.Fatalf("discovered %d artifacts, want 2: %+v", len(disc.Artifacts), disc.Artifacts)
	}
	if disc.Artifacts[0].NativeRelativePath != parentRel || disc.Artifacts[0].SessionID != "sess-main" {
		t.Fatalf("unexpected artifact[0]: %+v", disc.Artifacts[0])
	}
	if disc.Artifacts[1].NativeRelativePath != subRel || disc.Artifacts[1].SessionID != "sess-main" {
		t.Fatalf("unexpected artifact[1]: %+v", disc.Artifacts[1])
	}
	if len(disc.Refused) != 3 {
		t.Fatalf("refused %d paths, want 3 (file escape, dir escape, dangling): %+v", len(disc.Refused), disc.Refused)
	}
}

func TestStatePreservesUnknownFieldsAndParsedOffset(t *testing.T) {
	home := t.TempDir()
	rel := ".claude/projects/-workspace/sess-fwd.jsonl"
	abs := writeArtifact(t, home, rel, "{\"seq\":1}\n")

	up := newFakeUploader()
	s, err := shipper.New(shipper.Config{HomeDir: home, OriginHubID: "hub-a", Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	// Inject future fields and a P7 parsed_offset into state.json directly.
	rawBytes, err := os.ReadFile(shipper.StatePath(home))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(rawBytes, &doc); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	doc["future_top_level"] = map[string]any{"v": 42}
	arts := doc["artifacts"].(map[string]any)
	entry := arts[rel].(map[string]any)
	entry["parsed_offset"] = 10
	entry["future_normalizer_state"] = "opaque-token"

	updated, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal patched state: %v", err)
	}
	if err := os.WriteFile(shipper.StatePath(home), updated, 0o600); err != nil {
		t.Fatalf("write patched state: %v", err)
	}

	// Append and ship a new segment; unknown fields and parsed_offset must survive.
	appendArtifact(t, abs, "{\"seq\":2}\n")
	r, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if r.SegmentsShipped != 1 {
		t.Fatalf("SegmentsShipped=%d, want 1", r.SegmentsShipped)
	}

	afterBytes, err := os.ReadFile(shipper.StatePath(home))
	if err != nil {
		t.Fatalf("read state after round: %v", err)
	}
	var after map[string]any
	if err := json.Unmarshal(afterBytes, &after); err != nil {
		t.Fatalf("unmarshal state after round: %v", err)
	}
	if _, ok := after["future_top_level"]; !ok {
		t.Fatalf("top-level unknown field was dropped: %s", string(afterBytes))
	}
	afterEntry := after["artifacts"].(map[string]any)[rel].(map[string]any)
	if afterEntry["parsed_offset"] != float64(10) {
		t.Fatalf("parsed_offset=%v, want 10 (shipper must preserve and never advance it)", afterEntry["parsed_offset"])
	}
	if afterEntry["future_normalizer_state"] != "opaque-token" {
		t.Fatalf("artifact unknown field was dropped: %v", afterEntry["future_normalizer_state"])
	}
}

func TestRoundBudgetBoundsWork(t *testing.T) {
	home := t.TempDir()
	up := newFakeUploader()
	s, err := shipper.New(shipper.Config{
		HomeDir:         home,
		MaxSegmentBytes: 100,
		MaxRoundBytes:   20,
		Uploader:        up,
		Now:             fixedClock(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	rel := ".claude/projects/-workspace/sess-budget.jsonl"
	// Three 12-byte lines = 36 bytes; MaxRoundBytes=20 allows only 1 line per round.
	writeArtifact(t, home, rel, "{\"line\":01}\n{\"line\":02}\n{\"line\":03}\n")

	r1, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPreStop})
	if err != nil {
		t.Fatalf("RunRound 1: %v", err)
	}
	if r1.BytesShipped != 12 || !r1.BudgetExhausted {
		t.Fatalf("round 1: BytesShipped=%d BudgetExhausted=%v, want 12/true", r1.BytesShipped, r1.BudgetExhausted)
	}

	// Override MaxBytes in RoundOptions to ship the remaining 24 bytes.
	r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic, MaxBytes: 100})
	if err != nil {
		t.Fatalf("RunRound 2: %v", err)
	}
	if r2.BytesShipped != 24 || r2.BudgetExhausted {
		t.Fatalf("round 2: BytesShipped=%d BudgetExhausted=%v, want 24/false", r2.BytesShipped, r2.BudgetExhausted)
	}
}

func TestLostACKWithFileGrowthReplaysIdempotently(t *testing.T) {
	home := t.TempDir()
	rel := ".claude/projects/-workspace/sess-r1.jsonl"
	pre := "{\"pre\":1}\n"
	abs := writeArtifact(t, home, rel, pre)

	up := newFakeUploader()
	s, err := shipper.New(shipper.Config{HomeDir: home, RunID: "run-1", Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	// Append line A. On round 1, Hub commits the segment but the ACK is lost.
	lineA := "{\"a\":1}\n"
	appendArtifact(t, abs, lineA)
	up.onShip = func(_ context.Context, seg shipper.Segment, attempt int) (shipper.ShipResult, error, bool) {
		if attempt == 1 {
			up.mu.Lock()
			up.committed[fakeHubKey{
				Path:       seg.NativeRelativePath,
				Generation: seg.Generation,
				ByteStart:  seg.ByteStart,
			}] = seg
			up.mu.Unlock()
			return shipper.ShipResult{Outcome: shipper.OutcomeRetryable, Message: "lost ack"}, errors.New("eof"), true
		}
		return shipper.ShipResult{}, nil, false
	}

	r1, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 1: %v", err)
	}
	if r1.SegmentsShipped != 0 || len(r1.ArtifactErrors) != 1 {
		t.Fatalf("round 1: expected retryable error, got %+v", r1)
	}

	// File grows with line B before retry.
	lineB := "{\"b\":2}\n"
	appendArtifact(t, abs, lineB)

	// Round 2 replays exact [len(pre), len(pre)+len(lineA)) idempotently and
	// then ships line B; generation stays 0 and pre-baseline bytes are never shipped.
	r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 2: %v", err)
	}
	if r2.GenerationBumps != 0 || r2.SegmentsShipped != 2 {
		t.Fatalf("round 2: GenerationBumps=%d SegmentsShipped=%d, want 0 and 2", r2.GenerationBumps, r2.SegmentsShipped)
	}

	// Round 3 with another append confirms byte-exact continuity on gen 0.
	lineC := "{\"c\":3}\n"
	appendArtifact(t, abs, lineC)
	r3, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 3: %v", err)
	}
	if r3.GenerationBumps != 0 || r3.SegmentsShipped != 1 {
		t.Fatalf("round 3: unexpected %+v", r3)
	}

	st, _, err := shipper.LoadState(home)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.Artifacts[rel].Generation != 0 || st.Artifacts[rel].Pending != nil {
		t.Fatalf("expected gen=0 and pending=nil, got %+v", st.Artifacts[rel])
	}
	if got := string(up.reassemble(t, rel, 0)); got != lineA+lineB+lineC {
		t.Fatalf("gen 0 reassembled=%q, want %q (pre-baseline must not be shipped)", got, lineA+lineB+lineC)
	}
}

func TestLostACKOnFinalIncompleteTailCompletedBeforeRetry(t *testing.T) {
	home := t.TempDir()
	up := newFakeUploader()
	s, err := shipper.New(shipper.Config{HomeDir: home, RunID: "run-1", Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	rel := ".claude/projects/-workspace/sess-tail.jsonl"
	part1 := "{\"a\":1}\n{\"tail\":"
	abs := writeArtifact(t, home, rel, part1)

	// Final round commits on Hub with IncompleteTail=true, but loses ACK.
	up.onShip = func(_ context.Context, seg shipper.Segment, attempt int) (shipper.ShipResult, error, bool) {
		if attempt == 1 {
			if !seg.IncompleteTail {
				t.Errorf("expected IncompleteTail=true on attempt 1")
			}
			up.mu.Lock()
			up.committed[fakeHubKey{
				Path:       seg.NativeRelativePath,
				Generation: seg.Generation,
				ByteStart:  seg.ByteStart,
			}] = seg
			up.mu.Unlock()
			return shipper.ShipResult{Outcome: shipper.OutcomeRetryable}, errors.New("lost ack"), true
		}
		return shipper.ShipResult{}, nil, false
	}

	if _, err := s.RunRound(context.Background(), shipper.RoundOptions{Final: true, Reason: shipper.ReasonPreStop}); err != nil {
		t.Fatalf("RunRound 1: %v", err)
	}

	// Tail line is completed before the next round.
	part2 := "true}\n"
	appendArtifact(t, abs, part2)

	r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Final: false, Reason: shipper.ReasonResumeCatchup})
	if err != nil {
		t.Fatalf("RunRound 2: %v", err)
	}
	if r2.GenerationBumps != 0 || r2.SegmentsShipped != 2 {
		t.Fatalf("round 2: GenerationBumps=%d SegmentsShipped=%d, want 0 and 2", r2.GenerationBumps, r2.SegmentsShipped)
	}
	if !up.attempts[1].IncompleteTail {
		t.Fatalf("expected replayed pending segment to preserve IncompleteTail=true")
	}
	if got := string(up.reassemble(t, rel, 0)); got != part1+part2 {
		t.Fatalf("reassembled=%q, want %q", got, part1+part2)
	}
}

func TestParsedOffsetBaselineAndGenerationBumpReset(t *testing.T) {
	home := t.TempDir()
	rel := ".claude/projects/-workspace/sess-parsed.jsonl"
	pre := "{\"pre\":1}\n{\"pre\":2}\n"
	abs := writeArtifact(t, home, rel, pre)

	up := newFakeUploader()
	s, err := shipper.New(shipper.Config{HomeDir: home, Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	st, ok, err := shipper.LoadState(home)
	if err != nil || !ok {
		t.Fatalf("LoadState: ok=%v err=%v", ok, err)
	}
	entry := st.Artifacts[rel]
	if entry.ParsedOffset != int64(len(pre)) || entry.RawCommittedOffset != int64(len(pre)) {
		t.Fatalf("baseline offsets: parsed=%d raw=%d, want both %d", entry.ParsedOffset, entry.RawCommittedOffset, len(pre))
	}

	// Truncate the file to trigger a generation bump; ParsedOffset must reset to 0.
	short := "{\"gen\":1}\n"
	if err := os.WriteFile(abs, []byte(short), 0o644); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	r, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if r.GenerationBumps != 1 {
		t.Fatalf("GenerationBumps=%d, want 1", r.GenerationBumps)
	}

	st2, _, err := shipper.LoadState(home)
	if err != nil {
		t.Fatalf("LoadState 2: %v", err)
	}
	entry2 := st2.Artifacts[rel]
	if entry2.Generation != 1 || entry2.ParsedOffset != 0 || entry2.RawCommittedOffset != int64(len(short)) {
		t.Fatalf("post-bump entry: gen=%d parsed=%d raw=%d, want 1/0/%d",
			entry2.Generation, entry2.ParsedOffset, entry2.RawCommittedOffset, len(short))
	}
}

func TestSymlinkInsideHomeButOutsideRuleRootRefused(t *testing.T) {
	home := t.TempDir()

	// Create ~/.claude/.credentials.json (inside home, outside .claude/projects/).
	credsPath := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(credsPath), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if err := os.WriteFile(credsPath, []byte(`{"token":"synthetic"}`), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}

	// Valid artifact inside rule root.
	validRel := ".claude/projects/-workspace/sess-ok.jsonl"
	writeArtifact(t, home, validRel, "{\"ok\":1}\n")

	// Symlink file pointing to ~/.claude/.credentials.json.
	credsLink := filepath.Join(home, ".claude", "projects", "-workspace", "creds.jsonl")
	if err := os.Symlink(credsPath, credsLink); err != nil {
		t.Fatalf("symlink creds: %v", err)
	}
	// Intermediate dir symlink pointing to ~ (home).
	homeLink := filepath.Join(home, ".claude", "projects", "home-link")
	if err := os.Symlink(home, homeLink); err != nil {
		t.Fatalf("symlink home: %v", err)
	}

	disc, err := shipper.Discover(home, shipper.ClaudeHarnessSpec())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(disc.Artifacts) != 1 || disc.Artifacts[0].NativeRelativePath != validRel {
		t.Fatalf("expected only %s, got %+v", validRel, disc.Artifacts)
	}
	if len(disc.Refused) != 2 {
		t.Fatalf("expected 2 refused links (creds.jsonl and home-link), got %+v", disc.Refused)
	}
}

func TestDeviceChangeWithSameInodeDoesNotBumpGeneration(t *testing.T) {
	home := t.TempDir()
	rel := ".claude/projects/-workspace/sess-dev.jsonl"
	abs := writeArtifact(t, home, rel, "{\"seq\":1}\n")

	up := newFakeUploader()
	s, err := shipper.New(shipper.Config{HomeDir: home, Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	// Simulate broker reading over virtiofs/NFS: st_dev differs, st_ino matches.
	st, _, err := shipper.LoadState(home)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	st.Artifacts[rel].Device += 99999
	if err := shipper.SaveState(home, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	appendArtifact(t, abs, "{\"seq\":2}\n")
	r, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if r.GenerationBumps != 0 || r.SegmentsShipped != 1 {
		t.Fatalf("device change with same inode must not bump generation: %+v", r)
	}
	if got := string(up.reassemble(t, rel, 0)); got != "{\"seq\":2}\n" {
		t.Fatalf("reassembled=%q, want only post-baseline append", got)
	}
}

func TestOutcomeRejectedParksArtifactWithoutBlockingSiblings(t *testing.T) {
	home := t.TempDir()
	up := newFakeUploader()
	relBad := ".claude/projects/-workspace/sess-a-reject.jsonl"
	relGood := ".claude/projects/-workspace/sess-b-ok.jsonl"

	up.onShip = func(_ context.Context, seg shipper.Segment, _ int) (shipper.ShipResult, error, bool) {
		if seg.NativeRelativePath == relBad {
			return shipper.ShipResult{Outcome: shipper.OutcomeRejected, Message: "400 invalid_segment"}, nil, true
		}
		return shipper.ShipResult{}, nil, false
	}

	s, err := shipper.New(shipper.Config{HomeDir: home, Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	writeArtifact(t, home, relBad, "{\"bad\":1}\n")
	writeArtifact(t, home, relGood, "{\"good\":1}\n")

	r1, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 1: %v", err)
	}
	if r1.SegmentsShipped != 1 || len(r1.ArtifactErrors) != 1 {
		t.Fatalf("round 1: expected 1 shipped + 1 rejected, got %+v", r1)
	}

	attemptsAfterR1 := len(up.attempts)
	r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 2: %v", err)
	}
	if len(up.attempts) != attemptsAfterR1 || len(r2.ArtifactErrors) != 0 {
		t.Fatalf("parked artifact must not be retried forever: attempts %d -> %d, errs=%v",
			attemptsAfterR1, len(up.attempts), r2.ArtifactErrors)
	}

	st, _, err := shipper.LoadState(home)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !st.Artifacts[relBad].Rejected || st.Artifacts[relBad].RejectedReason != "400 invalid_segment" {
		t.Fatalf("expected rejected state persisted: %+v", st.Artifacts[relBad])
	}
}

func TestOversizeLineClampedToRoundBudget(t *testing.T) {
	home := t.TempDir()
	up := newFakeUploader()
	s, err := shipper.New(shipper.Config{
		HomeDir:         home,
		MaxSegmentBytes: 100,
		MaxRoundBytes:   32, // smaller than MaxSegmentBytes -> must clamp
		Uploader:        up,
		Now:             fixedClock(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	rel := ".claude/projects/-workspace/sess-over.jsonl"
	// Single 50-byte line with no newline yet; exceeds MaxRoundBytes=32.
	writeArtifact(t, home, rel, strings.Repeat("z", 50))

	r1, err := s.RunRound(context.Background(), shipper.RoundOptions{Final: false, Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 1: %v", err)
	}
	if r1.SegmentsShipped != 1 || r1.BytesShipped != 32 {
		t.Fatalf("expected oversize line to split at clamped round budget (32B), got %+v", r1)
	}
}

func TestLazyBaselineEntryBaselinesAtFirstOpen(t *testing.T) {
	home := t.TempDir()
	rel := ".claude/projects/-workspace/sess-lazy.jsonl"
	pre := "{\"pre\":1}\n"
	abs := writeArtifact(t, home, rel, pre)

	up := newFakeUploader()
	s, err := shipper.New(shipper.Config{HomeDir: home, Uploader: up, Now: fixedClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	// Simulate an entry that failed to open during Baseline and was marked NeedsBaseline.
	st, _, err := shipper.LoadState(home)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	st.Artifacts[rel] = &shipper.ArtifactState{
		NativeRelativePath: rel,
		ArtifactKind:       shipper.ArtifactKindJSONLAppend,
		Baseline:           true,
		NeedsBaseline:      true,
	}
	if err := shipper.SaveState(home, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// First RunRound lazily baselines at EOF without shipping pre-existing bytes.
	r1, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 1: %v", err)
	}
	if r1.SegmentsShipped != 0 {
		t.Fatalf("lazy baseline must not ship pre-existing bytes: %+v", r1)
	}
	stAfter, _, err := shipper.LoadState(home)
	if err != nil {
		t.Fatalf("LoadState after: %v", err)
	}
	if stAfter.Artifacts[rel].NeedsBaseline || stAfter.Artifacts[rel].RawCommittedOffset != int64(len(pre)) || stAfter.Artifacts[rel].ParsedOffset != int64(len(pre)) {
		t.Fatalf("unexpected state after lazy baseline: %+v", stAfter.Artifacts[rel])
	}

	// Subsequent append ships normally.
	appendArtifact(t, abs, "{\"post\":2}\n")
	r2, err := s.RunRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	if err != nil {
		t.Fatalf("RunRound 2: %v", err)
	}
	if r2.SegmentsShipped != 1 || string(up.reassemble(t, rel, 0)) != "{\"post\":2}\n" {
		t.Fatalf("unexpected post-lazy-baseline ship: r2=%+v", r2)
	}
}
