/*
Copyright 2025 The Scion Authors.
*/

package procreap

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// managedPIDs tracks the PIDs of child processes that are currently owned by
// an in-flight os/exec.Cmd anywhere in the process, keyed by pid, with the
// *Token returned by the matching RegisterManagedPID call as the value.
//
// The reaper (below) must never call wait4 on a PID in this set: exec.Cmd's
// own Wait call is the sole intended reaper for it, via a PID-specific
// wait4(pid, ...). The kernel only lets one waiter observe a given child's
// exit status; a concurrent, generic wait4(-1, ...) racing that PID-specific
// call is exactly what produced the intermittent "waitid: no child
// processes" (ECHILD) failures observed during agent clone.
// RegisterManagedPID/UnregisterManagedPID (used by RunManaged and friends in
// managed.go) keep this set in sync with in-flight exec.Cmd calls.
var managedPIDs sync.Map // map[int]*Token

// execGate closes the registration gap between a managed process's Start()
// returning and its PID landing in managedPIDs: RunManaged, Supervisor.Run,
// and services.managedService.start all hold execGate.RLock() across that
// window (see Gated), and reapUnmanagedZombies holds execGate.Lock() around
// its whole scan-and-reap pass. Since a write lock excludes all read locks,
// the reaper can never observe a "just exited, not yet registered" PID —
// either the Start()+Register() sequence completes (and the PID is
// visible in managedPIDs) before the reaper's pass begins, or the reaper's
// pass finishes (and releases the lock) before that sequence is allowed to
// start. Without this, a child that manages to exit in the brief window
// between Start() returning and RegisterManagedPID being called can still
// be stolen by the reaper — this was observed in practice via a
// short-lived `sh -c` child racing a live reaper goroutine.
var execGate sync.RWMutex

// Gated runs start (expected to call cmd.Start() and, on success,
// RegisterManagedPID(cmd.Process.Pid)) while holding execGate for reading,
// so no reap pass can run concurrently with it.
func Gated(start func() error) error {
	execGate.RLock()
	defer execGate.RUnlock()
	return start()
}

// Token is an opaque per-registration handle returned by RegisterManagedPID
// and required by UnregisterManagedPID. Its identity (not its pid) is what
// UnregisterManagedPID matches against, which closes a PID-reuse race:
// once a Wait call returns, the kernel is free to hand that same PID to a
// brand new process immediately — including one registered by a *different*
// concurrent RunManaged/Supervisor.Run/service-start call before the first
// call's deferred Unregister gets scheduled. A pid-only Delete(pid) would
// then delete the new registration out from under its own still-running
// owner, leaving the reaper free to steal it exactly like the race this
// package exists to prevent. Comparing the token (a distinct pointer per
// registration) via CompareAndDelete means an unregister call can only ever
// remove the registration it was actually issued for.
//
// The struct is deliberately non-empty ("_ byte"): Go's runtime is free to
// back every *zero-size* allocation with the same address (the well-known
// "zerobase" optimization), so two distinct RegisterManagedPID calls could
// otherwise return pointers that compare equal, silently defeating the
// per-registration identity this type exists to provide.
type Token struct{ _ byte }

// RegisterManagedPID marks pid as owned by an in-flight exec.Cmd, so the
// SIGCHLD reaper leaves it alone. Callers must call UnregisterManagedPID
// with the returned token once the owning Cmd.Wait call returns, regardless
// of outcome.
func RegisterManagedPID(pid int) *Token {
	tok := &Token{}
	managedPIDs.Store(pid, tok)
	return tok
}

// UnregisterManagedPID releases the registration identified by tok, as
// returned by the matching RegisterManagedPID call. If pid has since been
// reused by a different, newer registration, tok will not match its current
// token and this is a safe no-op — see Token's doc comment.
func UnregisterManagedPID(pid int, tok *Token) {
	managedPIDs.CompareAndDelete(pid, tok)
}

// isManagedPID reports whether pid is currently owned by an in-flight
// exec.Cmd started through RunManaged, CombinedOutputManaged, or
// OutputManaged.
func isManagedPID(pid int) bool {
	_, ok := managedPIDs.Load(pid)
	return ok
}

