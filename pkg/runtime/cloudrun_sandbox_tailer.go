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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// -----------------------------------------------------------------------
// Entrypoint log tailer
//
// SCOPE: This channel makes STARTUP failures visible. It does not make
// runtime failures visible. Silence from this channel after a successful
// startup means "init completed and handed off to tmux", not "the agent
// is healthy."
//
// The entrypoint log (.scion-entrypoint.log) captures the init phase:
// outer-shell exec failures, sciontool init provisioning, secret staging,
// metadata emulator startup, telemetry init, and tmux launch. It goes
// quiet the moment init execs into tmux. Everything after that — hook
// crashes, status errors, harness failures — lands in agent.log or on
// a tmux pty that nobody captures, and is NOT shipped by this tailer.
//
// Tailing agent.log for the runtime phase is designed as a possible
// phase 2 and is not in this PR.
// -----------------------------------------------------------------------

// tailerBufferCap is the maximum size of the partial-line buffer before
// a forced emit. 64KB is generous: the longest plausible single line is
// a debug.Stack() trace (~4–8KB). This prevents unbounded accumulation
// from a file with no newlines.
const tailerBufferCap = 64 * 1024

// tailerOutput is the JSON envelope written to stdout for Cloud Logging.
// Cloud Run captures host stdout and creates Cloud Logging entries with
// promoted labels. No service account, no token, no client library required.
type tailerOutput struct {
	Severity string            `json:"severity"`
	Message  string            `json:"message"`
	Labels   map[string]string `json:"logging.googleapis.com/labels"`
}

// tailerWriter abstracts the output destination for testing.
// In production this is os.Stdout; tests substitute a buffer.
type tailerWriter = io.Writer

// openRetrySchedule defines the backoff intervals for opening the
// entrypoint log. Total wait: 250+500+1000+2000+4000 = 7750ms ≈ 8s.
// After the schedule is exhausted, falls back to 5s polling.
var openRetrySchedule = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
}

// tailEntrypointLog tails the agent's entrypoint log file and re-emits
// each line to the host process's stdout as GCP-structured JSON.
//
// Lifecycle: started alongside watchSandbox after liveness probes pass,
// cancelled by deleteOrWorkaround (same context as watchSandbox).
func (r *CloudRunSandboxRuntime) tailEntrypointLog(ctx context.Context, slug, agentID, project string, paths scionPaths, offset int64) {
	logPath := filepath.Join(paths.agentHome, entrypointLogFile)
	doTailEntrypointLog(ctx, logPath, slug, agentID, project, offset, os.Stdout)
}

// doTailEntrypointLog is the testable core. The output destination is
// injected so tests can capture structured JSON without touching stdout.
func doTailEntrypointLog(ctx context.Context, logPath, slug, agentID, project string, offset int64, w tailerWriter) {
	labels := map[string]string{
		"component":  "entrypoint-log-tailer",
		"agent_id":   agentID,
		"sandbox":    slug,
		"project_id": project,
	}

	emit := func(severity, message string, extra map[string]string) {
		merged := make(map[string]string, len(labels)+len(extra))
		for k, v := range labels {
			merged[k] = v
		}
		for k, v := range extra {
			merged[k] = v
		}
		out := tailerOutput{
			Severity: severity,
			Message:  message,
			Labels:   merged,
		}
		data, err := json.Marshal(out)
		if err != nil {
			// Should never happen with string fields; log and continue.
			runtimeLog.Warn("entrypoint-log-tailer: failed to marshal JSON",
				"error", err, "sandbox", slug)
			return
		}
		data = append(data, '\n')
		_, _ = w.Write(data)
	}

	// --- Phase 1: Open the file with bounded retry ---
	f, opened := openWithRetry(ctx, logPath, slug, emit)
	if !opened {
		// Context was cancelled and the file was never opened.
		// This is the highest-value diagnostic: the sandbox terminated
		// without ever creating its entrypoint log.
		if ctx.Err() != nil {
			emit("ERROR",
				fmt.Sprintf("no entrypoint log was ever created for sandbox %q — "+
					"no diagnostic output was captured from this sandbox", slug),
				map[string]string{"file_never_created": "true"})
		}
		return
	}
	defer f.Close()

	// --- Phase 2: Seek to offset, detecting truncation ---
	pos := seekToOffset(f, offset, slug, emit)

	// --- Phase 3: Read loop ---
	var lineBuf []byte
	readBuf := make([]byte, 4096)

	// Backoff state for EOF polling.
	const (
		fastCount = 10
		slowCount = 10
	)
	eofCount := 0

	for {
		// Check context before each iteration.
		if ctx.Err() != nil {
			flushPartial(lineBuf, emit)
			return
		}

		n, readErr := f.Read(readBuf)
		if n > 0 {
			// Reset backoff on successful read.
			eofCount = 0
			pos += int64(n)

			lineBuf = append(lineBuf, readBuf[:n]...)
			lineBuf = emitCompleteLines(lineBuf, emit)

			// Buffer cap: prevent unbounded growth from a file with no newlines.
			if len(lineBuf) > tailerBufferCap {
				emit("INFO", string(lineBuf)+" [...line truncated at 64KB, no newline found]",
					map[string]string{"truncated_line": "true"})
				lineBuf = lineBuf[:0]
			}
		}

		if readErr != nil && readErr != io.EOF {
			// File deleted mid-tail or I/O error.
			if errors.Is(readErr, fs.ErrNotExist) {
				runtimeLog.Info("entrypoint-log-tailer: file deleted mid-tail",
					"path", logPath, "sandbox", slug)
			} else {
				runtimeLog.Warn("entrypoint-log-tailer: read error",
					"path", logPath, "error", readErr, "sandbox", slug)
			}
			flushPartial(lineBuf, emit)
			return
		}

		if readErr == io.EOF {
			eofCount++

			// Determine backoff sleep duration.
			var sleep time.Duration
			switch {
			case eofCount <= fastCount:
				sleep = 250 * time.Millisecond
			case eofCount <= fastCount+slowCount:
				sleep = 1 * time.Second
			default:
				sleep = 5 * time.Second
			}

			select {
			case <-ctx.Done():
				flushPartial(lineBuf, emit)
				return
			case <-time.After(sleep):
			}

			// After sleep, check for truncation.
			pos = checkTruncation(f, pos, logPath, slug, emit)
		}
	}
}

