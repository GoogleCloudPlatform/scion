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

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuffer is a thread-safe wrapper around bytes.Buffer for capturing
// tailer output in concurrent tests.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (sb *safeBuffer) Write(p []byte) (int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Write(p)
}

func (sb *safeBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.String()
}

// parseTailerLines parses the output buffer into a slice of tailerOutput
// structs for assertion.
func parseTailerLines(t *testing.T, sb *safeBuffer) []tailerOutput {
	t.Helper()
	raw := sb.String()
	if raw == "" {
		return nil
	}
	var results []tailerOutput
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var out tailerOutput
		if err := json.Unmarshal([]byte(line), &out); err != nil {
			t.Fatalf("failed to parse tailer output line %q: %v", line, err)
		}
		results = append(results, out)
	}
	return results
}

// waitForTailerOutput polls until the output buffer contains at least n
// lines or the timeout elapses. Returns the parsed lines.
func waitForTailerOutput(t *testing.T, sb *safeBuffer, n int, timeout time.Duration) []tailerOutput {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		lines := parseTailerLines(t, sb)
		if len(lines) >= n {
			return lines
		}
		time.Sleep(50 * time.Millisecond)
	}
	lines := parseTailerLines(t, sb)
	if len(lines) < n {
		t.Fatalf("timed out waiting for %d lines; got %d: %s", n, len(lines), sb.String())
	}
	return lines
}

// -----------------------------------------------------------------------
// Test: Normal append — lines emitted from offset, prior content not
// re-shipped.
// -----------------------------------------------------------------------

func TestTailer_NormalAppendFromOffset(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, entrypointLogFile)

	// Write prior-run content that should be skipped.
	prior := "prior-run line 1\nprior-run line 2\n"
	if err := os.WriteFile(logPath, []byte(prior), 0644); err != nil {
		t.Fatal(err)
	}
	offset := int64(len(prior))

	// Append new content AFTER the offset.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	newLines := "new line 1\nnew line 2\nnew line 3\n"
	if _, err := f.WriteString(newLines); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())

	go doTailEntrypointLog(ctx, logPath, "test-slug", "agent-1", "proj-1", offset, &out)

	lines := waitForTailerOutput(t, &out, 3, 5*time.Second)
	cancel()

	// Verify we got exactly the new lines, not the prior content.
	wantMessages := []string{"new line 1", "new line 2", "new line 3"}
	for i, want := range wantMessages {
		if lines[i].Message != want {
			t.Errorf("line[%d].Message = %q, want %q", i, lines[i].Message, want)
		}
		if lines[i].Severity != "INFO" {
			t.Errorf("line[%d].Severity = %q, want INFO", i, lines[i].Severity)
		}
		if lines[i].Labels["component"] != "entrypoint-log-tailer" {
			t.Errorf("line[%d] missing component label", i)
		}
		if lines[i].Labels["agent_id"] != "agent-1" {
			t.Errorf("line[%d] agent_id = %q, want agent-1", i, lines[i].Labels["agent_id"])
		}
	}

	// Verify prior-run lines were NOT emitted.
	for _, l := range lines {
		if strings.Contains(l.Message, "prior-run") {
			t.Errorf("prior-run content leaked: %q", l.Message)
		}
	}
}

// -----------------------------------------------------------------------
// Test: Unterminated final line → emitted with partial:true.
// This is the MOST IMPORTANT test — the partial line is the crash message.
// -----------------------------------------------------------------------

