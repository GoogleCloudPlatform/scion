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

package tmuxdeliver_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/tmuxdeliver"
)

// socketRunner runs argv locally, with every tmux call on a private server
// socket so the test never touches another tmux session.
type socketRunner struct{ socket string }

func (s socketRunner) Run(ctx context.Context, argv []string, stdin io.Reader) (string, error) {
	if len(argv) > 0 && argv[0] == "tmux" {
		argv = append([]string{"tmux", "-S", s.socket}, argv[1:]...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdin
	out, err := cmd.Output()
	return string(out), err
}

// TestRealTmux_PasteArrivesAsText delivers into a real tmux session whose
// pane writes its input to a file: a pasted paste-end marker arrives as inert
// text, and the submit key ends the line.
func TestRealTmux_PasteArrivesAsText(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		// With SCION_TEST_REQUIRE_TMUX=1 a missing tmux fails instead of
		// skipping (the repo convention, ptone/scion#3312). The CI job that
		// runs SQLITE_NONHUB_PKGS, which lists this package, installs tmux
		// and sets it, so the test cannot silently stop running there.
		if os.Getenv("SCION_TEST_REQUIRE_TMUX") == "1" {
			t.Fatal("tmux is required (SCION_TEST_REQUIRE_TMUX=1) but is not on PATH")
		}
		t.Skip("tmux not on PATH")
	}
	dir := t.TempDir()
	r := socketRunner{socket: filepath.Join(dir, "sock")}
	outFile := filepath.Join(dir, "out")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := r.Run(ctx, []string{"tmux", "new-session", "-d", "-s", "scion", "-x", "200", "-y", "20",
		fmt.Sprintf("cat > %s", outFile)}, nil); err != nil {
		t.Skipf("cannot start tmux: %v", err)
	}
	defer func() { _, _ = r.Run(context.Background(), []string{"tmux", "kill-server"}, nil) }()

	name, err := tmuxdeliver.BufferName("realtmux", 1)
	if err != nil {
		t.Fatal(err)
	}
	req := tmuxdeliver.Request{
		Op:         tmuxdeliver.OpPaste,
		Text:       "hello \x1b[201~world\u009b!",
		SubmitKeys: []string{"Enter"},
		BufferName: name,
	}
	if err := tmuxdeliver.Deliver(ctx, r, req); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	want := "hello [201~world!\n"
	deadline := time.Now().Add(10 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(outFile)
		got = string(b)
		if strings.HasSuffix(got, "\n") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got != want {
		t.Fatalf("pane received %q, want %q", got, want)
	}
	if out, _ := r.Run(ctx, []string{"tmux", "list-buffers"}, nil); strings.Contains(out, name) {
		t.Errorf("buffer %s left behind: %q", name, out)
	}
}

// startBracketedPane starts a tmux session whose pane asks for bracketed
// paste and then writes what it receives, made visible by cat -v, to the
// returned file. It skips the test when tmux is missing or cannot start, and
// returns once tmux has seen the pane's request: tmux brackets a paste only
// for a pane that asked.
func startBracketedPane(ctx context.Context, t *testing.T) (socketRunner, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("SCION_TEST_REQUIRE_TMUX") == "1" {
			t.Fatal("tmux is required (SCION_TEST_REQUIRE_TMUX=1) but is not on PATH")
		}
		t.Skip("tmux not on PATH")
	}
	dir := t.TempDir()
	r := socketRunner{socket: filepath.Join(dir, "sock")}
	outFile := filepath.Join(dir, "out")
	// -f /dev/null: the default mode keys, whatever this machine's tmux.conf
	// says.
	if _, err := r.Run(ctx, []string{"tmux", "-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "200", "-y", "20",
		fmt.Sprintf(`printf '\033[?2004hPANE-READY'; exec cat -v > %s`, outFile)}, nil); err != nil {
		t.Skipf("cannot start tmux: %v", err)
	}
	t.Cleanup(func() { _, _ = r.Run(context.Background(), []string{"tmux", "kill-server"}, nil) })
	// tmux parses the pane's output in order, so once the marker printed
	// after the request is on the screen, the request has been seen.
	waitFor(t, "the pane to ask for bracketed paste", func() bool {
		out, _ := r.Run(ctx, []string{"tmux", "capture-pane", "-p", "-t", tmuxdeliver.Target}, nil)
		_, err := os.Stat(outFile)
		return err == nil && strings.Contains(out, "PANE-READY")
	})
	return r, outFile
}

// enterCopyMode puts the pane in copy-mode, as an operator scrolling back
// does, and fails the test if it is not in a mode afterwards.
func enterCopyMode(ctx context.Context, t *testing.T, r socketRunner) {
	t.Helper()
	if _, err := r.Run(ctx, []string{"tmux", "copy-mode", "-t", tmuxdeliver.Target}, nil); err != nil {
		t.Fatalf("copy-mode: %v", err)
	}
	out, err := r.Run(ctx, []string{"tmux", "display-message", "-p", "-t", tmuxdeliver.Target, "#{pane_in_mode}"}, nil)
	if err != nil || strings.TrimSpace(out) != "1" {
		t.Fatalf("pane_in_mode = %q, %v; want 1", out, err)
	}
}

// waitForOutput waits for the pane to have written want to outFile.
func waitForOutput(t *testing.T, outFile, want string) {
	t.Helper()
	var got string
	waitFor(t, fmt.Sprintf("the pane to receive %q", want), func() bool {
		b, _ := os.ReadFile(outFile)
		got = string(b)
		return got == want
	}, func() string { return fmt.Sprintf("pane received %q", got) })
}

// TestRealTmux_DeliversIntoCopyMode delivers into a pane an operator left in
// copy-mode by scrolling back, running a program that asked for bracketed
// paste. Without leaving the mode first, tmux pastes the text unbracketed
// (each line arrives as typed, ended by a CR) and, with its default emacs
// mode keys, swallows the submit key.
func TestRealTmux_DeliversIntoCopyMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, outFile := startBracketedPane(ctx, t)
	enterCopyMode(ctx, t, r)

	name, err := tmuxdeliver.BufferName("copymode", 1)
	if err != nil {
		t.Fatal(err)
	}
	req := tmuxdeliver.Request{
		Op:         tmuxdeliver.OpPaste,
		Text:       "two\nlines",
		SubmitKeys: []string{"Enter"},
		BufferName: name,
	}
	if err := tmuxdeliver.Deliver(ctx, r, req); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	waitForOutput(t, outFile, "^[[200~two\nlines^[[201~\n")
}

