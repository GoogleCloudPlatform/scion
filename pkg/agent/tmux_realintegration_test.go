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
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestRealTmuxLoadBufferDeliversLargePayload is an end-to-end check against a
// real tmux server (ptone/scion#2256): it drives deliverImmediate's actual
// argv through a thin Runtime shim onto a real tmux socket, and verifies a
// 200 KB message arrives at the receiving process byte-for-byte — well past
// the 16 KB argv cap that broke "tmux set-buffer -- <message>".
//
// This exercises tmux itself rather than mocking the Runtime abstraction (as
// every other test in this package does). A container runtime (docker) was
// not available in the environment this test was written in, so the target
// pane runs under a private, temporary local tmux server instead of inside a
// container; this test is skipped in short mode and when tmux itself is not
// installed.
func TestRealTmuxLoadBufferDeliversLargePayload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-tmux integration test in short mode")
	}

	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed; skipping real-tmux integration test")
	}

	// A short directory (os.MkdirTemp rather than t.TempDir, which embeds the
	// full test name) keeps the socket path within the Unix sun_path limit,
	// which a long TMPDIR or test name can otherwise exceed on macOS.
	dir, err := os.MkdirTemp("", "tmx")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "sock")
	outFile := filepath.Join(dir, "out")

	runTmux := func(args ...string) (string, error) {
		cmd := exec.Command(tmuxPath, append([]string{"-S", sock}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("tmux %v failed: %w (%s)", args, err, out)
		}
		return string(out), nil
	}
	mustTmux := func(args ...string) string {
		t.Helper()
		out, err := runTmux(args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// A pane running "cat" redirected to a file stands in for the agent's
	// terminal input: whatever is pasted into the pane arrives on cat's
	// stdin, and cat writes it back out verbatim. The session is named
	// "scion" so it matches deliverImmediate's hardcoded "-t scion:0" target.
	// "-f /dev/null" keeps the new server from loading the invoking user's
	// ~/.tmux.conf, which can otherwise change pane behavior (e.g.
	// remain-on-exit, a default-command) and make the test environment-
	// dependent; it only needs to be on the call that starts the server.
	mustTmux("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "220", "-y", "50", "cat > "+outFile)
	// Best-effort cleanup: once cat exits below (via C-d), tmux's default
	// exit-empty behavior tears the server down on its own, so a later
	// kill-server legitimately finds nothing left to kill.
	defer func() {
		_, _ = runTmux("kill-server")
	}()

	// Build a payload well past the 16 KB argv cap that broke set-buffer,
	// containing newlines, quotes and angle brackets.
	var b strings.Builder
	line := `line with "quotes", <angle> brackets & an ampersand` + "\n"
	for b.Len() < 200000 {
		b.WriteString(line)
	}
	payload := b.String()

	// Drive deliverImmediate's actual argv against the private tmux socket
	// via a thin Runtime shim, rather than a hand-copy of its commands: if
	// the argv deliverImmediate builds ever changes, this test exercises it
	// directly instead of a stale copy that would keep passing regardless.
	shim := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{ContainerID: "local", Name: "test-agent", Labels: map[string]string{"scion.name": "test-agent"}},
			}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			return runTmux(cmd[1:]...)
		},
		ExecWithStdinFunc: func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
			c := exec.Command(tmuxPath, append([]string{"-S", sock}, cmd[1:]...)...)
			c.Stdin = stdin
			out, err := c.CombinedOutput()
			if err != nil {
				return string(out), fmt.Errorf("tmux %v failed: %w (%s)", cmd[1:], err, out)
			}
			return string(out), nil
		},
	}
	mgr := &AgentManager{Runtime: shim}

	if err := mgr.deliverImmediate(context.Background(), "test-agent", "", payload, false); err != nil {
		t.Fatalf("deliverImmediate failed: %v", err)
	}

	// Bounded poll on the pane's output instead of a fixed sleep before
	// sending EOF: GNU/busybox cat writes each read immediately, so outFile
	// grows as the paste lands, and this lets the test proceed as soon as it
	// has rather than depending on a timing-sensitive guess.
	wantSize := int64(len(payload))
	pollDeadline := time.Now().Add(10 * time.Second)
	for {
		if info, statErr := os.Stat(outFile); statErr == nil && info.Size() >= wantSize {
			break
		}
		if time.Now().After(pollDeadline) {
			t.Fatal("timed out waiting for the paste to reach the output file")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Send EOF so cat exits and flushes its stdio buffer to outFile. Killing
	// the server first (SIGHUP) can terminate cat before its last,
	// not-yet-full stdio buffer is flushed, truncating the tail of the file.
	// cat exiting also tears down the pane's only session, so the tmux
	// server itself may exit immediately afterward (default exit-empty) —
	// this is expected and is not polled for; only the file content is.
	mustTmux("send-keys", "-t", "scion:0", "C-d")

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

	// deliverImmediate also sends trailing confirmation Enter keypresses
	// after the paste, unrelated to the payload itself, so the pane output
	// may contain a little more than the payload. Check that the payload
	// landed byte-for-byte as a prefix rather than requiring exact equality.
	if !bytes.HasPrefix(stripped, []byte(payload)) {
		t.Fatalf("pane output did not start with the payload byte-for-byte: got %d bytes, want a prefix of length %d", len(stripped), len(payload))
	}
}
