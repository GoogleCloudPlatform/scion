/*
Copyright 2025 The Scion Authors.
*/

package procreap

import (
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestNaiveGlobalReaper_RacesCmdWait reproduces the underlying kernel/Go
// mechanism behind the PID-1 reaper race: a SIGCHLD handler that reaps *any*
// exited child via wait4(-1, WNOHANG) — the pre-fix behavior of
// StartReaper — races any concurrently-running exec.Cmd.Wait for the same
// PID. Whichever call wins the race reaps the process; the other observes
// ECHILD, which Go surfaces from Cmd.Wait as an error containing "no child
// processes" — exactly the "waitid: no child processes" errors reported
// from broker-01.
//
// This test intentionally does NOT exercise our package's reaper (that's
// TestRunManaged_SurvivesReaperRace below). It isolates the general
// mechanism to document why any process that both (a) reaps children
// generically via SIGCHLD and (b) runs its own tracked exec.Cmd
// subprocesses — exactly what cmd/sciontool/commands/init.go's
// gitCloneWorkspace does, from the same process as StartReaper — cannot
// safely use a naive "reap everything" loop.
func TestNaiveGlobalReaper_RacesCmdWait(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("wait4/SIGCHLD semantics are linux-specific")
	}

	stop := make(chan struct{})
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGCHLD)
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		for {
			select {
			case <-stop:
				return
			case <-sigs:
				for {
					var ws syscall.WaitStatus
					pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
					if err != nil || pid <= 0 {
						break
					}
				}
			}
		}
	}()
	// Must join the goroutine, not just signal it to stop: sigs has a
	// buffer of 1, so a SIGCHLD can still be sitting in it when the test
	// returns, and select can pick that ready case over a concurrently
	// closed stop. An unjoined goroutine could then run one more
	// wait4(-1, WNOHANG) loop after this test has already returned —
	// reaping whatever the *next* test's children happen to be at that
	// moment and handing them ECHILD. Blocking on reaperDone (as
	// TestRunManaged_SurvivesReaperRace below already does) is what
	// actually prevents that: this naive reaper cannot outlive the test.
	t.Cleanup(func() {
		signal.Stop(sigs)
		close(stop)
		<-reaperDone
	})

	const iterations = 300
	var echildCount int64
	var wg sync.WaitGroup
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command("true")
			if err := cmd.Start(); err != nil {
				return
			}
			if err := cmd.Wait(); err != nil && strings.Contains(err.Error(), "no child processes") {
				atomic.AddInt64(&echildCount, 1)
			}
		}()
	}
	wg.Wait()

	if echildCount == 0 {
		t.Skip("race did not reproduce on this run (inherently timing-dependent); " +
			"this does not mean the mechanism is safe, only that this run got lucky. Rerun, " +
			"or increase `iterations`, if this skips consistently in CI.")
	}
	t.Logf("naive global reaper stole %d/%d child exit statuses (surfaced as ECHILD from Cmd.Wait)", echildCount, iterations)
}

// TestRunManaged_SurvivesReaperRace exercises the actual fix end-to-end:
// with a SIGCHLD reaper driven by the real reapUnmanagedZombies function
// running concurrently, many concurrent RunManaged calls (used by
// gitCloneWorkspace's git subprocesses and by the Supervisor's own child)
// must never see their exit status stolen, because RunManaged registers
// each PID as managed — under execGate, closing the Start()-to-register
// race too — before the reaper can observe it exit, and
// reapUnmanagedZombies skips managed PIDs.
//
// The reaper goroutine here is scoped to this test (started and stopped
// within it) rather than calling the package's StartReaper, which runs for
// the life of the process — leaving that running would make it race
// unrelated tests' plain (non-managed) exec.Cmd calls elsewhere in this
// package, and would swallow the zombie state
// TestReapUnmanagedZombies_ReapsUnmanagedZombie needs to observe.
//
// Unlike TestNaiveGlobalReaper_RacesCmdWait, this test's correctness does
// not depend on winning a timing race: the managed-PID skip is structural,
// so this must pass deterministically (run with -race too).
func TestRunManaged_SurvivesReaperRace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("wait4/SIGCHLD semantics are linux-specific")
	}

	stop := make(chan struct{})
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGCHLD)
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		for {
			select {
			case <-stop:
				return
			case <-sigs:
				reapUnmanagedZombies()
			}
		}
	}()
	t.Cleanup(func() {
		signal.Stop(sigs)
		close(stop)
		<-reaperDone
	})

	const iterations = 300
	var wg sync.WaitGroup
	errs := make(chan error, iterations)
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command("true")
			errs <- RunManaged(cmd)
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("RunManaged returned unexpected error under concurrent reaper: %v", err)
		}
	}
}

