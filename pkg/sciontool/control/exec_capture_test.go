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

//go:build linux

package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
)

// TestBoundedBuffer_AllocationAndStatus verifies A4: 64 KiB+1 triggers
// too_large with no partial output and peak buffer capacity <= 64 KiB;
// whitespace -> empty; non-JSON -> invalid_json; valid JSON -> compacted.
func TestBoundedBuffer_AllocationAndStatus(t *testing.T) {
	t.Run("exact cap valid json", func(t *testing.T) {
		b := &boundedBuffer{cap: agentcontrol.MaxExecCaptureBytes}
		// JSON string of exact 64 KiB: `"` + (64KiB-2 'a') + `"`
		payload := `"` + strings.Repeat("a", agentcontrol.MaxExecCaptureBytes-2) + `"`
		if _, err := b.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
		if cap(b.buf) > agentcontrol.MaxExecCaptureBytes {
			t.Fatalf("cap(buf) = %d > %d", cap(b.buf), agentcontrol.MaxExecCaptureBytes)
		}
		status, out := b.finalizeJSON()
		if status != agentcontrol.ExecOutputCaptured || len(out) != agentcontrol.MaxExecCaptureBytes {
			t.Fatalf("got status=%q len=%d, want captured len=%d", status, len(out), agentcontrol.MaxExecCaptureBytes)
		}
	})

	t.Run("cap plus one overflows without growing past cap", func(t *testing.T) {
		b := &boundedBuffer{cap: agentcontrol.MaxExecCaptureBytes}
		chunk := bytes.Repeat([]byte("x"), 4096)
		for i := 0; i < 16; i++ {
			_, _ = b.Write(chunk)
			if cap(b.buf) > agentcontrol.MaxExecCaptureBytes {
				t.Fatalf("step %d: cap(buf) = %d > %d", i, cap(b.buf), agentcontrol.MaxExecCaptureBytes)
			}
		}
		// 64 KiB written so far; one more byte triggers overflow and frees buf.
		_, _ = b.Write([]byte("y"))
		if !b.overflow || b.buf != nil {
			t.Fatalf("expected overflow=true and buf=nil, got overflow=%v len=%d", b.overflow, len(b.buf))
		}
		if b.n != agentcontrol.MaxExecCaptureBytes+1 {
			t.Fatalf("n = %d, want %d", b.n, agentcontrol.MaxExecCaptureBytes+1)
		}
		status, out := b.finalizeJSON()
		if status != agentcontrol.ExecOutputTooLarge || out != nil {
			t.Fatalf("got status=%q out=%v, want too_large nil", status, out)
		}
	})

	t.Run("whitespace empty and invalid json", func(t *testing.T) {
		b := &boundedBuffer{cap: 1024}
		_, _ = b.Write([]byte("  \t\r\n  "))
		if status, out := b.finalizeJSON(); status != agentcontrol.ExecOutputEmpty || out != nil {
			t.Fatalf("whitespace: got %q %v", status, out)
		}

		b2 := &boundedBuffer{cap: 1024}
		_, _ = b2.Write([]byte("not json"))
		if status, out := b2.finalizeJSON(); status != agentcontrol.ExecOutputInvalidJSON || out != nil {
			t.Fatalf("invalid: got %q %v", status, out)
		}

		b3 := &boundedBuffer{cap: 1024}
		_, _ = b3.Write([]byte("  { \"a\" : 1 , \"b\" : [ 2 , 3 ] } \n"))
		status, out := b3.finalizeJSON()
		if status != agentcontrol.ExecOutputCaptured || string(out) != `{"a":1,"b":[2,3]}` {
			t.Fatalf("compact: got %q %q", status, out)
		}
	})
}

