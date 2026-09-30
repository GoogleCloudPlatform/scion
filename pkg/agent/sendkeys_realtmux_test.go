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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// requireTmuxVersionFloor skips the calling real-tmux test, rather than
// merely logging a version, if the tmux binary at tmuxPath does not meet
// SendKeys's delivery mechanism minimum (contract §2.3's "Transport
// requirements"; see minTmuxMajor/minTmuxMinor in manager.go): below that
// floor, "tmux source-file -" cannot read a script from stdin at all, so a
// real-tmux test would otherwise fail for a reason unrelated to what it is
// actually checking.
func requireTmuxVersionFloor(t *testing.T, tmuxPath string) {
	t.Helper()
	out, err := exec.Command(tmuxPath, "-V").CombinedOutput()
	if err != nil {
		t.Fatalf("tmux -V failed: %v (%s)", err, out)
	}
	version := strings.TrimSpace(string(out))
	t.Logf("running against %s", version)
	major, minor, perr := parseTmuxVersion(version)
	if perr != nil {
		t.Fatalf("could not parse tmux version from %q: %v", version, perr)
	}
	if !tmuxVersionAtLeast(major, minor, minTmuxMajor, minTmuxMinor) {
		t.Skipf("tmux %s is below the minimum version %d.%d the delivery mechanism requires", version, minTmuxMajor, minTmuxMinor)
	}
}

