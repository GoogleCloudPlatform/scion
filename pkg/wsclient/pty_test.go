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

package wsclient

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/term"
)

func TestTerminalResetSequences(t *testing.T) {
	// Verify all expected escape sequences are present.
	expectedSequences := []struct {
		seq  string
		desc string
	}{
		{"\x1b[?1049l", "exit alternate screen buffer (rmcup)"},
		{"\x1b[?25h", "show cursor (cnorm)"},
		{"\x1b[r", "reset scroll region"},
		{"\x1b[?1000l", "disable mouse click tracking"},
		{"\x1b[?1002l", "disable mouse drag tracking"},
		{"\x1b[?1003l", "disable mouse all-motion tracking"},
		{"\x1b[?1006l", "disable SGR mouse mode"},
		{"\x1b[?2004l", "disable bracketed paste mode"},
	}

	for _, tc := range expectedSequences {
		if !strings.Contains(terminalResetSequences, tc.seq) {
			t.Errorf("terminalResetSequences missing %s (%s)", tc.desc, tc.seq)
		}
	}
}

func TestRestoreTerminalWritesResetSequences(t *testing.T) {
	// Capture what restoreTerminal writes to stdout by temporarily
	// replacing os.Stdout with a pipe.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	// Create a PTYClient with a non-nil termState so restoreTerminal
	// enters the reset path. We use a fake term.State obtained by saving
	// the pipe fd's state (the fd won't be a real terminal, so
	// term.Restore will be a no-op / error, but the escape sequences
	// should still be written).
	client := &PTYClient{
		oldFd:     int(w.Fd()),
		termState: &term.State{}, // non-nil sentinel
	}

	client.restoreTerminal(true)

	// Close the write end so the read end sees EOF.
	_ = w.Close()
	os.Stdout = origStdout

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	_ = r.Close()

	output := string(buf[:n])
	if output != terminalResetSequences {
		t.Errorf("restoreTerminal output = %q, want %q", output, terminalResetSequences)
	}
}

func TestRestoreTerminalSkipsResetOnError(t *testing.T) {
	// When writeResetSeqs is false (error path), restoreTerminal should
	// not write escape sequences so error output remains visible.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	client := &PTYClient{
		oldFd:     int(w.Fd()),
		termState: &term.State{}, // non-nil sentinel
	}

	client.restoreTerminal(false)

	_ = w.Close()
	os.Stdout = origStdout

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	_ = r.Close()

	if n != 0 {
		t.Errorf("restoreTerminal on error wrote %d bytes: %q, want no output", n, string(buf[:n]))
	}
}

func TestRestoreTerminalNoOpWhenNoState(t *testing.T) {
	// When termState is nil, restoreTerminal should not write anything.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	client := &PTYClient{
		oldFd:     int(w.Fd()),
		termState: nil,
	}

	client.restoreTerminal(true)

	_ = w.Close()
	os.Stdout = origStdout

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	_ = r.Close()

	if n != 0 {
		t.Errorf("restoreTerminal with nil termState wrote %d bytes: %q", n, string(buf[:n]))
	}
}
