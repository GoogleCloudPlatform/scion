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

package trajectory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/trajectory/shipper"
)

type recordingUploader struct {
	mu       sync.Mutex
	outcome  shipper.ShipOutcome
	message  string
	segments []shipper.Segment
	ch       chan shipper.Segment
}

func (u *recordingUploader) Ship(_ context.Context, seg shipper.Segment) (shipper.ShipResult, error) {
	u.mu.Lock()
	u.segments = append(u.segments, seg)
	outcome := u.outcome
	msg := u.message
	u.mu.Unlock()
	if u.ch != nil {
		select {
		case u.ch <- seg:
		default:
		}
	}
	return shipper.ShipResult{Outcome: outcome, SegmentID: "seg-1", Message: msg}, nil
}

func (u *recordingUploader) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.segments)
}

func TestRunner_TriggersWakeDebounceAndPreStop(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	artPath := filepath.Join(projDir, "sess-1.jsonl")
	if err := os.WriteFile(artPath, []byte("{\"baseline\":true}\n"), 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}

	up := &recordingUploader{
		outcome: shipper.OutcomeCommitted,
		ch:      make(chan shipper.Segment, 16),
	}

	r, err := NewRunner(Config{
		HomeDir:          home,
		RunID:            "run-1",
		Uploader:         up,
		PeriodicInterval: time.Hour,
		ModelEndDebounce: 40 * time.Millisecond,
		PreStopTimeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait briefly for initial resume_catchup baseline to complete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok, _ := shipper.LoadState(home); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 1. Append a complete line and wake via model_end (debounced).
	f, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	_, _ = f.WriteString("{\"turn\":1}\n")
	_ = f.Close()

	NotifyWake(home, WakeModelEnd)
	select {
	case seg := <-up.ch:
		if seg.IncompleteTail {
			t.Fatalf("expected complete line from model_end, got incomplete_tail=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for debounced model_end wake round")
	}

	// 2. Append another complete line and wake via session_end (immediate).
	f2, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open append 2: %v", err)
	}
	_, _ = f2.WriteString("{\"turn\":2}\n")
	_ = f2.Close()

	NotifyWake(home, WakeSessionEnd)
	select {
	case <-up.ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for session_end wake round")
	}

	// 3. Append a trailing partial line (no newline) and trigger PreStop (Final=true).
	f3, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open append 3: %v", err)
	}
	_, _ = f3.WriteString("{\"partial\":true}")
	_ = f3.Close()

	r.PreStop(context.Background())
	select {
	case seg := <-up.ch:
		if !seg.IncompleteTail {
			t.Fatalf("expected PreStop Final=true to ship incomplete_tail=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("expected PreStop to ship trailing partial bytes")
	}
}

func TestRunner_PolicyRefusalRebaselinesAndBacksOff(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rel := ".claude/projects/-workspace/sess-pol.jsonl"
	artPath := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.WriteFile(artPath, []byte("{\"seq\":1}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	up := &recordingUploader{
		outcome: shipper.OutcomePolicyRefused,
		message: "capture_metadata_only",
	}
	// Pre-baseline so first round sees new bytes.
	s, err := shipper.New(shipper.Config{HomeDir: home, RunID: "run-pol"})
	if err != nil {
		t.Fatalf("shipper.New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	line2 := "{\"seq\":2}\n"
	f, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	_, _ = f.WriteString(line2)
	_ = f.Close()

	r, err := NewRunner(Config{
		HomeDir:            home,
		RunID:              "run-pol",
		Uploader:           up,
		UnavailableBackoff: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	r.executeRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic}, time.Second)
	if up.count() != 1 {
		t.Fatalf("expected 1 Ship attempt before policy refusal backoff, got %d", up.count())
	}

	// State should be rebaselined to current EOF with LastRebaselineReason recorded.
	st, ok, err := shipper.LoadState(home)
	if err != nil || !ok {
		t.Fatalf("LoadState: ok=%v err=%v", ok, err)
	}
	if st.LastRebaselineReason != "capture_metadata_only" {
		t.Fatalf("LastRebaselineReason=%q, want capture_metadata_only", st.LastRebaselineReason)
	}
	wantEOF := int64(len("{\"seq\":1}\n") + len(line2))
	if st.Artifacts[rel].RawCommittedOffset != wantEOF || st.Artifacts[rel].ParsedOffset != wantEOF {
		t.Fatalf("offsets not rebaselined to EOF=%d: %+v", wantEOF, st.Artifacts[rel])
	}

	// Subsequent round during backoff must not call Uploader.Ship.
	r.executeRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic}, time.Second)
	if up.count() != 1 {
		t.Fatalf("expected backoff to suppress subsequent Ship attempts, got %d", up.count())
	}
}

func TestResolveRunID_JWTFallback(t *testing.T) {
	t.Setenv(hub.EnvLaunchID, "")
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claimsBytes, _ := json.Marshal(map[string]string{"sub": "agent-1", "run_id": "run-from-jwt"})
	payload := base64.RawURLEncoding.EncodeToString(claimsBytes)
	jwtTok := header + "." + payload + ".sig"

	client := hub.NewClientWithConfig("http://127.0.0.1:9999", jwtTok, "agent-1")
	if got := ResolveRunID(client); got != "run-from-jwt" {
		t.Fatalf("ResolveRunID=%q, want run-from-jwt", got)
	}
}

func TestRunner_SessionEndWakeThrottledAndCoalesced(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	artPath := filepath.Join(projDir, "sess-burst.jsonl")
	if err := os.WriteFile(artPath, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Uploader that always returns retryable so every round triggers a Ship call
	// on the pending line (letting us count rounds triggered by wakes).
	up := &recordingUploader{
		outcome: shipper.OutcomeRetryable,
		message: "count-round",
	}

	s, err := shipper.New(shipper.Config{HomeDir: home, RunID: "run-burst"})
	if err != nil {
		t.Fatalf("shipper.New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if err := os.WriteFile(artPath, []byte("{\"seq\":1}\n"), 0o644); err != nil {
		t.Fatalf("write line: %v", err)
	}

	r, err := NewRunner(Config{
		HomeDir:               home,
		RunID:                 "run-burst",
		Uploader:              up,
		PeriodicInterval:      time.Hour,
		SessionEndMinInterval: 60 * time.Millisecond,
		PreStopTimeout:        time.Second,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for initial resume_catchup round.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && up.count() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	beforeBurst := up.count()

	// Send 1000 session_end wakes over ~100ms.
	burstStart := time.Now()
	for i := 0; i < 1000; i++ {
		NotifyWake(home, WakeSessionEnd)
		if i%100 == 0 && time.Since(burstStart) < 100*time.Millisecond {
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Allow at most one trailing coalesced timer to fire.
	time.Sleep(100 * time.Millisecond)
	r.PreStop(context.Background())

	// Total rounds during the 200ms burst+tail (plus 1 PreStop) must be small
	// and bounded (far below 1000).
	burstRounds := up.count() - beforeBurst
	if burstRounds < 1 || burstRounds > 6 {
		t.Fatalf("expected 1..6 throttled/coalesced rounds for 1000 wakes, got %d", burstRounds)
	}
}

func TestRunner_CaptureUnavailableRebaselinesAcrossRestart(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-workspace")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rel := ".claude/projects/-workspace/sess-404.jsonl"
	artPath := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.WriteFile(artPath, []byte("{\"init\":0}\n"), 0o644); err != nil {
		t.Fatalf("write init: %v", err)
	}

	// Start with experiment OFF (404).
	up := &recordingUploader{outcome: shipper.OutcomeCaptureUnavailable}
	r1, err := NewRunner(Config{
		HomeDir:            home,
		RunID:              "run-404",
		Uploader:           up,
		UnavailableBackoff: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewRunner 1: %v", err)
	}

	// Initial baseline happens on first round, then append A while experiment is OFF.
	r1.executeRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonResumeCatchup}, time.Second)
	lineA := "{\"written_while_off\":\"A\"}\n"
	f, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append A: %v", err)
	}
	_, _ = f.WriteString(lineA)
	_ = f.Close()

	// Round gets 404 -> rebaselines to EOF (skipping A) and backs off.
	r1.executeRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonPeriodic}, time.Second)
	if up.count() != 1 {
		t.Fatalf("expected 1 prepare attempt (404), got %d", up.count())
	}

	// Flip experiment ON and append B.
	up2 := &recordingUploader{outcome: shipper.OutcomeCommitted}
	lineB := "{\"written_while_on\":\"B\"}\n"
	f2, err := os.OpenFile(artPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append B: %v", err)
	}
	_, _ = f2.WriteString(lineB)
	_ = f2.Close()

	// Simulate container restart: fresh Runner over the same home runs resume_catchup.
	r2, err := NewRunner(Config{
		HomeDir:            home,
		RunID:              "run-404-restarted",
		Uploader:           up2,
		UnavailableBackoff: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewRunner 2: %v", err)
	}
	r2.executeRound(context.Background(), shipper.RoundOptions{Reason: shipper.ReasonResumeCatchup}, time.Second)

	if up2.count() != 1 {
		t.Fatalf("expected exactly 1 shipped segment (only B), got %d", up2.count())
	}
	wantStart := int64(len("{\"init\":0}\n") + len(lineA))
	gotSeg := up2.segments[0]
	if gotSeg.ByteStart != wantStart || gotSeg.RawSize != int64(len(lineB)) {
		t.Fatalf("expected only B [start=%d len=%d] to ship, got [start=%d len=%d]",
			wantStart, len(lineB), gotSeg.ByteStart, gotSeg.RawSize)
	}
}