// TestRealTmuxSendKeys drives AgentManager.SendKeys's actual delivery
// mechanism against a private, disposable tmux server — never an active
// agent terminal, per the campaign's hard constraint — verifying named keys
// (Enter, Escape), Unicode, spaces, literal "@text", quotes, a backslash, a
// dollar sign, semicolons (embedded and trailing), a multiline string, and
// directive-like content resembling a tmux command all reach the target
// exactly, with no automatic Enter added by SendKeys itself and no
// content ever interpreted as anything other than literal input or (for the
// two named keys) the single keypress it names.
//
// Delivery goes through "tmux source-file -" over stdin (see
// sendKeysScript): the mock's ExecWithStdinFunc pipes the generated command
// to a real tmux binary exactly as AgentManager.ExecWithStdin does, so this
// exercises the real encode/decode round trip, not just the encoder's own
// unit test.
func TestRealTmuxSendKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-tmux integration test in short mode")
	}

	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed; skipping real-tmux integration test")
	}
	requireTmuxVersionFloor(t, tmuxPath)

	dir, err := os.MkdirTemp("", "tmxk")
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

	t.Cleanup(func() {
		_, _ = runTmux("kill-server")
	})

	// A pane running "cat" redirected to a file stands in for the agent's
	// terminal input. Session named "scion" so it matches SendKeys's
	// hardcoded "-t scion:0" target.
	mustTmux("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "220", "-y", "50", "cat > "+outFile)

	shim := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				ContainerID: "local",
				Name:        "test-agent",
				Phase:       string(state.PhaseRunning),
				Labels: map[string]string{
					"scion.name": "test-agent",
					"agent_id":   "agent-abc",
				},
			}}, nil
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

	var want strings.Builder

	send := func(keys string) {
		t.Helper()
		if err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", keys); err != nil {
			t.Fatalf("SendKeys(%q) failed: %v", keys, err)
		}
	}
	// literal sends keys and records it as literal text in the expected
	// output — for content that is not a recognized tmux key name.
	literal := func(keys string) {
		send(keys)
		want.WriteString(keys)
	}

	// cat's stdout is block-buffered here (not a tty), so intermediate
	// writes are not guaranteed to reach outFile until EOF flushes the
	// buffer — unlike TestRealTmuxLoadBufferDeliversLargePayload's 200 KB
	// payload, which exceeds the buffer and can be polled incrementally. So
	// every send below happens before a single end-of-sequence poll rather
	// than being checked one at a time.

	// Literal text containing Unicode, spaces and a literal "@name" — must
	// reach the pane byte-for-byte, never parsed or tokenized.
	literal("héllo @builder 世界 do the thing")

	// A trailing ';', content already ending in a literal backslash
	// followed by ';' (0 and 1 "extra" backslash before the trailing ';'
	// respectively), and an embedded (non-trailing) ';' — every one of
	// these must reach the pane as exactly itself.
	literal(";")
	literal("a;")
	literal(`a\;`)
	literal(`a\\;`)
	literal("a;b;c")

	// A double quote and a backslash — the two characters the generated
	// tmux command's own quoting would otherwise need to treat specially —
	// and a dollar sign, all octal-escaped like every other byte, so none
	// of them are ever interpreted by tmux's command parser.
	literal(`quote"inside`)
	literal(`back\slash`)
	literal("dollar$sign")

	// A multiline string: newlines inside the payload are just more escaped
	// bytes, not line breaks in the generated command file.
	literal("line1\nline2")

	// Directive-like content resembling a tmux command a naive
	// implementation might accidentally let escape the intended single
	// argument. If this were ever interpreted as a real command instead of
	// literal text, "kill-server" would tear down the whole tmux server
	// this test depends on, failing loudly rather than silently.
	literal(`"; kill-server; #`)

	// "Up Up Enter" is the contract's own §2.3 whole-argument case: as one
	// string containing spaces, it does not
	// match any single named key exactly, so it must be typed as 11 literal
	// characters, never interpreted as three separate keypresses.
	literal("Up Up Enter")

	// "Enter" is a recognized tmux key name and must be interpreted as an
	// actual keypress (arriving as a bare LF via the pty's ICRNL
	// translation), not typed as the five literal characters "Enter".
	send("Enter")
	want.WriteString("\n")

	// "Escape" is also a recognized tmux key name; cat has no ESC-driven
	// behavior of its own, so it should just pass the literal ESC (0x1b)
	// byte through, not the six literal characters "Escape".
	send("Escape")
	want.WriteString("\x1b")

	// "Up" is a recognized arrow-key name — checked as its actual escape
	// sequence, not just that *something* was
	// sent. In the pane's default (non-application) cursor-key mode this is
	// the ANSI CSI sequence ESC '[' 'A', confirmed by hand against this
	// tmux binary.
	send("Up")
	want.WriteString("\x1b[A")

	// "Tab" is a recognized named key producing a single control byte,
	// confirmed by hand against this tmux
	// binary to arrive as a bare horizontal tab, not the three literal
	// characters "Tab".
	send("Tab")
	want.WriteString("\t")

	mustTmux("send-keys", "-t", "scion:0", "C-d")

	// Wait for the pane's output to settle before reading it.
	const quietFor = 100 * time.Millisecond
	deadline := time.Now().Add(5 * time.Second)
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
			t.Fatalf("timed out waiting for pane output to settle (last size %d)", lastSize)
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading pane output: %v", err)
	}

	if string(got) != want.String() {
		t.Fatalf("pane output = %q, want %q", got, want.String())
	}
}

// TestRealTmuxSendKeys_NoSessionIsTerminalNotReady covers the case where,
// against a real tmux server that has no "scion" session at
// all, the readiness probe's "has-session" must genuinely fail, and SendKeys
// must report that truthfully as ErrTerminalNotReady, with no "source-file"
// delivery call ever attempted.
func TestRealTmuxSendKeys_NoSessionIsTerminalNotReady(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-tmux integration test in short mode")
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed; skipping real-tmux integration test")
	}
	requireTmuxVersionFloor(t, tmuxPath)

	dir, err := os.MkdirTemp("", "tmxk-nosession")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sock")

	runTmux := func(args ...string) (string, error) {
		cmd := exec.Command(tmuxPath, append([]string{"-S", sock}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("tmux %v failed: %w (%s)", args, err, out)
		}
		return string(out), nil
	}
	t.Cleanup(func() { _, _ = runTmux("kill-server") })

	// Start the server with an unrelated session so it genuinely exists,
	// but never create a "scion" session — has-session must fail
	// truthfully, not because the server itself is unreachable.
	if _, err := runTmux("-f", "/dev/null", "new-session", "-d", "-s", "other"); err != nil {
		t.Fatalf("starting tmux server: %v", err)
	}

	var sourceFileCalled bool
	shim := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				ContainerID: "local",
				Name:        "test-agent",
				Phase:       string(state.PhaseRunning),
				Labels: map[string]string{
					"scion.name": "test-agent",
					"agent_id":   "agent-abc",
				},
			}}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			return runTmux(cmd[1:]...)
		},
		ExecWithStdinFunc: func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
			sourceFileCalled = true
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

	err = mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, agentkeys.ErrTerminalNotReady) {
		t.Fatalf("SendKeys error = %v, want agentkeys.ErrTerminalNotReady", err)
	}
	if sourceFileCalled {
		t.Fatal("SendKeys must not attempt delivery ('source-file') once the readiness probe genuinely fails")
	}
}

