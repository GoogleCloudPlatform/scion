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

package agent

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealTmuxLoadBufferDeliversLargePayload is an end-to-end check against a
// real tmux server (ptone/scion#2256): it loads 200 KB through the exact
// "tmux load-buffer -b <name> -" / "tmux paste-buffer ... -p -d -b <name>"
// argv pair deliverImmediate uses, and verifies the payload arrives at the
// receiving process byte-for-byte — well past the 16 KB argv cap that broke
// "tmux set-buffer -- <message>".
//
// This exercises tmux itself rather than our Runtime abstraction (Exec and
// ExecWithStdin are mocked everywhere else in this package). A container
// runtime (docker) was not available in the environment this test was
// written in, so the target pane runs under a private, temporary local tmux
// server instead of inside a container; this test is skipped when tmux
// itself is not installed.
func TestRealTmuxLoadBufferDeliversLargePayload(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed; skipping real-tmux integration test")
	}

	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")
	outFile := filepath.Join(dir, "out")

	runTmux := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(tmuxPath, append([]string{"-S", sock}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v failed: %v (%s)", args, err, out)
		}
		return out
	}

	// A pane running "cat" redirected to a file stands in for the agent's
	// terminal input: whatever is pasted into the pane arrives on cat's
	// stdin, and cat writes it back out verbatim.
	runTmux("new-session", "-d", "-s", "t", "-x", "220", "-y", "50", "cat > "+outFile)
	// Best-effort cleanup: once cat exits below (via C-d), tmux's default
	// exit-empty behavior tears the server down on its own, so a later
	// kill-server legitimately finds nothing left to kill.
	defer func() {
		_ = exec.Command(tmuxPath, "-S", sock, "kill-server").Run()
	}()

	// Build a payload well past the 16 KB argv cap that broke set-buffer,
	// containing newlines, quotes and angle brackets.
	var b strings.Builder
	line := `line with "quotes", <angle> brackets & an ampersand` + "\n"
	for b.Len() < 200000 {
		b.WriteString(line)
	}
	payload := b.String()

	loadCmd := exec.Command(tmuxPath, "-S", sock, "load-buffer", "-b", msgBufferName, "-")
	loadCmd.Stdin = strings.NewReader(payload)
	if out, err := loadCmd.CombinedOutput(); err != nil {
		t.Fatalf("tmux load-buffer failed: %v (%s)", err, out)
	}

	// Mirror deliverImmediate's paste-buffer argv exactly (same -p, -d, -b
	// <name> shape; only the target differs because this test's session
	// isn't named "scion"). Note: tmux only wraps the paste in bracketed-paste
	// escape codes when the destination application has itself requested
	// bracketed-paste mode; plain "cat" never does, so no markers appear here.
	runTmux("paste-buffer", "-t", "t:0.0", "-p", "-d", "-b", msgBufferName)

	// Give tmux time to finish feeding the pasted bytes into the pane's pty
	// before sending EOF; sending EOF too early truncates the paste.
	time.Sleep(1 * time.Second)

	// Send EOF so cat exits and flushes its stdio buffer to outFile. Killing
	// the server first (SIGHUP) can terminate cat before its last,
	// not-yet-full stdio buffer is flushed, truncating the tail of the file.
	// cat exiting also tears down the pane's only session, so the tmux
	// server itself may exit immediately afterward (default exit-empty) —
	// this is expected and is not polled for; only the file content is.
	runTmux("send-keys", "-t", "t:0.0", "C-d")

	wantSize := int64(len(payload))

	deadline := time.Now().Add(10 * time.Second)
	for {
		if info, err := os.Stat(outFile); err == nil && info.Size() >= wantSize {
			break
		}
		if time.Now().After(deadline) {
			size := int64(-1)
			if info, statErr := os.Stat(outFile); statErr == nil {
				size = info.Size()
			}
			t.Fatalf("timed out waiting for the pasted payload to reach the output file (want >= %d bytes, got %d)", wantSize, size)
		}
		time.Sleep(50 * time.Millisecond)
	}

	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading pane output: %v", err)
	}
	// Defensively strip bracketed-paste markers in case some environment's
	// tmux does add them for a non-bracketed-paste-aware destination; a real
	// harness handles these itself, "cat" would just copy them through.
	stripped := bytes.TrimPrefix(got, []byte("\x1b[200~"))
	stripped = bytes.TrimSuffix(stripped, []byte("\x1b[201~"))

	if string(stripped) != payload {
		t.Fatalf("pane output did not match payload byte-for-byte: got %d bytes, want %d bytes", len(stripped), len(payload))
	}
}