// scrollingRunner is an operator who scrolls back while a delivery runs: it
// puts the pane in copy-mode after each command whose tmux verb is in after.
type scrollingRunner struct {
	socketRunner
	t     *testing.T
	after map[string]bool
}

func (s scrollingRunner) Run(ctx context.Context, argv []string, stdin io.Reader) (string, error) {
	out, err := s.socketRunner.Run(ctx, argv, stdin)
	if len(argv) > 1 && s.after[argv[1]] {
		enterCopyMode(ctx, s.t, s.socketRunner)
	}
	return out, err
}

// TestRealTmux_OperatorScrollsDuringDelivery: the pane leaves copy-mode
// right before the paste and before each extra submit pass, not only at the
// start. An operator who scrolls back while the buffer loads, and again
// during the extra submit delay, still gets a bracketed paste and both
// submits.
func TestRealTmux_OperatorScrollsDuringDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, outFile := startBracketedPane(ctx, t)

	name, err := tmuxdeliver.BufferName("scrolling", 1)
	if err != nil {
		t.Fatal(err)
	}
	req := tmuxdeliver.Request{
		Op:           tmuxdeliver.OpPaste,
		Text:         "two\nlines",
		SubmitKeys:   []string{"Enter"},
		ExtraSubmits: 1,
		BufferName:   name,
	}
	sr := scrollingRunner{socketRunner: r, t: t, after: map[string]bool{"load-buffer": true, "send-keys": true}}
	if err := tmuxdeliver.Deliver(ctx, sr, req); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	waitForOutput(t, outFile, "^[[200~two\nlines^[[201~\n\n")
}

// waitFor polls cond for up to 10 seconds and fails the test, naming what,
// if it never holds. detail, when given, adds the last observed state.
func waitFor(t *testing.T, what string, cond func() bool, detail ...func() string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	msg := "timed out waiting for " + what
	for _, d := range detail {
		msg += ": " + d()
	}
	t.Fatal(msg)
}
