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
	"fmt"
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
	_ = s.Sample(time.Now(), "", time.Time{}) // baseline

	// A fork-free shell busy loop (builtin arithmetic only), so the CPU is
	// spent in the sampled process itself rather than in short-lived
	// children that exit between samples.
	cmd := exec.Command("sh", "-c", `i=0; while :; do i=$((i+1)); done`)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	// Wait for the child itself to accumulate well over MinCPUTicks
	// rather than for a fixed wall-clock time: a container with a CPU
	// quota under load may give it only a sliver of a core.
	statPath := fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		b, err := os.ReadFile(statPath)
		require.NoError(t, err)
		p, ok := parseStat(string(b))
		require.True(t, ok)
		if p.ticks >= 2*MinCPUTicks {
			break
		}
		require.True(t, time.Now().Before(deadline), "the busy child got %d ticks in 2 minutes", p.ticks)
		time.Sleep(200 * time.Millisecond)
	}
	lw := s.Sample(time.Now(), "", time.Time{})
	require.NotNil(t, lw, "a CPU-busy child must be seen as live work")
	assert.Equal(t, KindProcess, lw.Kind)
	assert.True(t, lw.SuppressStall)
}

// The real /proc formats Tier 1 reads: the boot time and this process's
// I/O counters.
func TestRealProcBootTimeAndIO(t *testing.T) {
	boot := readBootTime("/proc")
	require.False(t, boot.IsZero(), "/proc/stat has no btime")
	assert.True(t, boot.Before(time.Now()))

	n, ok := readIOChars("/proc", os.Getpid())
	if !ok {
		t.Skip("/proc/self/io unreadable here (hardened runtime)")
	}
	assert.Positive(t, n, "a running test has read something")
}