func TestTailer_UnterminatedFinalLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, entrypointLogFile)

	// Write a complete line followed by an unterminated fragment.
	content := "complete line\npartial crash messa"
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())

	go doTailEntrypointLog(ctx, logPath, "slug", "agent-1", "proj-1", 0, &out)

	// Wait for the complete line to appear.
	waitForTailerOutput(t, &out, 1, 5*time.Second)

	// Cancel context — this should flush the partial line.
	cancel()

	// Give the goroutine a moment to flush and exit.
	time.Sleep(300 * time.Millisecond)

	lines := parseTailerLines(t, &out)
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines (1 complete + 1 partial), got %d: %s",
			len(lines), out.String())
	}

	// First line: the complete line.
	if lines[0].Message != "complete line" {
		t.Errorf("line[0].Message = %q, want %q", lines[0].Message, "complete line")
	}
	if lines[0].Labels["partial"] != "" {
		t.Errorf("line[0] should not have partial label")
	}

	// Last line: the partial fragment with partial:true.
	last := lines[len(lines)-1]
	if last.Message != "partial crash messa" {
		t.Errorf("partial line Message = %q, want %q", last.Message, "partial crash messa")
	}
	if last.Labels["partial"] != "true" {
		t.Errorf("partial line missing partial:true label, got labels: %v", last.Labels)
	}
}

// -----------------------------------------------------------------------
// Test: Truncation mid-tail → warning emitted, reading resumes from 0.
// -----------------------------------------------------------------------

func TestTailer_TruncationMidTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, entrypointLogFile)

	// Create initial content.
	initial := "initial line 1\ninitial line 2\n"
	if err := os.WriteFile(logPath, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go doTailEntrypointLog(ctx, logPath, "slug", "agent-1", "proj-1", 0, &out)

	// Wait for both initial lines to be emitted.
	waitForTailerOutput(t, &out, 2, 5*time.Second)

	// Truncate the file and write new, shorter content.
	if err := os.WriteFile(logPath, []byte("after truncation\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// The tailer should detect truncation (file size < position), emit a
	// WARNING, and re-read from 0.
	lines := waitForTailerOutput(t, &out, 4, 10*time.Second)
	cancel()

	// Find the truncation warning.
	foundWarning := false
	foundAfterTrunc := false
	for _, l := range lines {
		if l.Severity == "WARNING" && l.Labels["truncation_detected"] == "true" {
			foundWarning = true
		}
		if l.Message == "after truncation" {
			foundAfterTrunc = true
		}
	}

	if !foundWarning {
		t.Errorf("no truncation warning found in output:\n%s", out.String())
	}
	if !foundAfterTrunc {
		t.Errorf("content after truncation not found in output:\n%s", out.String())
	}
}

// -----------------------------------------------------------------------
// Test: File never created + context cancelled → file_never_created emitted.
// -----------------------------------------------------------------------

func TestTailer_FileNeverCreated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "nonexistent", entrypointLogFile)
	// The parent directory doesn't exist, so the file can never be opened.

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())

	// Use a short retry schedule for testing.
	origSchedule := openRetrySchedule
	openRetrySchedule = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	defer func() { openRetrySchedule = origSchedule }()

	go doTailEntrypointLog(ctx, logPath, "slug", "agent-1", "proj-1", 0, &out)

	// Wait a bit then cancel — the file never appears.
	time.Sleep(200 * time.Millisecond)
	cancel()

	// Give the goroutine time to emit the file_never_created line.
	time.Sleep(300 * time.Millisecond)

	lines := parseTailerLines(t, &out)
	if len(lines) == 0 {
		t.Fatal("expected at least 1 line (file_never_created), got 0")
	}

	// Find the file_never_created error.
	found := false
	for _, l := range lines {
		if l.Severity == "ERROR" && l.Labels["file_never_created"] == "true" {
			found = true
			if !strings.Contains(l.Message, "no entrypoint log was ever created") {
				t.Errorf("file_never_created message = %q, want to contain observation text", l.Message)
			}
		}
	}
	if !found {
		t.Errorf("file_never_created ERROR not found in output:\n%s", out.String())
	}
}

// -----------------------------------------------------------------------
// Test: Context cancellation with a partial line pending → partial emitted.
// -----------------------------------------------------------------------

