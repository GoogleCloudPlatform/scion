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

package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildLookCmd(t *testing.T) {
	tests := []struct {
		name     string
		plain    bool
		full     bool
		numLines int
		want     []string
	}{
		{
			name: "default flags",
			want: []string{"tmux", "capture-pane", "-pe", "-t", "scion"},
		},
		{
			name:  "plain only",
			plain: true,
			want:  []string{"tmux", "capture-pane", "-p", "-t", "scion"},
		},
		{
			name: "full only",
			full: true,
			want: []string{"tmux", "capture-pane", "-peS", "-", "-t", "scion"},
		},
		{
			name:  "plain and full",
			plain: true,
			full:  true,
			want:  []string{"tmux", "capture-pane", "-pS", "-", "-t", "scion"},
		},
		{
			name:     "num-lines",
			numLines: 75,
			want:     []string{"tmux", "capture-pane", "-peS", "-75", "-t", "scion"},
		},
		{
			name:     "num-lines with plain",
			plain:    true,
			numLines: 100,
			want:     []string{"tmux", "capture-pane", "-pS", "-100", "-t", "scion"},
		},
		{
			name:     "num-lines overrides full",
			full:     true,
			numLines: 50,
			want:     []string{"tmux", "capture-pane", "-peS", "-50", "-t", "scion"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := buildLookCmd(tt.plain, tt.full, tt.numLines)
			assert.Equal(t, tt.want, cmd)
		})
	}
}

func TestPrintLookOutput_NonInteractive(t *testing.T) {
	// Save and restore global state.
	origNonInteractive := nonInteractive
	defer func() { nonInteractive = origNonInteractive }()
	nonInteractive = true

	// Capture stdout.
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printLookOutput("hello world\n")

	_ = w.Close()
	os.Stdout = oldStdout

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	got := string(buf[:n])

	// Non-interactive: output should be printed verbatim with no border.
	assert.Equal(t, "hello world\n", got)
	assert.NotContains(t, got, "⌄")
	assert.NotContains(t, got, "^")
}

func TestPrintLookOutput_FallbackWhenNotTerminal(t *testing.T) {
	// In test environment, stdout is a pipe so term.GetSize will fail,
	// which exercises the fallback (no border) path.
	origNonInteractive := nonInteractive
	defer func() { nonInteractive = origNonInteractive }()
	nonInteractive = false

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printLookOutput("some output\n")

	_ = w.Close()
	os.Stdout = oldStdout

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	got := string(buf[:n])

	// Fallback: no border because stdout is not a terminal.
	assert.Equal(t, "some output\n", got)
	assert.NotContains(t, got, "⌄")
	assert.NotContains(t, got, "^")
}

// lookExecServer is a fake hub whose exec endpoint returns execBody and whose
// agent lookup returns an agent with no identity header to show.
func lookExecServer(t *testing.T, execBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/p1/agents/worker/exec":
			_, _ = w.Write([]byte(execBody))
		case "/api/v1/projects/p1/agents/worker":
			_, _ = w.Write([]byte(`{"id": "a1", "slug": "worker"}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func runLookViaHub(t *testing.T, execBody string) (stdout, stderr string, err error) {
	t.Helper()
	server := lookExecServer(t, execBody)
	defer server.Close()
	client, cerr := hubclient.New(server.URL)
	require.NoError(t, cerr)
	origNonInteractive := nonInteractive
	nonInteractive = true
	defer func() { nonInteractive = origNonInteractive }()
	stdout, stderr = captureStdoutStderr(t, func() {
		err = lookViaHub(&HubContext{Client: client, Endpoint: server.URL, ProjectID: "p1"}, "worker", []string{"tmux"})
	})
	return stdout, stderr, err
}

// A successful capture prints the pane text, and only the pane text, on
// stdout.
func TestLookViaHub_Success(t *testing.T) {
	stdout, _, err := runLookViaHub(t, `{"output": "pane line 1\npane line 2\n", "exitCode": 0}`)
	require.NoError(t, err)
	assert.Equal(t, "pane line 1\npane line 2\n", stdout)
}

// A failed capture (the hub returns 200 with the error text as output and a
// non-zero exit code) is an error: nothing reaches stdout, and the error
// names the agent, the exit code and the first line of the output.
func TestLookViaHub_NonZeroExitCode(t *testing.T) {
	stdout, stderr, err := runLookViaHub(t, `{"output": "no server running on /tmp/tmux-1000/default\nsecond line\n", "exitCode": 1}`)
	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.NotContains(t, stderr, "second line")
	assert.Equal(t, "capturing terminal for agent 'worker' failed (exit 1): no server running on /tmp/tmux-1000/default", err.Error())
}

// A transport failure such as ssh exiting 255 with no output still fails,
// without a dangling colon.
func TestLookViaHub_NonZeroExitCodeNoOutput(t *testing.T) {
	stdout, _, err := runLookViaHub(t, `{"output": "", "exitCode": 255}`)
	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.Equal(t, "capturing terminal for agent 'worker' failed (exit 255)", err.Error())
}
