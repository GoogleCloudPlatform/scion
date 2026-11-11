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
