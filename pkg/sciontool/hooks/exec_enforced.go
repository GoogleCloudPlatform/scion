/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// EnforcedHooksDir is the dedicated, root-owned directory a
// privilege-drop-enforcing caller's bootstrap handler redirects
// broker-delivered $HOME/.scion/hooks/ content into, instead of chowning it
// to the workload (see pkg/sciontool/substrate's writeBootstrapFile for the
// current example). Kept as a single named identifier — not derived from
// anything else — so switching it later is a one-line change shared by
// both the writer and the reader (this package's LifecycleManager
// registration in cmd/sciontool/commands/init.go).
//
// Deliberately separate from /etc/scion/hooks (LifecycleManager's own
// system-default fallback): that directory's contents come from the image,
// not from a per-agent bootstrap write, and mixing the two would make a
// bootstrap-delivered file indistinguishable from one baked into the image.
//
// A package var, not a const, purely so a test can point it at a throwaway
// directory (e.g. to exercise skipRefusedEntry's EnforcedHooksDir exception
// without writing to the real, root-owned "/run/scion/hooks"). Production
// code never reassigns it. See pkg/sciontool/substrate's own private
// root-tmp-dir constant for the sibling directory kept under /run/scion for
// the same reason and with the same lifecycle: substrate-serve creates and
// clears both at every bootstrap.
var EnforcedHooksDir = "/run/scion/hooks"

// ErrScriptRefused is wrapped into the error openScriptNoFollow returns when
// the leaf itself — never a directory-chain component, which fails a
// different way (classifyChainOpenError) and is never treated as skippable
// — is a symlink or not a regular file. LifecycleManager's runScriptHooks
// uses errors.Is against this sentinel to decide whether a refused entry
// under a workload-owned hooks directory should be logged and skipped
// (letting the event's remaining hooks still run) instead of aborting the
// whole event — see skipRefusedEntry's own doc comment for why, and why
// EnforcedHooksDir is excluded from that treatment.
var ErrScriptRefused = errors.New("hooks: script is a symlink or not a regular file")

// openChainNoFollow opens every path component from "/" down to (and
// including) dir, refusing to follow a symlink at any component
// (O_NOFOLLOW), and returns each level's ownership (root-to-leaf order)
// alongside the final directory's own open fd (caller must close it).
//
// This is what makes the ownership chain DecideExecAsRoot checks the exact
// directories the script is opened under a moment later — not a directory a
// concurrent process swapped for a symlink between an earlier stat-by-path
// and this call. Every open is relative to the previous, already-verified
// fd (openat, never a fresh path lookup from "/"), so there is no window
// between checking a component and using it.
func openChainNoFollow(dir string) (fd int, chain []NodeOwnership, err error) {
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) {
		return -1, nil, fmt.Errorf("hooks: %q is not an absolute path", dir)
	}

	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

	currentFd, err := unix.Open("/", dirFlags, 0)
	if err != nil {
		return -1, nil, fmt.Errorf("hooks: open /: %w", err)
	}
	rootOwnership, err := fstatOwnership(currentFd)
	if err != nil {
		_ = unix.Close(currentFd)
		return -1, nil, fmt.Errorf("hooks: fstat /: %w", err)
	}
	chain = append(chain, rootOwnership)

	if clean == "/" {
		return currentFd, chain, nil
	}

	for _, comp := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		nextFd, openErr := unix.Openat(currentFd, comp, dirFlags, 0)
		_ = unix.Close(currentFd)
		if openErr != nil {
			return -1, nil, classifyChainOpenError(comp, openErr)
		}
		currentFd = nextFd
		ownership, statErr := fstatOwnership(currentFd)
		if statErr != nil {
			_ = unix.Close(currentFd)
			return -1, nil, fmt.Errorf("hooks: fstat %q: %w", comp, statErr)
		}
		chain = append(chain, ownership)
	}
	return currentFd, chain, nil
}