// TestRunManaged_SurvivesConcurrentReapPasses stresses execGate directly,
// independent of SIGCHLD delivery/coalescing timing: a goroutine calls
// reapUnmanagedZombies in a tight loop (far more often than any real
// SIGCHLD stream would) concurrently with many RunManaged calls. This is
// the most direct regression test for the Start()-to-register race fixed
// by execGate: without it, this test reliably fails.
func TestRunManaged_SurvivesConcurrentReapPasses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("wait4/SIGCHLD semantics are linux-specific")
	}

	stop := make(chan struct{})
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		for {
			select {
			case <-stop:
				return
			default:
				reapUnmanagedZombies()
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-reaperDone
	})

	const iterations = 300
	var wg sync.WaitGroup
	errs := make(chan error, iterations)
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command("true")
			errs <- RunManaged(cmd)
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("RunManaged returned unexpected error under a tight concurrent reap loop: %v", err)
		}
	}
}

// TestReapUnmanagedZombies_SkipsManagedPID is a more direct, non-signal-timing
// unit test of the same guarantee: a PID registered as managed must survive
// a reap pass even while it sits as a zombie (i.e. its owner hasn't called
// Wait yet), so the eventual owner Wait() call still succeeds.
func TestReapUnmanagedZombies_SkipsManagedPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("wait4/SIGCHLD semantics are linux-specific")
	}

	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start test process: %v", err)
	}
	pid := cmd.Process.Pid
	tok := RegisterManagedPID(pid)
	defer UnregisterManagedPID(pid, tok)

	waitUntilZombie(t, pid)

	// Simulate a reap pass happening concurrently with the (not-yet-called)
	// owner Wait().
	reapUnmanagedZombies()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("owner Cmd.Wait() failed after a concurrent reap pass for a managed pid: %v", err)
	}
}

// TestReapUnmanagedZombies_ReapsUnmanagedZombie verifies the reaper still
// fulfills its actual job: a zombie that nobody has registered or is
// waiting for (a genuine orphan/grandchild reparented to PID 1) must still
// be reaped.
func TestReapUnmanagedZombies_ReapsUnmanagedZombie(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("wait4/SIGCHLD semantics are linux-specific")
	}

	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start test process: %v", err)
	}
	pid := cmd.Process.Pid
	// Deliberately never register or Wait() — this simulates an orphan.

	waitUntilZombie(t, pid)

	reapUnmanagedZombies()

	for _, p := range zombiePIDs() {
		if p == pid {
			t.Fatalf("pid %d is still a zombie after reapUnmanagedZombies", pid)
		}
	}
}

// waitUntilZombie polls /proc until pid is observed in zombie state, or
// fails the test after a timeout.
func waitUntilZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range zombiePIDs() {
			if p == pid {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pid %d never became a zombie within the timeout", pid)
}

// zombiePIDs scans /proc and returns the PIDs of processes currently in
// zombie state. It exists only for tests (waitUntilZombie above, and
// TestReapUnmanagedZombies_ReapsUnmanagedZombie): production code calls
// scanZombies directly since it also needs each zombie's name.
func zombiePIDs() []int {
	zombies := scanZombies()
	pids := make([]int, len(zombies))
	for i, z := range zombies {
		pids[i] = z.pid
	}
	return pids
}