// TestRealTmuxSendKeys_DeliveryFailureAfterProbeIsAmbiguous covers the
// case where, once a real tmux readiness probe has genuinely
// succeeded, a real delivery-time failure — here, the target session is
// removed between the probe and the delivery call — must reach SendKeys as
// a plain error matching none of the proven-before-execution sentinels.
func TestRealTmuxSendKeys_DeliveryFailureAfterProbeIsAmbiguous(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-tmux integration test in short mode")
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed; skipping real-tmux integration test")
	}
	requireTmuxVersionFloor(t, tmuxPath)

	dir, err := os.MkdirTemp("", "tmxk-killrace")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sock")

	runTmux := func(args ...string) (string, error) {
		cmd := exec.Command(tmuxPath, append([]string{"-S", sock}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("tmux %v failed: %w (%s)", args, err, out)
		}
		return string(out), nil
	}
	t.Cleanup(func() { _, _ = runTmux("kill-server") })

	if _, err := runTmux("-f", "/dev/null", "new-session", "-d", "-s", "scion"); err != nil {
		t.Fatalf("starting tmux server: %v", err)
	}
	// A second, unrelated session so the server stays alive once "scion" is
	// killed below: without it, killing the
	// only session also exits the server, and the delivery call would fail
	// because no server is reachable at all, rather than because the
	// target session is missing — a different failure than the one this
	// test means to exercise.
	if _, err := runTmux("-f", "/dev/null", "new-session", "-d", "-s", "other"); err != nil {
		t.Fatalf("starting the unrelated keep-alive session: %v", err)
	}

	shim := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				ContainerID: "local",
				Name:        "test-agent",
				Phase:       string(state.PhaseRunning),
				Labels: map[string]string{
					"scion.name": "test-agent",
					"agent_id":   "agent-abc",
				},
			}}, nil
		},
		// The readiness probe runs while the "scion" session still exists,
		// so it genuinely succeeds.
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			return runTmux(cmd[1:]...)
		},
		ExecWithStdinFunc: func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
			// Remove the target session (but not the server, which the
			// unrelated "other" session above keeps alive) between the
			// successful probe and the actual delivery, so the real tmux
			// binary's send-keys command (run inside "source-file -")
			// genuinely fails with "can't find session: scion" against a
			// target that no longer exists — a real, not simulated,
			// missing-target delivery-time failure, not merely "no server
			// reachable."
			if _, err := runTmux("kill-session", "-t", "scion"); err != nil {
				t.Fatalf("killing session ahead of delivery: %v", err)
			}
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

	err = mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if err == nil {
		t.Fatal("expected a genuine delivery-time failure once the session was removed after the probe")
	}
	if errors.Is(err, agentkeys.ErrTargetNotFound) || errors.Is(err, agentkeys.ErrAgentNotRunning) || errors.Is(err, agentkeys.ErrTerminalNotReady) || errors.Is(err, ErrKeysNotStarted) {
		t.Fatalf("a real delivery-time failure must never be reported as one of the proven-before-execution sentinels, got: %v", err)
	}
	// Confirms this is genuinely the missing-target failure the correction
	// names, not merely "no server reachable":
	// the "other" session above keeps the server alive after "scion" is
	// killed, so tmux's own real error text for a missing session must
	// survive into SendKeys's returned error.
	if !strings.Contains(err.Error(), "can't find session") {
		t.Fatalf("expected tmux's real missing-session error text to survive, got: %v", err)
	}
}