// openWithRetry attempts to open the file with bounded retry and backoff.
// Returns the open file and true, or nil and false if the file was never
// successfully opened (context cancelled or persistent error).
func openWithRetry(ctx context.Context, logPath, slug string, emit func(string, string, map[string]string)) (*os.File, bool) {
	const postRetryInterval = 5 * time.Second

	for attempt := 0; ; attempt++ {
		f, err := os.Open(logPath)
		if err == nil {
			return f, true
		}

		if !errors.Is(err, fs.ErrNotExist) {
			// Non-ENOENT error: permission denied, I/O error, etc.
			emit("WARNING",
				fmt.Sprintf("entrypoint log open failed: %v — retrying", err),
				nil)
		}

		// Determine wait interval.
		var wait time.Duration
		if attempt < len(openRetrySchedule) {
			wait = openRetrySchedule[attempt]
		} else {
			wait = postRetryInterval
		}

		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(wait):
		}
	}
}

// seekToOffset stats the open file, detects truncation, and seeks to the
// correct position. Returns the resulting file position.
func seekToOffset(f *os.File, offset int64, slug string, emit func(string, string, map[string]string)) int64 {
	st, err := f.Stat()
	if err != nil {
		// Can't stat the open file — unusual but not fatal. Start from
		// current position (0 after open).
		runtimeLog.Warn("entrypoint-log-tailer: stat after open failed",
			"error", err, "sandbox", slug)
		return 0
	}

	currentSize := st.Size()
	if currentSize < offset {
		// Truncation detected: file is shorter than the expected offset.
		emit("WARNING",
			fmt.Sprintf("entrypoint log truncated: file size (%d bytes) is less than expected offset (%d bytes); "+
				"reading from start — this may include lines from a prior run. See PR 1323 (append-mode prerequisite).",
				currentSize, offset),
			map[string]string{"truncation_detected": "true"})
		offset = 0
	}

	pos, err := f.Seek(offset, io.SeekStart)
	if err != nil {
		runtimeLog.Warn("entrypoint-log-tailer: seek failed, reading from current position",
			"offset", offset, "error", err, "sandbox", slug)
		return 0
	}
	return pos
}

// checkTruncation stats the file and, if it has been truncated (current size
// less than our position), resets to the beginning after emitting a warning.
// Returns the updated position.
func checkTruncation(f *os.File, pos int64, logPath, slug string, emit func(string, string, map[string]string)) int64 {
	st, err := f.Stat()
	if err != nil {
		// Stat error — the file may have been deleted. The next Read will
		// surface the error.
		return pos
	}
	if st.Size() < pos {
		emit("WARNING",
			fmt.Sprintf("entrypoint log truncated: file size (%d bytes) is less than current position (%d bytes); "+
				"reading from start — this may include lines from a prior run. See PR 1323 (append-mode prerequisite).",
				st.Size(), pos),
			map[string]string{"truncation_detected": "true"})
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			runtimeLog.Warn("entrypoint-log-tailer: seek-to-0 after truncation failed",
				"error", err, "sandbox", slug)
		}
		return 0
	}
	// Update position to current file position after reads.
	// We track this via stat size when no reads happened (EOF path).
	return pos
}

// emitCompleteLines splits the buffer on newlines, emits each complete line,
// and returns the remainder (the partial trailing content).
func emitCompleteLines(buf []byte, emit func(string, string, map[string]string)) []byte {
	for {
		idx := -1
		for i, b := range buf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			return buf
		}
		line := string(buf[:idx])
		if len(line) > 0 {
			emit("INFO", line, nil)
		}
		buf = buf[idx+1:]
	}
}

// flushPartial emits the remaining buffer content with a partial:true label
// if the buffer is non-empty. This is the single most important behaviour in
// the read loop: without it, the tailer ships every line except the last one,
// and the last one is the one naming the cause of death.
func flushPartial(buf []byte, emit func(string, string, map[string]string)) {
	if len(buf) == 0 {
		return
	}
	emit("INFO", string(buf), map[string]string{"partial": "true"})
}