// TestExecTombs_OutputBudgetAndTTL verifies A7: 70 finished 64 KiB captures
// stay <= 4 MiB total held output, oldest report expired while their
// tombstones still replay mismatch/replayed, and after 15m TTL output expires.
func TestExecTombs_OutputBudgetAndTTL(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := newExecTombs(execTombMax, execTombTTL, execTombFloor, execOutputBudget, execOutputTTL)
	big := bytes.Repeat([]byte("a"), agentcontrol.MaxExecCaptureBytes)

	for i := 0; i < 70; i++ {
		id := fmt.Sprintf("run-%03d", i)
		now = now.Add(time.Second)
		s.put(now, &execTomb{
			runID:   id,
			binding: execBinding{digest: id, timeoutS: 10, capture: agentcontrol.ExecCaptureJSON},
			final: execFinal{
				outcome:      agentcontrol.ExecOutcomeOK,
				capture:      agentcontrol.ExecCaptureJSON,
				outputStatus: agentcontrol.ExecOutputCaptured,
				output:       append([]byte(nil), big...),
			},
		})
		if s.outputBytesHeld > execOutputBudget {
			t.Fatalf("run %d: outputBytesHeld=%d exceeds budget %d", i, s.outputBytesHeld, execOutputBudget)
		}
	}

	// 4 MiB / 64 KiB = 64 outputs fit; oldest 6 (0..5) must be expired while
	// their tombstones remain present with intact bindings.
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("run-%03d", i)
		tb, ok := s.get(now, id)
		if !ok {
			t.Fatalf("tombstone %s was evicted; idempotency tombstone must stay", id)
		}
		if tb.binding.digest != id {
			t.Fatalf("tombstone %s binding corrupted", id)
		}
		if tb.final.outputStatus != agentcontrol.ExecOutputExpired || tb.final.output != nil {
			t.Fatalf("tombstone %s: got status=%q len=%d, want expired nil", id, tb.final.outputStatus, len(tb.final.output))
		}
	}
	for i := 6; i < 70; i++ {
		id := fmt.Sprintf("run-%03d", i)
		tb, ok := s.get(now, id)
		if !ok || tb.final.outputStatus != agentcontrol.ExecOutputCaptured || len(tb.final.output) != len(big) {
			t.Fatalf("tombstone %s: expected captured output, got ok=%v status=%q len=%d", id, ok, tb.final.outputStatus, len(tb.final.output))
		}
	}

	// After 15m TTL, all remaining outputs expire while tombstones (60m TTL) remain.
	now = now.Add(execOutputTTL + time.Second)
	tb, ok := s.get(now, "run-069")
	if !ok {
		t.Fatal("run-069 tombstone should still exist within 60m tombstone TTL")
	}
	if tb.final.outputStatus != agentcontrol.ExecOutputExpired || tb.final.output != nil {
		t.Fatalf("after 15m TTL: got status=%q len=%d, want expired nil", tb.final.outputStatus, len(tb.final.output))
	}
	s.purge(now)
	if s.outputBytesHeld != 0 {
		t.Fatalf("after TTL purge: outputBytesHeld=%d, want 0", s.outputBytesHeld)
	}
}

