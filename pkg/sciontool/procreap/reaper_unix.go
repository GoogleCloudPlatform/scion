//go:build unix

/*
Copyright 2025 The Scion Authors.
*/

package procreap

import (
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"os"
	"os/signal"
	"syscall"
)

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
