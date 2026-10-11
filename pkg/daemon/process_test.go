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

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchesCommand(t *testing.T) {
	want := []string{"server", "start", "--foreground"}
	tests := []struct {
		name string
		args []string
		exe  string
		ok   bool
	}{
		{"exact", []string{"/usr/local/bin/scion", "server", "start", "--foreground", "--enable-hub"}, "/usr/local/bin/scion", true},
		{"other path same binary", []string{"/opt/a/scion", "server", "start", "--foreground"}, "/opt/b/scion", true},
		{"other binary", []string{"/usr/bin/sleep", "server", "start", "--foreground"}, "/usr/local/bin/scion", false},
		{"other subcommand", []string{"/usr/local/bin/scion", "server", "status"}, "/usr/local/bin/scion", false},
		{"not foreground", []string{"/usr/local/bin/scion", "server", "start"}, "/usr/local/bin/scion", false},
		{"empty args", nil, "/usr/local/bin/scion", false},
		{"empty executable", []string{"scion", "server", "start", "--foreground"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.ok, MatchesCommand(tt.args, tt.exe, want...))
		})
	}
}

// TestProcessArgs_Self reads the command line of the test process itself.
func TestProcessArgs_Self(t *testing.T) {
	args, err := ProcessArgs(os.Getpid())
	require.NoError(t, err)
	require.NotEmpty(t, args)
	assert.Equal(t, filepath.Base(os.Args[0]), filepath.Base(args[0]))
}

// TestIsProcessCommand_Child identifies a real child by its command line and
// rejects it for a different executable.
func TestIsProcessCommand_Child(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	cmd := exec.Command(sleepPath, "30")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	ok, err := IsProcessCommand(cmd.Process.Pid, sleepPath, "30")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = IsProcessCommand(cmd.Process.Pid, "/usr/local/bin/scion", "server", "start", "--foreground")
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestIsProcessCommand_Unreadable refuses a process it cannot identify.
func TestIsProcessCommand_Unreadable(t *testing.T) {
	for _, pid := range []int{0, -1} {
		ok, err := IsProcessCommand(pid, "/usr/local/bin/scion")
		assert.Error(t, err)
		assert.False(t, ok)
	}

	// A process that has exited and been reaped.
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	ok, err := IsProcessCommand(cmd.Process.Pid, "/usr/local/bin/scion")
	assert.Error(t, err)
	assert.False(t, ok)
}