// openScriptNoFollow opens the leaf script by name, relative to the
// already-opened and verified parent directory fd, refusing to follow a
// symlink there (O_NOFOLLOW), never blocking on the open regardless of file
// type (O_NONBLOCK), and refusing anything that is not a regular file once
// opened — a FIFO, device, or socket is rejected the same way a symlink is,
// via ErrScriptRefused, never executed. Returns the open file (caller must
// close it) and its own ownership.
//
// The fd IS opened O_CLOEXEC: the caller never execs this fd directly at
// its own, arbitrary fd number (see execViaFd's doc comment for why — it
// passes the returned *os.File via exec.Cmd.ExtraFiles instead, which lands
// a fresh, independently-flagged duplicate at a fixed fd in the child
// regardless of this fd's own CLOEXEC bit). Keeping this copy CLOEXEC means
// it can never leak into
// any OTHER, unrelated child this process forks while it happens to be
// open — a hook script (which can carry secrets; 30-project-custom is
// 0700 for exactly that reason) would otherwise be readable by any such
// child, not just the one that is supposed to run it.
func openScriptNoFollow(parentFd int, name string) (f *os.File, ownership NodeOwnership, err error) {
	// O_NONBLOCK: without it, opening a workload-planted FIFO with no writer
	// blocks this open(2) call forever, hanging all hook processing — a
	// availability hole a workload can trigger just by mknod-ing a FIFO
	// where a hook script is expected. O_NONBLOCK makes the open return
	// immediately regardless of the file type; the fstat-and-reject-non-
	// regular check right below is what actually refuses the FIFO (or a
	// device or socket) once the open has returned. O_NONBLOCK has no effect
	// on a regular file's own I/O, so it changes nothing for the ordinary
	// case this function exists to handle.
	fd, err := unix.Openat(parentFd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, NodeOwnership{}, fmt.Errorf("hooks: script %q is a symlink; refusing: %w", name, ErrScriptRefused)
		}
		if errors.Is(err, unix.ENXIO) {
			// A socket special file (and some device files with no
			// corresponding device) fails open(2) itself with ENXIO, before
			// there is ever an fd to fstat — the regular-file check below
			// never gets a chance to run. Refuse it the same way, via the
			// same sentinel, rather than surfacing a bare "no such device or
			// address" as an unrelated I/O error.
			return nil, NodeOwnership{}, fmt.Errorf("hooks: %q cannot be opened as a regular file (ENXIO); refusing: %w", name, ErrScriptRefused)
		}
		return nil, NodeOwnership{}, err
	}
	var raw unix.Stat_t
	if statErr := unix.Fstat(fd, &raw); statErr != nil {
		_ = unix.Close(fd)
		return nil, NodeOwnership{}, statErr
	}
	if raw.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, NodeOwnership{}, fmt.Errorf("hooks: %q is not a regular file (mode %#o); refusing: %w", name, raw.Mode&unix.S_IFMT, ErrScriptRefused)
	}
	// os.NewFile, not the raw fd, from here on: it owns the fd's lifecycle
	// (Close clears the runtime finalizer it registers), and it is what
	// exec.Cmd.ExtraFiles requires.
	return os.NewFile(uintptr(fd), name), ownershipFromStat(raw), nil
}

// fstatOwnership Fstats an already-open directory fd and converts the
// result to NodeOwnership.
func fstatOwnership(fd int) (NodeOwnership, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return NodeOwnership{}, err
	}
	return ownershipFromStat(st), nil
}

func ownershipFromStat(st unix.Stat_t) NodeOwnership {
	return NodeOwnership{UID: st.Uid, Perm: uint32(st.Mode) & 0o7777}
}

// execFdSlot is the fixed fd number the script file lands at in the child
// process: exec.Cmd.ExtraFiles' entry 0 becomes fd 3 (0, 1, 2 are stdin/
// stdout/stderr), and execViaFd always passes exactly one file, so the
// child's copy is always fd 3 — never the parent's own, arbitrary fd
// number for f. execScriptPath is the fixed path that resolves it.
const execFdSlot = 3