func TestTailer_ContextCancelWithPartial(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, entrypointLogFile)

	// Write only a partial line (no newline).
	if err := os.WriteFile(logPath, []byte("dying mid-write"), 0644); err != nil {
		t.Fatal(err)
	}

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		doTailEntrypointLog(ctx, logPath, "slug", "agent-1", "proj-1", 0, &out)
		close(done)
	}()

	// Give the tailer time to read the content into its buffer.
	time.Sleep(500 * time.Millisecond)

	// Cancel — the partial line should be flushed.
	cancel()

	// Wait for goroutine exit.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tailer goroutine did not exit after context cancellation")
	}

	lines := parseTailerLines(t, &out)
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 line (partial), got %d: %s", len(lines), out.String())
	}
	if lines[0].Message != "dying mid-write" {
		t.Errorf("partial message = %q, want %q", lines[0].Message, "dying mid-write")
	}
	if lines[0].Labels["partial"] != "true" {
		t.Errorf("missing partial:true label")
	}
}

// -----------------------------------------------------------------------
// Test: File deleted mid-tail with partial content → partial flushed.
//
// On Linux, os.File.Read on a deleted file does NOT return an error — the
// fd remains valid (Unix semantics: unlink removes the directory entry,
// but the open fd keeps the inode alive). So exit path 2 (read error from
// ENOENT) is unreachable on POSIX systems with the current approach.
//
// This test verifies the practical behavior: when the file is deleted and
// then the context is cancelled, the partial content is still flushed.
// The context cancellation is the mechanism that actually triggers exit
// on Linux when a file vanishes and the sandbox is torn down.
// -----------------------------------------------------------------------

func TestTailer_FileDeletedMidTailPartialFlush(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, entrypointLogFile)

	// Write a partial line (no newline) — simulates a writer that dies mid-line.
	if err := os.WriteFile(logPath, []byte("partial before delete"), 0644); err != nil {
		t.Fatal(err)
	}

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		doTailEntrypointLog(ctx, logPath, "slug", "agent-1", "proj-1", 0, &out)
		close(done)
	}()

	// Give the tailer time to read the content into its buffer.
	time.Sleep(500 * time.Millisecond)

	// Delete the file. On Linux, the open fd is unaffected, but the
	// file is gone from the filesystem. Cancel the context to simulate
	// deleteOrWorkaround tearing down the sandbox.
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	cancel()

	// Wait for goroutine exit.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tailer goroutine did not exit after file deletion + cancel")
	}

	lines := parseTailerLines(t, &out)
	if len(lines) < 1 {
		t.Fatalf("expected at least 1 line (partial), got %d: %s", len(lines), out.String())
	}

	// The last line should be the partial content.
	last := lines[len(lines)-1]
	if last.Message != "partial before delete" {
		t.Errorf("partial message = %q, want %q", last.Message, "partial before delete")
	}
	if last.Labels["partial"] != "true" {
		t.Errorf("missing partial:true label on deleted-file flush")
	}
}

// -----------------------------------------------------------------------
// Test: Buffer cap exceeded → truncated_line:true, no unbounded growth.
// -----------------------------------------------------------------------

func TestTailer_BufferCapExceeded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, entrypointLogFile)

	// Write more than 64KB without any newline.
	bigContent := strings.Repeat("x", tailerBufferCap+1024)
	if err := os.WriteFile(logPath, []byte(bigContent), 0644); err != nil {
		t.Fatal(err)
	}

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())

	go doTailEntrypointLog(ctx, logPath, "slug", "agent-1", "proj-1", 0, &out)

	// Wait for the truncated line to appear.
	lines := waitForTailerOutput(t, &out, 1, 5*time.Second)
	cancel()

	// The first emitted line should have truncated_line:true.
	if lines[0].Labels["truncated_line"] != "true" {
		t.Errorf("expected truncated_line:true label, got labels: %v", lines[0].Labels)
	}
	if !strings.Contains(lines[0].Message, "[...line truncated at 64KB, no newline found]") {
		t.Errorf("truncated line message missing suffix: %q", lines[0].Message)
	}
}