// zombieProc pairs a zombie child's PID with its process name (best-effort;
// "unknown" if the name could not be read), as observed during a single
// scanZombies pass.
type zombieProc struct {
	pid  int
	name string
}

// parseProcPID parses a /proc directory entry name as a process ID, and
// reports whether it identifies a process scanZombies should ever consider:
// non-numeric entries (most of /proc's other files) are rejected, and so is
// PID 1 — init (this process, when running as PID 1) is never a reapable
// child of itself.
func parseProcPID(name string) (int, bool) {
	pid, err := strconv.Atoi(name)
	if err != nil || pid <= 1 {
		return 0, false
	}
	return pid, true
}

// scanZombies does one /proc walk and returns every current zombie child
// along with its process name, so callers never need a second walk just to
// resolve names. Reading /proc/<pid>/comm only for entries already confirmed
// zombie by /proc/<pid>/stat (rather than for every process in /proc) keeps
// that second read cheap: it happens only for the processes a reap pass is
// actually going to act on.
func scanZombies() []zombieProc {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var zombies []zombieProc
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, ok := parseProcPID(entry.Name())
		if !ok {
			continue
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		if !isZombieStat(string(stat)) {
			continue
		}
		name := "unknown"
		if comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
			if n := strings.TrimSpace(string(comm)); n != "" {
				name = n
			}
		}
		zombies = append(zombies, zombieProc{pid: pid, name: name})
	}
	return zombies
}

// isZombieStat reports whether the contents of a /proc/<pid>/stat file
// describe a zombie process. The state field follows the last ')': the comm
// field is parenthesized and may itself contain spaces or parens, so the
// state cannot be found by naive whitespace splitting from the front.
func isZombieStat(stat string) bool {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return false
	}
	return stat[i+2] == 'Z'
}

// StartReaper starts a goroutine that reaps zombie child processes which are
// not currently owned by an in-flight exec.Cmd, and logs them.
//
// This is critical when running as PID 1 in a container: orphaned
// grandchildren reparent to PID 1 when their original parent exits, and
// PID 1 must reap them or they accumulate as zombies forever. But this
// process (sciontool init) also runs plenty of its own tracked subprocesses
// directly via exec.Cmd (git during workspace clone, the supervised harness
// child) — those must be left for their owning Cmd.Wait call to reap.
// Reaping them here too, via a generic wait4(-1, ...), races Cmd.Wait for
// the exact same PID and intermittently steals the exit status the owner
// was waiting for, which surfaces as "waitid: no child processes" (ECHILD)
// from exec.Cmd.Wait. Filtering to only the zombie PIDs that no exec.Cmd
// currently claims (see managedPIDs) avoids that race while still
// fulfilling PID 1's reaping duty for everything else.
func StartReaper() {
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGCHLD)

		for range sigs {
			reapUnmanagedZombies()
		}
	}()
}

// reapUnmanagedZombies reaps every current zombie child except those
// registered as owned by an in-flight exec.Cmd. It is split out from
// StartReaper so tests can drive it directly without signal delivery.
func reapUnmanagedZombies() {
	// Exclude any in-flight Start()+RegisterManagedPID sequence for the
	// duration of this pass — see execGate's doc comment.
	execGate.Lock()
	defer execGate.Unlock()

	for _, z := range scanZombies() {
		if isManagedPID(z.pid) {
			// An in-flight exec.Cmd owns this PID; its own Wait call will
			// reap it. Stealing it here would race that call.
			continue
		}

		var ws syscall.WaitStatus
		reapedPID, err := syscall.Wait4(z.pid, &ws, syscall.WNOHANG, nil)
		if err != nil || reapedPID <= 0 {
			// Already reaped by someone else (e.g. it became managed and
			// was waited on between our scan and this call), or WNOHANG
			// found it not actually reapable yet — either way, skip.
			continue
		}

		reason := "exited"
		if ws.Signaled() {
			reason = "killed by signal " + ws.Signal().String()
		}
		log.Info("Reaped zombie process %d (%s) (reason: %s, exit code: %d)", z.pid, z.name, reason, ws.ExitStatus())
	}
}