var execScriptPath = fmt.Sprintf("/proc/self/fd/%d", execFdSlot)

// UnsupportedPlatformError is returned by execViaFd on any platform other
// than Linux. Running a verified hook script through its open fd relies on
// /proc/self/fd, which is Linux-specific; enforced hook execution fails
// closed there instead of falling back to a different, untested exec
// mechanism (such as /dev/fd) or to running the script by path.
type UnsupportedPlatformError struct {
	GOOS string
}

func (e *UnsupportedPlatformError) Error() string {
	return fmt.Sprintf("hooks: running a verified hook script by file descriptor is supported only on linux, not %s", e.GOOS)
}

// execViaFdGOOS is the platform execViaFd checks before building a command.
// Production always leaves it at runtime.GOOS; a test overrides it to
// exercise the non-Linux refusal on a Linux host.
var execViaFdGOOS = runtime.GOOS

// execViaFd builds an *exec.Cmd that runs the file referenced by the
// already-open, already-verified f — never displayPath, which is used only
// for argv[0]/logging and is never itself opened or resolved again.
//
// This is the fexecve(3)-equivalent trick for a language (Go) whose os/exec
// has no native fexecve: /proc/self/fd/<n> is a magic symlink the kernel
// resolves directly to the fd's own open file description, not through a
// further filesystem path lookup, so execve() on it runs exactly the inode
// f refers to regardless of what (if anything) now sits at the script's
// original path.
//
// f is passed via cmd.ExtraFiles, not opened without O_CLOEXEC and exec'd
// at its own fd number: ExtraFiles makes exec.Cmd dup f into a FRESH file
// descriptor at a fixed slot (execFdSlot, i.e. fd 3) in the child, post-fork
// and pre-exec — a duplicate with its own independent close-on-exec flag
// (cleared by that dup, regardless of f's own), so it correctly survives
// the child's own subsequent exec (needed for a shebang script: the kernel
// hands the interpreter that same path, which must still resolve). f itself
// stays exactly as CLOEXEC as openScriptNoFollow opened it throughout: it
// is never inherited by any OTHER, unrelated child this process might fork,
// only by this one, through the explicit ExtraFiles hand-off: execing
// "/proc/self/fd/<original-number>" directly would instead require clearing
// f's own CLOEXEC bit, exposing it to every other child this process forks
// for as long as f stays open.
//
// On any platform other than Linux it returns an *UnsupportedPlatformError
// and no command: /proc/self/fd is Linux-specific, and the enforced hook
// path fails closed rather than exec the script some other way.
func execViaFd(f *os.File, displayPath string) (*exec.Cmd, error) {
	if execViaFdGOOS != "linux" {
		return nil, &UnsupportedPlatformError{GOOS: execViaFdGOOS}
	}
	cmd := exec.Command(execScriptPath)
	cmd.Args = []string{displayPath}
	cmd.ExtraFiles = []*os.File{f}
	return cmd, nil
}

// fdIsExecutable Fstats an already-open file and reports whether any of the
// owner/group/other execute bits is set.
func fdIsExecutable(f *os.File) (bool, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return false, err
	}
	return st.Mode&0o111 != 0, nil
}

// closeFd closes a raw file descriptor opened by this file's helpers.
func closeFd(fd int) error {
	return unix.Close(fd)
}

// classifyChainOpenError turns the errno openat(O_DIRECTORY|O_NOFOLLOW)
// returns for "this component isn't usable" into a clear, content-free
// error. On Linux, a symlink or a plain file blocking the path can both
// surface as ENOTDIR (the kernel checks O_DIRECTORY before O_NOFOLLOW) or
// ELOOP depending on the exact component; either way the open has already
// been correctly refused, so this only affects the error's wording.
func classifyChainOpenError(comp string, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return fmt.Errorf("hooks: path component %q is a symlink or not a directory; refusing", comp)
	}
	return fmt.Errorf("hooks: open path component %q: %w", comp, err)
}
