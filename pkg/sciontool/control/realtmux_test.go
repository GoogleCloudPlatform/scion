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

package control

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
)

// TestRealTmux_Deliver512KiB delivers a 512 KiB body through the route and
// the local Runner into a real tmux session on a private server: it arrives
// byte-exact, and the per-delivery buffer is gone afterwards.
func TestRealTmux_Deliver512KiB(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("SCION_TEST_REQUIRE_TMUX") == "1" {
			t.Fatal("tmux is required (SCION_TEST_REQUIRE_TMUX=1) but is not on PATH")
		}
		t.Skip("tmux not on PATH")
	}
	dir := t.TempDir()
	t.Setenv("TMUX_TMPDIR", dir) // a private tmux server for this test
	outFile := filepath.Join(dir, "out")
	run := NewLocalRunner(0, 0, "", false)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := run.Run(ctx, []string{"tmux", "new-session", "-d", "-s", "scion", "-x", "200", "-y", "20",
		"stty -echo; cat > " + outFile}, nil); err != nil {
		t.Skipf("cannot start tmux: %v", err)
	}
	defer func() { _, _ = run.Run(context.Background(), []string{"tmux", "kill-server"}, nil) }()

	// Lines stay well under the tty's canonical line limit.
	var b strings.Builder
	for i := 0; b.Len() < agentcontrol.MaxDeliverText-200; i++ {
		fmt.Fprintf(&b, "line %06d %s\n", i, strings.Repeat("x", 80))
	}
	text := strings.TrimSuffix(b.String(), "\n")
	r := baseReq(time.Now())
	r.Text = &text
	r.ExtraSubmits = nil
	h := New(Options{KickTokenRefresh: func() {}, Deliver: &DeliverOptions{Runner: run}})
	code, resp := post(t, h, r)
	if code != http.StatusOK || resp.Outcome != agentcontrol.OutcomeDelivered {
		t.Fatalf("= %d %+v", code, resp)
	}
	want := text + "\n"
	var got string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(outFile)
		got = string(data)
		if len(got) >= len(want) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got != want {
		t.Fatalf("pane received %d bytes, want %d (byte-exact)", len(got), len(want))
	}
	if out, _ := run.Run(ctx, []string{"tmux", "list-buffers"}, nil); strings.Contains(out, "scion-msg-d-1") {
		t.Errorf("buffer left behind: %q", out)
	}
}