// -----------------------------------------------------------------------
// Test: Goroutine exits cleanly on cancellation — no leak.
// -----------------------------------------------------------------------

func TestTailer_GoroutineExitsOnCancel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, entrypointLogFile)

	if err := os.WriteFile(logPath, []byte("line\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		doTailEntrypointLog(ctx, logPath, "slug", "agent-1", "proj-1", 0, &out)
		close(done)
	}()

	// Wait for initial content to be consumed.
	waitForTailerOutput(t, &out, 1, 5*time.Second)

	cancel()

	select {
	case <-done:
		// Goroutine exited — no leak.
	case <-time.After(5 * time.Second):
		t.Fatal("tailer goroutine did not exit within 5s after context cancellation")
	}
}

// -----------------------------------------------------------------------
// Test: Offset 0 on first-ever run (no prior file).
// -----------------------------------------------------------------------

func TestTailer_FirstRunNoFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, entrypointLogFile)
	// File doesn't exist yet — simulate first-ever run.

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())

	go doTailEntrypointLog(ctx, logPath, "slug", "agent-1", "proj-1", 0, &out)

	// Give the retry loop time to start, then create the file.
	time.Sleep(300 * time.Millisecond)
	if err := os.WriteFile(logPath, []byte("first boot line\n"), 0644); err != nil {
		t.Fatal(err)
	}

	lines := waitForTailerOutput(t, &out, 1, 10*time.Second)
	cancel()

	if lines[0].Message != "first boot line" {
		t.Errorf("message = %q, want %q", lines[0].Message, "first boot line")
	}
}

// -----------------------------------------------------------------------
// Test: Stat offset with truncation at startup.
// -----------------------------------------------------------------------

func TestTailer_TruncationAtStartup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, entrypointLogFile)

	// Create a file shorter than the offset to trigger truncation detection.
	short := "short\n"
	if err := os.WriteFile(logPath, []byte(short), 0644); err != nil {
		t.Fatal(err)
	}

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())

	// Offset is 1000 but file is only 6 bytes — truncation.
	go doTailEntrypointLog(ctx, logPath, "slug", "agent-1", "proj-1", 1000, &out)

	lines := waitForTailerOutput(t, &out, 2, 5*time.Second)
	cancel()

	// Should have a truncation warning followed by the content.
	foundWarning := false
	foundContent := false
	for _, l := range lines {
		if l.Severity == "WARNING" && l.Labels["truncation_detected"] == "true" {
			foundWarning = true
		}
		if l.Message == "short" {
			foundContent = true
		}
	}
	if !foundWarning {
		t.Errorf("missing truncation warning:\n%s", out.String())
	}
	if !foundContent {
		t.Errorf("missing file content after truncation reset:\n%s", out.String())
	}
}

// -----------------------------------------------------------------------
// Test: Live append — tailer picks up new content as it's written.
// -----------------------------------------------------------------------

