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
// every other test in this package does). It drives a local tmux server
// directly, so it needs no container runtime: the target pane runs under a
// private, temporary tmux server rather than inside a container. The test is
// skipped in short mode and when tmux itself is not installed.
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

	// Best-effort; exit-empty may already have stopped the server. Uses
	// t.Cleanup (not defer) and is registered before new-session, so it
	// runs even if that call fails. LIFO order means it runs before the
	// socket dir is removed.
	t.Cleanup(func() {
		_, _ = runTmux("kill-server")
	})

	// A pane running "cat" redirected to a file stands in for the agent's
	// terminal input: whatever is pasted into the pane arrives on cat's
	// stdin, and cat writes it back out verbatim. The session is named
	// "scion" so it matches deliverImmediate's hardcoded "-t scion:0" target.
	// "-f /dev/null" keeps the new server from loading the invoking user's
	// ~/.tmux.conf, which can otherwise change pane behavior (e.g.
	// remain-on-exit, a default-command) and make the test environment-
	// dependent; it only needs to be on the call that starts the server.
	mustTmux("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "220", "-y", "50", "cat > "+outFile)

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

	// Wait for outFile to stop growing rather than re-checking the same
	// size threshold the first poll already satisfied (that would return
	// immediately and prove nothing new): a second, unwanted paste lands
	// after the first and takes its own moment to arrive, so the file needs
	// to be quiet for a bit before it's safe to say nothing more is coming.
	const quietFor = 100 * time.Millisecond
	deadline := time.Now().Add(10 * time.Second)
	lastSize := int64(-1)
	quietSince := time.Now()
	for {
		info, statErr := os.Stat(outFile)
		if statErr == nil {
			if info.Size() != lastSize {
				lastSize = info.Size()
				quietSince = time.Now()
			} else if time.Since(quietSince) >= quietFor {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the pane output to settle (last size %d)", lastSize)
		}
		time.Sleep(20 * time.Millisecond)
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

	// The payload must land byte-for-byte, and exactly once: a double paste
	// would reappear right after the first copy, not blend into it.
	if !bytes.HasPrefix(stripped, []byte(payload)) {
		t.Fatalf("pane output did not start with the payload byte-for-byte: got %d bytes, want a prefix of length %d", len(stripped), len(payload))
	}
	// deliverImmediate also sends a trailing confirmation Enter keypress
	// after the paste, unrelated to the payload itself, which arrives as a
	// bare LF (via the pty's ICRNL translation of the Enter's CR). Bound
	// what follows the payload to just that, rather than accepting any
	// trailing bytes: a second paste would put a full, non-"\n" copy of the
	// payload there instead, and this catches it.
	tail := stripped[len(payload):]
	for _, c := range tail {
		if c != '\n' {
			prefixLen := len(tail)
			if prefixLen > 200 {
				prefixLen = 200
			}
			t.Fatalf("pane output carried more than the payload plus trailing Enters: got %d extra byte(s) after the payload, starting %q", len(tail), tail[:prefixLen])
		}
	}
}

// TestRealTmuxDeliversIntoCopyMode checks against a real tmux server that a
// message delivered while the pane is in copy-mode (left there by an
// operator's scroll) is both pasted and submitted. A send-keys Enter would be
// dispatched through copy-mode's key table and never reach the pane; the
// pasted-CR submit must arrive, and must leave the pane in copy-mode.
func TestRealTmuxDeliversIntoCopyMode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-tmux integration test in short mode")
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed; skipping real-tmux integration test")
	}

	dir, err := os.MkdirTemp("", "tmx")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sock")
	outFile := filepath.Join(dir, "out")

	runTmux := func(args ...string) (string, error) {
		out, err := exec.Command(tmuxPath, append([]string{"-S", sock}, args...)...).CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("tmux %v failed: %w (%s)", args, err, out)
		}
		return string(out), nil
	}
	t.Cleanup(func() { _, _ = runTmux("kill-server") })

	// "cat" in canonical tty mode only writes a line once it is submitted,
	// so the message text reaching outFile proves the CR arrived too.
	if _, err := runTmux("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "120", "-y", "30", "cat > "+outFile); err != nil {
		t.Fatal(err)
	}
	if _, err := runTmux("copy-mode", "-t", "scion:0"); err != nil {
		t.Fatal(err)
	}

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
	if err := mgr.deliverImmediate(context.Background(), "test-agent", "", "hello from copy-mode", false); err != nil {
		t.Fatalf("deliverImmediate failed: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		got, _ := os.ReadFile(outFile)
		if strings.Contains(string(got), "hello from copy-mode\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("message was not submitted from copy-mode; pane output %q", got)
		}
		time.Sleep(20 * time.Millisecond)
	}

	inMode, err := runTmux("display-message", "-p", "-t", "scion:0", "#{pane_in_mode}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(inMode) != "1" {
		t.Errorf("delivery left copy-mode; the operator's scroll position would be lost (pane_in_mode=%q)", inMode)
	}

	bufs, err := runTmux("list-buffers", "-F", "#{buffer_name}")
	if err != nil && !strings.Contains(err.Error(), "no buffers") {
		t.Fatal(err)
	}
	for _, name := range strings.Fields(bufs) {
		if strings.HasPrefix(name, msgBufferPrefix) || strings.HasPrefix(name, submitBufferPrefix) {
			t.Errorf("delivery left buffer %q behind", name)
		}
	}
}