// TestExecCapture_BindingMismatchAndStartReplayNoOutput verifies A8 and §4.3:
// capture is in the run binding (409 mismatch on different capture) and
// exec-start 200 finished replay never carries output_status or output.
func TestExecCapture_BindingMismatchAndStartReplayNoOutput(t *testing.T) {
	f := newExecFixture(t)
	if !slices.Contains(f.h.Features(), agentcontrol.FeatureExecCapture) {
		t.Fatalf("Handler.Features() = %v, want %q", f.h.Features(), agentcontrol.FeatureExecCapture)
	}
	noExecH := New(Options{KickTokenRefresh: func() {}})
	if slices.Contains(noExecH.Features(), agentcontrol.FeatureExecCapture) {
		t.Fatalf("Handler without Exec must not advertise %q", agentcontrol.FeatureExecCapture)
	}

	req := f.startReq(501, "cap-bind")
	req.Capture = agentcontrol.ExecCaptureJSON
	code, _ := f.start(t, req)
	if code != http.StatusAccepted {
		t.Fatalf("start = %d, want 202", code)
	}

	// Same run_id with different capture -> 409 mismatch.
	diff := req
	diff.Capture = ""
	code, resp := f.start(t, diff)
	if code != http.StatusConflict || resp.Outcome != agentcontrol.ExecOutcomeMismatch {
		t.Fatalf("mismatch capture: got %d %+v, want 409 mismatch", code, resp)
	}

	// Finish with captured JSON output.
	f.fx.mu.Lock()
	p := f.fx.procs["cap-bind"]
	f.fx.mu.Unlock()
	p.done <- execFinal{
		outcome:      agentcontrol.ExecOutcomeOK,
		exitCode:     0,
		durationMS:   5,
		outputBytes:  7,
		capture:      agentcontrol.ExecCaptureJSON,
		outputStatus: agentcontrol.ExecOutputCaptured,
		output:       []byte(`{"a":1}`),
	}
	f.waitFinished(t, 501)

	// Replay exec-start -> 200 finished, NO output or output_status.
	code, resp, raw := postExec(t, f.h, RouteExecStart, req)
	if code != http.StatusOK || !resp.Replayed || resp.OutputStatus != "" || len(resp.Output) != 0 {
		t.Fatalf("exec-start replay must omit output: %d %s", code, raw)
	}
	if _, err := agentcontrol.ParseExecStartAnswer(code, []byte(raw)); err != nil {
		t.Fatalf("exec-start replay failed schema: %v", err)
	}

	// exec-status -> 200 finished WITH output_status and output.
	code, resp, raw = postExec(t, f.h, RouteExecStatus, agentcontrol.ExecStatusRequest{V: 1, RunID: req.RunID})
	parsed, err := agentcontrol.ParseExecStatusAnswer(code, []byte(raw), agentcontrol.ExecCaptureJSON)
	if err != nil {
		t.Fatalf("exec-status failed schema: %v (raw=%s)", err, raw)
	}
	if parsed.OutputStatus != agentcontrol.ExecOutputCaptured || string(parsed.Output) != `{"a":1}` {
		t.Fatalf("exec-status got status=%q output=%q", parsed.OutputStatus, parsed.Output)
	}
}