func TestTailer_LiveAppend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, entrypointLogFile)

	// Create file with initial content.
	if err := os.WriteFile(logPath, []byte("init line\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go doTailEntrypointLog(ctx, logPath, "slug", "agent-1", "proj-1", 0, &out)

	// Wait for initial line.
	waitForTailerOutput(t, &out, 1, 5*time.Second)

	// Append more content.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("appended line\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	// Wait for the appended line.
	lines := waitForTailerOutput(t, &out, 2, 5*time.Second)
	cancel()

	if lines[1].Message != "appended line" {
		t.Errorf("appended message = %q, want %q", lines[1].Message, "appended line")
	}
}

// -----------------------------------------------------------------------
// Test: JSON output format matches GCP structured logging.
// -----------------------------------------------------------------------

func TestTailer_JSONFormat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, entrypointLogFile)

	if err := os.WriteFile(logPath, []byte("test msg\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())

	go doTailEntrypointLog(ctx, logPath, "my-slug", "my-agent", "my-project", 0, &out)

	lines := waitForTailerOutput(t, &out, 1, 5*time.Second)
	cancel()

	// Verify the JSON structure exactly.
	l := lines[0]
	if l.Severity != "INFO" {
		t.Errorf("severity = %q, want INFO", l.Severity)
	}
	if l.Message != "test msg" {
		t.Errorf("message = %q, want %q", l.Message, "test msg")
	}
	if l.Labels["component"] != "entrypoint-log-tailer" {
		t.Errorf("component = %q, want entrypoint-log-tailer", l.Labels["component"])
	}
	if l.Labels["agent_id"] != "my-agent" {
		t.Errorf("agent_id = %q, want my-agent", l.Labels["agent_id"])
	}
	if l.Labels["sandbox"] != "my-slug" {
		t.Errorf("sandbox = %q, want my-slug", l.Labels["sandbox"])
	}
	if l.Labels["project_id"] != "my-project" {
		t.Errorf("project_id = %q, want my-project", l.Labels["project_id"])
	}

	// Verify the raw JSON has the correct key for Cloud Logging labels.
	raw := strings.TrimSpace(out.String())
	var rawMap map[string]interface{}
	if err := json.Unmarshal([]byte(strings.Split(raw, "\n")[0]), &rawMap); err != nil {
		t.Fatal(err)
	}
	if _, ok := rawMap["logging.googleapis.com/labels"]; !ok {
		t.Error("JSON output missing 'logging.googleapis.com/labels' key")
	}
}

// -----------------------------------------------------------------------
// Test: Empty lines are not emitted.
// -----------------------------------------------------------------------

func TestTailer_EmptyLinesSkipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, entrypointLogFile)

	// Write content with empty lines.
	if err := os.WriteFile(logPath, []byte("line one\n\n\nline two\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())

	go doTailEntrypointLog(ctx, logPath, "slug", "agent-1", "proj-1", 0, &out)

	lines := waitForTailerOutput(t, &out, 2, 5*time.Second)
	cancel()

	if lines[0].Message != "line one" {
		t.Errorf("line[0] = %q, want %q", lines[0].Message, "line one")
	}
	if lines[1].Message != "line two" {
		t.Errorf("line[1] = %q, want %q", lines[1].Message, "line two")
	}
}

// -----------------------------------------------------------------------
// Unit test: flushPartial emits with partial:true when buffer is non-empty,
// and is a no-op when buffer is empty. This directly proves the logic used
// by exit path 2 (read error / ENOENT) — the integration path is
// unreachable on Linux (see TestTailer_FileDeletedMidTailPartialFlush)
// but the function is proven correct here.
// -----------------------------------------------------------------------

func TestTailer_FlushPartialUnit(t *testing.T) {
	t.Parallel()

	t.Run("non-empty buffer emits with partial:true", func(t *testing.T) {
		var emitted []tailerOutput
		emit := func(severity, message string, extra map[string]string) {
			out := tailerOutput{Severity: severity, Message: message, Labels: extra}
			emitted = append(emitted, out)
		}

		flushPartial([]byte("crash message fragment"), emit)

		if len(emitted) != 1 {
			t.Fatalf("expected 1 emission, got %d", len(emitted))
		}
		if emitted[0].Message != "crash message fragment" {
			t.Errorf("message = %q, want %q", emitted[0].Message, "crash message fragment")
		}
		if emitted[0].Labels["partial"] != "true" {
			t.Errorf("missing partial:true label")
		}
		if emitted[0].Severity != "INFO" {
			t.Errorf("severity = %q, want INFO", emitted[0].Severity)
		}
	})

	t.Run("empty buffer is no-op", func(t *testing.T) {
		var emitted []tailerOutput
		emit := func(severity, message string, extra map[string]string) {
			out := tailerOutput{Severity: severity, Message: message, Labels: extra}
			emitted = append(emitted, out)
		}

		flushPartial(nil, emit)
		flushPartial([]byte{}, emit)

		if len(emitted) != 0 {
			t.Errorf("expected 0 emissions for empty buffer, got %d", len(emitted))
		}
	})
}
