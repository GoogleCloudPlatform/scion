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

//go:build linux

package liveness

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Against the real /proc: a child process burning CPU is live work.
func TestSample_RealProcBusyChild(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	s := NewSampler(Config{AgentUID: os.Getuid(), IsManagedPID: func(int) bool { return false }})
	_ = s.Sample() // baseline

	// A shell busy loop for about a second: well over MinCPUTicks.
	cmd := exec.Command("sh", "-c", `end=$(( $(date +%s) + 2 )); while [ $(date +%s) -lt $end ]; do :; done`)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	var lw *LiveWork
	deadline := time.Now().Add(5 * time.Second)
	for lw == nil && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		lw = s.Sample()
	}
	require.NotNil(t, lw, "a CPU-busy child must be seen as live work")
	assert.Equal(t, KindProcess, lw.Kind)
	assert.True(t, lw.SuppressStall)
}