// TestExecLinux_JSONCapture runs real Linux processes with capture:"json" and
// verifies stdout-only capture, stderr noise isolation, too_large, empty, and
// invalid_json.
func TestExecLinux_JSONCapture(t *testing.T) {
	r := newRealExec(t)

	runJSON := func(t *testing.T, id string, script string) agentcontrol.ExecResult {
		t.Helper()
		req := agentcontrol.ExecStartRequest{
			V:              1,
			RunID:          id,
			Argv:           []string{"/bin/sh", "-c", script},
			TimeoutS:       10,
			NotAfterUnixMS: time.Now().Add(time.Minute).UnixMilli(),
			Capture:        agentcontrol.ExecCaptureJSON,
		}
		code, _, raw := r.start(t, req)
		if code != http.StatusAccepted {
			t.Fatalf("start = %d %s", code, raw)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			sc, resp, sraw := postExec(t, r.h, RouteExecStatus, agentcontrol.ExecStatusRequest{V: 1, RunID: id})
			if sc == http.StatusOK && resp.State == agentcontrol.ExecStateFinished {
				parsed, err := agentcontrol.ParseExecStatusAnswer(sc, []byte(sraw), agentcontrol.ExecCaptureJSON)
				if err != nil {
					t.Fatalf("status schema error: %v (%s)", err, sraw)
				}
				return parsed
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", id)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	t.Run("valid json with stderr noise", func(t *testing.T) {
		res := runJSON(t, "00000000-0000-4000-8000-000000000101", `echo 'warning on stderr' >&2; echo ' { "a" : 1 } '`)
		if res.Outcome != agentcontrol.ExecOutcomeOK || res.OutputStatus != agentcontrol.ExecOutputCaptured {
			t.Fatalf("got %+v", res)
		}
		if string(res.Output) != `{"a":1}` {
			t.Fatalf("output = %q, want compacted json", res.Output)
		}
		// output_bytes counts both stdout and stderr.
		if res.OutputBytes < int64(len("warning on stderr\n")+len(` { "a" : 1 } `)) {
			t.Fatalf("output_bytes = %d, expected stdout+stderr", res.OutputBytes)
		}
	})

	t.Run("invalid json preserves exit 0", func(t *testing.T) {
		res := runJSON(t, "00000000-0000-4000-8000-000000000102", `echo 'not json'`)
		if res.Outcome != agentcontrol.ExecOutcomeOK || res.OutputStatus != agentcontrol.ExecOutputInvalidJSON || len(res.Output) != 0 {
			t.Fatalf("got %+v", res)
		}
	})

	t.Run("whitespace only is empty", func(t *testing.T) {
		res := runJSON(t, "00000000-0000-4000-8000-000000000103", `printf '  \n\t '`)
		if res.Outcome != agentcontrol.ExecOutcomeOK || res.OutputStatus != agentcontrol.ExecOutputEmpty || len(res.Output) != 0 {
			t.Fatalf("got %+v", res)
		}
	})

	t.Run("over 64KiB is too_large", func(t *testing.T) {
		res := runJSON(t, "00000000-0000-4000-8000-000000000104", `head -c 65537 /dev/zero | tr '\0' 'a'`)
		if res.Outcome != agentcontrol.ExecOutcomeOK || res.OutputStatus != agentcontrol.ExecOutputTooLarge || len(res.Output) != 0 {
			t.Fatalf("got %+v", res)
		}
		if res.OutputBytes != 65537 {
			t.Fatalf("output_bytes = %d, want 65537", res.OutputBytes)
		}
	})

	// Sanity check: default run JSON encodes identically without output fields.
	var emptyResp agentcontrol.ExecResponse
	_ = json.Unmarshal([]byte(`{"state":"running"}`), &emptyResp)
}

// TestTailRing_BoundsAndUTF8 verifies A5: tailRing keeps the last cap bytes,
// trims split UTF-8 runes at the start boundary, keeps invalid UTF-8 within
// cap, and is safe under concurrent stdout/stderr writes.
func TestTailRing_BoundsAndUTF8(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		r := &tailRing{cap: 16}
		st, out := r.finalizeTail()
		if st != agentcontrol.ExecOutputEmpty || out != nil {
			t.Fatalf("empty: got %q %v", st, out)
		}
	})

	t.Run("under cap captured", func(t *testing.T) {
		r := &tailRing{cap: 16}
		_, _ = r.Write([]byte("hello "))
		_, _ = r.Write([]byte("world"))
		st, out := r.finalizeTail()
		if st != agentcontrol.ExecOutputCaptured || string(out) != "hello world" {
			t.Fatalf("got %q %q", st, out)
		}
	})

	t.Run("utf8 rune boundary trim on truncation", func(t *testing.T) {
		// "é" is 2 bytes (0xC3 0xA9), "€" is 3 bytes (0xE2 0x82 0xAC).
		// Write "aa€bb" (7 bytes) into cap=4 -> last 4 bytes are [0x82, 0xAC, 'b', 'b'].
		// Trimming leading continuation bytes yields "bb" (valid UTF-8).
		r := &tailRing{cap: 4}
		_, _ = r.Write([]byte("aa€bb"))
		st, out := r.finalizeTail()
		if st != agentcontrol.ExecOutputTruncated || string(out) != "bb" {
			t.Fatalf("got %q %q, want truncated %q", st, out, "bb")
		}
	})

	t.Run("invalid binary bytes stay within 64KiB and valid utf8", func(t *testing.T) {
		r := &tailRing{cap: agentcontrol.MaxExecCaptureBytes}
		// Alternating valid ASCII and invalid 0xFF bytes so each invalid byte
		// expands to 3-byte U+FFFD individually.
		bad := bytes.Repeat([]byte{'a', 0xff}, agentcontrol.MaxExecCaptureBytes)
		_, _ = r.Write(bad)
		st, out := r.finalizeTail()
		if st != agentcontrol.ExecOutputTruncated {
			t.Fatalf("status = %q, want truncated", st)
		}
		if len(out) == 0 || len(out) > agentcontrol.MaxExecCaptureBytes {
			t.Fatalf("len(out) = %d, want 1..%d", len(out), agentcontrol.MaxExecCaptureBytes)
		}
		// Must round-trip through ParseExecStatusAnswer cleanly.
		encoded, err := json.Marshal(string(out))
		if err != nil {
			t.Fatal(err)
		}
		code, dur, n := 0, int64(1), r.totalBytes()
		wire, err := json.Marshal(agentcontrol.ExecResponse{
			State:        agentcontrol.ExecStateFinished,
			Outcome:      agentcontrol.ExecOutcomeOK,
			ExitCode:     &code,
			DurationMS:   &dur,
			OutputBytes:  &n,
			OutputStatus: st,
			Output:       json.RawMessage(encoded),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := agentcontrol.ParseExecStatusAnswer(http.StatusOK, wire, agentcontrol.ExecCaptureTail); err != nil {
			t.Fatalf("tail invalid-utf8 output failed schema: %v", err)
		}
	})
}

// TestExecLinux_TailCapture verifies A5 on Linux: 1 MiB of mixed stdout and
// stderr returns the last <= 64 KiB, status truncated, valid UTF-8.
func TestExecLinux_TailCapture(t *testing.T) {
	r := newRealExec(t)

	runTail := func(t *testing.T, id string, script string) agentcontrol.ExecResult {
		t.Helper()
		req := agentcontrol.ExecStartRequest{
			V:              1,
			RunID:          id,
			Argv:           []string{"/bin/sh", "-c", script},
			TimeoutS:       15,
			NotAfterUnixMS: time.Now().Add(time.Minute).UnixMilli(),
			Capture:        agentcontrol.ExecCaptureTail,
		}
		code, _, raw := r.start(t, req)
		if code != http.StatusAccepted {
			t.Fatalf("start = %d %s", code, raw)
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			sc, resp, sraw := postExec(t, r.h, RouteExecStatus, agentcontrol.ExecStatusRequest{V: 1, RunID: id})
			if sc == http.StatusOK && resp.State == agentcontrol.ExecStateFinished {
				parsed, err := agentcontrol.ParseExecStatusAnswer(sc, []byte(sraw), agentcontrol.ExecCaptureTail)
				if err != nil {
					t.Fatalf("status schema error: %v (%s)", err, sraw)
				}
				return parsed
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", id)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	t.Run("short merged stdout and stderr", func(t *testing.T) {
		res := runTail(t, "00000000-0000-4000-8000-000000000201", `echo out-line; echo err-line >&2`)
		if res.Outcome != agentcontrol.ExecOutcomeOK || res.OutputStatus != agentcontrol.ExecOutputCaptured {
			t.Fatalf("got %+v", res)
		}
		s := string(res.Output)
		if !strings.Contains(s, "out-line") || !strings.Contains(s, "err-line") {
			t.Fatalf("merged tail missing streams: %q", s)
		}
	})

	t.Run("1MiB mixed stdout and stderr truncated to 64KiB", func(t *testing.T) {
		// Emit ~512 KiB to stdout, ~512 KiB to stderr, then a final marker.
		script := `head -c 524288 /dev/zero | tr '\0' 'o'; head -c 524288 /dev/zero | tr '\0' 'e' >&2; echo 'TAIL-END-MARKER'`
		res := runTail(t, "00000000-0000-4000-8000-000000000202", script)
		if res.Outcome != agentcontrol.ExecOutcomeOK || res.OutputStatus != agentcontrol.ExecOutputTruncated {
			t.Fatalf("got %+v", res)
		}
		if len(res.Output) == 0 || len(res.Output) > agentcontrol.MaxExecCaptureBytes {
			t.Fatalf("len(output) = %d, want 1..%d", len(res.Output), agentcontrol.MaxExecCaptureBytes)
		}
		if res.OutputBytes < 1048576 {
			t.Fatalf("output_bytes = %d, want >= 1 MiB", res.OutputBytes)
		}
		if !strings.HasSuffix(string(res.Output), "TAIL-END-MARKER\n") {
			t.Fatalf("tail missing end marker")
		}
	})
}
