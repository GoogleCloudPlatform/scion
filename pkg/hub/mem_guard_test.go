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

package hub

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const memGuardChildEnv = "SCION_HUB_TEST_MEM_GUARD_CHILD"

// TestMemGuard_TripsOnlyAboveLimit drives the guard loop with a fake sampler.
func TestMemGuard_TripsOnlyAboveLimit(t *testing.T) {
	t.Parallel()
	samples := []uint64{10, 100, 50, 101, 500}
	i := 0
	sample := func() (uint64, string, bool) {
		// Past the end, report no signal: the guard logs and stops, and the
		// empty-log assertion below fails instead of the test panicking.
		if i >= len(samples) {
			return 0, "fake", false
		}
		v := samples[i]
		i++
		return v, "fake", true
	}
	var gotUsed, gotLimit uint64
	calls := 0
	abort := func(used, limit uint64, source string) {
		calls++
		gotUsed, gotLimit = used, limit
		if source != "fake" {
			t.Errorf("abort source = %q, want fake", source)
		}
	}
	var log bytes.Buffer
	runMemGuard(make(chan struct{}), &log, time.Millisecond, 100, sample, abort)
	if log.Len() != 0 {
		t.Errorf("unexpected guard output: %q", log.String())
	}
	if calls != 1 || gotUsed != 101 || gotLimit != 100 {
		t.Fatalf("abort calls=%d used=%d limit=%d; want one call at 101 > 100", calls, gotUsed, gotLimit)
	}
	if i != 4 {
		t.Fatalf("sampled %d times, want 4 (stop at the first sample over the limit)", i)
	}
}

// TestMemGuard_StopsWithoutSignal checks that the loop exits instead of
// spinning when no memory signal is available.
func TestMemGuard_StopsWithoutSignal(t *testing.T) {
	t.Parallel()
	called := false
	var log bytes.Buffer
	runMemGuard(make(chan struct{}), &log, time.Millisecond, 1,
		func() (uint64, string, bool) { return 0, "", false },
		func(uint64, uint64, string) { called = true })
	if called {
		t.Fatal("abort called with no memory signal")
	}
	if !strings.Contains(log.String(), "guard disabled") {
		t.Errorf("guard output = %q, want a guard-disabled notice", log.String())
	}
}

// TestMemGuard_StartedByTestMain fails if a TestMain stops calling
// startMemGuard. The file has no build tag, so it checks both TestMains.
func TestMemGuard_StartedByTestMain(t *testing.T) {
	if os.Getenv(memGuardCapEnv) == "0" {
		t.Skip("memory guard disabled by " + memGuardCapEnv)
	}
	if !memGuardRunning.Load() {
		t.Fatal("TestMain did not start the memory guard (startMemGuard)")
	}
}

// TestMemGuard_UsageReportsMemory checks that the real sampler sees this
// process, which uses far more than 1 MiB.
func TestMemGuard_UsageReportsMemory(t *testing.T) {
	t.Parallel()
	used, source, ok := memGuardUsage()
	if !ok || used < 1<<20 || source == "" {
		t.Fatalf("memGuardUsage() = %d, %q, %v; want > 1 MiB from a named source", used, source, ok)
	}
}

// TestMemGuard_AbortsRunawayProcess re-runs this test binary with the
// TestMain guard disabled. The child starts its own guard with a 1-byte cap,
// so the real sampler and abort path fire while the child test is running,
// without allocating anything. The child must exit 3 with the banner, a heap
// profile and goroutine stacks that name it.
func TestMemGuard_AbortsRunawayProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMemGuard_RunawayChild$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		memGuardChildEnv+"=1",
		memGuardCapEnv+"=0",
		memGuardIntervalEnv+"=10ms",
		// The child exits without TestMain teardown, so it must not create
		// an integration-build Postgres database that nothing drops.
		"SCION_TEST_POSTGRES_URL=",
		"TMPDIR="+t.TempDir(),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("child: want exit code 3 from the memory guard, got err=%v\nstderr:\n%s", err, stderr.String())
	}
	out := stderr.String()
	for _, want := range []string{"exceeds cap 0 MiB", "heap profile written to", "TestMemGuard_RunawayChild"} {
		if !strings.Contains(out, want) {
			t.Errorf("child stderr missing %q\nstderr:\n%s", want, out)
		}
	}
}

// TestMemGuard_RunawayChild is the subprocess body for
// TestMemGuard_AbortsRunawayProcess. It does nothing in a normal run.
func TestMemGuard_RunawayChild(t *testing.T) {
	if os.Getenv(memGuardChildEnv) != "1" {
		t.Skip("subprocess helper for TestMemGuard_AbortsRunawayProcess")
	}
	t.Setenv(memGuardCapEnv, "1")
	stop := startMemGuard()
	defer stop()
	time.Sleep(10 * time.Second)
	t.Fatal("memory guard did not abort within 10s at a 1-byte cap")
}
